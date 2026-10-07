package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ILinkClient 微信 iLink Bot API 客户端
type ILinkClient struct {
	baseURL  string // 登录后返回的 baseurl，默认 https://ilinkai.weixin.qq.com
	botToken string
	botID    string // ilink_bot_id
	userID   string // ilink_user_id
	http     *http.Client
	mu       sync.RWMutex
	// context_token 按用户持久化，重启后可继续回复
	contextTokens map[string]string
	tokenPath     string
	// 消息去重（5分钟滑窗）。语义重要变更：只记录**已成功处理**的消息，
	// 绝不在处理之前写入（旧实现在 GetUpdates 里就标记已见，处理失败即永久丢消息）。
	recentIDs map[string]time.Time
	// 长轮询游标（= 已提交游标，只在整批处理成功后推进）
	cursor string
	// 待提交游标：GetUpdates 记下服务端返回的新游标，CommitCursor() 才正式提交并落盘
	pendingBuf string
	// 会话过期标记
	sessionExpired bool
	// 重绑（网页扫码）状态；用独立锁，避免占用 c.mu 影响收发主链路
	rebindMu     sync.Mutex
	rebindActive bool
	rebindStatus string // idle|fetching|waiting|scaned|confirmed|expired|failed
	rebindQRData string // data:image/png;base64,...
	rebindError  string
	rebindAt     time.Time
	// 优雅关闭：取消长轮询中的 HTTP 请求
	ctx    context.Context
	cancel context.CancelFunc
}

// ILinkCredentials 登录凭据（持久化到磁盘）
type ILinkCredentials struct {
	BotToken string `json:"bot_token"`
	BotID    string `json:"ilink_bot_id"`
	UserID   string `json:"ilink_user_id"`
	BaseURL  string `json:"baseurl"`
}

// ILinkMessage 收到的一条消息
type ILinkMessage struct {
	FromUserID   string `json:"from_user_id"`
	ToUserID     string `json:"to_user_id"`
	ContextToken string `json:"context_token"`
	// message_id 实际是 int64（如 7511443439720901896），不是 string；
	// 用 json.Number 兼容数字/字符串两种形态，取用时统一 .String()
	MessageID json.Number `json:"message_id"`
	// 时间字段实际是 create_time_ms（毫秒），没有 create_time
	CreateTimeMs int64       `json:"create_time_ms"`
	ItemList     []ILinkItem `json:"item_list"`
}

// ILinkItem 消息内容项
//
// 文本在 text_item 字段而不是 text：
//
//	{"type":1,...,"text_item":{"text":"帮助"}}
type ILinkItem struct {
	Type      int         `json:"type"` // 1=text, 2=image, 3=file, 4=voice, 5=video
	Text      *ILinkText  `json:"text_item,omitempty"`
	ImageItem *ILinkMedia `json:"image_item,omitempty"`
	FileItem  *ILinkMedia `json:"file_item,omitempty"`
	VoiceItem *ILinkMedia `json:"voice_item,omitempty"`
	VideoItem *ILinkMedia `json:"video_item,omitempty"`
}

// ILinkText 文本内容
type ILinkText struct {
	Text string `json:"text"`
}

// ILinkMedia 媒体内容（CDN 引用）
type ILinkMedia struct {
	Media struct {
		EncryptQueryParam string `json:"encrypt_query_param"`
		AESKey            string `json:"aes_key"`
	} `json:"media"`
	FileName string `json:"file_name,omitempty"`
}

// 通用响应结构
type ilinkResponse struct {
	Ret     int    `json:"ret"`
	Errcode int    `json:"errcode"`
	Errmsg  string `json:"errmsg"`
}

// getUpdatesResponse getupdates 返回
type getUpdatesResponse struct {
	ilinkResponse
	Msgs          []ILinkMessage `json:"msgs"`
	GetUpdatesBuf string         `json:"get_updates_buf"`
}

// NewILinkClient 创建 iLink 客户端
func NewILinkClient(tokenPath string) *ILinkClient {
	ctx, cancel := context.WithCancel(context.Background())
	return &ILinkClient{
		baseURL:       "https://ilinkai.weixin.qq.com",
		http:          &http.Client{Timeout: 60 * time.Second},
		tokenPath:     tokenPath,
		contextTokens: make(map[string]string),
		recentIDs:     make(map[string]time.Time),
		ctx:           ctx,
		cancel:        cancel,
	}
}

// Shutdown 取消所有进行中的 HTTP 请求（用于优雅关闭）
func (c *ILinkClient) Shutdown() {
	c.cancel()
}

// LoadCredentials 从磁盘加载登录凭据
func (c *ILinkClient) LoadCredentials() error {
	data, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return err
	}
	var cred ILinkCredentials
	if err := json.Unmarshal(data, &cred); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.botToken = cred.BotToken
	c.botID = cred.BotID
	c.userID = cred.UserID
	if cred.BaseURL != "" {
		c.baseURL = cred.BaseURL
	}
	return nil
}

// SaveCredentials 保存登录凭据到磁盘
func (c *ILinkClient) SaveCredentials() error {
	c.mu.RLock()
	cred := ILinkCredentials{
		BotToken: c.botToken,
		BotID:    c.botID,
		UserID:   c.userID,
		BaseURL:  c.baseURL,
	}
	c.mu.RUnlock()
	data, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return err
	}
	// 凭据文件损坏等于要重新扫码登录，用原子写避免半截 JSON
	return writeFileAtomic(c.tokenPath, data)
}

// IsLoggedIn 是否已登录
func (c *ILinkClient) IsLoggedIn() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.botToken != "" && c.botID != ""
}

// wechatUin 生成 X-WECHAT-UIN 头
func wechatUin() string {
	var b [4]byte
	rand.Read(b[:])
	u := binary.LittleEndian.Uint32(b[:])
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%d", u)))
}

// doRequest 发起 API 请求
func (c *ILinkClient) doRequest(method, path string, body interface{}, result interface{}) error {
	c.mu.RLock()
	token := c.botToken
	base := c.baseURL
	c.mu.RUnlock()

	var reqBody []byte
	var err error
	if body != nil {
		reqBody, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}

	req, err := http.NewRequest(method, base+path, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req = req.WithContext(c.ctx)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("AuthorizationType", "ilink_bot_token")
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-WECHAT-UIN", wechatUin())

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	// 网关返回非 2xx（如 401 token 失效、502 服务不可用）时必须报错，
	// 否则 body 恰好是合法 JSON 时 resp.Ret==0 会被当成"成功但无消息"，长轮询静默空转
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(respBody))
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		return fmt.Errorf("%s %s 返回 HTTP %d: %s", method, path, resp.StatusCode, msg)
	}

	if result != nil {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("解析响应失败: %w, body=%s", err, string(respBody))
		}
	}

	// 检查会话过期
	var baseResp ilinkResponse
	if err := json.Unmarshal(respBody, &baseResp); err == nil {
		if baseResp.Errcode == -14 || baseResp.Ret == -14 {
			c.mu.Lock()
			c.sessionExpired = true
			c.mu.Unlock()
			return fmt.Errorf("会话已过期(errcode=-14)，请重新扫码登录")
		}
	}

	return nil
}

// QRCodeResponse 获取二维码返回。
//
// 实测 2026-10：/api/v1/wechat/qrcode 已下线（返回 404 空 body），现行接口是
// GET /ilink/bot/get_bot_qrcode?bot_type=3，且响应是扁平结构，没有 data 包裹：
//
//	{"ret":0,"qrcode":"<32位hex>","qrcode_img_content":"https://liteapp.weixin.qq.com/q/xxx?qrcode=...&bot_type=3"}
//
// 二维码链接字段叫 qrcode_img_content，不是旧文档里的 qrcode_url。
type QRCodeResponse struct {
	ilinkResponse
	QRCodeURL string `json:"qrcode_img_content"`
	QRCode    string `json:"qrcode"`
}

// GetQRCode 获取登录二维码
func (c *ILinkClient) GetQRCode() (*QRCodeResponse, error) {
	var resp QRCodeResponse
	// 登录接口不需要 token：doRequest 在 botToken 为空时不会带 Authorization 头。
	// bot_type=3 是 ClawBot 的固定取值。
	if err := c.doRequest("GET", "/ilink/bot/get_bot_qrcode?bot_type=3", nil, &resp); err != nil {
		return nil, err
	}
	if resp.Ret != 0 {
		return nil, fmt.Errorf("获取二维码返回 ret=%d errmsg=%s", resp.Ret, resp.Errmsg)
	}
	if resp.QRCode == "" {
		return nil, fmt.Errorf("获取二维码成功但响应里没有 qrcode")
	}
	return &resp, nil
}

// QRCodeStatusResponse 二维码状态返回（同样是扁平结构，无 data 包裹，也没有 credentials 子对象）。
//
//	等待中: {"ret":0,"status":"wait"}
//	已扫码: {"ret":0,"status":"scaned"}
//	已确认: {"ret":0,"status":"confirmed","bot_token":"...","ilink_bot_id":"...","ilink_user_id":"...","baseurl":"https://ilinkai.weixin.qq.com"}
//	已过期: {"ret":0,"status":"expired"}
type QRCodeStatusResponse struct {
	ilinkResponse
	Status   string `json:"status"`
	BotToken string `json:"bot_token"`
	BotID    string `json:"ilink_bot_id"`
	UserID   string `json:"ilink_user_id"`
	BaseURL  string `json:"baseurl"`
}

// PollQRCodeStatus 轮询二维码状态（内部会阻塞等待直到 confirmed / expired / 出错）
func (c *ILinkClient) PollQRCodeStatus(qrcode string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	// sleep 必须感知 c.ctx：Shutdown() 之后如果还在裸 time.Sleep，
	// 主程序会白等 2 秒才退出；ctx 取消时应立刻返回
	sleep := func() bool {
		select {
		case <-c.ctx.Done():
			return false
		case <-time.After(2 * time.Second):
			return true
		}
	}

	for {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("扫码超时")
		}

		var resp QRCodeStatusResponse
		// GET + query 参数，不是 POST + JSON body（POST 会被服务端拒绝，返回 {"ret":1}）。
		// 这是长轮询接口：qrcode 有效时服务端会 hold 住连接直到有人扫码，
		// 因此这里可能一直阻塞到 c.http 的 60s Timeout 被掐断（当作网络错误重试即可）；
		// qrcode 无效/失效时立即返回 {"ret":0,"status":"expired"}。
		err := c.doRequest("GET",
			"/ilink/bot/get_qrcode_status?qrcode="+url.QueryEscape(qrcode), nil, &resp)
		if err != nil {
			if strings.Contains(err.Error(), "errcode=-14") {
				return err
			}
			if c.ctx.Err() != nil {
				return c.ctx.Err()
			}
			// 其他错误（网络抖动等）继续重试
			if !sleep() {
				return c.ctx.Err()
			}
			continue
		}

		switch resp.Status {
		case "confirmed":
			if resp.BotToken == "" || resp.BotID == "" {
				return fmt.Errorf("confirmed 但未返回凭据（bot_token/ilink_bot_id 为空）")
			}
			c.mu.Lock()
			c.botToken = resp.BotToken
			c.botID = resp.BotID
			c.userID = resp.UserID
			if resp.BaseURL != "" {
				c.baseURL = resp.BaseURL
			}
			c.mu.Unlock()
			return nil
		case "expired":
			return fmt.Errorf("二维码已过期")
		case "wait", "scaned":
			// 继续等待
			if !sleep() {
				return c.ctx.Err()
			}
		default:
			if !sleep() {
				return c.ctx.Err()
			}
		}
	}
}

// GetUpdates 长轮询获取消息（服务端 hold ~35s）
func (c *ILinkClient) GetUpdates() ([]ILinkMessage, error) {
	c.mu.RLock()
	cursor := c.cursor
	c.mu.RUnlock()

	body := map[string]interface{}{
		"base_info":       map[string]string{"channel_version": "2.0.0"},
		"get_updates_buf": cursor,
	}

	var resp getUpdatesResponse
	if err := c.doRequest("POST", "/ilink/bot/getupdates", body, &resp); err != nil {
		return nil, err
	}

	if resp.Ret != 0 {
		return nil, fmt.Errorf("getupdates 返回 ret=%d errmsg=%s", resp.Ret, resp.Errmsg)
	}

	// 游标「后置提交」：这里只记下服务端给的新游标，不直接推进已提交游标。
	// 为什么：iLink 协议保证「旧游标会返回其后所有历史数据」，所以只要不在处理成功前推进，
	// 崩溃/失败后重拉就能拿回同一批消息（at-least-once），再由 ingest_ledger 保证不重复入库。
	// 仅在服务端返回非空游标时才记录，避免被清空导致从头重拉。
	if resp.GetUpdatesBuf != "" {
		c.mu.Lock()
		c.pendingBuf = resp.GetUpdatesBuf
		c.mu.Unlock()
	}

	// 只做「同一响应内重复」的轻量过滤；真正的不重复保证来自持久账本 ingest_ledger。
	// 注意：不能在这里把 key 记入 recentIDs——那会让「处理失败但已标记已见」的消息
	// 在游标回退重拉时被误杀，等于丢消息。
	var newMsgs []ILinkMessage
	inBatch := make(map[string]bool, len(resp.Msgs))
	for _, msg := range resp.Msgs {
		key := messageKey(&msg)
		if key != "" {
			if inBatch[key] {
				continue // 同一批内重复
			}
			if c.AlreadyProcessed(key) {
				continue // 5 分钟滑窗内已成功处理过
			}
			inBatch[key] = true
		}
		newMsgs = append(newMsgs, msg)
	}

	return newMsgs, nil
}

// SendText 发送文本消息（自动从缓存取 context_token）
func (c *ILinkClient) SendText(toUserID, text string) error {
	c.mu.RLock()
	token := c.contextTokens[toUserID]
	c.mu.RUnlock()

	if token == "" {
		return fmt.Errorf("没有该用户的 context_token，无法主动发送")
	}

	return c.sendTextWithToken(toUserID, text, token)
}

// SendTextWithToken 用指定 context_token 发送文本（收到消息时回复用）
func (c *ILinkClient) SendTextWithToken(toUserID, text, contextToken string) error {
	return c.sendTextWithToken(toUserID, text, contextToken)
}

func (c *ILinkClient) sendTextWithToken(toUserID, text, contextToken string) error {
	// 微信单条消息长度限制 4000 字符，超长分片
	const maxLen = 3800
	runes := []rune(text)
	if len(runes) <= maxLen {
		return c.sendChunk(toUserID, text, contextToken)
	}

	// 按段落分片，尽量在换行处断开
	for len(runes) > 0 {
		chunkLen := maxLen
		if len(runes) < maxLen {
			chunkLen = len(runes)
		}
		chunk := string(runes[:chunkLen])
		// 尝试在最后一个换行处断开：idx 是字节下标、maxLen 是字符数，
		// 必须先换算成字符数再比较，否则中文场景下阈值判断恒真
		if chunkLen == maxLen {
			if idx := strings.LastIndex(chunk, "\n"); idx >= 0 {
				if n := len([]rune(chunk[:idx])); n > maxLen/2 {
					chunkLen = n
					chunk = string(runes[:chunkLen])
				}
			}
		}
		if err := c.sendChunk(toUserID, chunk, contextToken); err != nil {
			return err
		}
		runes = runes[chunkLen:]
		if len(runes) > 0 {
			time.Sleep(300 * time.Millisecond) // 防止频控
		}
	}
	return nil
}

// clientID 生成每条消息唯一的 client_id。
// 服务端用它去重：重复 client_id 的消息会被静默丢弃，所以必须每条唯一。
func clientID() string {
	var b [16]byte
	rand.Read(b[:])
	return fmt.Sprintf("wechat-profile-bot:%d-%s", time.Now().UnixMilli(), hex.EncodeToString(b[:8]))
}

func (c *ILinkClient) sendChunk(toUserID, text, contextToken string) error {
	// 报文格式实测确认（与 getupdates 收到的结构对齐）：
	// - from_user_id 必须为空字符串，服务端按 token 自行填充
	// - client_id 每条唯一，重复会被静默丢弃
	// - message_type=2(BOT) message_state=2(FINISH)
	// - 文本项字段是 text_item 而不是 text，用错字段服务端会静默丢弃
	body := map[string]interface{}{
		"base_info": map[string]string{"channel_version": "2.0.0"},
		"msg": map[string]interface{}{
			"from_user_id":  "",
			"to_user_id":    toUserID,
			"client_id":     clientID(),
			"message_type":  2,
			"message_state": 2,
			"context_token": contextToken,
			"item_list": []map[string]interface{}{
				{"type": 1, "text_item": map[string]string{"text": text}},
			},
		},
	}

	var resp ilinkResponse
	if err := c.doRequest("POST", "/ilink/bot/sendmessage", body, &resp); err != nil {
		return err
	}
	if resp.Ret != 0 {
		return fmt.Errorf("sendmessage 返回 ret=%d errmsg=%s", resp.Ret, resp.Errmsg)
	}
	return nil
}

// SaveContextToken 缓存某用户的 context_token
func (c *ILinkClient) SaveContextToken(userID, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.contextTokens[userID] = token
	c.saveContextTokensLocked()
}

// LoadContextTokens 从磁盘加载 context_token 缓存
func (c *ILinkClient) LoadContextTokens() error {
	data, err := os.ReadFile(c.contextTokenPath())
	if err != nil {
		return err
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.contextTokens = m
	return nil
}

func (c *ILinkClient) contextTokenPath() string {
	dir := filepath.Dir(c.tokenPath)
	return filepath.Join(dir, "context_tokens.json")
}

// saveContextTokensLocked 落盘 context_token 缓存。调用方必须持有 c.mu 写锁。
//
// 用「临时文件 + rename」而不是直接 WriteFile：这个文件每收到一条消息就会写一次，
// 直接覆写时若进程被杀/机器断电，会留下半个 JSON，重启后 LoadContextTokens 解析失败，
// 所有用户的 context_token 全部丢失（表现为机器人再也无法主动回复任何人）。
func (c *ILinkClient) saveContextTokensLocked() {
	data, err := json.MarshalIndent(c.contextTokens, "", "  ")
	if err != nil {
		slog.Error("保存 context_token 缓存失败: 序列化", "err", err)
		return
	}
	if err := writeFileAtomic(c.contextTokenPath(), data); err != nil {
		slog.Error("保存 context_token 缓存失败", "err", err)
	}
}

// writeFileAtomic 原子写文件：先写同目录临时文件，再 rename 覆盖目标。
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // rename 成功后这里返回错误，可忽略
	}()

	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// SessionExpired 是否会话已过期
func (c *ILinkClient) SessionExpired() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessionExpired
}

// ResetSession 清除会话状态（重新登录前调用）
func (c *ILinkClient) ResetSession() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionExpired = false
	c.botToken = ""
	c.botID = ""
	c.userID = ""
	c.cursor = ""
	c.contextTokens = make(map[string]string)
	// 同时删掉磁盘上的 context_tokens.json，否则重启后会把过期 token 加载回来
	os.Remove(c.contextTokenPath())
}

// GetBotID 返回 bot ID
func (c *ILinkClient) GetBotID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.botID
}

// GetUserID 返回登录用户 ID
func (c *ILinkClient) GetUserID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.userID
}
