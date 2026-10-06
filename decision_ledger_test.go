package main

import (
	"database/sql"
	"testing"
	"time"
)

func countDecisionRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM relationship_action_log WHERE source='decision'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestDecisionFingerprintDeterministic 验证 §6.1：指纹确定、与 reason_codes 顺序无关、且每个分量都参与。
func TestDecisionFingerprintDeterministic(t *testing.T) {
	base := decisionFingerprint(7, "greet", []string{"cooling", "goal_deadline"}, "ctxv", "2026-10-06")
	// 顺序无关
	reordered := decisionFingerprint(7, "greet", []string{"goal_deadline", "cooling"}, "ctxv", "2026-10-06")
	if base != reordered {
		t.Fatal("reason_codes 顺序不应影响指纹")
	}
	// 每个分量变化都应变指纹
	type fpCase struct {
		cid  int64
		at   string
		code []string
		cv   string
		wk   string
	}
	cases := []fpCase{
		{8, "greet", []string{"cooling", "goal_deadline"}, "ctxv", "2026-10-06"},  // contact
		{7, "meet", []string{"cooling", "goal_deadline"}, "ctxv", "2026-10-06"},   // action type
		{7, "greet", []string{"cooling"}, "ctxv", "2026-10-06"},                   // codes
		{7, "greet", []string{"cooling", "goal_deadline"}, "ctxv2", "2026-10-06"}, // context version
		{7, "greet", []string{"cooling", "goal_deadline"}, "ctxv", "2026-10-07"},  // window
	}
	for i, c := range cases {
		fp := decisionFingerprint(c.cid, c.at, c.code, c.cv, c.wk)
		if fp == base {
			t.Fatalf("分量变化的指纹应与基线不同 (case %d)", i)
		}
	}
}

// TestRecordDecisionIdempotent 验证同指纹不重复生成：首次 created=true，二次 created=false 且同 id，仅一行。
func TestRecordDecisionIdempotent(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "去重人")
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	wk := DecisionWindowKey(now)

	id1, created1, err := RecordDecision(db, cid, "greet", []string{"cooling"}, "ctxv", wk, "破冰", now)
	if err != nil || !created1 || id1 <= 0 {
		t.Fatalf("首次应新建: id=%d created=%v err=%v", id1, created1, err)
	}
	id2, created2, err := RecordDecision(db, cid, "greet", []string{"cooling"}, "ctxv", wk, "破冰", now)
	if err != nil {
		t.Fatal(err)
	}
	if created2 || id2 != id1 {
		t.Fatalf("同指纹不得重复生成: created=%v id2=%d id1=%d", created2, id2, id1)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM relationship_action_log WHERE contact_id=?`, cid).Scan(&n)
	if n != 1 {
		t.Fatalf("应仅 1 行, got %d", n)
	}
}

// seedDecisionCandidates 铺两个必然进入决策的联系人（复用 decision_test 配方）。
func seedDecisionCandidates(t *testing.T, db *sql.DB) (int64, int64) {
	t.Helper()
	if err := ensureFollowupTables(db); err != nil {
		t.Fatal(err)
	}
	a := regressionContact(t, db, "闭环甲")
	b := regressionContact(t, db, "闭环乙")
	for _, c := range []int64{a, b} {
		if err := SaveProfile(db, c, `{"summary":"s"}`, "s", "i"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := RefreshRelationshipStates(db, time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	setForcedState(t, db, a, dynAtRisk, "urgent", 60)
	setForcedState(t, db, b, dynCooling, "watching", 30)
	if _, err := AddFollowup(db, a, "question", "等他回复方案", "", ""); err != nil {
		t.Fatal(err)
	}
	return a, b
}

// TestSurfaceDecisionsDedupNoGrowth 验证反复首页不会因「记录→context 变→再生成」而无限增长（自反馈防护）。
func TestSurfaceDecisionsDedupNoGrowth(t *testing.T) {
	db := regressionDB(t)
	seedDecisionCandidates(t, db)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	first, err := SurfaceDecisions(db, now, 3)
	if err != nil || len(first) == 0 {
		t.Fatalf("应产出候选: len=%d err=%v", len(first), err)
	}
	for _, c := range first {
		if c.ActionLogID <= 0 || c.DecisionFingerprint == "" {
			t.Fatalf("候选应带 action_log_id 与指纹: %+v", c)
		}
	}
	before := countDecisionRows(t, db)
	// 反复调用同窗：行数必须稳定（幂等去重，无增长）。
	for i := 0; i < 3; i++ {
		if _, err := SurfaceDecisions(db, now, 3); err != nil {
			t.Fatal(err)
		}
	}
	if after := countDecisionRows(t, db); after != before {
		t.Fatalf("同窗反复首页不得增长决策行: before=%d after=%d", before, after)
	}
}

// TestDecisionDismissNotPermanent 验证 §6.3：忽略记录原因、只屏蔽当前窗；跨窗自然重入（不永久屏蔽）。
func TestDecisionDismissNotPermanent(t *testing.T) {
	db := regressionDB(t)
	seedDecisionCandidates(t, db)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	list, err := SurfaceDecisions(db, now, 3)
	if err != nil || len(list) == 0 {
		t.Fatal(err)
	}
	target := list[0]
	// 忽略该决策。
	if err := DismissAction(db, target.ActionLogID, "现在不方便", now); err != nil {
		t.Fatal(err)
	}
	e, err := GetActionLog(db, target.ActionLogID)
	if err != nil || e == nil || e.Status != ActionStatusDismissed || e.DismissReason != "现在不方便" {
		t.Fatalf("忽略应记录状态与原因: %+v err=%v", e, err)
	}

	// 同窗再首页：被忽略项不再呈现（屏蔽本窗）。
	again, _ := SurfaceDecisions(db, now, 3)
	for _, c := range again {
		if c.ContactID == target.ContactID {
			t.Fatal("同窗内被忽略的决策不应再呈现")
		}
	}

	// 跨窗（次日，新指纹）：自然重入 → 证明未永久屏蔽。
	nextDay := now.AddDate(0, 0, 1)
	reentered, err := SurfaceDecisions(db, nextDay, 3)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range reentered {
		if c.ContactID == target.ContactID {
			found = true
		}
	}
	if !found {
		t.Fatal("跨窗后被忽略的决策应重新进入候选（不永久屏蔽）")
	}
}
