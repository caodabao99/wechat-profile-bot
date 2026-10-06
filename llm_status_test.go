package main

// Phase E（/api/status 增加 llm 块）回归：
//   - 状态响应含 llm.configured / llm.active（活动模型摘要）/ llm.proxy（含最近测试）/ llm.usage（今日摘要）；
//   - effective useProxy 需 profile.useProxy && proxy.enabled && url 非空 三者同真；
//   - 未配置模型时 llm.configured=false 且接口仍 200（降级不报错）。

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

type statusResp struct {
	OK  bool `json:"ok"`
	LLM struct {
		Configured bool `json:"configured"`
		Active     *struct {
			ID              string `json:"id"`
			Model           string `json:"model"`
			Region          string `json:"region"`
			DisableThinking bool   `json:"disableThinking"`
			UseProxy        bool   `json:"useProxy"`
		} `json:"active"`
		Proxy struct {
			Enabled    bool   `json:"enabled"`
			Configured bool   `json:"configured"`
			EgressIP   string `json:"egressIp"`
			DirectIP   string `json:"directIp"`
			TestedAt   string `json:"testedAt"`
		} `json:"proxy"`
		Usage *struct {
			Today struct {
				Calls int `json:"calls"`
			} `json:"today"`
		} `json:"usage"`
	} `json:"llm"`
}

func TestStatusIncludesLLMBlock(t *testing.T) {
	db := regressionDB(t)
	if err := saveLLMSettings(db, LLMSettings{
		ActiveProfileID: "m",
		Profiles: []LLMProfile{{
			ID: "m", Label: "GPT", Provider: "openai", Model: "gpt-4o",
			APIKey: "sk-x", BaseURL: "https://api.openai.com/v1",
			Region: regionForeign, DisableThinking: true, UseProxy: true,
		}},
		Proxy: LLMProxy{
			Enabled: true, URL: "http://127.0.0.1:7890", TestedAt: "2026-10-06T00:00:00Z",
			EgressIP: "1.1.1.1", DirectIP: "2.2.2.2",
		},
	}); err != nil {
		t.Fatal(err)
	}
	c := NewLLMClient(&Config{}).WithDB(db)
	c.logLLMCall(llmSpec{ProfileID: "m", Model: "gpt-4o", Provider: "openai"}, "", true, 200, llmUsage{Total: 10}, 100)

	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, llm: c, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	w := callAPI(s, http.MethodGet, "/api/status", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/status: %d %s", w.Code, w.Body.String())
	}
	var resp statusResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatal("status 应 ok=true")
	}
	if !resp.LLM.Configured {
		t.Fatal("配了密钥的活动档案应 configured=true")
	}
	if resp.LLM.Active == nil || resp.LLM.Active.Model != "gpt-4o" || !resp.LLM.Active.DisableThinking {
		t.Fatalf("活动模型摘要不符：%#v", resp.LLM.Active)
	}
	// profile.useProxy && proxy.enabled && url 非空 → 有效走代理
	if !resp.LLM.Active.UseProxy {
		t.Fatal("三者同真时应 useProxy=true")
	}
	if !resp.LLM.Proxy.Enabled || !resp.LLM.Proxy.Configured || resp.LLM.Proxy.EgressIP != "1.1.1.1" {
		t.Fatalf("代理状态不符：%#v", resp.LLM.Proxy)
	}
	if resp.LLM.Usage == nil || resp.LLM.Usage.Today.Calls != 1 {
		t.Fatalf("用量摘要应含今日 1 次调用，得 %#v", resp.LLM.Usage)
	}
}

func TestStatusLLMBlockUnconfiguredGraceful(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, llm: NewLLMClient(&Config{}).WithDB(db), sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	w := callAPI(s, http.MethodGet, "/api/status", "")
	if w.Code != http.StatusOK {
		t.Fatalf("未配置模型状态接口也应 200: %d %s", w.Code, w.Body.String())
	}
	var resp statusResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.LLM.Configured {
		t.Fatal("未配置应 configured=false")
	}
	if resp.LLM.Active != nil {
		t.Fatal("无活动档案不应有 active 摘要")
	}
}
