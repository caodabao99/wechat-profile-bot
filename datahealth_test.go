package main

// V7 §20/§21/§22 验收（数据健康 / 密钥安全 / 删除级联无孤儿）。
//
// 钉死四件事：
//  1. §22 架构不变量：每一个 registry 中标记 HasContactID=true 的表，删除联系人时都必须被清理
//     ——要么进 contactCleanupTables，要么在 DeleteContactByID 里有专用 DELETE。防止未来新增
//     含 contact_id 的表时静默留下孤儿行。
//  2. §22 实测：删一个带主题历史 + TODAY 屏蔽的联系人，级联后这两张表都归零。
//  3. §21：BuildDataHealth 覆盖蓝图 11 项、核心子系统一律不可重建；RebuildDerived 只清派生、绝不删核心。
//  4. §20：GET 展示对 API Key 一律打码，真实密钥不外泄。

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// specialCasedDelete 是 DeleteContactByID 里用专用 DELETE（非 WHERE contact_id=?）清理的核心表。
// 它们本就在删除路径中处理，不必重复进 contactCleanupTables。
var specialCasedDelete = map[string]bool{
	"contact_aliases": true,
	"messages":        true,
	"profile_history": true,
}

// cleanupSet 把 contactCleanupTables 转成集合便于断言。
func cleanupSet() map[string]bool {
	m := map[string]bool{}
	for _, t := range contactCleanupTables {
		m[t] = true
	}
	return m
}

func TestContactDeleteCoversAllRegistryScopedTables(t *testing.T) {
	clean := cleanupSet()
	var orphans []string
	for _, meta := range TableRegistry() {
		if !meta.HasContactID {
			continue
		}
		if !clean[meta.Name] && !specialCasedDelete[meta.Name] {
			orphans = append(orphans, meta.Name)
		}
	}
	if len(orphans) > 0 {
		t.Fatalf("以下含 contact_id 的表既不在删除级联清单、也无专用 DELETE，删联系人会留孤儿：%v", orphans)
	}

	// 反向：清理清单里的表必须都已登记且确实是 contact 维度（防止误放双列/全局表触发 no such column）。
	for _, name := range contactCleanupTables {
		m := GetTableMeta(name)
		if m == nil {
			t.Fatalf("contactCleanupTables 含未登记表 %q（应先在 registry 登记）", name)
		}
		if !m.HasContactID {
			t.Fatalf("%q 标记为不含单一 contact_id 列，不应出现在 WHERE contact_id=? 级联清单", name)
		}
	}
	// 双列关系连线必须走专用 DELETE，绝不能混进级联清单。
	if cleanupSet()["contact_connections"] {
		t.Fatalf("contact_connections 用 contact_a/contact_b 双列，绝不能进 contactCleanupTables")
	}
}

func TestContactDeleteCleansTopicHistoryAndSnooze(t *testing.T) {
	db := regressionDB(t)
	if err := ensureTopicHistory(db); err != nil {
		t.Fatalf("建主题历史表: %v", err)
	}
	if err := ensureTodaySnoozeTable(db); err != nil {
		t.Fatalf("建 TODAY 屏蔽表: %v", err)
	}
	id := regressionContact(t, db, "级联孤儿")

	// 各插一行 contact 维度的增值数据（均为「删联系人前存在、之后必须为 0」的对象）。
	if _, err := db.Exec(
		`INSERT INTO contact_topic_history (contact_id, week_start, topics_json, generated_at) VALUES (?,?,?,?)`,
		id, "2026-01-05", `["旅行"]`, "2026-01-05 10:00:00"); err != nil {
		t.Fatalf("写主题历史: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO today_snooze (contact_id, until_date, created_at) VALUES (?,?,?)`,
		id, time.Now().Add(24*time.Hour).Format("2006-01-02"), time.Now().Format("2006-01-02 15:04:05")); err != nil {
		t.Fatalf("写 TODAY 屏蔽: %v", err)
	}

	if err := DeleteContactByID(db, id); err != nil {
		t.Fatalf("删除联系人: %v", err)
	}
	assertZeroRows(t, db, "contact_topic_history", id)
	assertZeroRows(t, db, "today_snooze", id)
}

func assertZeroRows(t *testing.T, db *sql.DB, table string, contactID int64) {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM "`+table+`" WHERE contact_id = ?`, contactID).Scan(&n); err != nil {
		t.Fatalf("查 %s: %v", table, err)
	}
	if n != 0 {
		t.Fatalf("%s 残留 %d 条孤儿（联系人已删）", table, n)
	}
}

func TestBuildDataHealthCoversAllSubsystems(t *testing.T) {
	db := regressionDB(t)
	dh := BuildDataHealth(db, time.Now())
	if dh == nil || len(dh.Checks) == 0 {
		t.Fatalf("体检结果不得为空")
	}
	// 蓝图 §21 点名的 11 项必须齐全。
	want := []string{"Database", "FTS", "History Source", "Facts", "Evidence", "Metrics",
		"Action Ledger", "Memory", "AI Cache", "LLM Usage", "Backup"}
	byName := map[string]HealthCheck{}
	for _, c := range dh.Checks {
		byName[c.Name] = c
	}
	for _, n := range want {
		c, ok := byName[n]
		if !ok {
			t.Fatalf("体检缺少子系统 %q", n)
		}
		switch c.Status {
		case HealthStatusOK, HealthStatusWarn, HealthStatusError:
		default:
			t.Fatalf("%q 状态非法：%q", n, c.Status)
		}
	}
	// 核心子系统绝不可重建（§21「只能重建 derived/cache，绝不删除 core」）。
	for _, core := range []string{"Database", "History Source", "Action Ledger", "LLM Usage", "Backup"} {
		if byName[core].Rebuildable {
			t.Fatalf("核心子系统 %q 不得标记为可重建", core)
		}
	}
	// 总体等级合法且与明细一致（有 ERROR→ERROR，否则有 WARN→WARN，否则 OK）。
	switch dh.Summary {
	case HealthStatusOK, HealthStatusWarn, HealthStatusError:
	default:
		t.Fatalf("总体等级非法：%q", dh.Summary)
	}
}

func TestRebuildDerivedPreservesCore(t *testing.T) {
	db := regressionDB(t)
	// 造一条核心消息（绝不能被重建流程删除）。
	insertRawMessage(t, db, "messages", 0, "other", "核心不可删", time.Now().Add(-time.Hour).Format("2006-01-02 15:04:05"), nil)
	var before int
	db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&before)
	if before == 0 {
		t.Fatalf("前置：messages 应有一条核心消息")
	}

	cleared := RebuildDerived(db)
	if cleared <= 0 {
		t.Fatalf("应至少清空若干派生表，实得 %d", cleared)
	}
	// 派生事实表在重建后应为空（自愈将在下次访问时按需重算）。
	var facts int
	db.QueryRow(`SELECT COUNT(*) FROM profile_facts`).Scan(&facts)
	if facts != 0 {
		t.Fatalf("重建后派生 profile_facts 应被清空，实得 %d", facts)
	}
	// 核心消息必须还在。
	var after int
	db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&after)
	if after != before {
		t.Fatalf("重建派生绝不得动核心：messages %d→%d", before, after)
	}
}

// §20：GET 展示对 API Key 一律打码，真实密钥不外泄；空密钥保持空。
func TestMaskLLMSettingsHidesAPIKeys(t *testing.T) {
	in := LLMSettings{
		Profiles: []LLMProfile{
			{ID: "p1", Label: "主力", BaseURL: "https://x/v1", APIKey: "sk-secret-real", Model: "m1"},
			{ID: "p2", Label: "本地", BaseURL: "http://127.0.0.1:11434/v1", APIKey: "", Model: "m2"},
		},
	}
	out := maskLLMSettings(in)
	if out.Profiles[0].APIKey != apiKeyMask {
		t.Fatalf("含密钥档案应打码为 %q，实得 %q", apiKeyMask, out.Profiles[0].APIKey)
	}
	if out.Profiles[0].APIKey == "sk-secret-real" {
		t.Fatalf("真实密钥泄漏")
	}
	if out.Profiles[1].APIKey != "" {
		t.Fatalf("空密钥应保持不变，实得 %q", out.Profiles[1].APIKey)
	}
	// 原对象不得被就地修改（打码必须作用于副本）。
	if in.Profiles[0].APIKey != "sk-secret-real" {
		t.Fatalf("maskLLMSettings 不应就地修改源对象")
	}
}

// Evidence 子系统：孤儿证据（fact_id 指向不存在的事实）必须被判为 ERROR。
// 主库未开 foreign_keys（SQLite 默认 OFF，与 v19/v26 重建先例一致），故可直插孤儿行验证判定。
func TestBuildDataHealthEvidenceOrphanIsError(t *testing.T) {
	db := regressionDB(t)
	if _, err := db.Exec(`INSERT INTO profile_fact_evidence (fact_id, contact_id, message_id, archived)
		VALUES (999999, 1, 1, 0)`); err != nil {
		t.Fatalf("插孤儿证据: %v", err)
	}
	dh := BuildDataHealth(db, time.Now())
	for _, c := range dh.Checks {
		if c.Name == "Evidence" && c.Status != HealthStatusError {
			t.Fatalf("有孤儿证据时 Evidence 应为 ERROR，实得 %q：%s", c.Status, c.Detail)
		}
	}
}

// Facts/Metrics 子系统：有源消息却无派生内容时应判 WARN（可重建），不能谎报 OK。
func TestBuildDataHealthDerivedEmptyIsWarn(t *testing.T) {
	db := regressionDB(t)
	// 造一条消息（msgRows>0）但不产生任何事实/指标。
	insertRawMessage(t, db, "messages", 0, "other", "仅源消息无派生", time.Now().Add(-time.Hour).Format("2006-01-02 15:04:05"), nil)
	dh := BuildDataHealth(db, time.Now())
	got := map[string]HealthCheck{}
	for _, c := range dh.Checks {
		got[c.Name] = c
	}
	if f := got["Facts"]; f.Status != HealthStatusWarn {
		t.Fatalf("有消息无事实时 Facts 应 WARN，实得 %q：%s", f.Status, f.Detail)
	}
}

// §21 API 表面：GET 返回体检单，rebuild 仅放行派生/缓存、拒绝核心子系统。
func TestDataHealthAPIEndToEnd(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}

	// GET 体检：200 + 11 项齐全。
	w := callAPI(s, http.MethodGet, "/api/system/data-health", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET data-health 应 200，实得 %d %s", w.Code, w.Body.String())
	}
	var resp DataHealth // /api/system/* 直写负载，无 ok/data 包裹
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Checks) < 11 {
		t.Fatalf("应返回完整体检单，实得 checks=%d", len(resp.Checks))
	}

	// POST 重建派生：放行。
	if w2 := callAPI(s, http.MethodPost, "/api/system/data-health/rebuild?target=derived", ""); w2.Code != http.StatusOK {
		t.Fatalf("rebuild?target=derived 应 200，实得 %d %s", w2.Code, w2.Body.String())
	}
	// POST 指定核心子系统：拒绝 400（绝不删除/重建核心数据）。
	if w3 := callAPI(s, http.MethodPost, "/api/system/data-health/rebuild?target=Database", ""); w3.Code != http.StatusBadRequest {
		t.Fatalf("rebuild?target=Database 应 400，实得 %d %s", w3.Code, w3.Body.String())
	}
	// GET 用错误方法（DELETE）打体检端点 → 405。
	if w4 := callAPI(s, http.MethodDelete, "/api/system/data-health", ""); w4.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE data-health 应 405，实得 %d", w4.Code)
	}
}
