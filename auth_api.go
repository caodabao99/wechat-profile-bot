package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// 网页端双因素登录的专用接口（/api/auth/*）。
// 流程：
//  1. POST /api/auth/login         {token}            → stage=setup（首次，返回绑定二维码）
//                                   或 stage=2fa（已绑定，返回需输动态码）
//  2. POST /api/auth/2fa/enable    {token,secret,code} → 校验验证码并落盘密钥，颁发会话
//     POST /api/auth/2fa/verify    {token,code}        → 校验动态码，颁发会话
//  3. POST /api/auth/logout                             → 注销当前会话
//     POST /api/auth/2fa/disable   (会话) {code}        → 验证动态码后关闭 2FA

type loginRequest struct {
	Token string `json:"token"`
}

type twofaEnableRequest struct {
	Token  string `json:"token"`
	Secret string `json:"secret"`
	Code   string `json:"code"`
}

type twofaVerifyRequest struct {
	Token string `json:"token"`
	Code  string `json:"code"`
}

func decodeAuthBody(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return false
	}
	return true
}

func (s *apiServer) routeAuth(w http.ResponseWriter, r *http.Request, sub []string) {
	switch {
	case len(sub) == 1 && sub[0] == "login":
		s.hAuthLogin(w, r)
	case len(sub) == 2 && sub[0] == "2fa" && sub[1] == "enable":
		s.hAuthEnable(w, r)
	case len(sub) == 2 && sub[0] == "2fa" && sub[1] == "verify":
		s.hAuthVerify(w, r)
	case len(sub) == 2 && sub[0] == "2fa" && sub[1] == "disable":
		s.hAuthDisable(w, r)
	case len(sub) == 1 && sub[0] == "logout":
		s.hAuthLogout(w, r)
	default:
		writeErr(w, http.StatusNotFound, "未知认证接口")
	}
}

// hAuthLogin 第一因素校验：apiToken 正确后告知前端是「首次绑定」还是「输动态码」
func (s *apiServer) hAuthLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeAuthBody(w, r, &req) {
		return
	}

	// 未配置 apiToken（不推荐）：直接颁发会话，保证页面仍可进
	if s.cfg.APIToken == "" {
		tok, err := s.sessions.create()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "会话创建失败")
			return
		}
		slog.Warn("apiToken 未配置，网页登录跳过双因素认证")
		writeJSON(w, http.StatusOK, map[string]string{"stage": "ok", "session": tok})
		return
	}

	if !tokenMatches(strings.TrimSpace(req.Token), s.cfg.APIToken) {
		// 登录通道第一因素失败：计入封禁计数（同一 IP 累计 10 次即永久封禁）
		s.guard.RecordAuthFailure(s.realIP(r), "Token 错误")
		writeErr(w, http.StatusUnauthorized, "Token 无效")
		return
	}

	if totpEnabled() {
		writeJSON(w, http.StatusOK, map[string]string{"stage": "2fa"})
		return
	}

	// 首次登录：现场生成密钥和绑定二维码（密钥先不落盘，验证码确认通过才保存，
	// 避免扫了码但没完成绑定产生死密钥）
	secret, err := totpNewSecret()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成 2FA 密钥失败: "+err.Error())
		return
	}
	account := s.cfg.MyName
	if account == "" {
		account = "admin"
	}
	otpauth := totpProvisioningURI(secret, account)
	qr, err := qrcodeDataURI(otpauth)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成二维码失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"stage":   "setup",
		"secret":  secret,
		"otpauth": otpauth,
		"qrPng":   qr,
	})
}

// issueSession 双因素通过后统一颁发会话，并清除该 IP 的登录失败计数
func (s *apiServer) issueSession(w http.ResponseWriter, r *http.Request) {
	tok, err := s.sessions.create()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "会话创建失败")
		return
	}
	s.guard.RecordAuthSuccess(s.realIP(r))
	writeJSON(w, http.StatusOK, map[string]string{"stage": "ok", "session": tok})
}

// hAuthEnable 首次绑定：校验验证码 → 保存密钥 → 颁发会话
func (s *apiServer) hAuthEnable(w http.ResponseWriter, r *http.Request) {
	var req twofaEnableRequest
	if !decodeAuthBody(w, r, &req) {
		return
	}
	if s.cfg.APIToken != "" && !tokenMatches(strings.TrimSpace(req.Token), s.cfg.APIToken) {
		s.guard.RecordAuthFailure(s.realIP(r), "绑定时 Token 错误")
		writeErr(w, http.StatusUnauthorized, "Token 无效")
		return
	}
	totpMu.Lock()
	defer totpMu.Unlock()
	if _, err := os.Lstat(totpSecretPath()); err == nil {
		writeErr(w, http.StatusConflict, "2FA 已绑定，请使用动态码登录")
		return
	} else if !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, "检查 2FA 密钥失败")
		return
	}
	if strings.TrimSpace(req.Secret) == "" {
		writeErr(w, http.StatusBadRequest, "缺少绑定密钥，请返回上一步重新获取二维码")
		return
	}
	if !totpValidate(req.Secret, req.Code) {
		s.guard.RecordAuthFailure(s.realIP(r), "绑定验证码错误")
		writeErr(w, http.StatusBadRequest, "验证码无效或已过期，请确认验证器时间准确后重试")
		return
	}
	if err := totpSaveSecret(&totpSecretFile{
		Secret:    strings.ToUpper(strings.TrimSpace(req.Secret)),
		CreatedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存 2FA 密钥失败: "+err.Error())
		return
	}
	slog.Info("网页端双因素认证已启用", "secret_file", totpSecretPath())
	s.issueSession(w, r)
}

// hAuthVerify 已绑定用户的日常登录：校验 TOTP 动态码
func (s *apiServer) hAuthVerify(w http.ResponseWriter, r *http.Request) {
	var req twofaVerifyRequest
	if !decodeAuthBody(w, r, &req) {
		return
	}
	if s.cfg.APIToken != "" && !tokenMatches(strings.TrimSpace(req.Token), s.cfg.APIToken) {
		s.guard.RecordAuthFailure(s.realIP(r), "登录时 Token 错误")
		writeErr(w, http.StatusUnauthorized, "Token 无效")
		return
	}
	totpMu.Lock()
	defer totpMu.Unlock()
	f, err := totpLoadSecret()
	if err != nil {
		slog.Error("读取 2FA 密钥失败", "err", err)
		writeErr(w, http.StatusInternalServerError, "2FA 未正确配置，请在服务器删除 totp_secret.json 后重新绑定")
		return
	}
	if !totpValidate(f.Secret, req.Code) {
		s.guard.RecordAuthFailure(s.realIP(r), "动态码错误")
		writeErr(w, http.StatusBadRequest, "动态码无效或已过期，请确认输入的是验证器当前显示的 6 位码且手机时间准确")
		return
	}
	s.issueSession(w, r)
}

// hAuthLogout 退出：吊销当前网页会话（apiToken 不是会话，无法在此吊销）
func (s *apiServer) hAuthLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	if tok := bearerToken(r); tok != "" {
		s.sessions.revoke(tok)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// hAuthDisable 关闭 2FA：必须持有有效网页会话并再次验证动态码，
// 通过后删除密钥文件并吊销全部网页会话。手机丢失无法走这里，直接在服务器删文件。
func (s *apiServer) hAuthDisable(w http.ResponseWriter, r *http.Request) {
	tok := bearerToken(r)
	if !s.sessions.valid(tok) {
		writeErr(w, http.StatusUnauthorized, "网页会话已过期，请重新登录")
		return
	}
	var req twofaVerifyRequest
	if !decodeAuthBody(w, r, &req) {
		return
	}
	totpMu.Lock()
	defer totpMu.Unlock()
	f, err := totpLoadSecret()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "2FA 未启用")
		return
	}
	if !totpValidate(f.Secret, req.Code) {
		// 已持有有效会话才能走到这里，不计入封禁（避免用户手滑输错码把自己封死），
		// 但要在安全日志留痕：会话令牌若被盗，攻击者会在这里试动态码
		s.guard.RecordDenied(s.realIP(r), "/api/auth/2fa/disable", "关闭2FA动态码错误")
		writeErr(w, http.StatusBadRequest, "动态码无效或已过期")
		return
	}
	if err := os.Remove(totpSecretPath()); err != nil && !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, "删除密钥文件失败: "+err.Error())
		return
	}
	s.sessions.revokeAll()
	s.guard.RecordEvent(s.realIP(r), "2FA 已关闭，全部网页会话已吊销")
	slog.Warn("网页端双因素认证已关闭")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
