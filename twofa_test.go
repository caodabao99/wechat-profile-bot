package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// 同一动态码在其 30 秒有效期内可重复校验通过：
// 覆盖「网页登录 → 退出 → 立刻用验证器上仍显示的同一个码重新登录」场景，
// 该行为是刻意取消 TOTP 时间步防重放后的约定，不要重新加回 lastStep 拒绝逻辑。
func TestTOTPValidateAcceptsRepeatedCode(t *testing.T) {
	const secret = "JBSWY3DPEHPK3PXP"
	code, err := totpCodeAt(secret, time.Now().Unix()/totpPeriod)
	if err != nil {
		t.Fatal(err)
	}
	if !totpValidate(secret, code) {
		t.Fatal("first validation should succeed")
	}
	if !totpValidate(secret, code) {
		t.Fatal("same code must remain valid on immediate re-login (no step replay rejection)")
	}
}

func TestTOTPValidateRejectsWrongCode(t *testing.T) {
	const secret = "JBSWY3DPEHPK3PXP"
	if totpValidate(secret, "000000") {
		// 极低概率撞上真实码，换一个明显不相关的码再判一次
		if totpValidate(secret, "123456") {
			t.Fatal("wrong code accepted")
		}
	}
	if totpValidate(secret, "12345") {
		t.Fatal("short code accepted")
	}
	if totpValidate(secret, "abcdef") {
		t.Fatal("non-numeric code accepted")
	}
	if totpValidate("not-base32!!!", "123456") {
		t.Fatal("invalid secret accepted")
	}
}

// 完整 HTTP 场景：同一动态码连续两次走 /api/auth/2fa/verify 都必须成功，
// 中间先退出第一次的会话，模拟登录→退出→立刻重登。
func TestAuthVerifySameCodeAfterLogout(t *testing.T) {
	dir := t.TempDir()
	original := totpSecretPath
	totpSecretPath = func() string { return filepath.Join(dir, "totp_secret.json") }
	defer func() { totpSecretPath = original }()

	const secret = "JBSWY3DPEHPK3PXP"
	if err := totpSaveSecret(&totpSecretFile{Secret: secret, CreatedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	s := &apiServer{
		cfg:      &Config{APIToken: "test-token"},
		sessions: &webSessionStore{sessions: map[string]time.Time{}, path: filepath.Join(dir, "sessions.json")},
	}

	code, err := totpCodeAt(secret, time.Now().Unix()/totpPeriod)
	if err != nil {
		t.Fatal(err)
	}
	verify := func() *httptest.ResponseRecorder {
		body, _ := json.Marshal(twofaVerifyRequest{Token: "test-token", Code: code})
		w := httptest.NewRecorder()
		s.hAuthVerify(w, httptest.NewRequest(http.MethodPost, "/api/auth/2fa/verify", bytes.NewReader(body)))
		return w
	}

	first := verify()
	if first.Code != http.StatusOK {
		t.Fatalf("first login status = %d: %s", first.Code, first.Body.String())
	}
	var resp struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &resp); err != nil || resp.Session == "" {
		t.Fatalf("missing session: %s", first.Body.String())
	}

	// 退出第一次的会话
	lw := httptest.NewRecorder()
	lr := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	lr.Header.Set("Authorization", "Bearer "+resp.Session)
	s.hAuthLogout(lw, lr)
	if lw.Code != http.StatusOK {
		t.Fatalf("logout status = %d", lw.Code)
	}

	// 立刻用同一个动态码重新登录，必须仍然成功（旧版在此返回 400「同一验证码不能重复使用」）
	second := verify()
	if second.Code != http.StatusOK {
		t.Fatalf("re-login with same code status = %d: %s", second.Code, second.Body.String())
	}

	// 错误的码依旧被拒
	bad, _ := json.Marshal(twofaVerifyRequest{Token: "test-token", Code: "654321"})
	bw := httptest.NewRecorder()
	s.hAuthVerify(bw, httptest.NewRequest(http.MethodPost, "/api/auth/2fa/verify", bytes.NewReader(bad)))
	if bw.Code != http.StatusBadRequest {
		t.Fatalf("wrong code status = %d: %s", bw.Code, bw.Body.String())
	}
}
