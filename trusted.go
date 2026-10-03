package main

// 可信客户端（免重复登录）：网页端完成 Token + 2FA 登录后，可勾选
// 「信任此设备」，服务端签发一枚长效令牌（默认 90 天，使用即滑动续期），
// 存于浏览器 localStorage。下次打开页面时前端用这枚令牌直接换取普通
// 网页会话，跳过 Token 和动态码输入。
//
// 安全边界：
//   - 令牌文件 trusted_clients.json 与 totp_secret.json 同级同权限（0600），
//     且在备份加密的旁路文件清单之外——它只是便利凭证，泄露可吊销
//   - 换取会话仍受 IP 白名单 + 封禁名单约束（走 /api/auth/* 通道）
//   - 关闭 2FA 时连带吊销全部可信客户端（安全状态变化，便利凭证全部作废）
//   - 管理页可随时查看（令牌打码）、单独吊销、全部吊销

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// trustedClientTTL 可信令牌有效期（每次成功使用后续期）
const trustedClientTTL = 90 * 24 * time.Hour

// maxTrustedClients 可信设备数量上限，超出时淘汰最久未使用的
const maxTrustedClients = 20

// TrustedClient 一台被信任的设备
type TrustedClient struct {
	Name       string `json:"name"`  // 设备名（前端传入，截断 40 字符）
	Token      string `json:"token"` // 长效令牌（32 字节 hex）
	CreatedAt  string `json:"createdAt"`
	ExpiresAt  string `json:"expiresAt"`
	LastUsedAt string `json:"lastUsedAt,omitempty"`
	LastIP     string `json:"lastIp,omitempty"`
}

type trustedClientStore struct {
	mu      sync.Mutex
	clients map[string]*TrustedClient // key: token
	path    string
}

func trustedClientsPath() string {
	return filepath.Join(dataDir(), "trusted_clients.json")
}

func initTrustedClientStore() *trustedClientStore {
	st := &trustedClientStore{
		clients: map[string]*TrustedClient{},
		path:    trustedClientsPath(),
	}
	st.load()
	return st
}

// load 启动时加载令牌文件，顺手清掉过期条目。文件不存在是正常情况。
func (st *trustedClientStore) load() {
	st.mu.Lock()
	defer st.mu.Unlock()
	raw, err := os.ReadFile(st.path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("可信客户端文件读取失败", "err", err)
		}
		return
	}
	var file struct {
		Clients []*TrustedClient `json:"clients"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		slog.Warn("可信客户端文件解析失败，已忽略", "err", err)
		return
	}
	now := time.Now()
	for _, c := range file.Clients {
		if c == nil || c.Token == "" {
			continue
		}
		if exp, err := time.Parse(time.RFC3339, c.ExpiresAt); err == nil && now.After(exp) {
			continue // 过期丢弃
		}
		st.clients[c.Token] = c
	}
	if len(st.clients) > 0 {
		slog.Info("已加载可信客户端", "count", len(st.clients))
	}
}

// saveLocked 持久化到磁盘（0600）。调用方必须持有 st.mu。
func (st *trustedClientStore) saveLocked() {
	file := struct {
		Clients []*TrustedClient `json:"clients"`
	}{Clients: make([]*TrustedClient, 0, len(st.clients))}
	for _, c := range st.clients {
		file.Clients = append(file.Clients, c)
	}
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		slog.Error("可信客户端序列化失败", "err", err)
		return
	}
	if err := os.WriteFile(st.path, raw, 0600); err != nil {
		slog.Error("可信客户端写入失败", "err", err)
	}
}

// create 签发一枚新的可信令牌。设备数超上限时淘汰最久未使用的。
func (st *trustedClientStore) create(name, ip string) (*TrustedClient, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	now := time.Now()
	c := &TrustedClient{
		Name:      name,
		Token:     hex.EncodeToString(buf),
		CreatedAt: now.Format(time.RFC3339),
		ExpiresAt: now.Add(trustedClientTTL).Format(time.RFC3339),
		LastIP:    ip,
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.clients) >= maxTrustedClients {
		var oldestTok string
		var oldest time.Time
		first := true
		for tok, oc := range st.clients {
			mark := oc.LastUsedAt
			if mark == "" {
				mark = oc.CreatedAt
			}
			t, err := time.Parse(time.RFC3339, mark)
			if err != nil {
				continue
			}
			if first || t.Before(oldest) {
				first, oldestTok, oldest = false, tok, t
			}
		}
		if oldestTok != "" {
			delete(st.clients, oldestTok)
		}
	}
	st.clients[c.Token] = c
	st.saveLocked()
	return c, nil
}

// validate 校验可信令牌：有效则滑动续期并返回副本；无效/过期返回 nil。
func (st *trustedClientStore) validate(token, ip string) *TrustedClient {
	if token == "" {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	c, ok := st.clients[token]
	if !ok {
		return nil
	}
	now := time.Now()
	if exp, err := time.Parse(time.RFC3339, c.ExpiresAt); err != nil || now.After(exp) {
		delete(st.clients, token)
		st.saveLocked()
		return nil
	}
	// 滑动续期
	c.ExpiresAt = now.Add(trustedClientTTL).Format(time.RFC3339)
	c.LastUsedAt = now.Format(time.RFC3339)
	c.LastIP = ip
	st.saveLocked()
	out := *c
	return &out
}

// revoke 吊销一枚令牌（按完整令牌匹配）
func (st *trustedClientStore) revoke(token string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.clients[token]; !ok {
		// 也允许用「token 前缀」吊销（管理页只展示打码令牌时传前缀）
		for tok := range st.clients {
			if len(token) >= 8 && len(tok) >= len(token) && tok[:len(token)] == token {
				delete(st.clients, tok)
				st.saveLocked()
				return true
			}
		}
		return false
	}
	delete(st.clients, token)
	st.saveLocked()
	return true
}

// revokeAll 吊销全部可信客户端
func (st *trustedClientStore) revokeAll() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.clients = map[string]*TrustedClient{}
	st.saveLocked()
}

// list 返回打码后的设备列表（新的在前），令牌只保留前 8 位用于吊销匹配
func (st *trustedClientStore) list() []map[string]string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]map[string]string, 0, len(st.clients))
	for _, c := range st.clients {
		masked := c.Token
		if len(masked) > 8 {
			masked = masked[:8] + "…"
		}
		out = append(out, map[string]string{
			"name":        c.Name,
			"token":       masked,
			"tokenPrefix": c.Token[:8],
			"createdAt":   c.CreatedAt,
			"expiresAt":   c.ExpiresAt,
			"lastUsedAt":  c.LastUsedAt,
			"lastIp":      c.LastIP,
		})
	}
	// 按创建时间倒序
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j]["createdAt"] > out[i]["createdAt"] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// ---------- HTTP 管理接口（/api/trusted/*，需已登录会话或 apiToken） ----------

// routeTrusted /api/trusted/{sub} 子路由：可信设备的查看与吊销。
// 签发不在此处——签发只发生在完整登录（Token+2FA）成功时。
func (s *apiServer) routeTrusted(w http.ResponseWriter, r *http.Request, sub []string) {
	switch {
	case len(sub) == 1 && sub[0] == "list" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{"clients": s.trusted.list()})
	case len(sub) == 1 && sub[0] == "revoke" && r.Method == http.MethodPost:
		var req struct {
			Token string `json:"token"` // 完整令牌或列表里返回的 tokenPrefix
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体解析失败")
			return
		}
		if strings.TrimSpace(req.Token) == "" {
			writeErr(w, http.StatusBadRequest, "缺少 token")
			return
		}
		if !s.trusted.revoke(strings.TrimSpace(req.Token)) {
			writeErr(w, http.StatusNotFound, "未找到该可信设备（可能已吊销或过期）")
			return
		}
		s.guard.RecordEvent(s.realIP(r), "已吊销一台可信设备")
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case len(sub) == 1 && sub[0] == "revoke-all" && r.Method == http.MethodPost:
		s.trusted.revokeAll()
		s.guard.RecordEvent(s.realIP(r), "已吊销全部可信设备")
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeErr(w, http.StatusNotFound, "未知可信设备接口")
	}
}
