package main

import (
	"database/sql"
	"testing"
	"time"
)

func TestCreateExperimentValidation(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "实验对象")
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// 周期非正 → 拒绝
	if _, err := CreateExperiment(db, CreateExperimentInput{ContactID: cid, DurationDays: 0, StartDate: "2026-01-01"}, now); err == nil {
		t.Fatal("duration<=0 应被拒绝")
	}
	// 联系人不存在 → 拒绝
	if _, err := CreateExperiment(db, CreateExperimentInput{ContactID: 999999, DurationDays: 14, StartDate: "2026-01-01"}, now); err == nil {
		t.Fatal("不存在的联系人应被拒绝")
	}
	// 正常：draft + end=start+duration
	id, err := CreateExperiment(db, CreateExperimentInput{
		ContactID: cid, Goal: "改善关系", Strategy: "低压力联系，每周一次", AvoidStrategy: "连续追问",
		DurationDays: 14, StartDate: "2026-01-01",
	}, now)
	if err != nil || id <= 0 {
		t.Fatalf("创建失败: id=%d err=%v", id, err)
	}
	e, err := GetExperiment(db, id)
	if err != nil || e == nil {
		t.Fatalf("读取失败: %+v err=%v", e, err)
	}
	if e.Status != ExpStatusDraft {
		t.Errorf("初始应为 draft, got %s", e.Status)
	}
	if e.StartDate != "2026-01-01" || e.EndDate != "2026-01-15" {
		t.Errorf("起止日期应为 2026-01-01→2026-01-15, got %s→%s", e.StartDate, e.EndDate)
	}
	if e.Conclusion != "" || e.Metrics == "" {
		t.Errorf("初始无结论、指标默认非空, got %+v", e)
	}
}

func TestExperimentLifecycleAndResult(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "生命周期实验")
	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	id, err := CreateExperiment(db, CreateExperimentInput{ContactID: cid, Goal: "g", DurationDays: 14}, now)
	if err != nil {
		t.Fatal(err)
	}
	// 非法状态 → 拒绝
	if err := SetExperimentStatus(db, id, "bogus", now); err == nil {
		t.Fatal("非法状态应被拒绝")
	}
	// draft → running
	if err := SetExperimentStatus(db, id, ExpStatusRunning, now); err != nil {
		t.Fatal(err)
	}
	// 非法结论 → 拒绝
	if err := UpdateExperimentResult(db, id, "causal", "note", "", "", now); err == nil {
		t.Fatal("因果式非法结论应被拒绝（§9.3）")
	}
	// 合法结论 → 落库并置 completed
	if err := UpdateExperimentResult(db, id, ExpConclusionImproved, "历史数据中该策略与互动上升相关（非因果证明）", `{"avg":1}`, `{"avg":2}`, now); err != nil {
		t.Fatal(err)
	}
	e, _ := GetExperiment(db, id)
	if e.Status != ExpStatusCompleted || e.Conclusion != ExpConclusionImproved {
		t.Fatalf("应置 completed+improved, got %+v", e)
	}
	// 不存在的实验 → ErrNoRows
	if err := SetExperimentStatus(db, 999999, ExpStatusRunning, now); err != sql.ErrNoRows {
		t.Errorf("不存在实验应 ErrNoRows, got %v", err)
	}
}

func TestListExperiments(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "列表实验")
	now := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := CreateExperiment(db, CreateExperimentInput{ContactID: cid, Goal: "g", DurationDays: 7}, now); err != nil {
			t.Fatal(err)
		}
	}
	all, err := ListExperiments(db, cid, "", 10)
	if err != nil || len(all) != 3 {
		t.Fatalf("应列出 3 条, got %d err=%v", len(all), err)
	}
	draft, _ := ListExperiments(db, cid, ExpStatusDraft, 10)
	if len(draft) != 3 {
		t.Errorf("应 3 条 draft, got %d", len(draft))
	}
	running, _ := ListExperiments(db, cid, ExpStatusRunning, 10)
	if len(running) != 0 {
		t.Errorf("无 running, got %d", len(running))
	}
}

// 治理三处同步（audit 类，同行动账本先例）：registry 登记 + 不进 derivedTables + 进 contactCleanupTables。
func TestExperimentGovernanceSync(t *testing.T) {
	m := GetTableMeta("relationship_experiment")
	if m == nil {
		t.Fatal("relationship_experiment 应登记进 registry")
	}
	if m.Type != "audit" || !m.Backup || m.Rebuild || !m.HasContactID {
		t.Fatalf("应为 audit/Backup/不可重建/带 contact_id, got %+v", m)
	}
	for _, dt := range derivedTables {
		if dt == "relationship_experiment" {
			t.Fatal("实验定义不可重建，绝不能进 derivedTables（会被恢复清空）")
		}
	}
	var inCleanup bool
	for _, ct := range contactCleanupTables {
		if ct == "relationship_experiment" {
			inCleanup = true
			break
		}
	}
	if !inCleanup {
		t.Fatal("relationship_experiment 应在 contactCleanupTables 级联清理清单")
	}
	var hasStmt bool
	for _, s := range contactCleanupStmts() {
		if s == `DELETE FROM relationship_experiment WHERE contact_id = ?` {
			hasStmt = true
			break
		}
	}
	if !hasStmt {
		t.Fatal("contactCleanupStmts 应包含 relationship_experiment 的清理语句")
	}
}
