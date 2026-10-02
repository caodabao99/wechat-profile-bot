package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// 网页管理端二次认证（TOTP，RFC 6238，兼容 Google Authenticator / 微软 Authenticator /
// 微信 / 支付宝等支持 otpauth 的验证器）。
//
// 安全模型（单人自托管）：
//   - apiToken 是第一因素（你知道的长随机串），TOTP 动态码是第二因素（你手机上的验证器）
//   - 浏览器通过两步后只拿到一个有期限、可吊销的「网页会话令牌」，令牌存在 localStorage，
//     不再长期持有 apiToken；服务端重启会话仍然有效（持久化到 web_sessions.json）
//   - 桌面端远程模式继续直接用 apiToken 走 Authorization 头，中间件两者都认
//   - TOTP 密钥存独立文件 totp_secret.json（0600），不进 config.json；手机/验证器丢失时
//     在服务器上删除该文件，下次网页登录会重新走绑定流程
const (
	totpDigits   = 6  // 动态码位数
	totpPeriod   = 30 // 时间步长（秒）
	totpWindow   = 1  // 允许前后各 1 个时间步（±30s 时钟偏差）
	sessionTTL   = 7 * 24 * time.Hour
	sessionRenew = 24 * time.Hour // 剩余有效期不足此值时活动续期
	totpIssuer   = "WeChatProfileBot"
)

// dataDir 返回运行数据目录（Docker 下 /config，否则为可执行文件同目录）
func dataDir() string {
	if _, err := os.Stat("/config"); err == nil {
		return "/config"
	}
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// ---------------- TOTP 密钥文件 ----------------

type totpSecretFile struct {
	Secret       string `json:"secret"`       // base32（无填充）的共享密钥
	CreatedAt    string `json:"createdAt"`    // RFC3339
	LastUsedStep int64  `json:"lastUsedStep"` // 最近一次成功验证的时间步，用于防重放
}

func totpSecretPath() string { return filepath.Join(dataDir(), "totp_secret.json") }

func totpLoadSecret() (*totpSecretFile, error) {
	b, err := os.ReadFile(totpSecretPath())
	if err != nil {
		return nil, err
	}
	var f totpSecretFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	if f.Secret == "" {
		return nil, fmt.Errorf("totp_secret.json 内容无效")
	}
	return &f, nil
}

func totpSaveSecret(f *totpSecretFile) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(totpSecretPath(), b, 0600)
}

func totpEnabled() bool {
	_, err := os.Stat(totpSecretPath())
	return err == nil
}

// totpNewSecret 生成 160bit 随机密钥，返回 base32（无填充大写）形式
func totpNewSecret() (string, error) {
	raw := make([]byte, 20) // SHA1 推荐 160bit
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw), nil
}

// totpProvisioningURI 生成 otpauth:// 二维码内容
func totpProvisioningURI(secret, account string) string {
	label := totpIssuer
	if account != "" {
		label += ":" + account
	}
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", totpIssuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(totpDigits))
	q.Set("period", fmt.Sprint(totpPeriod))
	return "otpauth://totp/" + url.PathEscape(label) + "?" + q.Encode()
}

// totpCodeAt 计算指定时间步的动态码（仅测试与验证内部使用）
func totpCodeAt(secretB32 string, step int64) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.TrimSpace(secretB32)))
	if err != nil {
		return "", fmt.Errorf("密钥不是合法 base32: %w", err)
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	off := int(sum[len(sum)-1] & 0x0f)
	v := (int(sum[off])&0x7f)<<24 |
		(int(sum[off+1])&0xff)<<16 |
		(int(sum[off+2])&0xff)<<8 |
		(int(sum[off+3]) & 0xff)
	return fmt.Sprintf("%0*d", totpDigits, v%1000000), nil
}

// totpValidate 校验用户输入的 6 位码。
// lastStep 为该密钥上次成功使用的时间步（防重放），返回本次命中的时间步供调用方落盘。
// 允许 ±totpWindow 个时间步以容忍手机时钟偏差；成功命中步必须大于 lastStep。
func totpValidate(secretB32, code string, lastStep int64) (int64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return 0, false
	}
	nowStep := time.Now().Unix() / totpPeriod
	for d := int64(-totpWindow); d <= totpWindow; d++ {
		step := nowStep + d
		if step <= lastStep {
			continue // 该时间步（或更早）的码已被成功使用过，拒绝重放
		}
		want, err := totpCodeAt(secretB32, step)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// ---------------- 网页会话 ----------------

// webSessionStore 管理网页登录后的会话令牌：内存 + 文件持久化，服务端重启不丢登录态
type webSessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time // token -> 过期时刻
	path     string
}

func newWebSessionStore() *webSessionStore {
	return &webSessionStore{sessions: map[string]time.Time{}, path: sessionPath()}
}

func sessionPath() string { return filepath.Join(dataDir(), "web_sessions.json") }

// initWebSessionStore 从磁盘加载历史会话并清理已过期项
func initWebSessionStore() *webSessionStore {
	s := newWebSessionStore()
	b, err := os.ReadFile(s.path)
	if err == nil {
		var raw map[string]string
		if json.Unmarshal(b, &raw) == nil {
			now := time.Now()
			for tok, expStr := range raw {
				if exp, err := time.Parse(time.RFC3339, expStr); err == nil && exp.After(now) {
					s.sessions[tok] = exp
				}
			}
		}
	}
	return s
}

func (s *webSessionStore) saveLocked() {
	raw := make(map[string]string, len(s.sessions))
	now := time.Now()
	for tok, exp := range s.sessions {
		if exp.After(now) {
			raw[tok] = exp.Format(time.RFC3339)
		}
	}
	b, _ := json.MarshalIndent(raw, "", "  ")
	if err := os.WriteFile(s.path, b, 0600); err != nil {
		slog.Warn("网页会话持久化失败", "err", err)
	}
}

// create 颁发新会话令牌
func (s *webSessionStore) create() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := fmt.Sprintf("%x", buf)
	s.mu.Lock()
	s.sessions[tok] = time.Now().Add(sessionTTL)
	s.saveLocked()
	s.mu.Unlock()
	return tok, nil
}

// valid 校验会话；活动期间自动续期（剩余不足 sessionRenew 时延长到完整 TTL）
func (s *webSessionStore) valid(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[tok]
	now := time.Now()
	if !ok || !exp.After(now) {
		delete(s.sessions, tok)
		return false
	}
	if time.Until(exp) < sessionRenew {
		s.sessions[tok] = now.Add(sessionTTL)
		s.saveLocked()
	}
	return true
}

// revoke 注销单个会话（退出登录）
func (s *webSessionStore) revoke(tok string) {
	s.mu.Lock()
	delete(s.sessions, tok)
	s.saveLocked()
	s.mu.Unlock()
}

// revokeAll 注销全部网页会话（关闭 2FA 时使用，强制所有人重新认证）
func (s *webSessionStore) revokeAll() {
	s.mu.Lock()
	s.sessions = map[string]time.Time{}
	s.saveLocked()
	s.mu.Unlock()
}

// bearerToken 取 Authorization: Bearer xxx 的令牌部分
func bearerToken(r *http.Request) string {
	return strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
}

// tokenMatches 常量时间比较 apiToken，防止时序侧信道
func tokenMatches(a, b string) bool {
	if b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// qrcodeDataURI 把文本编码成 PNG 二维码的 data URI，供登录页直接 <img src>
func qrcodeDataURI(text string) (string, error) {
	png, err := qrcode.Encode(text, qrcode.Medium, 220)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}
