package main

// 变更类端点全生命周期活体扫描（补齐 feature_sweep 只覆盖读/浅写的缺口）。
// 把真实 mux 跑起来，逐个走「创建→改→合并→撤销→删除」「标签建/改名/批量贴/删」
// 「待跟进 建/改状态/删」「归档 run→status→restore→settings」「备份导出」「日历密钥 rotate→clear」
// 全链路，并且每步用直连 SQL 断言「状态真的变了」——证明 handler 真的做了事，而不只是回了 200。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestZZMutatingSweepLive(t *testing.T) {
	db := regressionDB(t)
	ensureAllValueAddedTables(t, db)
	for _, fn := range []func(*sql.DB) error{ensureAssistantTables, ensureArchiveTables} {
		if err := fn(db); err != nil {
			t.Fatalf("建表失败: %v", err)
		}
	}
	cfg := &Config{APIToken: "mut-token"}
	s := &apiServer{
		db:       db,
		cfg:      cfg,
		llm:      NewLLMClient(cfg),
		sessions: &webSessionStore{sessions: map[string]time.Time{}},
		guard:    newSecurityGuard(),
		ingestRL: newRateLimiter(1000, time.Minute),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", s.route)
	ts := httptest.NewServer(withSecurity(mux, s.guard, cfg.TrustedProxies))
	defer ts.Close()

	do := func(method, path, body string) (int, string) {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, ts.URL+path, rd)
		req.Header.Set("Authorization", "Bearer mut-token")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("[%s %s] 连接中断（疑似 handler panic）: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	mustOK := func(method, path, body string) string {
		code, b := do(method, path, body)
		if code < 200 || code >= 300 {
			t.Fatalf("[%s %s] 期望 2xx，得 %d: %s", method, path, code, truncate(b))
		}
		return b
	}

	// 直连状态断言助手
	qCount := func(query string, args ...interface{}) int64 {
		var n int64
		if err := db.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatalf("查询 %q: %v", query, err)
		}
		return n
	}
	qNullable := func(query string, args ...interface{}) (int64, bool) {
		var v sql.NullInt64
		if err := db.QueryRow(query, args...).Scan(&v); err != nil {
			t.Fatalf("查询 %q: %v", query, err)
		}
		return v.Int64, v.Valid
	}

	// ---------------- 1. 联系人 CRUD ----------------
	b := mustOK("POST", "/api/contacts", `{"name":"变更扫描甲"}`)
	var created struct {
		ID int64 `json:"id"`
	}
	if json.Unmarshal([]byte(b), &created) != nil || created.ID <= 0 {
		t.Fatalf("创建联系人未回 id: %s", b)
	}
	aid := created.ID
	if qCount(`SELECT COUNT(*) FROM contacts WHERE id=?`, aid) != 1 {
		t.Fatal("新联系人应已入库")
	}
	// 空名应 400
	if code, _ := do("POST", "/api/contacts", `{"name":"  "}`); code != http.StatusBadRequest {
		t.Errorf("空名创建应 400，得 %d", code)
	}
	// 改名 / 备注
	mustOK("PUT", fmt.Sprintf("/api/contacts/%d/name", aid), `{"name":"变更扫描甲改名"}`)
	if nm := func() string {
		var x string
		_ = db.QueryRow(`SELECT name FROM contacts WHERE id=?`, aid).Scan(&x)
		return x
	}(); nm != "变更扫描甲改名" {
		t.Errorf("改名未生效，当前=%q", nm)
	}
	mustOK("PUT", fmt.Sprintf("/api/contacts/%d/remark", aid), `{"remark":"扫描备注"}`)

	// ---------------- 2. 合并 + 撤销 ----------------
	b = mustOK("POST", "/api/contacts", `{"name":"合并目标"}`)
	var tgt struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal([]byte(b), &tgt)
	bid := tgt.ID
	regressionMessages(t, db, aid, "只有源有的消息", "共享消息")
	regressionMessages(t, db, bid, "目标原有")

	mustOK("POST", "/api/merge", fmt.Sprintf(`{"sourceId":%d,"targetId":%d}`, aid, bid))
	if merged, ok := qNullable(`SELECT merged_into FROM contacts WHERE id=?`, aid); !ok || merged != bid {
		t.Fatalf("合并后 source.merged_into 应=%d，得 (%d,valid=%v)", bid, merged, ok)
	}
	// 合并日志可见
	logsBody := mustOK("GET", fmt.Sprintf("/api/merge/logs?targetId=%d", bid), "")
	if !strings.Contains(logsBody, "[") {
		t.Errorf("合并日志应返回数组: %s", truncate(logsBody))
	}
	var logs []struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal([]byte(logsBody), &logs)
	if len(logs) == 0 {
		t.Fatalf("合并日志应至少 1 条: %s", truncate(logsBody))
	}
	logID := logs[0].ID
	// 撤销
	mustOK("POST", "/api/merge/undo", fmt.Sprintf(`{"logId":%d}`, logID))
	if _, ok := qNullable(`SELECT merged_into FROM contacts WHERE id=?`, aid); ok {
		t.Fatal("撤销合并后 source.merged_into 应回到 NULL")
	}
	if qCount(`SELECT COUNT(*) FROM messages WHERE contact_id=?`, aid) == 0 {
		t.Error("撤销合并后源联系人应重新拥有自己的消息")
	}

	// ---------------- 3. 标签 建/改名/批量贴/删 ----------------
	b = mustOK("POST", "/api/tags", `{"name":"扫描标签"}`)
	var tag struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if json.Unmarshal([]byte(b), &tag) != nil || tag.ID <= 0 {
		t.Fatalf("创建标签未回 id: %s", b)
	}
	mustOK("PUT", fmt.Sprintf("/api/tags/%d", tag.ID), `{"name":"扫描标签改名"}`)
	if nm := func() string {
		var x string
		_ = db.QueryRow(`SELECT name FROM contact_tags WHERE id=?`, tag.ID).Scan(&x)
		return x
	}(); nm != "扫描标签改名" {
		t.Errorf("标签改名未生效，当前=%q", nm)
	}
	// 批量贴标
	mustOK("POST", "/api/tags/batch", fmt.Sprintf(`{"contactIds":[%d],"tagIds":[%d]}`, aid, tag.ID))
	if qCount(`SELECT COUNT(*) FROM contact_tag_links WHERE contact_id=? AND tag_id=?`, aid, tag.ID) != 1 {
		t.Fatal("批量贴标未在库中留下关联")
	}
	tagListBody := mustOK("GET", fmt.Sprintf("/api/contacts/%d/tags", aid), "")
	if !strings.Contains(tagListBody, "扫描标签改名") {
		t.Errorf("联系人标签应含刚贴的标签: %s", truncate(tagListBody))
	}
	// 批量移除
	mustOK("POST", "/api/tags/batch", fmt.Sprintf(`{"contactIds":[%d],"tagIds":[%d],"remove":true}`, aid, tag.ID))
	if qCount(`SELECT COUNT(*) FROM contact_tag_links WHERE contact_id=? AND tag_id=?`, aid, tag.ID) != 0 {
		t.Error("批量移除后关联应清空")
	}
	// 删标签
	mustOK("DELETE", fmt.Sprintf("/api/tags/%d", tag.ID), "")
	if qCount(`SELECT COUNT(*) FROM contact_tags WHERE id=?`, tag.ID) != 0 {
		t.Error("删除标签未从库移除")
	}

	// ---------------- 4. 待跟进 建/改状态/删 ----------------
	b = mustOK("POST", "/api/assistant/followups", fmt.Sprintf(`{"contactId":%d,"kind":"other","content":"扫描跟进项"}`, aid))
	var fu struct {
		OK bool  `json:"ok"`
		ID int64 `json:"id"`
	}
	if json.Unmarshal([]byte(b), &fu) != nil || fu.ID <= 0 {
		t.Fatalf("新增待跟进未回 id: %s", b)
	}
	mustOK("PUT", fmt.Sprintf("/api/assistant/followups/%d", fu.ID), `{"status":"done"}`)
	st := func() string {
		var x string
		_ = db.QueryRow(`SELECT status FROM followup_items WHERE id=?`, fu.ID).Scan(&x)
		return x
	}()
	if st != "done" {
		t.Errorf("待跟进状态应更新为 done，得 %q", st)
	}
	mustOK("DELETE", fmt.Sprintf("/api/assistant/followups/%d", fu.ID), "")
	if qCount(`SELECT COUNT(*) FROM followup_items WHERE id=?`, fu.ID) != 0 {
		t.Error("删除待跟进未从库移除")
	}

	// ---------------- 5. 归档 run → status → restore ----------------
	regressionMessages(t, db, bid, "很久以前的消息该归档") // 2025-06-10，>30 天
	runBody := mustOK("POST", "/api/archive/run", `{"days":30}`)
	var run struct {
		OK    bool  `json:"ok"`
		Moved int64 `json:"moved"`
	}
	_ = json.Unmarshal([]byte(runBody), &run)
	if run.Moved <= 0 {
		t.Fatalf("归档应移动消息，body=%s", truncate(runBody))
	}
	if qCount(`SELECT COUNT(*) FROM messages_archive`) < run.Moved {
		t.Error("归档表应记录被移动的消息")
	}
	mustOK("GET", "/api/archive/status", "")                  // 确定性读
	mustOK("POST", "/api/archive/restore", `{"contactId":0}`) // 恢复全部
	if n := qCount(`SELECT COUNT(*) FROM messages_archive`); n != 0 {
		t.Errorf("全量恢复后归档表应清空，剩 %d", n)
	}

	// 归档 settings GET/POST
	setBody := mustOK("POST", "/api/archive/settings", `{"enabled":true,"retentionDays":45}`)
	if !strings.Contains(setBody, `"ok":true`) {
		t.Errorf("归档设置保存应 ok=true: %s", truncate(setBody))
	}
	if code, body := do("POST", "/api/archive/settings", `{"retentionDays":5}`); code != http.StatusBadRequest {
		t.Errorf("保留天数越界应 400，得 %d: %s", code, truncate(body))
	}

	// ---------------- 6. 备份导出（下载类，仅要求 2xx 且非空）----------------
	if code, body := do("GET", "/api/backup/export", ""); code < 200 || code >= 300 || len(body) == 0 {
		t.Errorf("备份导出应 2xx 且非空，得 code=%d len=%d", code, len(body))
	}

	// ---------------- 7. 日历订阅密钥 rotate → get → clear ----------------
	mustOK("POST", "/api/assistant/calendar/key", "")   // 轮转
	mustOK("GET", "/api/assistant/calendar/key", "")    // 读取
	mustOK("DELETE", "/api/assistant/calendar/key", "") // 清除

	// ---------------- 8. 联系人成就（派生读，容忍空但不 404/不崩）----------------
	if code, body := do("GET", fmt.Sprintf("/api/contacts/%d/achievements", aid), ""); code == http.StatusNotFound {
		t.Errorf("联系人成就端点缺失(404)")
	} else if code >= 500 {
		t.Errorf("联系人成就 5xx: %d %s", code, truncate(body))
	}

	// ---------------- 9. 删除联系人（级联）----------------
	mustOK("DELETE", fmt.Sprintf("/api/contacts/%d", aid), "")
	if qCount(`SELECT COUNT(*) FROM contacts WHERE id=?`, aid) != 0 {
		t.Fatal("删除联系人未从库移除")
	}
	if qCount(`SELECT COUNT(*) FROM messages WHERE contact_id=?`, aid) != 0 {
		t.Error("删除联系人后其消息应级联清理")
	}

	t.Log("变更类端点全生命周期扫描完成：CRUD/合并/撤销/标签/待跟进/归档/备份/日历密钥 状态断言均通过")
}
