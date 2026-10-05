package main

// TOTP 附加安全测试（与 twofa_test.go 互补，专注此前 0% 覆盖的路径）：
//   - 用 RFC 6238 附录 B 已知答案向量校验 HMAC-SHA1 动态截断（最易出 bug 处）
//   - totpNewSecret / totpProvisioningURI 正确性
//   - tokenMatches 常量时间比较语义
//   - webSessionStore 生命周期：创建/校验/未知/过期清理/活动续期/单条吊销/全量吊销
// 现有的 totpValidate 基础用例已在 twofa_test.go 覆盖，这里不重复。

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// RFC 6238 SHA1 密钥 "12345678901234567890" 的 base32（无填充）
const rfc6238Secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

func TestTOTPCodeAtKnownVectors(t *testing.T) {
	// 取自 RFC 6238 附录 B 的 8 位码，取末 6 位；step = T/30
	cases := []struct {
		step int64
		want string
	}{
		{59 / 30, "287082"},         // 94287082
		{1111111109 / 30, "081804"}, // 07081804
		{1111111111 / 30, "050471"}, // 14050471
		{1234567893 / 30, "005924"}, // 89005924
		{2000000000 / 30, "279037"}, // 69279037
	}
	for _, c := range cases {
		got, err := totpCodeAt(rfc6238Secret, c.step)
		if err != nil {
			t.Fatalf("step=%d 计算出错: %v", c.step, err)
		}
		if got != c.want {
			t.Errorf("step=%d 动态码=%q 期望=%q（RFC 6238 向量不符，截断逻辑可能有 bug）", c.step, got, c.want)
		}
	}
	if code, _ := totpCodeAt(rfc6238Secret, 1); len(code) != totpDigits {
		t.Fatalf("动态码应为 %d 位，得 %q", totpDigits, code)
	}
}

func TestTOTPCodeAtBadBase32(t *testing.T) {
	if _, err := totpCodeAt("!!!非base32!!!", 1); err == nil {
		t.Fatal("非法 base32 密钥应报错")
	}
}

func TestTOTPValidateWindowTolerance(t *testing.T) {
	nowStep := time.Now().Unix() / totpPeriod
	for d := int64(-totpWindow); d <= totpWindow; d++ {
		code, err := totpCodeAt(rfc6238Secret, nowStep+d)
		if err != nil {
			t.Fatal(err)
		}
		if !totpValidate(rfc6238Secret, code) {
			t.Fatalf("窗口内 d=%d 的码 %q 应通过校验", d, code)
		}
	}
	inWindow := map[string]bool{}
	for d := int64(-totpWindow); d <= totpWindow; d++ {
		c, _ := totpCodeAt(rfc6238Secret, nowStep+d)
		inWindow[c] = true
	}
	far, _ := totpCodeAt(rfc6238Secret, nowStep-2)
	if !inWindow[far] && totpValidate(rfc6238Secret, far) {
		t.Fatalf("窗口外码 %q 不应通过", far)
	}
}

func TestTOTPNewSecret(t *testing.T) {
	s1, err := totpNewSecret()
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := totpNewSecret()
	if s1 == s2 {
		t.Fatal("两次生成的密钥不应相同")
	}
	if _, err := totpCodeAt(s1, 1); err != nil { // 生成的密钥应是合法 base32 可用
		t.Fatalf("生成的密钥应可用于计算: %v", err)
	}
}

func TestTOTPProvisioningURI(t *testing.T) {
	uri := totpProvisioningURI(rfc6238Secret, "me@example.com")
	if !strings.HasPrefix(uri, "otpauth://totp/") {
		t.Fatalf("应以 otpauth://totp/ 开头: %q", uri)
	}
	for _, must := range []string{"secret=" + rfc6238Secret, "issuer=WeChatProfileBot", "algorithm=SHA1", "digits=6", "period=30"} {
		if !strings.Contains(uri, must) {
			t.Errorf("otpauth URI 缺少 %q：%s", must, uri)
		}
	}
	uri2 := totpProvisioningURI(rfc6238Secret, "")
	if !strings.HasPrefix(uri2, "otpauth://totp/WeChatProfileBot") {
		t.Errorf("无 account 时 label 应仅为 issuer: %q", uri2)
	}
}

func TestTokenMatches(t *testing.T) {
	if !tokenMatches("secret", "secret") {
		t.Fatal("相同应匹配")
	}
	if tokenMatches("secret", "") {
		t.Fatal("空输入不应匹配")
	}
	if tokenMatches("secret", "secreu") {
		t.Fatal("不同不应匹配")
	}
}

func TestWebSessionStoreLifecycle(t *testing.T) {
	s := &webSessionStore{sessions: map[string]time.Time{}, path: filepath.Join(t.TempDir(), "web_sessions.json")}

	tok, err := s.create()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 64 { // 32 字节十六进制
		t.Fatalf("令牌应为 64 位十六进制，得 %d 位", len(tok))
	}
	if !s.valid(tok) {
		t.Fatal("新建会话应立即有效")
	}
	if s.valid("deadbeef") {
		t.Fatal("未知令牌应无效")
	}
	if s.valid("") {
		t.Fatal("空令牌应无效")
	}

	// 过期会话：valid 返回 false 并清理
	s.sessions["expired"] = time.Now().Add(-time.Minute)
	if s.valid("expired") {
		t.Fatal("已过期会话应无效")
	}
	if _, ok := s.sessions["expired"]; ok {
		t.Fatal("过期会话应被删除")
	}

	// 活动续期：剩余不足 sessionRenew 时 valid 应延长到完整 TTL
	soon := time.Now().Add(time.Hour) // < sessionRenew(24h)
	s.sessions["renew"] = soon
	if !s.valid("renew") {
		t.Fatal("未过期会话应有效")
	}
	if !s.sessions["renew"].After(soon.Add(time.Hour)) {
		t.Fatalf("接近过期应续期，原=%v 现=%v", soon, s.sessions["renew"])
	}

	s.revoke(tok)
	if s.valid(tok) {
		t.Fatal("注销后应无效")
	}

	s.create()
	s.create()
	s.revokeAll()
	if len(s.sessions) != 0 {
		t.Fatalf("revokeAll 后应无会话，剩 %d", len(s.sessions))
	}
}
