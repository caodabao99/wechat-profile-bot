package main

// Phase A（v6.2 模型与代理）回归：
//   - 纯函数 isForeignBaseURL；
//   - LLMSettings.normalize（清洗/活动指针修复）；
//   - save/load round-trip + 掩码密钥沿用旧值；
//   - resolveSpec 运行时优先活动档案、回落 config.json；
//   - /api/llm/{settings,active,profile,profile/delete} 端到端（含密钥打码）。

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestLLMIsForeignBaseURL(t *testing.T) {
	if !isForeignBaseURL("https://api.openai.com/v1") {
		t.Fatal("openai 应判为国外")
	}
	if !isForeignBaseURL("https://api.anthropic.com") {
		t.Fatal("anthropic 应判为国外")
	}
	if isForeignBaseURL("https://api.deepseek.com") {
		t.Fatal("deepseek 不应判为国外")
	}
	if isForeignBaseURL("") {
		t.Fatal("空串不应判为国外")
	}
}

func TestLLMSettingsNormalize(t *testing.T) {
	s := LLMSettings{
		ActiveProfileID: "不存在的ID", // 应被清空后回指首个
		Profiles: []LLMProfile{
			{ID: "a", Label: "  甲  ", BaseURL: "https://x.com/", Model: " m1 ", Region: "weird"},
			{BaseURL: "https://y.com", Model: "m2"}, // 无 ID → 自动生成
		},
	}
	s.normalize()
	if s.Profiles[0].Label != "甲" {
		t.Fatalf("Label 应去空白，得 %q", s.Profiles[0].Label)
	}
	if s.Profiles[0].BaseURL != "https://x.com" {
		t.Fatalf("BaseURL 应去末尾斜杠，得 %q", s.Profiles[0].BaseURL)
	}
	if s.Profiles[0].Region != regionDomestic {
		t.Fatalf("非法地区应归 domestic，得 %q", s.Profiles[0].Region)
	}
	if s.Profiles[1].ID == "" {
		t.Fatal("缺失 ID 应自动补齐")
	}
	if s.ActiveProfileID != "a" {
		t.Fatalf("活动指针应修复为首个档案 a，得 %q", s.ActiveProfileID)
	}
	if ap := s.activeProfile(); ap == nil || ap.ID != "a" {
		t.Fatalf("activeProfile 应命中 a，得 %#v", ap)
	}
}

func TestLLMSettingsRoundTripAndMask(t *testing.T) {
	db := regressionDB(t)
	// 落一份含真实密钥的两档案设置
	in := LLMSettings{
		ActiveProfileID: "x",
		Profiles: []LLMProfile{
			{ID: "x", Label: "国内", BaseURL: "https://api.deepseek.com", APIKey: "sk-real-1", Model: "deepseek-chat", DisableThinking: true},
			{ID: "y", Label: "国外", BaseURL: "https://api.openai.com/v1", APIKey: "sk-real-2", Model: "gpt-4o-mini", Region: regionForeign, UseProxy: true},
		},
		Proxy: LLMProxy{Enabled: true, URL: "http://127.0.0.1:7890"},
	}
	if err := saveLLMSettings(db, in); err != nil {
		t.Fatal(err)
	}
	got, err := loadLLMSettings(db)
	if err != nil {
		t.Fatal(err)
	}
	if got.ActiveProfileID != "x" || len(got.Profiles) != 2 {
		t.Fatalf("round-trip 结构不符：%#v", got)
	}
	if got.Profiles[0].APIKey != "sk-real-1" {
		t.Fatalf("密钥应原样持久化，得 %q", got.Profiles[0].APIKey)
	}
	if !got.Proxy.Enabled || got.Proxy.URL != "http://127.0.0.1:7890" {
		t.Fatalf("代理设置应持久化，得 %#v", got.Proxy)
	}

	// 回传掩码 → 沿用旧密钥（模拟前端不改密钥的保存）
	next := got
	for i := range next.Profiles {
		next.Profiles[i].APIKey = apiKeyMask
	}
	next.Profiles[0].Label = "改名"
	if err := saveLLMSettings(db, next); err != nil {
		t.Fatal(err)
	}
	after, _ := loadLLMSettings(db)
	if after.Profiles[0].APIKey != "sk-real-1" {
		t.Fatalf("回传掩码应保留旧密钥，得 %q", after.Profiles[0].APIKey)
	}
	if after.Profiles[0].Label != "改名" {
		t.Fatalf("非密钥字段应更新，得 %q", after.Profiles[0].Label)
	}
}

func TestLLMResolveSpec(t *testing.T) {
	db := regressionDB(t)
	set := LLMSettings{
		ActiveProfileID: "d",
		Profiles: []LLMProfile{
			{ID: "d", Label: "DeepSeek", BaseURL: "https://api.deepseek.com", APIKey: "sk-d", Model: "deepseek-chat", DisableThinking: true},
		},
	}
	if err := saveLLMSettings(db, set); err != nil {
		t.Fatal(err)
	}
	// 运行时优先 DB 活动档案（即便启动配置指向别处）
	fallback := &Config{LLM: LLMConfig{ApiKey: "sk-startup", BaseURL: "https://old.example.com", Model: "old-model"}}
	c := NewLLMClient(fallback).WithDB(db)
	spec := c.resolveSpec()
	if spec.Model != "deepseek-chat" || spec.APIKey != "sk-d" || spec.BaseURL != "https://api.deepseek.com" {
		t.Fatalf("应解析到活动档案，得 %#v", spec)
	}
	if !spec.DisableThinking {
		t.Fatal("活动档案 disableThinking=true 应生效")
	}
	if spec.UseProxy {
		t.Fatal("代理未启用时不应走代理")
	}
	if c.modelOf() != "deepseek-chat" {
		t.Fatalf("modelOf 应返回活动模型，得 %q", c.modelOf())
	}

	// 切活动档案 → 立即反映（无需重启）
	set.ActiveProfileID = "e"
	set.Profiles = append(set.Profiles, LLMProfile{ID: "e", BaseURL: "https://api.groq.com/openai/v1", APIKey: "sk-e", Model: "llama-x", Region: regionForeign, UseProxy: true})
	set.Proxy = LLMProxy{Enabled: true, URL: "http://127.0.0.1:8888"}
	if err := saveLLMSettings(db, set); err != nil {
		t.Fatal(err)
	}
	spec2 := c.resolveSpec()
	if spec2.Model != "llama-x" || !spec2.UseProxy || spec2.ProxyURL != "http://127.0.0.1:8888" {
		t.Fatalf("切换后应走国外档案 + 代理，得 %#v", spec2)
	}

	// db=nil → 回落启动配置（老部署/测试语义）
	c2 := NewLLMClient(fallback)
	if s := c2.resolveSpec(); s.Model != "old-model" || s.APIKey != "sk-startup" {
		t.Fatalf("无 db 应回落 config.json，得 %#v", s)
	}
}

// ── 预设目录 ──

func TestLLMPresetsCatalog(t *testing.T) {
	ps := llmPresets()
	if len(ps) == 0 {
		t.Fatal("预设目录不应为空")
	}
	for _, p := range ps {
		if p.Key == "" || p.Label == "" || p.BaseURL == "" {
			t.Fatalf("预设字段缺失：%#v", p)
		}
		if p.BaseURL[len(p.BaseURL)-1] == '/' {
			t.Fatalf("预设 baseURL 不应以斜杠结尾：%s", p.BaseURL)
		}
		if len(p.Models) == 0 {
			t.Fatalf("预设应含默认模型：%s", p.Key)
		}
		if p.Region == regionForeign && !p.UseProxy {
			t.Fatalf("国外预设应默认建议走代理：%s", p.Key)
		}
	}
	// 按 key 命中 + 映射为档案草稿
	openai, ok := presetByKey("openai")
	if !ok {
		t.Fatal("应能找到 openai 预设")
	}
	draft := presetToProfile(openai, "")
	if draft.Model != "gpt-4o-mini" || !draft.UseProxy || draft.Region != regionForeign || !draft.DisableThinking {
		t.Fatalf("预设→档案草稿不符：%#v", draft)
	}
	if _, ok := presetByKey("nope"); ok {
		t.Fatal("不存在的 key 不应命中")
	}
}

func TestLLMPresetsAPI(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	w := callAPI(s, http.MethodGet, "/api/llm/presets", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET presets: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK      bool          `json:"ok"`
		Presets []ModelPreset `json:"presets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Presets) != len(llmPresets()) {
		t.Fatalf("API 返回预设数应等于目录，得 %d", len(resp.Presets))
	}
}

func TestLLMAPIEndToEnd(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}

	// GET 初始（可能为空档案，因测试中 config 未必有 llm）——应 200 且带 settings
	w := callAPI(s, http.MethodGet, "/api/llm/settings", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET settings: %d %s", w.Code, w.Body.String())
	}

	// PUT 保存两档案（含真实密钥）
	put := `{"activeProfileId":"a","profiles":[{"id":"a","label":"国内","baseURL":"https://api.deepseek.com","apiKey":"sk-A","model":"deepseek-chat","disableThinking":true},{"id":"b","label":"国外","baseURL":"https://api.openai.com/v1","apiKey":"sk-B","model":"gpt-4o-mini","region":"foreign","useProxy":true}]}`
	w = callAPI(s, http.MethodPut, "/api/llm/settings", put)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT settings: %d %s", w.Code, w.Body.String())
	}

	// GET 回读：密钥必须打码，绝不明文外泄
	w = callAPI(s, http.MethodGet, "/api/llm/settings", "")
	var resp struct {
		OK       bool        `json:"ok"`
		Settings LLMSettings `json:"settings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Settings.Profiles) != 2 {
		t.Fatalf("应返回 2 档案，得 %d", len(resp.Settings.Profiles))
	}
	for _, p := range resp.Settings.Profiles {
		if p.APIKey == "sk-A" || p.APIKey == "sk-B" {
			t.Fatalf("GET 不得回传真实密钥，得 %q", p.APIKey)
		}
	}

	// 切换活动档案 → b
	w = callAPI(s, http.MethodPost, "/api/llm/active", `{"id":"b"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST active: %d %s", w.Code, w.Body.String())
	}
	// 切到不存在的 → 400
	w = callAPI(s, http.MethodPost, "/api/llm/active", `{"id":"zzz"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("切到不存在档案应 400，得 %d", w.Code)
	}

	// 新增档案
	w = callAPI(s, http.MethodPost, "/api/llm/profile", `{"label":"自定义","baseURL":"https://my.llm/v1","apiKey":"sk-C","model":"custom-1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST profile: %d %s", w.Code, w.Body.String())
	}
	// 空字段 → 400
	w = callAPI(s, http.MethodPost, "/api/llm/profile", `{"label":"缺地址"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺 baseURL/model 应 400，得 %d", w.Code)
	}

	// 删除活动档案 b → 活动指针自动改指
	w = callAPI(s, http.MethodPost, "/api/llm/profile/delete", `{"id":"b"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST profile/delete: %d %s", w.Code, w.Body.String())
	}

	// 未知子路径 404
	if w = callAPI(s, http.MethodGet, "/api/llm/unknown", ""); w.Code != http.StatusNotFound {
		t.Fatalf("未知子路径应 404，得 %d", w.Code)
	}
}
