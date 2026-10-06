package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// decodeAction 从 {“action”:…} 响应里取出行动条目。
func decodeAction(t *testing.T, body string) ActionLogEntry {
	t.Helper()
	var res struct {
		Action *ActionLogEntry `json:"action"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("解析响应失败: %v body=%s", err, body)
	}
	if res.Action == nil {
		t.Fatalf("响应缺 action 字段: %s", body)
	}
	return *res.Action
}

// TestActionAPIEndToEnd 走真实 HTTP 路由验证：建→列→迁移→用户确认结果，及越权/非法来源。
func TestActionAPIEndToEnd(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	cid := regressionContact(t, db, "账本人")

	// 新建（非法 source → 400）
	if w := callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/actions", cid), `{"source":"bogus","action_text":"x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("非法 source 应 400, got %d %s", w.Code, w.Body.String())
	}

	// 新建（合法）
	w := callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/actions", cid), `{"source":"manual","source_ref":"","action_type":"meet","action_text":"约见面"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("新建应 201, got %d %s", w.Code, w.Body.String())
	}
	created := decodeAction(t, w.Body.String())
	if created.Status != ActionStatusGenerated || created.ContactID != cid {
		t.Fatalf("新建起点应 generated: %+v", created)
	}

	// 列表
	w = callAPI(s, http.MethodGet, fmt.Sprintf("/api/contacts/%d/actions", cid), "")
	if w.Code != http.StatusOK {
		t.Fatalf("列表应 200, got %d", w.Code)
	}
	var listRes struct {
		Actions []ActionLogEntry `json:"actions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listRes); err != nil {
		t.Fatal(err)
	}
	if len(listRes.Actions) != 1 {
		t.Fatalf("应列出 1 条, got %d", len(listRes.Actions))
	}

	// 生命周期迁移 → acted
	w = callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/actions/%d/transition", cid, created.ID), `{"status":"acted"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("迁移应 200, got %d %s", w.Code, w.Body.String())
	}
	if got := decodeAction(t, w.Body.String()); got.Status != ActionStatusActed || got.ActedAt == "" {
		t.Fatalf("迁移后应 acted 且补 acted_at: %+v", got)
	}

	// 用户确认结果 → confirmed
	w = callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/actions/%d/outcome", cid, created.ID), `{"outcome":"positive","note":"感觉不错"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("结果应 200, got %d %s", w.Code, w.Body.String())
	}
	if got := decodeAction(t, w.Body.String()); got.OutcomeProvenance != ActionProvenanceConfirmed || got.Outcome != ActionOutcomePositive {
		t.Fatalf("API 回填结果应 confirmed/positive: %+v", got)
	}

	// 越权：用别的联系人路径操作该记录 → 404
	other := regressionContact(t, db, "无关人")
	w = callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/actions/%d/transition", other, created.ID), `{"status":"viewed"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("越权操作应 404, got %d %s", w.Code, w.Body.String())
	}
}

// TestContextIncludesActionLog 验证行动账本接入认知上下文：记录后 context 的 ActionLog 非空、
// 且 context_version 随行动变化（供 AI cache 精确失效）。
func TestContextIncludesActionLog(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "上下文行动")
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	cc0, err := BuildContactContext(db, cid, TaskCoach, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(cc0.ActionLog) != 0 {
		t.Fatalf("初始应无行动: %d", len(cc0.ActionLog))
	}
	v0 := cc0.ContextVersion

	if _, err := LogAction(db, cid, "coach", "sugg-1", "greet", "主动问候", now); err != nil {
		t.Fatal(err)
	}
	cc1, err := BuildContactContext(db, cid, TaskCoach, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(cc1.ActionLog) != 1 {
		t.Fatalf("应读到 1 条行动: %d", len(cc1.ActionLog))
	}
	if cc1.ContextVersion == v0 {
		t.Fatal("新增行动应使 context_version 前进（cache 精确失效）")
	}
}
