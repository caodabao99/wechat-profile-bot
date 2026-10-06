package main

// Phase 9 Relationship Risk Center（蓝图 §13）回归。
// 覆盖：纯函数（riskDaysUntil/overdueSeverity）、自锁源（imbalance/fact_conflict）、
// BuildRisks 七类聚合 + severity 排序 + §13.2 可解释四要素 + FACT/INFERENCE 分层、
// 以及 GET /api/risks 端到端与 severity 过滤。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// seedStateRow 直插一行 relationship_state，使 ListRelationshipStates 非空、从而不自愈重算，
// 让基于状态的降温/沉默风险可被确定性断言。
func seedStateRow(t *testing.T, db *sql.DB, contactID int64, dyn, alert, reason string) {
	t.Helper()
	if err := ensureRelationshipState(db); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db,
		`INSERT OR REPLACE INTO relationship_state
		 (contact_id, base_state, dynamic_state, intimacy, trend_state, alert, health, reason, changed_at, computed_at)
		 VALUES (?, 'established', ?, 60, 'stable', ?, 50, ?, '', '')`,
		contactID, dyn, alert, reason)
}

func risksWithType(items []RiskItem, ty string) []RiskItem {
	out := []RiskItem{}
	for _, it := range items {
		if it.Type == ty {
			out = append(out, it)
		}
	}
	return out
}

// assertExplainable §13.2：每条风险都要能解释数据来源/最近变化/建议行动，且分层明确。
func assertExplainable(t *testing.T, items []RiskItem) {
	t.Helper()
	for _, it := range items {
		if it.DataOrigin == "" || it.RecentChange == "" || it.SuggestedAction == "" {
			t.Fatalf("风险 %#v 缺少可解释四要素", it)
		}
		if it.Layer != "FACT" && it.Layer != "INFERENCE" {
			t.Fatalf("风险 %s 分层应为 FACT/INFERENCE，得 %q", it.Type, it.Layer)
		}
		switch it.Severity {
		case "high", "medium", "low":
		default:
			t.Fatalf("风险 %s 严重度应为 high/medium/low，得 %q", it.Type, it.Severity)
		}
	}
}

func TestRiskDaysUntilAndSeverityPure(t *testing.T) {
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.Local)
	if d, ok := riskDaysUntil(now, "2026-10-05"); !ok || d != -5 {
		t.Fatalf("过去日期应 -5，得 %d ok=%v", d, ok)
	}
	if d, ok := riskDaysUntil(now, "2026-10-12"); !ok || d != 2 {
		t.Fatalf("未来日期应 +2，得 %d ok=%v", d, ok)
	}
	if _, ok := riskDaysUntil(now, ""); ok {
		t.Fatal("空串应判无效")
	}
	if _, ok := riskDaysUntil(now, "乱码"); ok {
		t.Fatal("不可解析应判无效")
	}
	// 关键：明确把「无效」与「昨天(-1)」分开——昨天必须 ok 且为 -1
	if d, ok := riskDaysUntil(now, "2026-10-09"); !ok || d != -1 {
		t.Fatalf("昨天应 ok 且 -1（区别于无效的 -1 语义），得 %d ok=%v", d, ok)
	}
	if s := overdueSeverity(7); s != "high" {
		t.Fatalf("逾期 7 天应 high，得 %s", s)
	}
	if s := overdueSeverity(3); s != "medium" {
		t.Fatalf("逾期 3 天应 medium，得 %s", s)
	}
}

func TestImbalancedContactsLocked(t *testing.T) {
	db := regressionDB(t)
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.Local)
	hot := regressionContact(t, db, "一头热")   // me 多 other 少 → 失衡
	even := regressionContact(t, db, "很对等")  // 五五开 → 不判
	tiny := regressionContact(t, db, "样本不足") // 总量太小 → 不判
	day := now.Format("2006-01-02")
	mustExec(t, db, `INSERT INTO relationship_daily_metrics (contact_id, day, me_count, other_count) VALUES (?,?,?,?)`, hot, day, 10, 0)
	mustExec(t, db, `INSERT INTO relationship_daily_metrics (contact_id, day, me_count, other_count) VALUES (?,?,?,?)`, even, day, 5, 5)
	mustExec(t, db, `INSERT INTO relationship_daily_metrics (contact_id, day, me_count, other_count) VALUES (?,?,?,?)`, tiny, day, 3, 0)

	rows, err := imbalancedContactsLocked(db, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]imbalanceRow{}
	for _, r := range rows {
		got[r.contactID] = r
	}
	if r := got[hot]; r.me != 10 || r.other != 0 {
		t.Fatalf("一头热应 (10,0)，得 %#v", r)
	}
	if _, ok := got[even]; !ok {
		t.Fatal("对等联系人应仍被返回（由调用方按占比过滤），聚合层不裁剪")
	}
	// 调用侧过滤逻辑：只有 hot 触发失衡
	items := []RiskItem{}
	for _, r := range rows {
		total := r.me + r.other
		if total < riskImbalanceMinTotal {
			continue
		}
		if float64(r.me)/float64(total) >= riskImbalanceMeRatio {
			items = append(items, RiskItem{ContactID: r.contactID})
		}
	}
	if len(items) != 1 || items[0].ContactID != hot {
		t.Fatalf("仅一头热应判失衡，得 %#v", items)
	}
}

func TestFactConflictsLocked(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "老王")
	mustExec(t, db,
		`INSERT INTO profile_facts (contact_id, fact_type, fact_value, status) VALUES (?,?,?, 'active')`,
		cid, "occupation", "律师")
	var factID int64
	if err := db.QueryRow(`SELECT id FROM profile_facts WHERE contact_id=?`, cid).Scan(&factID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db,
		`INSERT INTO profile_fact_evidence (fact_id, contact_id, message_id, evidence_type) VALUES (?,?,?, ?)`,
		factID, cid, 1, EvConflict)

	rows, err := factConflictsLocked(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].contactID != cid || rows[0].factValue != "律师" {
		t.Fatalf("应召回 1 条冲突事实（律师），得 %#v", rows)
	}
}

// TestBuildRisksFromStates 用受控 relationship_state 行驱动状态类风险，断言七类里的
// cooling/dormant 聚合、severity 归一、排序与可解释四要素。
func TestBuildRisksFromStates(t *testing.T) {
	db := regressionDB(t)
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.Local)
	cool := regressionContact(t, db, "降温中")
	risk := regressionContact(t, db, "快流失")
	dormant := regressionContact(t, db, "久未联系")
	stable := regressionContact(t, db, "很稳定")
	seedStateRow(t, db, cool, dynCooling, "watching", "近 30 天互动明显减少")
	seedStateRow(t, db, risk, dynAtRisk, "urgent", "断点风险高")
	seedStateRow(t, db, dormant, dynDormant, "none", "已 40 天未联系")
	seedStateRow(t, db, stable, dynStable, "none", "互动平稳")

	items, err := BuildRisks(db, now)
	if err != nil {
		t.Fatal(err)
	}
	assertExplainable(t, items)

	if got := risksWithType(items, riskCooling); len(got) != 2 {
		t.Fatalf("应有 2 条降温类风险（cooling+at_risk），得 %d", len(got))
	}
	// at_risk 或 urgent → high；纯 cooling（watching）→ medium
	var sawHigh, sawMedium bool
	for _, it := range risksWithType(items, riskCooling) {
		if it.ContactID == risk && it.Severity != "high" {
			t.Fatalf("at_risk/urgent 应为 high，得 %s", it.Severity)
		}
		if it.ContactID == cool && it.Severity != "medium" {
			t.Fatalf("watching cooling 应为 medium，得 %s", it.Severity)
		}
		if it.Severity == "high" {
			sawHigh = true
		}
		if it.Severity == "medium" {
			sawMedium = true
		}
	}
	if !sawHigh || !sawMedium {
		t.Fatal("应同时出现 high 与 medium 两档降温风险")
	}
	sil := risksWithType(items, riskLongSilence)
	if len(sil) != 1 || sil[0].ContactID != dormant {
		t.Fatalf("应恰有 1 条长期沉默（dormant），得 %#v", sil)
	}
	// stable 不产风险
	for _, it := range items {
		if it.ContactID == stable {
			t.Fatalf("stable 联系人不应产生风险，得 %#v", it)
		}
	}
	// 排序：首位应是 high（at_risk）
	if len(items) == 0 || items[0].Severity != "high" {
		t.Fatalf("首条应为 high 严重度，得 %#v", items)
	}
}

// TestBuildRisksAllSourcesMixed 同时喂入项目/待办逾期、事实冲突、失衡、重要日子已过，
// 断言七类风险各自被聚合且分层/严重度正确。
func TestBuildRisksAllSourcesMixed(t *testing.T) {
	db := regressionDB(t)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local)
	// 待办表懒建：测试直接调 AddFollowup 前先确保 followup_items 存在
	if err := ensureFollowupTables(db); err != nil {
		t.Fatal(err)
	}
	// 阻断状态自愈：插一行 stable，使 ListRelationshipStates 非空
	blocker := regressionContact(t, db, "占位")
	seedStateRow(t, db, blocker, dynStable, "none", "")

	// 项目逾期（10 天前 → high）
	proj := regressionContact(t, db, "项目人")
	if _, err := CreateProject(db, CreateProjectInput{
		ContactID: proj, Title: "推进合作", Status: "active",
		NextAction: "约电话", NextActionDue: now.AddDate(0, 0, -10).Format("2006-01-02"),
	}, now); err != nil {
		t.Fatal(err)
	}

	// 待办逾期（3 天前 → medium）
	fu := regressionContact(t, db, "待办人")
	if _, err := AddFollowup(db, fu, "promise", "还他两千", "2000", now.AddDate(0, 0, -3).Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}

	// 事实冲突
	conf := regressionContact(t, db, "冲突人")
	mustExec(t, db, `INSERT INTO profile_facts (contact_id, fact_type, fact_value, status) VALUES (?,?,?, 'active')`, conf, "location", "北京")
	var fid int64
	if err := db.QueryRow(`SELECT id FROM profile_facts WHERE contact_id=?`, conf).Scan(&fid); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO profile_fact_evidence (fact_id, contact_id, message_id, evidence_type) VALUES (?,?,?, ?)`, fid, conf, 9, EvConflict)

	// 互动失衡
	imb := regressionContact(t, db, "失衡人")
	mustExec(t, db, `INSERT INTO relationship_daily_metrics (contact_id, day, me_count, other_count) VALUES (?,?,?,?)`, imb, now.Format("2006-01-02"), 12, 1)

	// 重要日子已过（2 天前的生日 → high）
	dt := regressionContact(t, db, "寿星")
	pd := now.AddDate(0, 0, -2)
	pj := fmt.Sprintf(`{"basic_info":{"important_dates":[%q]}}`, fmt.Sprintf("生日 %d月%d日", int(pd.Month()), pd.Day()))
	mustExec(t, db, `UPDATE contacts SET profile_json=? WHERE id=?`, pj, dt)

	items, err := BuildRisks(db, now)
	if err != nil {
		t.Fatal(err)
	}
	assertExplainable(t, items)

	find := func(ty string, cid int64) *RiskItem {
		for i := range items {
			if items[i].Type == ty && items[i].ContactID == cid {
				return &items[i]
			}
		}
		return nil
	}
	if it := find(riskProjectOverdue, proj); it == nil || it.Layer != "FACT" || it.Severity != "high" {
		t.Fatalf("项目逾期应 FACT/high，得 %#v", it)
	}
	if it := find(riskFollowupOverdue, fu); it == nil || it.Layer != "FACT" || it.Severity != "medium" {
		t.Fatalf("待办逾期应 FACT/medium，得 %#v", it)
	}
	if it := find(riskFactConflict, conf); it == nil || it.Layer != "INFERENCE" {
		t.Fatalf("事实冲突应 INFERENCE，得 %#v", it)
	}
	if it := find(riskImbalance, imb); it == nil || it.Layer != "INFERENCE" {
		t.Fatalf("互动失衡应 INFERENCE，得 %#v", it)
	}
	if it := find(riskDateMissed, dt); it == nil || it.Layer != "INFERENCE" || it.Severity != "high" {
		t.Fatalf("重要日子刚过应 INFERENCE/high，得 %#v", it)
	}

	byType := CountRisksByType(items)
	if byType[riskProjectOverdue] != 1 || byType[riskFollowupOverdue] != 1 || byType[riskFactConflict] != 1 {
		t.Fatalf("按类型计数异常，得 %#v", byType)
	}
}

// TestRisksAPIEndToEnd 走真实路由验证 GET /api/risks 与 severity 过滤。
func TestRisksAPIEndToEnd(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}

	cool := regressionContact(t, db, "降温")
	seedStateRow(t, db, cool, dynAtRisk, "urgent", "断点风险高")

	w := callAPI(s, http.MethodGet, "/api/risks", "")
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		OK     bool           `json:"ok"`
		Count  int            `json:"count"`
		ByType map[string]int `json:"byType"`
		Items  []RiskItem     `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Count != len(resp.Items) || resp.Count < 1 {
		t.Fatalf("响应异常 ok=%v count=%d len=%d", resp.OK, resp.Count, len(resp.Items))
	}
	if resp.Items[0].Type != riskCooling || resp.Items[0].Severity != "high" {
		t.Fatalf("首条应为 cooling/high，得 %#v", resp.Items[0])
	}

	// severity=low 过滤：本例只有 high → 空
	w2 := callAPI(s, http.MethodGet, "/api/risks?severity=low", "")
	var resp2 struct {
		Items []RiskItem `json:"items"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
		t.Fatal(err)
	}
	for _, it := range resp2.Items {
		if it.Severity != "low" {
			t.Fatalf("severity=low 过滤后仍含非 low：%#v", it)
		}
	}

	// 未知子路径 404
	w3 := callAPI(s, http.MethodGet, "/api/risks/unknown", "")
	if w3.Code != http.StatusNotFound {
		t.Fatalf("未知子路径应 404，得 %d", w3.Code)
	}
}
