package main

// 自定义请求体参数(extraBody)的验收：
//   - validateLLMExtraBody：空放行；合法 JSON 对象放行；数组/标量/坏 JSON 拒绝；
//   - saveLLMSettings：坏 extraBody 直接拒绝，不落库；
//   - 端到端：extraBody 逐键合并进真实 HTTP 请求体，可覆盖内置默认(temperature/thinking/max_tokens)，
//     但结构性键 messages 受保护、不被 extraBody 覆盖（防请求畸形）。
// 全程 httptest，零真实外网。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestValidateLLMExtraBody(t *testing.T) {
	ok := []string{"", `{}`, `{"thinking":{"type":"low"}}`, `{"temperature":0.3}`}
	for _, s := range ok {
		if err := validateLLMExtraBody("p", s); err != nil {
			t.Fatalf("合法值被拒: %q → %v", s, err)
		}
	}
	bad := []string{`{`, `[1,2]`, `"str"`, `123`, `{"a":`}
	for _, s := range bad {
		if err := validateLLMExtraBody("p", s); err == nil {
			t.Fatalf("非法 JSON 对象应被拒: %q", s)
		}
	}
}

func TestSaveRejectsBadExtraBody(t *testing.T) {
	db := regressionDB(t)
	err := saveLLMSettings(db, LLMSettings{Profiles: []LLMProfile{
		{ID: "x", Label: "坏档案", BaseURL: "https://api.example.com", APIKey: "k", Model: "m", ExtraBody: "{not-json"},
	}})
	if err == nil {
		t.Fatal("非法 extraBody 应被 saveLLMSettings 拒绝")
	}
}

func TestExtraBodyMergedIntoRequest(t *testing.T) {
	var mu sync.Mutex
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		got = m
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"ok\":true}"}}],"usage":{"total_tokens":5}}`))
	}))
	defer srv.Close()

	client := NewLLMClient(&Config{LLM: LLMConfig{
		ApiKey:          "test-key",
		BaseURL:         srv.URL,
		Model:           "glm-x",
		DisableThinking: false,
		ExtraBody:       `{"thinking":{"type":"low"},"temperature":0.9,"max_tokens":64,"messages":[{"role":"user","content":"HACKED"}]}`,
	}})
	if _, err := client.Call("真实提示词"); err != nil {
		t.Fatalf("调用失败: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got == nil {
		t.Fatal("未捕获到请求体")
	}
	if got["model"] != "glm-x" {
		t.Fatalf("model 应为 glm-x，实得 %v", got["model"])
	}
	if v, _ := got["temperature"].(float64); v != 0.9 {
		t.Fatalf("extraBody 应覆盖 temperature 为 0.9，实得 %v", got["temperature"])
	}
	if v, _ := got["max_tokens"].(float64); v != 64 {
		t.Fatalf("max_tokens 应合并为 64，实得 %v", got["max_tokens"])
	}
	th, _ := got["thinking"].(map[string]interface{})
	if th == nil || th["type"] != "low" {
		t.Fatalf("thinking 应合并为 {type:low}，实得 %v", got["thinking"])
	}
	// 结构性键 messages 必须未被 extraBody 覆盖：仍是真实提示词，而非 HACKED。
	msgs, _ := got["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Fatalf("messages 应恰一条，实得 %v", got["messages"])
	}
	m0, _ := msgs[0].(map[string]interface{})
	if m0["content"] != "真实提示词" {
		t.Fatalf("messages 必须受保护、不被 extraBody 覆盖，实得 content=%v", m0["content"])
	}
	if strings.Contains(fmtAny(got["messages"]), "HACKED") {
		t.Fatal("messages 被 extraBody 覆盖，保护失效")
	}
}

func fmtAny(v interface{}) string { b, _ := json.Marshal(v); return string(b) }
