package main

// v4.5.0 C：全量 JSON 数据导出（可选脱敏）HTTP 层回归。
// 覆盖：缺 token 401、POST 405、GET 200 + 附件头 + 合法 JSON、行数与库一致、
//       redact=1 时姓名→伪名、消息正文抹除而结构/计数保留、未知/过深子路径 404。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// exportTables 解析导出响应体里的 tables 段。
func exportTables(t *testing.T, body string) map[string][]map[string]any {
	t.Helper()
	var doc struct {
		ExportedAt string                      `json:"exportedAt"`
		Version    string                      `json:"version"`
		Redacted   bool                        `json:"redacted"`
		Tables     map[string][]map[string]any `json:"tables"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("导出体非合法 JSON: %v", err)
	}
	if doc.ExportedAt == "" {
		t.Error("应含 exportedAt")
	}
	return doc.Tables
}

func newExportServer(t *testing.T, seeded int) (*apiServer, func() int) {
	t.Helper()
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	// 种若干联系人 + 每人一条含正文的消息，供行数/脱敏断言。
	for i := 0; i < seeded; i++ {
		cid := regressionContact(t, db, "导出对象-"+string(rune('a'+i)))
		regressionMessages(t, db, cid, "这是一段私密正文内容")
	}
	count := func() int {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM contacts`).Scan(&n)
		return n
	}
	return s, count
}

func TestDataExportUnauthorizedAndBadMethod(t *testing.T) {
	s, _ := newExportServer(t, 2)
	// 无 Bearer → 401
	wno := httptest.NewRecorder()
	s.route(wno, httptest.NewRequest(http.MethodGet, "/api/data/export", nil))
	if wno.Code != http.StatusUnauthorized {
		t.Fatalf("未授权应 401, got %d", wno.Code)
	}
	// POST → 405
	if c := callAPI(s, http.MethodPost, "/api/data/export", ""); c.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST export 应 405, got %d", c.Code)
	}
	// 未知子路径 → 404
	if c := callAPI(s, http.MethodGet, "/api/data/nope", ""); c.Code != http.StatusNotFound {
		t.Errorf("未知子路径应 404, got %d", c.Code)
	}
	// 过深子路径 → 404
	if c := callAPI(s, http.MethodGet, "/api/data/export/deep", ""); c.Code != http.StatusNotFound {
		t.Errorf("过深子路径应 404, got %d", c.Code)
	}
}

func TestDataExportPlainMatchesRowCount(t *testing.T) {
	const seeded = 3
	s, count := newExportServer(t, seeded)
	w := callAPI(s, http.MethodGet, "/api/data/export", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET export: %d %s", w.Code, w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".json") {
		t.Errorf("应带 JSON 附件下载头, got %q", cd)
	}
	tables := exportTables(t, w.Body.String())
	rows, ok := tables["contacts"]
	if !ok {
		t.Fatal("导出应含 contacts 表")
	}
	if len(rows) != count() {
		t.Errorf("contacts 行数应与库一致: got %d want %d", len(rows), count())
	}
	msgs, ok := tables["messages"]
	if !ok {
		t.Fatal("导出应含 messages 表")
	}
	if len(msgs) != seeded {
		t.Errorf("messages 行数不符: got %d want %d", len(msgs), seeded)
	}
	// 明文导出：正文应原样保留。
	var sawContent bool
	for _, m := range msgs {
		if c, _ := m["content"].(string); strings.Contains(c, "私密正文") {
			sawContent = true
		}
	}
	if !sawContent {
		t.Error("未脱敏时消息正文应保留")
	}
}

func TestDataExportRedacted(t *testing.T) {
	s, _ := newExportServer(t, 3)
	w := callAPI(s, http.MethodGet, "/api/data/export?redact=1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET export?redact=1: %d %s", w.Code, w.Body.String())
	}
	var doc struct {
		Redacted bool                        `json:"redacted"`
		Tables   map[string][]map[string]any `json:"tables"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Redacted {
		t.Error("redacted 标志应为 true")
	}
	// 姓名→伪名（联系人#<id>）。
	for _, c := range doc.Tables["contacts"] {
		name, _ := c["name"].(string)
		if !strings.HasPrefix(name, "联系人#") {
			t.Errorf("脱敏后姓名应为伪名, got %q", name)
		}
	}
	// 消息正文抹除为 ***，但结构（id/sender/msg_hash）保留。
	for _, m := range doc.Tables["messages"] {
		if content, _ := m["content"].(string); content != "***" {
			t.Errorf("脱敏后正文应被抹除, got %q", content)
		}
		if m["msg_hash"] == nil || m["msg_hash"] == "" {
			t.Error("脱敏应保留 msg_hash 等结构字段")
		}
	}
}
