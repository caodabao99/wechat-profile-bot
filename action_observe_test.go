package main

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// insertMetrics 直插 relationship_daily_metrics 一行（测试用于构造前/后窗口的互动量）。
func insertMetrics(t *testing.T, db *sql.DB, cid int64, day string, me, other int) {
	t.Helper()
	mustExec(t, db,
		`INSERT INTO relationship_daily_metrics (contact_id, day, me_count, other_count, first_unix, last_unix)
		 VALUES (?, ?, ?, ?, ?, ?)`, cid, day, me, other, 0, 0)
}

// TestObserveActionOutcomes 验证 §5.4：执行满 7 天后据互动量变化估算 outcome（provenance=estimated），
// 幂等、未满 7 天不动、确认不被估算覆盖、note 不声称因果。
func TestObserveActionOutcomes(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "小刚")
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	actedTime := now.AddDate(0, 0, -10) // 执行于 10 天前，已满足 ≥7 天观察窗口

	id, err := LogAction(db, cid, "manual", "", "meet", "约见面", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := TransitionActionStatus(db, id, ActionStatusActed, "", actedTime); err != nil {
		t.Fatal(err)
	}

	// 前窗口（执行前）几乎无互动，后窗口（执行后至今）互动明显上升 → 应估为 positive。
	preDay := actedTime.AddDate(0, 0, -2).Format("2006-01-02")
	postDay := actedTime.AddDate(0, 0, 2).Format("2006-01-02")
	insertMetrics(t, db, cid, preDay, 1, 0)
	insertMetrics(t, db, cid, postDay, 10, 0)

	observed, err := ObserveActionOutcomes(db, now)
	if err != nil {
		t.Fatal(err)
	}
	if observed != 1 {
		t.Fatalf("应观察到 1 条, got %d", observed)
	}
	list, _ := ListActionLog(db, cid, 0)
	if len(list) != 1 {
		t.Fatalf("记录数异常 len=%d", len(list))
	}
	e := list[0]
	if e.Outcome != ActionOutcomePositive || e.OutcomeProvenance != ActionProvenanceEstimated {
		t.Fatalf("应估为 positive/estimated, got %s/%s", e.Outcome, e.OutcomeProvenance)
	}
	if e.OutcomeDays < 7 {
		t.Fatalf("outcome_days 应 ≥7, got %d", e.OutcomeDays)
	}
	if !strings.Contains(e.OutcomeNote, "非因果") {
		t.Fatalf("note 必须声明非因果, got %q", e.OutcomeNote)
	}

	// 幂等：再次观察，已 estimated 的不被重复处理。
	if n, _ := ObserveActionOutcomes(db, now); n != 0 {
		t.Fatalf("重复观察应为 0, got %d", n)
	}
}

// TestObserveSkipsRecentAndConfirmed 验证：未满 7 天不动；用户已确认的永不被系统估算覆盖。
func TestObserveSkipsRecentAndConfirmed(t *testing.T) {
	db := regressionDB(t)

	// A：3 天前才执行 → 未到期，不观察。
	ca := regressionContact(t, db, "A")
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	ia, _ := LogAction(db, ca, "coach", "", "x", "y", now)
	if err := TransitionActionStatus(db, ia, ActionStatusActed, "", now.AddDate(0, 0, -3)); err != nil {
		t.Fatal(err)
	}
	insertMetrics(t, db, ca, now.AddDate(0, 0, 1).Format("2006-01-02"), 9, 0)

	// B：10 天前执行，但用户已 confirmed → 观察不得覆盖。
	cb := regressionContact(t, db, "B")
	actedB := now.AddDate(0, 0, -10)
	ib, _ := LogAction(db, cb, "goal", "", "x", "y", now)
	if err := TransitionActionStatus(db, ib, ActionStatusActed, "", actedB); err != nil {
		t.Fatal(err)
	}
	if err := SetActionOutcome(db, ib, ActionOutcomeNegative, ActionProvenanceConfirmed, "用户说没效果", now); err != nil {
		t.Fatal(err)
	}
	insertMetrics(t, db, cb, actedB.AddDate(0, 0, 1).Format("2006-01-02"), 20, 0)

	observed, _ := ObserveActionOutcomes(db, now)
	if observed != 0 {
		t.Fatalf("未到期 + 已确认，均不该被估算, got %d", observed)
	}
	lb, _ := ListActionLog(db, cb, 0)
	if lb[0].Outcome != ActionOutcomeNegative || lb[0].OutcomeProvenance != ActionProvenanceConfirmed {
		t.Fatalf("确认结果应完好不被估算污染: %s/%s", lb[0].Outcome, lb[0].OutcomeProvenance)
	}
	la, _ := ListActionLog(db, ca, 0)
	if la[0].OutcomeProvenance != "" {
		t.Fatalf("未满 7 天不该有 outcome, got %q", la[0].OutcomeProvenance)
	}
}
