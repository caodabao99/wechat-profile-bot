package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bot 命令处理器
type Bot struct {
	db     *sql.DB
	llm    *LLMClient
	client *ILinkClient
	cfg    *Config
}

// botTaskTimeout 后台任务（意图分析、画像补充/重生成）的最长执行时间。
// LLM 客户端单次超时 60s、失败重试一次，最坏约 122s；这些任务用的是脱离消息处理的
// context，必须自带超时，否则模型端挂住时 goroutine 会一直悬着。
const botTaskTimeout = 3 * time.Minute

// pendingDeleteTTL 删除操作二次确认的有效期
const pendingDeleteTTL = 2 * time.Minute

// pendingDelete 一次待确认的删除请求
type pendingDelete struct {
	contactID   int64
	requestName string // 用户「删除 X」里输入的 X，确认时必须原样对上
	display     string // 展示名（含备注）
	msgCount    int
	expiresAt   time.Time
}

// deleteRestoreMu 必须先于 dbMu/pendingDeleteMu 获取，覆盖查询、登记和实际删除。
// API 恢复持有同一把锁，避免旧确认在数据库替换后继续执行。
var deleteRestoreMu sync.Mutex

var (
	pendingDeleteMu sync.Mutex
	pendingDeletes  = make(map[string]pendingDelete)
)

// setPendingDelete 记录某用户发起的待确认删除
func setPendingDelete(userID string, p pendingDelete) {
	p.expiresAt = time.Now().Add(pendingDeleteTTL)
	pendingDeleteMu.Lock()
	pendingDeletes[userID] = p
	pendingDeleteMu.Unlock()
}

// takePendingDelete 取出并清除某用户的待确认删除项。
// 不存在或已过期都返回 ok=false（过期项顺手删掉，避免表无限增长）。
func takePendingDelete(userID string) (pendingDelete, bool) {
	pendingDeleteMu.Lock()
	defer pendingDeleteMu.Unlock()
	p, ok := pendingDeletes[userID]
	if !ok {
		return p, false
	}
	delete(pendingDeletes, userID)
	if time.Now().After(p.expiresAt) {
		return p, false
	}
	return p, true
}

// chatLogFlushDelay 微信把一段长粘贴文本拆成多条消息逐条送达时，
// 等待后续分片的时长。计时器每来一片（含图片等非文本消息）都会重置，
// 所以它覆盖的是相邻两片之间的间隔，不是整批的总时长——
// 100 条记录发几分钟没关系，只要相邻两片间隔不超过该值就会合并成一批。
// 图片上传慢会拉大间隔，20 秒是比较稳妥的取值。
const chatLogFlushDelay = 20 * time.Second

// pendingBatch 某个用户的一批待合并消息分片
type pendingBatch struct {
	texts        []string
	contextToken string
	mediaCount   int // 被跳过的图片/语音等非文本消息数
	timer        *time.Timer
}

var (
	pendingBatchMu sync.Mutex
	pendingBatches = make(map[string]*pendingBatch)
)

// isCommand 判断解析出的首个词是否是命令。
// 非命令文本（聊天记录分片、闲聊）不能进命令 switch，要先缓冲合并。
func isCommand(cmd string) bool {
	switch cmd {
	case "帮助", "help", "?", "列表", "画像", "历史", "备注", "补充", "合并",
		"撤销合并", "合并记录", "删除", "统计", "重登", "状态", "确认删除", "改写", "草稿检查", "画像变化",
		"面板", "网址", "地址":
		return true
	}
	return false
}

// bufferChatLog 把一条疑似聊天记录分片放入缓冲，静默期后统一处理。
// 处理结果由 flushPending 异步推送，所以这里返回空串（不立即回复）。
func (b *Bot) bufferChatLog(msg *ILinkMessage, text string) {
	pendingBatchMu.Lock()
	p := pendingBatches[msg.FromUserID]
	if p == nil {
		p = &pendingBatch{}
		pendingBatches[msg.FromUserID] = p
	}
	p.texts = append(p.texts, text)
	if msg.ContextToken != "" {
		p.contextToken = msg.ContextToken
	}
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = time.AfterFunc(chatLogFlushDelay, func() {
		b.flushPending(msg.FromUserID)
	})
	n := len(p.texts)
	pendingBatchMu.Unlock()
	if n > 1 {
		slog.Info("聊天记录分片已缓冲", "from", msg.FromUserID, "parts", n)
	}
}

// hasPendingBatch 判断某用户是否有待结算的聊天记录分片
func hasPendingBatch(userID string) bool {
	pendingBatchMu.Lock()
	defer pendingBatchMu.Unlock()
	_, ok := pendingBatches[userID]
	return ok
}

// touchPendingBatch 顺延某用户的合并窗口并累计跳过的非文本消息数。
// 用于图片/语音/视频等无法解析的消息：不计入文本，但要把计时器重置，
// 否则慢速上传的媒体消息会把一批记录从中间切成两半
func (b *Bot) touchPendingBatch(userID string) {
	pendingBatchMu.Lock()
	defer pendingBatchMu.Unlock()
	p := pendingBatches[userID]
	if p == nil {
		return
	}
	p.mediaCount++
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = time.AfterFunc(chatLogFlushDelay, func() {
		b.flushPending(userID)
	})
	slog.Info("跳过非文本消息并顺延合并窗口", "from", userID, "skippedMedia", p.mediaCount)
}

// flushPending 合并某个用户的全部分片并走聊天记录识别流程，结果异步推送。
func (b *Bot) flushPending(userID string) {
	pendingBatchMu.Lock()
	p := pendingBatches[userID]
	delete(pendingBatches, userID)
	pendingBatchMu.Unlock()
	if p == nil || len(p.texts) == 0 {
		return
	}
	if p.timer != nil {
		p.timer.Stop()
	}

	// 拼接分片：微信拆分发送时，分片末尾的换行会被吞掉
	// （实测「...数据」+「小齐」被拼成一行，昵称解析被污染），
	// 所以用 \n 连接各分片还原行结构。
	combined := strings.Join(p.texts, "\n")
	if len(p.texts) > 1 {
		slog.Info("合并聊天记录分片", "from", userID, "parts", len(p.texts), "totalLen", len([]rune(combined)), "skippedMedia", p.mediaCount)
	}

	fake := &ILinkMessage{
		FromUserID:   userID,
		ContextToken: p.contextToken,
		ItemList:     []ILinkItem{{Type: 1, Text: &ILinkText{Text: combined}}},
	}
	reply := b.handleChatLog(fake, combined)
	if reply == "" {
		return
	}
	// 告知用户有图片等非文本消息被跳过，避免「怎么少了几条」的困惑
	if p.mediaCount > 0 {
		reply += fmt.Sprintf("\n（已跳过 %d 条图片/非文本消息）", p.mediaCount)
	}
	// 优先用这批消息里最新的 context_token 回复；没有再走缓存
	if p.contextToken != "" {
		if err := b.client.SendTextWithToken(userID, reply, p.contextToken); err != nil {
			slog.Error("发送聊天记录识别结果失败", "to", userID, "err", err)
		}
	} else {
		b.push(userID, reply)
	}
}

// push 主动推送一条消息给指定用户。
// 失败只记日志：异步任务已经没有可回复的上下文，重试也无从告知用户。
func (b *Bot) push(userID, text string) {
	if err := b.client.SendText(userID, text); err != nil {
		slog.Error("推送消息失败", "to", userID, "err", err)
	}
}

// profileErrMsg 把画像相关的错误转成给用户看的文案
func profileErrMsg(err error) string {
	if errors.Is(err, ErrProfileBusy) {
		return ErrProfileBusy.Error()
	}
	return err.Error()
}

// NewBot 创建命令处理器
func NewBot(db *sql.DB, llm *LLMClient, client *ILinkClient, cfg *Config) *Bot {
	return &Bot{db: db, llm: llm, client: client, cfg: cfg}
}

// HandleMessage 处理一条入站消息，返回回复文本
func (b *Bot) HandleMessage(msg *ILinkMessage) string {
	// 提取文本内容
	text := extractText(msg)
	if text == "" {
		// 图片/语音/视频等非文本消息无法解析。
		// 如果正好处在聊天记录分片缓冲期内（多选转发里夹着图片/语音/视频很常见），
		// 静默跳过并顺延合并窗口——这些媒体消息上传慢，不顺延会把一批记录从中间切断；
		// 单独发一张图/一段语音时保留提示，否则用户会以为 bot 死了
		if hasPendingBatch(msg.FromUserID) {
			b.touchPendingBatch(msg.FromUserID)
			return ""
		}
		return "只支持文本消息，请发送文字或粘贴聊天记录"
	}

	// 缓存 context_token 以便后续回复
	if msg.ContextToken != "" {
		b.client.SaveContextToken(msg.FromUserID, msg.ContextToken)
	}

	// 命令解析
	cmd, args := parseCommand(text)

	// 微信把多选复制的长聊天记录拆成多条消息逐条送达：
	// 非命令文本先缓冲，静默 3 秒后合并成完整文本统一识别。
	if !isCommand(cmd) {
		b.bufferChatLog(msg, text)
		return ""
	}

	// 收到命令时，先结算之前缓冲的聊天记录分片（异步，不阻塞命令回复），
	// 否则它们会一直挂在缓冲区里等永远不会来的「下一片」
	if hasPendingBatch(msg.FromUserID) {
		go b.flushPending(msg.FromUserID)
	}

	switch cmd {
	case "改写", "草稿检查", "画像变化":
		return b.assistCommand(cmd, args)
	case "帮助", "help", "?":
		return b.helpText()
	case "列表":
		return b.listContacts()
	case "画像":
		return b.showProfile(args)
	case "历史":
		return b.showHistory(args)
	case "备注":
		return b.setRemark(args)
	case "补充":
		return b.supplementProfile(msg, args)
	case "合并":
		return b.mergeContacts(args)
	case "撤销合并":
		return b.undoMerge(args)
	case "合并记录":
		return b.mergeLogs(args)
	case "删除":
		return b.deleteContact(msg, args)
	case "统计":
		return b.showStats(args)
	case "重登":
		return b.relogin()
	case "状态":
		return b.status()
	case "面板", "网址", "地址":
		return b.webPanelURL()
	default:
		// 不是命令 → 当作聊天记录处理
		return b.handleChatLog(msg, text)
	}
}

// extractText 从消息中提取文本。
// 一条消息可能带多个 text item（微信把长文本拆开下发），
// 早先只取第一个就直接 return，粘贴的长聊天记录会被截掉后半段。
func extractText(msg *ILinkMessage) string {
	var parts []string
	for _, item := range msg.ItemList {
		if item.Type == 1 && item.Text != nil {
			if t := strings.TrimSpace(item.Text.Text); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// parseCommand 解析命令：第一个词是命令，其余是参数
func parseCommand(text string) (string, string) {
	text = strings.TrimSpace(text)
	// 按第一个空格或换行分割
	idx := strings.IndexAny(text, " \n")
	if idx < 0 {
		return text, ""
	}
	return text[:idx], strings.TrimSpace(text[idx+1:])
}

func (b *Bot) helpText() string {
	return `微信画像助手命令：

【直接粘贴聊天记录】
多选复制微信聊天记录发给我，自动识别并存入，同时返回意图分析
（内容较长被微信拆成多条发送时会自动合并，发完后稍等几秒出结果）

【回复辅助（仅展示，不代发）】
改写 昵称｜风格｜原回复 — 竖线中英文输入法都可以；风格：稳妥得体/简洁直接/亲切热情/委婉留余地
草稿检查 昵称｜准备发送的话 — 检查歧义并给出改进版本
画像变化 昵称 — 对比当前与上一历史画像，不调用模型
昵称含空格也可使用；原文中的竖线会保留。

【查询类】
画像 [昵称]        — 查看联系人画像（不填昵称显示最近更新的）
历史 [昵称] [编号] — 查看画像历史版本；加编号看该版完整画像（如：历史 小齐 1）
列表           — 查看所有联系人
统计 昵称       — 查看该联系人的消息统计

【管理类】
备注 昵称 备注内容   — 设置联系人备注
补充 昵称 信息内容   — 手动补充画像信息（后台处理，完成后单独推送）
合并 新昵称 旧昵称   — 合并两个联系人（新昵称并入旧昵称）
撤销合并 昵称       — 撤销最近一次的合并
合并记录           — 查看合并历史
删除 昵称           — 删除联系人及其所有数据（需再回复「确认删除 昵称」）

【系统类】
状态           — 查看登录与数据库状态（服务器磁盘/内存等资源监控请用网页管理端）
网址           — 返回管理面板可点链接（优先用 webBaseURL；未配则探测服务器公网 IP，回退局域网）
重登           — 重新扫码登录（会话过期时用）

【提示】
- 聊天记录识别靠的是微信"多选复制"功能，复制后直接粘贴发给我
- 设置备注后，所有命令里的「昵称」位置填昵称或备注都能识别
- 首次生成画像需要积累一定数量的对方消息
- 发"帮助"查看本说明`
}

func (b *Bot) listContacts() string {
	contacts, err := GetAllContacts(b.db, false)
	if err != nil {
		return "查询联系人失败: " + err.Error()
	}
	if len(contacts) == 0 {
		return "暂无联系人记录，请先粘贴聊天记录"
	}

	var sb strings.Builder
	sb.WriteString("联系人列表（按最近更新排序）\n\n")
	for i, c := range contacts {
		name := c.Name
		if c.Remark != "" {
			name = c.Remark + "（" + c.Name + "）"
		}
		mergeTag := ""
		if c.MergeCount > 0 {
			mergeTag = fmt.Sprintf(" [含%d人]", c.MergeCount)
		}
		fmt.Fprintf(&sb, "%d. %s%s — %d条对方消息\n   更新: %s\n\n",
			i+1, name, mergeTag, c.OtherMsgCount, displayTime(c.LastUpdated))
	}
	return sb.String()
}

func (b *Bot) showProfile(name string) string {
	if name == "" {
		// 显示最近更新的联系人
		contacts, err := GetAllContacts(b.db, false)
		if err != nil || len(contacts) == 0 {
			return "暂无联系人，请先粘贴聊天记录"
		}
		name = contacts[0].Name
	}

	cid, _, err := FindContactID(b.db, name)
	if err != nil {
		return "未找到联系人「" + name + "」"
	}

	contact, err := GetContactByIDWithMerged(b.db, cid)
	if err != nil {
		return "读取联系人失败: " + err.Error()
	}

	profileJSON := strings.TrimSpace(contact.ProfileJSON)
	if profileJSON == "" || profileJSON == "{}" {
		return fmt.Sprintf("「%s」暂无画像\n当前对方消息数: %d\n累计达到 %d 条后会自动生成",
			displayName(contact), contact.OtherMsgCount, b.cfg.Profile.ColdStartCount)
	}

	// 格式化画像输出
	var p Profile
	if err := json.Unmarshal([]byte(profileJSON), &p); err != nil {
		return "画像数据解析失败"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "【%s 的画像】\n\n", displayName(contact))
	sb.WriteString(formatProfileText(&p))
	fmt.Fprintf(&sb, "上次更新: %s\n对方消息数: %d", displayTime(contact.LastUpdated), contact.OtherMsgCount)
	return sb.String()
}

// formatProfileText 把画像结构体渲染成微信可读文本（不含标题和时间尾巴）。
// showProfile 和历史版本查看共用，保证两处呈现完全一致。
func formatProfileText(p *Profile) string {
	var sb strings.Builder

	if p.Summary != "" {
		fmt.Fprintf(&sb, "📌 概要\n%s\n\n", p.Summary)
	}

	if len(p.BasicInfo.Occupation) > 0 || len(p.BasicInfo.Location) > 0 || len(p.BasicInfo.ImportantDates) > 0 {
		sb.WriteString("📋 基本信息\n")
		if p.BasicInfo.Occupation != "" {
			fmt.Fprintf(&sb, "职业: %s\n", p.BasicInfo.Occupation)
		}
		if p.BasicInfo.Location != "" {
			fmt.Fprintf(&sb, "地区: %s\n", p.BasicInfo.Location)
		}
		for _, d := range p.BasicInfo.ImportantDates {
			fmt.Fprintf(&sb, "重要日子: %s\n", d)
		}
		sb.WriteString("\n")
	}

	if len(p.Personality) > 0 {
		sb.WriteString("🎭 性格特征\n")
		for _, v := range p.Personality {
			fmt.Fprintf(&sb, "· %s\n", v)
		}
		sb.WriteString("\n")
	}

	if len(p.Interests) > 0 {
		sb.WriteString("🎯 兴趣爱好\n")
		for _, v := range p.Interests {
			fmt.Fprintf(&sb, "· %s\n", v)
		}
		sb.WriteString("\n")
	}

	if p.CommunicationStyle.ReplyLength != "" || p.CommunicationStyle.Tone != "" {
		sb.WriteString("💬 沟通风格\n")
		if p.CommunicationStyle.ReplyLength != "" {
			fmt.Fprintf(&sb, "回复长短: %s\n", p.CommunicationStyle.ReplyLength)
		}
		if p.CommunicationStyle.Tone != "" {
			fmt.Fprintf(&sb, "语气: %s\n", p.CommunicationStyle.Tone)
		}
		if len(p.CommunicationStyle.FrequentPhrases) > 0 {
			fmt.Fprintf(&sb, "口头禅: %s\n", strings.Join(p.CommunicationStyle.FrequentPhrases, "、"))
		}
		if p.CommunicationStyle.EmojiUsage != "" {
			fmt.Fprintf(&sb, "表情习惯: %s\n", p.CommunicationStyle.EmojiUsage)
		}
		if p.CommunicationStyle.Initiative != "" {
			fmt.Fprintf(&sb, "主动程度: %s\n", p.CommunicationStyle.Initiative)
		}
		sb.WriteString("\n")
	}

	if len(p.EmotionalPatterns.Stressors) > 0 || len(p.EmotionalPatterns.ComfortTopics) > 0 {
		sb.WriteString("💢 情绪模式\n")
		for _, v := range p.EmotionalPatterns.Stressors {
			fmt.Fprintf(&sb, "压力源: %s\n", v)
		}
		for _, v := range p.EmotionalPatterns.ComfortTopics {
			fmt.Fprintf(&sb, "有效安慰: %s\n", v)
		}
		if p.EmotionalPatterns.WhenUpset != "" {
			fmt.Fprintf(&sb, "不高兴时: %s\n", p.EmotionalPatterns.WhenUpset)
		}
		sb.WriteString("\n")
	}

	if p.Relationship.Closeness != "" || len(p.Relationship.RecentEvents) > 0 {
		sb.WriteString("🤝 与我的关系\n")
		if p.Relationship.Closeness != "" {
			fmt.Fprintf(&sb, "亲密程度: %s\n", p.Relationship.Closeness)
		}
		if p.Relationship.InteractionPattern != "" {
			fmt.Fprintf(&sb, "互动模式: %s\n", p.Relationship.InteractionPattern)
		}
		for _, v := range p.Relationship.RecentEvents {
			fmt.Fprintf(&sb, "近期事件: %s\n", v)
		}
		sb.WriteString("\n")
	}

	if len(p.IntentPatterns) > 0 {
		sb.WriteString("🧠 意图模式\n")
		for k, v := range p.IntentPatterns {
			fmt.Fprintf(&sb, "· %s: %s\n", k, v)
		}
		sb.WriteString("\n")
	}

	if len(p.ImportantFacts) > 0 {
		sb.WriteString("⭐ 重要事实\n")
		for _, v := range p.ImportantFacts {
			fmt.Fprintf(&sb, "· %s\n", v)
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// showHistory 画像历史查询：
//
//	历史 [昵称]        — 列出最近的版本（1 为最新）
//	历史 [昵称] N      — 查看第 N 个版本的完整画像
//
// 不填昵称时用最近更新的联系人，与「画像」命令保持一致。
func (b *Bot) showHistory(args string) string {
	args = strings.TrimSpace(args)

	// 末尾一段是纯数字时视为版本号，其余整体作为昵称（昵称允许含空格）
	name := args
	version := 0
	if idx := strings.LastIndex(args, " "); idx >= 0 {
		tail := strings.TrimSpace(args[idx+1:])
		if n, err := strconv.Atoi(tail); err == nil && n > 0 {
			name = strings.TrimSpace(args[:idx])
			version = n
		}
	}

	if name == "" {
		contacts, err := GetAllContacts(b.db, false)
		if err != nil || len(contacts) == 0 {
			return "暂无联系人，请先粘贴聊天记录"
		}
		name = contacts[0].Name
	}

	cid, _, err := FindContactID(b.db, name)
	if err != nil {
		return "未找到联系人「" + name + "」"
	}
	contact, err := GetContactByIDWithMerged(b.db, cid)
	if err != nil {
		return "读取联系人失败: " + err.Error()
	}

	logs, err := GetProfileHistory(b.db, cid, 30)
	if err != nil {
		return "读取画像历史失败: " + err.Error()
	}
	if len(logs) == 0 {
		return fmt.Sprintf("「%s」暂无画像历史\n画像首次生成后，每次更新都会在这里留一个版本", displayName(contact))
	}

	// 查看指定版本
	if version > 0 {
		if version > len(logs) {
			return fmt.Sprintf("「%s」只有 %d 个历史版本，请用 1～%d 之间的编号", displayName(contact), len(logs), len(logs))
		}
		h := logs[version-1] // 列表 1 最新，底层按 id DESC 返回
		pj := strings.TrimSpace(h.ProfileJSON)
		header := fmt.Sprintf("【%s 的画像历史 · 第 %d 版】\n时间: %s\n说明: %s\n\n",
			displayName(contact), version, displayTime(h.CreatedAt),
			ifEmpty(h.ChangeSummary, "画像更新"))
		if pj == "" || pj == "{}" {
			return header + "该版本是事件记录（如改名、合并、生成失败），没有画像快照"
		}
		var p Profile
		if err := json.Unmarshal([]byte(pj), &p); err != nil {
			return header + "该版本画像数据解析失败"
		}
		return header + formatProfileText(&p)
	}

	// 列出版本
	var sb strings.Builder
	fmt.Fprintf(&sb, "「%s」的画像历史（共 %d 版，1 为最新）\n", displayName(contact), len(logs))
	for i, h := range logs {
		mark := "📄"
		if pj := strings.TrimSpace(h.ProfileJSON); pj == "" || pj == "{}" {
			mark = "🔸" // 事件记录，无画像快照
		}
		fmt.Fprintf(&sb, "\n%d. %s %s %s", i+1, mark, displayTime(h.CreatedAt),
			ifEmpty(strings.TrimSpace(h.ChangeSummary), "画像更新"))
	}
	sb.WriteString("\n\n回复「历史 " + name + " 编号」查看该版完整画像")
	return sb.String()
}

// ifEmpty 空串回落
func ifEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func (b *Bot) setRemark(args string) string {
	parts := strings.SplitN(args, " ", 2)
	if len(parts) < 2 {
		return "用法: 备注 昵称 备注内容"
	}
	name, remark := parts[0], parts[1]

	cid, _, err := FindContactID(b.db, name)
	if err != nil {
		return "未找到联系人「" + name + "」"
	}

	if err := UpdateContactRemark(b.db, cid, remark); err != nil {
		return "设置备注失败: " + err.Error()
	}
	return fmt.Sprintf("已设置「%s」的备注为「%s」", name, remark)
}

func (b *Bot) supplementProfile(msg *ILinkMessage, args string) string {
	parts := strings.SplitN(args, " ", 2)
	if len(parts) < 2 {
		return "用法: 补充 昵称 信息内容\n例如: 补充 张三 生日是5月20日"
	}
	name, note := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if note == "" {
		return "用法: 补充 昵称 信息内容\n例如: 补充 张三 生日是5月20日"
	}

	cid, _, err := FindContactID(b.db, name)
	if err != nil {
		return "未找到联系人「" + name + "」"
	}

	contact, err := GetContactByID(b.db, cid)
	if err != nil {
		return "读取联系人失败: " + err.Error()
	}

	// 补充画像要走一轮完整的 LLM 调用（最坏 ~122s），而入站消息是串行处理的：
	// 同步等结果会把整个长轮询循环卡住，期间所有人的消息都收不到。
	// 这里先回执，结果异步推送。
	userID := msg.FromUserID
	go func() {
		ctx, cancel := context.WithTimeout(withInteractiveCall(context.Background()), botTaskTimeout)
		defer cancel()

		if err := SupplementProfile(ctx, b.db, b.llm, cid, contact.Name, note); err != nil {
			b.push(userID, "补充画像失败: "+profileErrMsg(err))
			return
		}
		b.push(userID, fmt.Sprintf("已把补充信息合并到「%s」的画像中", displayName(contact)))
	}()

	return fmt.Sprintf("收到，正在把补充信息合并到「%s」的画像中...", displayName(contact))
}

func (b *Bot) mergeContacts(args string) string {
	parts := strings.SplitN(args, " ", 2)
	if len(parts) < 2 {
		return "用法: 合并 新昵称 旧昵称\n（新昵称的数据会并入旧昵称，旧昵称保留显示）"
	}
	sourceName, targetName := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])

	sourceID, _, err := FindContactID(b.db, sourceName)
	if err != nil {
		return "未找到源联系人「" + sourceName + "」"
	}
	targetID, _, err := FindContactID(b.db, targetName)
	if err != nil {
		return "未找到目标联系人「" + targetName + "」"
	}

	result, err := MergeContacts(b.db, sourceID, targetID, MergeOptions{
		UseSourceNameAsDisplay: false,
		RegenerateProfile:      true,
	})
	if err != nil {
		return "合并失败: " + err.Error()
	}

	// 异步重生成目标联系人画像（去重：已有任务则跳过）
	triggered := false
	if _, loaded := profileInFlight.LoadOrStore(targetID, true); !loaded {
		triggered = true
		go func() {
			defer profileInFlight.Delete(targetID)
			ctx, cancel := context.WithTimeout(withInteractiveCall(context.Background()), botTaskTimeout)
			defer cancel()

			target, err := GetContactByID(b.db, targetID)
			if err != nil {
				slog.Error("合并后重生成画像失败: 读取联系人", "err", err)
				return
			}
			msgs, err := GetAllMessages(b.db, targetID)
			if err != nil {
				slog.Error("合并后重生成画像失败: 读取消息", "err", err)
				return
			}
			if err := GenerateOrUpdateProfile(ctx, b.db, b.llm, targetID, target.Name, msgs); err != nil {
				slog.Error("合并后重生成画像失败", "err", err)
			}
		}()
	}

	tail := "画像已在后台排队，稍后会自动重新生成"
	if triggered {
		tail = "画像正在后台重新生成..."
	}
	return fmt.Sprintf("合并完成：「%s」并入「%s」\n搬移消息 %d 条，画像历史 %d 条\n%s",
		sourceName, targetName, result.MovedMessages, result.MovedHistory, tail)
}

func (b *Bot) undoMerge(args string) string {
	name := strings.TrimSpace(args)
	if name == "" {
		return "用法: 撤销合并 昵称\n（撤销该昵称作为目标联系人时最近的一次合并）"
	}

	cid, _, err := FindContactID(b.db, name)
	if err != nil {
		return "未找到联系人「" + name + "」"
	}

	logs, err := GetMergeLogsForTarget(b.db, cid)
	if err != nil || len(logs) == 0 {
		return "该联系人没有可撤销的合并记录"
	}

	if err := UndoMerge(b.db, logs[0].ID); err != nil {
		return "撤销合并失败: " + err.Error()
	}
	return fmt.Sprintf("已撤销「%s」的最近一次合并（%s）", name, logs[0].SourceName)
}

func (b *Bot) mergeLogs(args string) string {
	logs, err := GetMergeLogs(b.db, 20)
	if err != nil {
		return "查询合并记录失败: " + err.Error()
	}
	if len(logs) == 0 {
		return "暂无合并记录"
	}

	var sb strings.Builder
	sb.WriteString("合并记录\n\n")
	for _, l := range logs {
		status := ""
		if l.UndoneAt != "" {
			status = " [已撤销]"
		}
		fmt.Fprintf(&sb, "· %s → %s%s\n  %s\n\n", l.SourceName, l.TargetName, status, displayTime(l.CreatedAt))
	}
	return sb.String()
}

func (b *Bot) deleteContact(msg *ILinkMessage, args string) string {
	deleteRestoreMu.Lock()
	defer deleteRestoreMu.Unlock()
	name := strings.TrimSpace(args)
	if name == "" {
		return "用法: 删除 昵称\n（删除联系人及其所有消息和画像，不可恢复）"
	}

	cid, _, err := FindContactID(b.db, name)
	if err != nil {
		return "未找到联系人「" + name + "」"
	}

	contact, err := GetContactByID(b.db, cid)
	if err != nil {
		return "读取联系人失败: " + err.Error()
	}

	// 登记待确认项：确认时按这里锁定的 contactID 执行，不再重新按名字解析。
	// 否则「删除 张三」和「确认删除 张三」之间若发生过改名/合并，
	// 删掉的可能是另一个联系人。
	setPendingDelete(msg.FromUserID, pendingDelete{
		contactID:   cid,
		requestName: name,
		display:     displayName(contact),
		msgCount:    contact.OtherMsgCount,
	})

	return fmt.Sprintf("确认删除「%s」及其全部 %d 条消息和画像？\n回复「确认删除 %s」执行（%d 分钟内有效）",
		displayName(contact), contact.OtherMsgCount, name, int(pendingDeleteTTL/time.Minute))
}

// ConfirmDelete 二次确认后真正删除。
//
// 必须命中一条未过期的待确认记录才执行：早先这里是无状态的，
// 任何用户在任何时候发一条以「确认删除 」开头的消息都会立刻删库，
// 既没有真正的前置确认，也可能删掉与用户预期不符的联系人。
func (b *Bot) ConfirmDelete(userID, name string) string {
	deleteRestoreMu.Lock()
	defer deleteRestoreMu.Unlock()
	p, ok := takePendingDelete(userID)
	if !ok {
		return "没有待确认的删除操作（可能已超时）。请先发送「删除 昵称」"
	}
	if strings.TrimSpace(name) != p.requestName {
		return fmt.Sprintf("待确认的是「%s」，与你输入的「%s」不一致，已取消。如需删除请重新发送「删除 %s」",
			p.display, name, p.requestName)
	}

	if err := DeleteContactByID(b.db, p.contactID); err != nil {
		return "删除失败: " + err.Error()
	}
	return fmt.Sprintf("已删除「%s」及其全部 %d 条消息和画像", p.display, p.msgCount)
}

func (b *Bot) showStats(args string) string {
	name := strings.TrimSpace(args)
	if name == "" {
		return "用法: 统计 昵称"
	}

	cid, _, err := FindContactID(b.db, name)
	if err != nil {
		return "未找到联系人「" + name + "」"
	}

	stats, err := GetContactStats(b.db, cid)
	if err != nil {
		return "查询统计失败: " + err.Error()
	}

	return fmt.Sprintf("「%s」消息统计\n总消息: %d 条\n我发的: %d 条\n对方发的: %d 条\n时间跨度: %s ~ %s",
		name, stats.Total, stats.Mine, stats.Other,
		displayTime(stats.FirstTime), displayTime(stats.LastTime))
}

func (b *Bot) relogin() string {
	b.client.ResetSession()
	return "已重置会话，请重启程序后重新扫码登录"
}

func (b *Bot) status() string {
	status := "未登录"
	if b.client.IsLoggedIn() {
		status = "已登录"
	}
	if b.client.SessionExpired() {
		status = "会话已过期，请发送「重登」后重启程序"
	}

	// 数据库状态不能写死「正常」：磁盘满、文件被占用时这里会说谎，
	// 用户拿着「一切正常」的结论去排查别处。
	dbStatus := "正常"
	if err := b.db.Ping(); err != nil {
		dbStatus = "异常: " + err.Error()
	}

	// 只保留登录/会话/数据库概览（随身判断是否需要「重登」所用）；
	// 服务器磁盘/内存等资源监控属运维层，已改由网页端仪表盘展示。
	return fmt.Sprintf("运行状态\n登录状态: %s\nBot ID: %s\n用户 ID: %s\n数据库: %s",
		status, b.client.GetBotID(), b.client.GetUserID(), dbStatus)
}

// webPanelURL 回一条含可点链接的文本，供微信里直接打开管理面板。
// 优先 config.json 的 webBaseURL；未配置则探测服务器公网出口 IP（适配云端部署），
// 公网探测不可用才回退局域网。只回地址、不附 Token；面板自身有 Bearer + 2FA 保护。
func (b *Bot) webPanelURL() string {
	url, source, err := WebPanelURL(b.cfg)
	if err != nil {
		return "暂时无法确定面板地址：" + err.Error()
	}
	port := b.cfg.APIPort
	if port <= 0 {
		port = 17965
	}
	var sb strings.Builder
	sb.WriteString("管理面板地址：" + url + "\n")
	switch source {
	case panelSourceConfig:
		sb.WriteString("（来自 config.json 的 webBaseURL。）\n")
	case panelSourcePublic:
		fmt.Fprintf(&sb, "（这是自动探测到的服务器公网地址。从外网打开需你在云主机安全组/防火墙放行 %d 端口，或在路由器做端口映射；跨公网建议给 webBaseURL 配 https 域名，以免微信内置浏览器告警。）\n", port)
	default: // panelSourceLAN：公网探测不可用时的兵底
		sb.WriteString("（未能探测到公网 IP，上面是局域网地址，仅与服务器同一 WiFi/内网时可打开。外网直开请在 config.json 配置 webBaseURL 为你的可达地址。）\n")
	}
	sb.WriteString("打开后需输入 API Token 登录（面板已受 Token + 2FA 保护）。")
	return sb.String()
}

// handleChatLog 处理粘贴的聊天记录（复用 ingestAndStore 核心逻辑）
func (b *Bot) handleChatLog(msg *ILinkMessage, text string) string {
	msgFromUserID := msg.FromUserID

	// 检测是否是「确认删除 xxx」（也接受不带昵称的「确认删除」，目标由待确认记录决定）
	if text == "确认删除" || strings.HasPrefix(text, "确认删除 ") {
		return b.ConfirmDelete(msgFromUserID, strings.TrimSpace(strings.TrimPrefix(text, "确认删除")))
	}

	// 复用 API 层的核心识别+存库逻辑
	outcome, err := ingestAndStore(b.db, b.cfg, b.llm, text)
	if err != nil {
		return err.Error()
	}

	// 构建回复
	var sb strings.Builder
	fmt.Fprintf(&sb, "已识别「%s」的聊天记录\n", displayName(outcome.Contact))
	fmt.Fprintf(&sb, "本次解析: %d 条（新增 %d 条）\n", outcome.ParsedCount, outcome.NewCount)
	fmt.Fprintf(&sb, "累计对方消息: %d 条\n", outcome.Contact.OtherMsgCount)
	if outcome.ViaAlias {
		sb.WriteString("（通过别名关联到该联系人）\n")
	}

	if outcome.ProfileTriggered {
		sb.WriteString("\n画像正在后台更新，稍后可用「画像 " + outcome.Contact.Name + "」查看\n")
	} else if outcome.Contact.ProfileJSON == "" || outcome.Contact.ProfileJSON == "{}" {
		need := b.cfg.Profile.ColdStartCount - outcome.Contact.OtherMsgCount
		if need > 0 {
			fmt.Fprintf(&sb, "再积累 %d 条对方消息将首次生成画像\n", need)
		}
	}

	// 意图分析（异步推送结果）
	var latestOther string
	for i := len(outcome.Messages) - 1; i >= 0; i-- {
		if outcome.Messages[i].Sender == "other" {
			latestOther = outcome.Messages[i].Content
			break
		}
	}
	if latestOther == "" {
		// 没有对方消息就无从分析。早先这里无条件追加「意图分析中...」，
		// 用户会一直等一条永远不会来的推送。
		return sb.String()
	}
	sb.WriteString("\n意图分析中...")

	contactID := outcome.Contact.ID
	messages := outcome.Messages
	go func() {
		ctx, cancel := context.WithTimeout(withInteractiveCall(context.Background()), botTaskTimeout)
		defer cancel()

		result, err := AnalyzeIntent(ctx, b.db, b.llm, contactID, latestOther, messages)
		if err != nil {
			slog.Error("意图分析失败", "err", err)
			b.push(msgFromUserID, "意图分析失败: "+err.Error())
			return
		}

		analysis, replies := formatIntentResult(result)
		if analysis == "" {
			return
		}
		// 用缓存中最新的 context_token 发送（比本次消息的 token 更新）
		b.push(msgFromUserID, analysis)
		// 建议回复逐条单独发送：微信里整条结果只能整段复制，
		// 单独成消息长按就能只复制这一条，方便直接粘贴回复。
		// 风格标签放在末尾：复制后如不想要标签，从末尾一删即可，比开头好处理。
		for _, reply := range replies {
			b.push(msgFromUserID, fmt.Sprintf("%s【%s】", reply.Text, reply.Style))
		}
	}()

	return sb.String()
}

// formatIntentResult 格式化意图分析结果。
// 返回的 body 只含分析字段和置信度；建议回复单独返回，由调用方逐条发送
// （微信里整段消息只能整段复制，建议回复单独成消息才方便长按复制单条）。
func formatIntentResult(result map[string]interface{}) (string, []SuggestedReply) {
	if result == nil {
		return "", nil
	}

	// get 取字段并统一转成展示用字符串。
	// 模型返回的 confidence 是 JSON 数字（反序列化后为 float64），
	// 早先这里只做 .(string) 断言，断言必然失败，「置信度」一行永远不显示。
	get := func(key string) string {
		v, ok := result[key]
		if !ok || v == nil {
			return ""
		}
		switch t := v.(type) {
		case string:
			return strings.TrimSpace(t)
		case float64:
			return formatConfidence(t)
		case json.Number:
			if f, err := t.Float64(); err == nil {
				return formatConfidence(f)
			}
			return t.String()
		case bool:
			return strconv.FormatBool(t)
		}
		return strings.TrimSpace(fmt.Sprint(v))
	}

	var sb strings.Builder
	sb.WriteString("\n【意图分析结果】\n")

	if v := get("surface"); v != "" {
		fmt.Fprintf(&sb, "表面意思: %s\n", v)
	}
	if v := get("intent"); v != "" {
		fmt.Fprintf(&sb, "潜在意图: %s\n", v)
	}
	if v := get("emotion"); v != "" {
		fmt.Fprintf(&sb, "情绪状态: %s\n", v)
	}
	if v := get("subtext"); v != "" {
		fmt.Fprintf(&sb, "潜台词: %s\n", v)
	}
	replies := suggestedReplyItems(result)
	if len(replies) > 0 {
		sb.WriteString("建议回复见下方单独消息，长按单条即可复制\n")
	}
	if v := get("confidence"); v != "" {
		fmt.Fprintf(&sb, "置信度: %s\n", v)
	}

	return sb.String(), replies
}

// formatConfidence 把模型给的置信度数字格式化成人能读的文本。
// prompt 里要求的是 0~1 的小数，但不同模型经常自作主张给 0~100 的整数，
// 这里统一按百分比展示，避免出现「置信度: 85」这种看不出量纲的结果。
func formatConfidence(f float64) string {
	if f > 1 {
		f /= 100
	}
	if f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	return fmt.Sprintf("%.0f%%", f*100)
}
