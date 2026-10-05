package main

// v5.2.0 提示词管理接口 HTTP 契约测试（前端「分析插件（提示词）」面板依赖的端点）。
// 真实 SQLite + 真实 http 路由（s.route），覆盖 SV2 界面会用到的全部往返：
//   - 鉴权（无 Bearer → 401）
//   - GET 列表（13 项、字段齐备、初始全内置）
//   - GET 详情（default/effective/isCustom/vars）
//   - PUT 保存合法覆盖（→ isCustom true、effective 变为覆盖）
//   - PUT 非法覆盖（缺必填变量 → 400）、未知 key（→ 404）
//   - POST 预览（用样本 vars 字面渲染、不调模型）
//   - DELETE 重置（→ 回到内置默认、isCustom false）
//   - 保存后 GET 列表反映「已自定义」

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// callPrompts 用给定 Bearer 令牌对 s.route 发一次请求，返回 recorder。
func callPrompts(s *apiServer, method, path, body, token string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.route(w, r)
	return w
}

func newPromptsServer(t *testing.T) *apiServer {
	t.Helper()
	db := regressionDB(t)
	if err := ensurePromptTemplates(db); err != nil { // PUT/DELETE 需要表已存在
		t.Fatal(err)
	}
	return &apiServer{db: db, cfg: &Config{APIToken: "test-prompt-token"}, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
}

// mustJSON 把 v 序列化为 JSON 字符串，失败即 fatal。
func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPromptsAPIContract(t *testing.T) {
	s := newPromptsServer(t)
	const key = "ask_keywords"

	// —— 鉴权：无 Bearer 应 401 ——
	if w := callPrompts(s, http.MethodGet, "/api/assistant/prompts", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401, got %d %s", w.Code, w.Body.String())
	}

	// —— GET 列表 ——
	w := callPrompts(s, http.MethodGet, "/api/assistant/prompts", "", "test-prompt-token")
	if w.Code != http.StatusOK {
		t.Fatalf("GET list: %d %s", w.Code, w.Body.String())
	}
	var listResp struct {
		Items []struct {
			Key      string   `json:"key"`
			Title    string   `json:"title"`
			Feature  string   `json:"feature"`
			Vars     []string `json:"vars"`
			IsCustom bool     `json:"isCustom"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("列表解析失败: %v %s", err, w.Body.String())
	}
	if listResp.Total != 13 || len(listResp.Items) != 13 {
		t.Fatalf("列表应 13 项, got total=%d len=%d", listResp.Total, len(listResp.Items))
	}
	for _, it := range listResp.Items {
		if it.Key == "" || it.Title == "" || len(it.Vars) == 0 {
			t.Fatalf("列表项字段缺失: %+v", it)
		}
		if it.IsCustom {
			t.Fatalf("初始不应有自定义项: %s", it.Key)
		}
	}

	// —— GET 详情：初始 effective==default、isCustom false ——
	w = callPrompts(s, http.MethodGet, "/api/assistant/prompts/"+key, "", "test-prompt-token")
	if w.Code != http.StatusOK {
		t.Fatalf("GET detail: %d %s", w.Code, w.Body.String())
	}
	var d1 PromptTemplateDetail
	if err := json.Unmarshal(w.Body.Bytes(), &d1); err != nil {
		t.Fatal(err)
	}
	if d1.Key != key || d1.IsCustom {
		t.Fatalf("初始详情 key/isCustom 不符: %+v", d1)
	}
	if d1.Default == "" || d1.Effective != d1.Default {
		t.Fatalf("初始 effective 应等于 default")
	}
	if !strings.Contains(d1.Default, "{{question}}") {
		t.Fatalf("ask_keywords 默认应含占位符 {{question}}: %q", d1.Default)
	}

	// —— PUT 未知 key → 404 ——
	if w := callPrompts(s, http.MethodPut, "/api/assistant/prompts/no_such_key", `{"content":"{{question}}"}`, "test-prompt-token"); w.Code != http.StatusNotFound {
		t.Fatalf("未知 key PUT 应 404, got %d", w.Code)
	}

	// —— PUT 缺必填变量（不含 {{question}}）→ 400 ——
	if w := callPrompts(s, http.MethodPut, "/api/assistant/prompts/"+key, `{"content":"关键词提取，无占位符"}`, "test-prompt-token"); w.Code != http.StatusBadRequest {
		t.Fatalf("缺必填变量应 400, got %d %s", w.Code, w.Body.String())
	}

	// —— PUT 含未知变量 → 400 ——
	if w := callPrompts(s, http.MethodPut, "/api/assistant/prompts/"+key, `{"content":"{{question}} {{bogus}}"}`, "test-prompt-token"); w.Code != http.StatusBadRequest {
		t.Fatalf("含未知变量应 400, got %d %s", w.Code, w.Body.String())
	}

	// —— PUT 合法覆盖 → 200, isCustom true ——
	override := "提取关键词（自定义）：{{question}}"
	body := mustJSON(t, map[string]string{"content": override})
	w = callPrompts(s, http.MethodPut, "/api/assistant/prompts/"+key, body, "test-prompt-token")
	if w.Code != http.StatusOK {
		t.Fatalf("合法覆盖 PUT 应 200, got %d %s", w.Code, w.Body.String())
	}
	var d2 PromptTemplateDetail
	if err := json.Unmarshal(w.Body.Bytes(), &d2); err != nil {
		t.Fatal(err)
	}
	if !d2.IsCustom || d2.Effective != override || d2.Override != override {
		t.Fatalf("保存后详情应 isCustom true 且 effective/override 为覆盖内容: %+v", d2)
	}

	// —— GET 详情再确认 ——
	w = callPrompts(s, http.MethodGet, "/api/assistant/prompts/"+key, "", "test-prompt-token")
	var d3 PromptTemplateDetail
	if err := json.Unmarshal(w.Body.Bytes(), &d3); err != nil {
		t.Fatal(err)
	}
	if !d3.IsCustom || d3.Effective != override {
		t.Fatalf("重取详情应反映覆盖: %+v", d3)
	}

	// —— POST 预览：用样本 vars 渲染，命中覆盖且替换占位符，不含遗留 {{question}} ——
	w = callPrompts(s, http.MethodPost, "/api/assistant/prompts/"+key+"/preview", `{"vars":{"question":"最近怎么样"}}`, "test-prompt-token")
	if w.Code != http.StatusOK {
		t.Fatalf("preview 应 200, got %d %s", w.Code, w.Body.String())
	}
	var prev struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &prev); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prev.Prompt, "最近怎么样") || strings.Contains(prev.Prompt, "{{question}}") {
		t.Fatalf("预览应替换占位符且不遗留 {{question}}: %q", prev.Prompt)
	}

	// —— 保存后 GET 列表反映「已自定义」 ——
	w = callPrompts(s, http.MethodGet, "/api/assistant/prompts", "", "test-prompt-token")
	var listResp2 struct {
		Items []struct {
			Key      string `json:"key"`
			IsCustom bool   `json:"isCustom"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp2); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range listResp2.Items {
		if it.Key == key {
			found = true
			if !it.IsCustom {
				t.Fatalf("保存后列表该 item 应 isCustom true")
			}
		}
	}
	if !found {
		t.Fatalf("列表未含 %s", key)
	}

	// —— DELETE 重置 → 回默认、isCustom false ——
	w = callPrompts(s, http.MethodDelete, "/api/assistant/prompts/"+key, "", "test-prompt-token")
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE 应 200, got %d %s", w.Code, w.Body.String())
	}
	var d4 PromptTemplateDetail
	if err := json.Unmarshal(w.Body.Bytes(), &d4); err != nil {
		t.Fatal(err)
	}
	if d4.IsCustom || d4.Effective != d4.Default {
		t.Fatalf("重置后应回内置默认: %+v", d4)
	}
}

// TestPromptsAPIBadMethod 校验方法守卫：列表路径只允许 GET。
func TestPromptsAPIBadMethod(t *testing.T) {
	s := newPromptsServer(t)
	if w := callPrompts(s, http.MethodDelete, "/api/assistant/prompts", "", "test-prompt-token"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("列表 DELETE 应 405, got %d %s", w.Code, w.Body.String())
	}
}
