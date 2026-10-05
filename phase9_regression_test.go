package main

// OS 2.0 Phase 9 全量回归（规格第二十三/二十二/二十四章）：
//   - 联系人合并：用户创建的目标 / 项目必须一并迁到目标，撤销可精确还原（二十三·合并）；
//   - 联系人删除：新表 relationship_projects / relationship_goals / relationship_state 必须级联清理（二十三·删除）；
//   - 备份恢复：新增核心用户表必须随备份往返、派生表不入备份（二十二·备份恢复）；
//   - 数据层治理：新表 registry 元数据（Backup/Rebuild/HasContactID）不得漂移（二十一）。
// 全部确定性、不调模型。复用 shared_regression_test.go / backup_restore_tables_test.go 的
// regressionDB / regressionContact / regressionMessages / mustExec / restoreCount helper。

import (
	"path/filepath"
	"testing"
	"time"
)

// —— 二十三·合并：目标 / 项目随合并迁移，撤销精确还原 ——
func TestMergeTransfersGoalsAndProjectsAndUndoRestores(t *testing.T) {
	db := regressionDB(t)
	now := time.Date(2025, 6, 10, 12, 0, 0, 0, time.UTC)
	a := regressionContact(t, db, "source-甲")
	b := regressionContact(t, db, "target-乙")

	// source 名下一条目标 + 一条项目
	goalID, err := CreateGoal(db, CreateGoalInput{ContactID: a, Title: "每月至少联系一次", Metric: "monthly_me", TargetCount: 12}, now)
	if err != nil {
		t.Fatal(err)
	}
	projID, err := CreateProject(db, CreateProjectInput{ContactID: a, Title: "修复多年友谊", Status: "active", Stage: "building"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if goalID == 0 || projID == 0 {
		t.Fatalf("创建返回非法 ID goal=%d proj=%d", goalID, projID)
	}

	merged, err := MergeContacts(db, a, b, MergeOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// 合并后：目标/项目都应挂到 target(b)，source(a) 名下清零
	if got := restoreCount(t, db, `SELECT COUNT(*) FROM relationship_goals WHERE contact_id=?`, b); got != 1 {
		t.Fatalf("合并后目标应迁到 target，b 名下 goal=%d，期望 1", got)
	}
	if got := restoreCount(t, db, `SELECT COUNT(*) FROM relationship_goals WHERE contact_id=?`, a); got != 0 {
		t.Fatalf("合并后 source 不应残留 goal，a 名下=%d", got)
	}
	if got := restoreCount(t, db, `SELECT COUNT(*) FROM relationship_projects WHERE contact_id=?`, b); got != 1 {
		t.Fatalf("合并后项目应迁到 target，b 名下 project=%d，期望 1", got)
	}
	if got := restoreCount(t, db, `SELECT COUNT(*) FROM relationship_projects WHERE contact_id=?`, a); got != 0 {
		t.Fatalf("合并后 source 不应残留 project，a 名下=%d", got)
	}

	// 撤销：目标/项目精确还原到 source
	if err := UndoMerge(db, merged.MergeLogID); err != nil {
		t.Fatalf("撤销合并失败: %v", err)
	}
	if got := restoreCount(t, db, `SELECT COUNT(*) FROM relationship_goals WHERE id=? AND contact_id=?`, goalID, a); got != 1 {
		t.Fatalf("撤销后 goal 应回到 source，命中=%d", got)
	}
	if got := restoreCount(t, db, `SELECT COUNT(*) FROM relationship_projects WHERE id=? AND contact_id=?`, projID, a); got != 1 {
		t.Fatalf("撤销后 project 应回到 source，命中=%d", got)
	}
	if got := restoreCount(t, db, `SELECT COUNT(*) FROM relationship_projects WHERE contact_id=?`, b); got != 0 {
		t.Fatalf("撤销后 target 不应残留被迁来的 project，b 名下=%d", got)
	}
}

// —— 二十三·删除：新表级联清理 ——
func TestDeleteContactCleansProjectsGoalsAndState(t *testing.T) {
	db := regressionDB(t)
	now := time.Date(2025, 6, 10, 12, 0, 0, 0, time.UTC)
	c := regressionContact(t, db, "待删的人")
	regressionMessages(t, db, c, "你好", "最近怎么样")

	if _, err := CreateGoal(db, CreateGoalInput{ContactID: c, Title: "常联系", Metric: "monthly_me", TargetCount: 6}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateProject(db, CreateProjectInput{ContactID: c, Title: "合作项目", Status: "active", Stage: "discovery"}, now); err != nil {
		t.Fatal(err)
	}
	// 触发派生状态落库（relationship_state 有该联系人一行）
	if _, _, err := RefreshRelationshipStates(db, now, 0); err != nil {
		t.Fatal(err)
	}
	if got := restoreCount(t, db, `SELECT COUNT(*) FROM relationship_state WHERE contact_id=?`, c); got == 0 {
		t.Fatalf("前置条件：relationship_state 应已有该联系人行")
	}

	if err := DeleteContactByID(db, c); err != nil {
		t.Fatalf("删除联系人失败: %v", err)
	}
	for _, tc := range []struct{ table, col string }{
		{"relationship_projects", "contact_id"},
		{"relationship_goals", "contact_id"},
		{"relationship_state", "contact_id"},
	} {
		if got := restoreCount(t, db, `SELECT COUNT(*) FROM `+tc.table+` WHERE `+tc.col+`=?`, c); got != 0 {
			t.Fatalf("删除后 %s 应清空该联系人行，残留=%d", tc.table, got)
		}
	}
}

// —— 二十二 + 二十一：新增核心用户表随备份往返；派生表不入备份；无悬空 ——
func TestBackupRestoreCarriesProjectsGoalsDropsDerivedState(t *testing.T) {
	now := time.Date(2025, 6, 10, 12, 0, 0, 0, time.UTC)

	src := regressionDB(t)
	sc := regressionContact(t, src, "源人")
	regressionMessages(t, src, sc, "在吗", "改天聚")
	if _, err := CreateGoal(src, CreateGoalInput{ContactID: sc, Title: "源目标-唯一标识", Metric: "monthly_me", TargetCount: 3}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateProject(src, CreateProjectInput{ContactID: sc, Title: "源项目-唯一标识", Status: "active", Stage: "building"}, now); err != nil {
		t.Fatal(err)
	}

	dst := regressionDB(t)
	// 恢复表清单由主库现有表驱动：真实运行实例已懒建这两张表，测试须先 ensure 复现。
	if err := ensureGoals(dst); err != nil {
		t.Fatal(err)
	}
	if err := ensureProjects(dst); err != nil {
		t.Fatal(err)
	}
	dc := regressionContact(t, dst, "将被替换的旧人")
	if _, err := CreateProject(dst, CreateProjectInput{ContactID: dc, Title: "旧项目-应被覆盖", Status: "active", Stage: "building"}, now); err != nil {
		t.Fatal(err)
	}

	zipPath, cleanup, err := BuildBackupZip(src, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := RestoreBackupZip(dst, zipPath, filepath.Dir(zipPath), false); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}

	// 核心用户表随备份往返
	if got := restoreCount(t, dst, `SELECT COUNT(*) FROM relationship_goals WHERE title='源目标-唯一标识'`); got != 1 {
		t.Fatalf("恢复后应带入源 goal，命中=%d", got)
	}
	if got := restoreCount(t, dst, `SELECT COUNT(*) FROM relationship_projects WHERE title='源项目-唯一标识'`); got != 1 {
		t.Fatalf("恢复后应带入源 project，命中=%d", got)
	}
	// 目标库原有项目应被整表替换掉
	if got := restoreCount(t, dst, `SELECT COUNT(*) FROM relationship_projects WHERE title='旧项目-应被覆盖'`); got != 0 {
		t.Fatalf("目标库原有 project 应被清除，残留=%d", got)
	}
	// 无悬空：project/goal 的 contact_id 必须真实存在于恢复后的 contacts
	if got := restoreCount(t, dst, `SELECT COUNT(*) FROM relationship_projects p WHERE NOT EXISTS (SELECT 1 FROM contacts c WHERE c.id=p.contact_id)`); got != 0 {
		t.Fatalf("存在悬空 project（串数据）: %d", got)
	}
	if got := restoreCount(t, dst, `SELECT COUNT(*) FROM relationship_goals g WHERE NOT EXISTS (SELECT 1 FROM contacts c WHERE c.id=g.contact_id)`); got != 0 {
		t.Fatalf("存在悬空 goal（串数据）: %d", got)
	}
	// 派生表 relationship_state 不入备份（restoreSkipTables）：恢复后主库不得从备份带入任何源状态行。
	// 目标库本就未建该派生表（未触发重建）即为正确；若存在则行数必须为 0。
	if tableExistsLocked(dst, "relationship_state") {
		if got := restoreCount(t, dst, `SELECT COUNT(*) FROM relationship_state`); got != 0 {
			t.Fatalf("派生表 relationship_state 不应随备份恢复带入数据，got=%d", got)
		}
	}
}

// —— 二十一：数据层治理元数据不得漂移 ——
func TestRegistryMetaForOS2Tables(t *testing.T) {
	cases := []struct {
		name          string
		wantBackup    bool
		wantRebuild   bool
		wantContactID bool
	}{
		{"relationship_projects", true, false, true}, // 用户创建 core，须备份、非派生、含 contact_id
		{"relationship_goals", true, false, true},    // 同上
		{"relationship_state", false, true, true},    // 派生，不入备份、可重建、含 contact_id
	}
	for _, c := range cases {
		meta := GetTableMeta(c.name)
		if meta == nil {
			t.Fatalf("%s 未在 registry 登记", c.name)
		}
		if meta.Backup != c.wantBackup || meta.Rebuild != c.wantRebuild || meta.HasContactID != c.wantContactID {
			t.Fatalf("%s 元数据漂移: Backup=%v Rebuild=%v HasContactID=%v，期望 %v/%v/%v",
				c.name, meta.Backup, meta.Rebuild, meta.HasContactID, c.wantBackup, c.wantRebuild, c.wantContactID)
		}
	}
	// projects/goals 是真实用户数据，绝不能被误列入派生表清单（否则恢复会清空）
	for _, derived := range derivedTables {
		if derived == "relationship_projects" || derived == "relationship_goals" {
			t.Fatalf("%s 是用户数据，不应出现在 derivedTables 中", derived)
		}
	}
}
