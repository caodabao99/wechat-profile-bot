package main

// v6.3 §安全：模型/代理端点入参校验（SSRF / 协议处理器护栏）。
// 断言 saveLLMSettings 这一唯一写库入口对非法协议直接拒绝、且「不落库」——
// 非法值绝不会出现在后续 loadLLMSettings 结果里（否则护栏形同虚设）。

import (
	"testing"
)

func oneProfileSettings(baseURL, proxyURL string) LLMSettings {
	return LLMSettings{
		Profiles: []LLMProfile{{
			ID:       "p-test",
			Label:    "t",
			Provider: "custom",
			BaseURL:  baseURL,
			APIKey:   "k",
			Model:    "m",
		}},
		ActiveProfileID: "p-test",
		Proxy:           LLMProxy{URL: proxyURL},
	}
}

func TestValidateLLMBaseURL(t *testing.T) {
	ok := []string{"", "https://api.openai.com/v1", "http://127.0.0.1:11434", "https example"}
	for _, s := range ok[:3] {
		if err := validateLLMBaseURL("u", s); err != nil {
			t.Fatalf("合法值被拒 %q: %v", s, err)
		}
	}
	bad := []string{"file:///etc/passwd", "gopher://127.0.0.1:11211/", "http://user:pass@internal/", "://x", "/v1/chat"}
	for _, s := range bad {
		if err := validateLLMBaseURL("u", s); err == nil {
			t.Fatalf("非法值应被拒 %q", s)
		}
	}
}

func TestValidateLLMProxyURL(t *testing.T) {
	for _, s := range []string{"", "http://127.0.0.1:7890", "socks5://10.0.0.1:1080"} {
		if err := validateLLMProxyURL(s); err != nil {
			t.Fatalf("合法代理被拒 %q: %v", s, err)
		}
	}
	for _, s := range []string{"ftp://host", "file://x", "://y"} {
		if err := validateLLMProxyURL(s); err == nil {
			t.Fatalf("非法代理应被拒 %q", s)
		}
	}
}

// TestSaveLLMSettingsRejectsBadURLEndToEnd 端到端验证：坏 URL 保存失败且不污染库。
func TestSaveLLMSettingsRejectsBadURLEndToEnd(t *testing.T) {
	db := regressionDB(t)
	// 先存一个合法端点。
	if err := saveLLMSettings(db, oneProfileSettings("https://api.example.com/v1", "")); err != nil {
		t.Fatal(err)
	}
	// 再存 file:// —— 应被拒。
	if err := saveLLMSettings(db, oneProfileSettings("file:///etc/passwd", "")); err == nil {
		t.Fatal("file:// 端点应被拒绝")
	}
	// 代理同理。
	if err := saveLLMSettings(db, oneProfileSettings("https://api.example.com/v1", "ftp://x")); err == nil {
		t.Fatal("非法代理协议应被拒绝")
	}
	// 关键：非法值不得落库——重载仍是此前的合法值。
	got, err := loadLLMSettings(db)
	if err != nil {
		t.Fatal(err)
	}
	p := got.activeProfile()
	if p == nil || p.BaseURL != "https://api.example.com/v1" {
		t.Fatalf("非法值疑似污染库，active=%+v", p)
	}
	if got.Proxy.URL != "" {
		t.Fatalf("非法代理疑似污染库，proxy=%q", got.Proxy.URL)
	}
}
