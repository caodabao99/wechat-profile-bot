package main

// Phase C（模型调用量统计）回归：
//   - logLLMCall 直写 + ComputeLLMUsage 三窗口/按模型聚合；
//   - CallContext 端到端（httptest 模拟 OpenAI 兼容接口）验证 resolveSpec→真实调用→自动记用量→token 解析；
//   - 表缺失/无数据时聚合返回空视图不报错；
//   - GET /api/llm/usage 端到端。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLLMUsageRecordAndAggregate(t *testing.T) {
	db := regressionDB(t)
	c := NewLLMClient(&Config{}).WithDB(db)
	spec := llmSpec{ProfileID: "a", Label: "DeepSeek", Provider: "deepseek", Model: "deepseek-chat", Region: regionDomestic}

	// 2 成功 1 失败
	c.logLLMCall(spec, string(TaskProfile), true, 200, llmUsage{Prompt: 10, Completion: 5, Total: 15}, 100)
	c.logLLMCall(spec, string(TaskProfile), true, 200, llmUsage{Prompt: 20, Completion: 10, Total: 30}, 200)
	c.logLLMCall(spec, string(TaskProfile), false, 429, llmUsage{}, 50)

	u, err := ComputeLLMUsage(db)
	if err != nil {
		t.Fatal(err)
	}
	if u.Today.Calls != 3 || u.Today.Success != 2 || u.Today.Errors != 1 {
		t.Fatalf("今日应 3 调用/2 成功/1 失败，得 %#v", u.Today)
	}
	if u.Today.TotalTokens != 45 {
		t.Fatalf("今日总 token 应为 45，得 %d", u.Today.TotalTokens)
	}
	if u.Today.AvgLatencyMS != (100+200+50)/3 {
		t.Fatalf("平均延迟应 %d，得 %d", (100+200+50)/3, u.Today.AvgLatencyMS)
	}
	if u.Week.Calls != 3 || u.Month.Calls != 3 {
		t.Fatalf("7d/30d 窗口应同为 3，得 week=%d month=%d", u.Week.Calls, u.Month.Calls)
	}
	if len(u.ByModel) != 1 || u.ByModel[0].Model != "deepseek-chat" || u.ByModel[0].Calls != 3 || u.ByModel[0].Success != 2 {
		t.Fatalf("按模型分解应聚成 1 行 3 调用，得 %#v", u.ByModel)
	}
}

func TestLLMUsageEmptyNoError(t *testing.T) {
	db := regressionDB(t)
	u, err := ComputeLLMUsage(db)
	if err != nil {
		t.Fatalf("无数据不应报错：%v", err)
	}
	if u.Today.Calls != 0 || len(u.ByModel) != 0 {
		t.Fatalf("空库应返回空视图，得 %#v", u)
	}
}

func TestLLMCallContextRecordsUsage(t *testing.T) {
	db := regressionDB(t)
	// 模拟 OpenAI 兼容接口，返回固定内容 + usage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
	}))
	defer srv.Close()

	// 活动档案指向 httptest 服务器
	if err := saveLLMSettings(db, LLMSettings{
		ActiveProfileID: "mock",
		Profiles:        []LLMProfile{{ID: "mock", Label: "Mock", Provider: "custom", BaseURL: srv.URL, APIKey: "sk-mock", Model: "mock-model"}},
	}); err != nil {
		t.Fatal(err)
	}
	c := NewLLMClient(&Config{}).WithDB(db)
	content, err := c.CallContext(t.Context(), "hello")
	if err != nil {
		t.Fatalf("CallContext 失败: %v", err)
	}
	if content != `{"ok":true}` {
		t.Fatalf("内容不符，得 %q", content)
	}
	// 真实调用应被记一笔用量，token 来自响应 usage
	u, err := ComputeLLMUsage(db)
	if err != nil {
		t.Fatal(err)
	}
	if u.Today.Calls != 1 || u.Today.Success != 1 || u.Today.TotalTokens != 10 {
		t.Fatalf("应记 1 次成功调用、10 token，得 %#v", u.Today)
	}
	if len(u.ByModel) != 1 || u.ByModel[0].Model != "mock-model" {
		t.Fatalf("应按 mock-model 聚合，得 %#v", u.ByModel)
	}
}

func TestLLMUsageAPI(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	w := callAPI(s, http.MethodGet, "/api/llm/usage", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET usage: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK    bool     `json:"ok"`
		Usage LLMUsage `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatal("应 ok=true")
	}
}
