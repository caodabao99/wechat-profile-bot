package main

// 高阶洞察 HTTP 层回归：四只读端点自愈现算、鉴权 401、GET 端点拒 POST 405、
// 未知/过深/空子路径 404、recompute 仅 POST 且异步立即返回。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestInsightAPIEndToEnd(t *testing.T) {
	db := regressionAssistantDB(t)
	if err := saveAssistantSettings(db, defaultAssistantSettings()); err != nil {
		t.Fatal(err)
	}
	// 造一份互动，让整条流水线能现算出非空结果
	cid := regressionContact(t, db, "洞察API对象")
	now := time.Now()
	for d := 0; d < 8; d++ {
		day := now.AddDate(0, 0, -d)
		seedMessage(t, db, cid, "me", "在忙吗", fmt.Sprintf("insapi-me-%d", d), day)
		seedMessage(t, db, cid, "other", "还好啦", fmt.Sprintf("insapi-other-%d", d), day)
	}

	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}

	// 未授权 → 401
	wno := httptest.NewRecorder()
	s.route(wno, httptest.NewRequest(http.MethodGet, "/api/insight/network", nil))
	if wno.Code != http.StatusUnauthorized {
		t.Fatalf("未授权应 401, got %d", wno.Code)
	}

	// 四个只读端点：GET 自愈现算，应 200 + 合法 JSON + enabled=true
	for _, sub := range []string{"network", "self", "intervention", "briefing"} {
		w := callAPI(s, http.MethodGet, "/api/insight/"+sub, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET insight/%s: %d %s", sub, w.Code, w.Body.String())
		}
		var resp struct {
			Enabled     bool   `json:"enabled"`
			GeneratedAt string `json:"generatedAt"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("insight/%s 非 JSON: %v", sub, err)
		}
		if !resp.Enabled {
			t.Errorf("insight/%s enabled 应为 true", sub)
		}
		if !validJSON(w.Body.String()) {
			t.Errorf("insight/%s 响应应合法 JSON", sub)
		}
	}

	// network 端点应带 network.nodeCount 字段
	w := callAPI(s, http.MethodGet, "/api/insight/network", "")
	var netResp struct {
		Network struct {
			NodeCount int `json:"nodeCount"`
		} `json:"network"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &netResp); err != nil {
		t.Fatal(err)
	}

	// GET 端点拒绝 POST → 405
	for _, sub := range []string{"network", "self", "intervention", "briefing"} {
		if c := callAPI(s, http.MethodPost, "/api/insight/"+sub, ""); c.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST insight/%s 应 405, got %d", sub, c.Code)
		}
	}

	// 未知子路径 404
	if c := callAPI(s, http.MethodGet, "/api/insight/nope", ""); c.Code != http.StatusNotFound {
		t.Errorf("未知子路径应 404, got %d", c.Code)
	}
	// 过深子路径 404
	if c := callAPI(s, http.MethodGet, "/api/insight/network/deep", ""); c.Code != http.StatusNotFound {
		t.Errorf("过深子路径应 404, got %d", c.Code)
	}
	// 空子路径 404
	if c := callAPI(s, http.MethodGet, "/api/insight", ""); c.Code != http.StatusNotFound {
		t.Errorf("/api/insight 应 404, got %d", c.Code)
	}

	// POST recompute：异步、立即 200 + ok=true
	w = callAPI(s, http.MethodPost, "/api/insight/recompute", "")
	if w.Code != http.StatusOK {
		t.Fatalf("POST recompute: %d %s", w.Code, w.Body.String())
	}
	var rec struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil || !rec.OK {
		t.Fatalf("recompute 响应不符: %s err=%v", w.Body.String(), err)
	}
	// GET recompute 应 405
	if c := callAPI(s, http.MethodGet, "/api/insight/recompute", ""); c.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET recompute 应 405, got %d", c.Code)
	}
	// 给后台 goroutine 收尾时间，避免关库后仍用 db 干扰其它测试
	time.Sleep(500 * time.Millisecond)
}

// TestAdvancedInsightsEnabledDefault 校验：默认开（opt-out）；老库 JSON 缺该字段仍保持默认开；显式 false 生效。
func TestAdvancedInsightsEnabledDefault(t *testing.T) {
	if !defaultAssistantSettings().AdvancedInsightsEnabled {
		t.Fatal("新开关默认应为 true")
	}
	db := regressionAssistantDB(t)
	// 无记录 → 默认
	s, err := loadAssistantSettings(db)
	if err != nil || !s.AdvancedInsightsEnabled {
		t.Fatalf("无记录应默认开: %+v err=%v", s, err)
	}
	// 老 JSON（缺 advancedInsightsEnabled 字段）→ 解析后应保持默认 true
	now := time.Now().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO assistant_settings (id, settings_json, updated_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET settings_json=excluded.settings_json, updated_at=excluded.updated_at`,
		`{"enabled":false,"dailyCheckTime":"08:00"}`, now); err != nil {
		t.Fatal(err)
	}
	s, err = loadAssistantSettings(db)
	if err != nil {
		t.Fatal(err)
	}
	if !s.AdvancedInsightsEnabled {
		t.Errorf("老 JSON 缺字段应保持默认开, got %+v", s)
	}
	// 显式关闭 → false 生效
	st := defaultAssistantSettings()
	st.AdvancedInsightsEnabled = false
	if err := saveAssistantSettings(db, st); err != nil {
		t.Fatal(err)
	}
	s, err = loadAssistantSettings(db)
	if err != nil {
		t.Fatal(err)
	}
	if s.AdvancedInsightsEnabled {
		t.Error("显式关闭应持久化为 false")
	}
}

// v4.5.0 A：趋势端点。无历史 → weeks 为空数组；非自愈（不触发重算）；POST 拒 405；过深子路径 404。
func TestInsightTrendEndpoint(t *testing.T) {
	db := regressionAssistantDB(t)
	if err := saveAssistantSettings(db, defaultAssistantSettings()); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}

	// 空历史：200 + weeks 为空数组、latest 为 null。
	w := callAPI(s, http.MethodGet, "/api/insight/trend", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET insight/trend: %d %s", w.Code, w.Body.String())
	}
	var empty struct {
		Enabled bool            `json:"enabled"`
		Weeks   []TrendSnapshot `json:"weeks"`
		Latest  *TrendSnapshot  `json:"latest"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &empty); err != nil {
		t.Fatalf("trend 非 JSON: %v", err)
	}
	if len(empty.Weeks) != 0 || empty.Latest != nil {
		t.Errorf("空库应为 weeks:[] latest:null, got weeks=%v latest=%v", empty.Weeks, empty.Latest)
	}

	// 追加快照后应能读到升序序列。
	if err := appendTrendSnapshot(db, time.Now()); err != nil {
		t.Fatal(err)
	}
	w = callAPI(s, http.MethodGet, "/api/insight/trend?weeks=12", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET insight/trend?weeks=12: %d", w.Code)
	}
	var got struct {
		Weeks []TrendSnapshot `json:"weeks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Weeks) != 1 || got.Weeks[0].WeekStart == "" {
		t.Errorf("应读到 1 行快照, got %+v", got.Weeks)
	}

	// 非自愈：GET trend 不应触发整条流水线重算（网络缓存仍为空）。
	if n, _, _ := GetCachedNetwork(db); n != nil {
		t.Error("GET trend 不该触发网络层重算")
	}

	// POST → 405。
	if c := callAPI(s, http.MethodPost, "/api/insight/trend", ""); c.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST insight/trend 应 405, got %d", c.Code)
	}
	// 过深子路径 → 404。
	if c := callAPI(s, http.MethodGet, "/api/insight/trend/deep", ""); c.Code != http.StatusNotFound {
		t.Errorf("过深子路径应 404, got %d", c.Code)
	}
}
