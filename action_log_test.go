package main

import (
	"testing"
	"time"
)

// TestActionLogLifecycle 验证：LogAction 起点=generated → 逐态迁移 → acted/completed 补记 acted_at
// → 已执行不可回退 → ListActionLog 读出。
func TestActionLogLifecycle(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "小明")
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	id, err := LogAction(db, cid, "coach", "sugg-42", "reach_out", "主动问候破冰", now)
	if err != nil || id <= 0 {
		t.Fatalf("LogAction 失败: id=%d err=%v", id, err)
	}

	list, err := ListActionLog(db, cid, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("应读到 1 条: len=%d err=%v", len(list), err)
	}
	if list[0].Status != ActionStatusGenerated {
		t.Fatalf("新记录生命周期起点应为 generated, got %s", list[0].Status)
	}
	if list[0].ActedAt != "" {
		t.Fatalf("generated 态不应有 acted_at, got %q", list[0].ActedAt)
	}

	// viewed → accepted → deferred（带截止日）→ acted（补记 acted_at）
	if err := TransitionActionStatus(db, id, ActionStatusViewed, "", now); err != nil {
		t.Fatal(err)
	}
	if err := TransitionActionStatus(db, id, ActionStatusAccepted, "", now); err != nil {
		t.Fatal(err)
	}
	if err := TransitionActionStatus(db, id, ActionStatusDeferred, "2026-10-20", now); err != nil {
		t.Fatal(err)
	}
	list, _ = ListActionLog(db, cid, 0)
	if list[0].DeferredUntil != "2026-10-20" {
		t.Fatalf("deferred_until 应记录截止日, got %q", list[0].DeferredUntil)
	}

	later := now.Add(2 * 24 * time.Hour)
	if err := TransitionActionStatus(db, id, ActionStatusActed, "", later); err != nil {
		t.Fatal(err)
	}
	list, _ = ListActionLog(db, cid, 0)
	if list[0].Status != ActionStatusActed || list[0].ActedAt == "" {
		t.Fatalf("acted 态应补记 acted_at: status=%s acted=%q", list[0].Status, list[0].ActedAt)
	}

	// 已 acted 不可回退到 acted 之前的状态。
	if err := TransitionActionStatus(db, id, ActionStatusAccepted, "", now); err == nil {
		t.Fatal("已执行行动回退应被拒绝")
	}

	// 非法来源 / 非法状态被拒。
	if _, err := LogAction(db, cid, "bogus", "", "", "x", now); err == nil {
		t.Fatal("非法 source 应被拒绝")
	}
	if err := TransitionActionStatus(db, id, "not_a_status", "", now); err == nil {
		t.Fatal("非法 status 应被拒绝")
	}
}

// TestActionLogOutcomeSeparation 验证 §5.3「不能混淆」：estimated 不得覆盖 confirmed。
func TestActionLogOutcomeSeparation(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "小红")
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	id, err := LogAction(db, cid, "manual", "", "call", "打电话", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := TransitionActionStatus(db, id, ActionStatusActed, "", now); err != nil {
		t.Fatal(err)
	}

	// 系统先估算。
	if err := SetActionOutcome(db, id, ActionOutcomePositive, ActionProvenanceEstimated, "互动回暖", now.Add(7*24*time.Hour)); err != nil {
		t.Fatalf("首次 estimated 应可写入: %v", err)
	}
	list, _ := ListActionLog(db, cid, 0)
	if list[0].OutcomeProvenance != ActionProvenanceEstimated || list[0].Outcome != ActionOutcomePositive {
		t.Fatalf("估算结果写入异常: prov=%s outcome=%s", list[0].OutcomeProvenance, list[0].Outcome)
	}

	// 用户手工确认为正。
	if err := SetActionOutcome(db, id, ActionOutcomePositive, ActionProvenanceConfirmed, "用户反馈很好", now.Add(8*24*time.Hour)); err != nil {
		t.Fatalf("confirmed 应可覆盖 estimated: %v", err)
	}

	// 关键铁律：确认之后，系统估算不得覆盖。
	if err := SetActionOutcome(db, id, ActionOutcomeNegative, ActionProvenanceEstimated, "系统想改判", now.Add(9*24*time.Hour)); err == nil {
		t.Fatal("系统估算覆盖用户确认必须被拒绝")
	}
	list, _ = ListActionLog(db, cid, 0)
	if list[0].OutcomeProvenance != ActionProvenanceConfirmed || list[0].Outcome != ActionOutcomePositive {
		t.Fatalf("确认结果应保持不被污染: prov=%s outcome=%s", list[0].OutcomeProvenance, list[0].Outcome)
	}

	// 非法极性 / 非法来源被拒。
	if err := SetActionOutcome(db, id, "bogus", ActionProvenanceConfirmed, "", now); err == nil {
		t.Fatal("非法 outcome 应被拒绝")
	}
	if err := SetActionOutcome(db, id, ActionOutcomeNeutral, "", "", now); err == nil {
		t.Fatal("空 provenance 应被拒绝（须显式 estimated/confirmed）")
	}
}

// TestActionLogMissingContact 验证向不存在联系人记行动会被拒（不写脏数据）。
func TestActionLogMissingContact(t *testing.T) {
	db := regressionDB(t)
	if _, err := LogAction(db, 999999, "goal", "", "", "x", time.Now()); err == nil {
		t.Fatal("向不存在的联系人记行动应被拒绝")
	}
}

// TestActionLogGovernanceSync 验证治理三处同步：registry 登记 + 参与备份（不在 derivedTables）
// + 删联系人级联（在 contactCleanupTables）。这是 audit 类不可重建表的正确归类。
func TestActionLogGovernanceSync(t *testing.T) {
	m := GetTableMeta("relationship_action_log")
	if m == nil {
		t.Fatal("relationship_action_log 应登记进 registry")
	}
	if m.Type != "audit" || !m.Backup || m.Rebuild || !m.HasContactID {
		t.Fatalf("应为 audit/Backup/不可重建/带 contact_id, got %+v", m)
	}
	// 不可重建 → 绝不能进 derivedTables（否则恢复会被清空，丢用户行为史）。
	for _, dt := range derivedTables {
		if dt == "relationship_action_log" {
			t.Fatal("行动账本不应在 derivedTables（会被恢复清空）")
		}
	}
	// 含 contact_id → 必须在 contactCleanupTables（删联系人级联清理）。
	var inCleanup bool
	for _, ct := range contactCleanupTables {
		if ct == "relationship_action_log" {
			inCleanup = true
			break
		}
	}
	if !inCleanup {
		t.Fatal("行动账本应在 contactCleanupTables 级联清理清单")
	}
	// contactCleanupStmts 应据此生成带 contact_id 的 DELETE 语句。
	var hasStmt bool
	for _, s := range contactCleanupStmts() {
		if s == `DELETE FROM relationship_action_log WHERE contact_id = ?` {
			hasStmt = true
			break
		}
	}
	if !hasStmt {
		t.Fatal("contactCleanupStmts 应包含行动账本的清理语句")
	}
}
