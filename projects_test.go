package main

// Relationship Projects（OS 2.0 Phase 5，规格第八章）专项测试。
// 覆盖：CRUD、status/stage 校验与默认、PATCH 局部更新语义、completed 迁移补时间戳、
// 列表过滤与排序、删联系人级联清理（数据层治理一致性）。

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestProjectCreateValidationAndDefaults(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "王总")

	// 标题必填 → errProjectBadInput。
	if _, err := CreateProject(db, CreateProjectInput{ContactID: cid}, time.Now()); !errors.Is(err, errProjectBadInput) {
		t.Fatalf("空标题应报参数不合法, got %v", err)
	}
	// 联系人不存在 → sql.ErrNoRows。
	if _, err := CreateProject(db, CreateProjectInput{ContactID: 999999, Title: "x"}, time.Now()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("不存在联系人应报 ErrNoRows, got %v", err)
	}
	// 非法 status / stage。
	if _, err := CreateProject(db, CreateProjectInput{ContactID: cid, Title: "深圳项目", Status: "bogus"}, time.Now()); !errors.Is(err, errProjectBadInput) {
		t.Errorf("非法 status 应报错, got %v", err)
	}
	if _, err := CreateProject(db, CreateProjectInput{ContactID: cid, Title: "深圳项目", Stage: "bogus"}, time.Now()); !errors.Is(err, errProjectBadInput) {
		t.Errorf("非法 stage 应报错, got %v", err)
	}

	// 正常新建：默认 status=active / stage=discovery。
	id, err := CreateProject(db, CreateProjectInput{
		ContactID: cid, Title: "深圳项目", Description: "推进合作", TargetDate: "2026-10-08", NextAction: "跟进报价",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	v, err := GetProject(db, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != projectStatusActive || v.Stage != projectStageDiscovery {
		t.Errorf("默认应为 active/discovery, got %s/%s", v.Status, v.Stage)
	}
	if v.Name != "王总" {
		t.Errorf("视图应带联系人名, got %q", v.Name)
	}
	if v.CompletedAt != "" {
		t.Errorf("非 completed 项目 completed_at 应为空, got %q", v.CompletedAt)
	}
}

func TestProjectUpdatePatchAndCompletedAt(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "李经理")
	id, err := CreateProject(db, CreateProjectInput{ContactID: cid, Title: "续约", Stage: projectStageNegotiating}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	// PATCH：只改 stage，title 保持。
	newStage := projectStageClosing
	v, err := UpdateProject(db, id, UpdateProjectInput{Stage: &newStage}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if v.Stage != projectStageClosing || v.Title != "续约" {
		t.Errorf("PATCH 只应改 stage, got stage=%s title=%s", v.Stage, v.Title)
	}

	// 迁入 completed → 补 completed_at；再迁回 active → 清空。
	done := projectStatusCompleted
	v, err = UpdateProject(db, id, UpdateProjectInput{Status: &done}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if v.CompletedAt == "" {
		t.Error("迁入 completed 应写 completed_at")
	}
	act := projectStatusActive
	v, err = UpdateProject(db, id, UpdateProjectInput{Status: &act}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if v.CompletedAt != "" {
		t.Errorf("迁出 completed 应清空 completed_at, got %q", v.CompletedAt)
	}

	// 空标题覆写非法；不存在的项目 ErrNoRows。
	empty := "  "
	if _, err := UpdateProject(db, id, UpdateProjectInput{Title: &empty}, time.Now()); !errors.Is(err, errProjectBadInput) {
		t.Errorf("空标题覆写应报错, got %v", err)
	}
	if _, err := UpdateProject(db, 999999, UpdateProjectInput{Title: &[]string{"x"}[0]}, time.Now()); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("不存在项目应 ErrNoRows, got %v", err)
	}
}

func TestProjectListFilterAndOrder(t *testing.T) {
	db := regressionDB(t)
	a := regressionContact(t, db, "甲")
	b := regressionContact(t, db, "乙")
	// 甲两条（一 active 高优、一 paused）、乙一条 active。
	if _, err := CreateProject(db, CreateProjectInput{ContactID: a, Title: "甲-高优", Priority: 5}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateProject(db, CreateProjectInput{ContactID: a, Title: "甲-暂停", Status: projectStatusPaused}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateProject(db, CreateProjectInput{ContactID: b, Title: "乙-活跃"}, time.Now()); err != nil {
		t.Fatal(err)
	}

	all, err := ListProjects(db, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("全部应 3 条, got %d", len(all))
	}
	// 优先级降序 → 首条应为甲-高优。
	if all[0].Title != "甲-高优" {
		t.Errorf("应按 priority 降序, 首条 got %s", all[0].Title)
	}

	onlyA, err := ListProjects(db, a, "")
	if err != nil || len(onlyA) != 2 {
		t.Fatalf("按联系人过滤应 2 条, got %d err=%v", len(onlyA), err)
	}

	paused, err := ListProjects(db, 0, projectStatusPaused)
	if err != nil || len(paused) != 1 {
		t.Fatalf("status=paused 应 1 条, got %d err=%v", len(paused), err)
	}

	openList, err := ListProjects(db, 0, "open")
	if err != nil {
		t.Fatal(err)
	}
	if len(openList) != 3 { // active+paused 全含（无 completed/cancelled）
		t.Errorf("status=open 应含 active+paused 共 3 条, got %d", len(openList))
	}

	if _, err := ListProjects(db, 0, "bogus"); !errors.Is(err, errProjectBadInput) {
		t.Errorf("非法 status 过滤应报错, got %v", err)
	}
}

func TestProjectDeleteAndContactCascade(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "待删人")
	id, err := CreateProject(db, CreateProjectInput{ContactID: cid, Title: "项目"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// 删除单条。
	ok, err := DeleteProject(db, id)
	if err != nil || !ok {
		t.Fatalf("删除应成功, ok=%v err=%v", ok, err)
	}
	if ok, _ := DeleteProject(db, id); ok {
		t.Error("重复删除应幂等返回 false")
	}

	// 数据层治理：contactCleanupTables 须含本表（联系人删除时级联清理）。
	var inCleanup bool
	for _, name := range contactCleanupTables {
		if name == "relationship_projects" {
			inCleanup = true
		}
	}
	if !inCleanup {
		t.Error("relationship_projects 必须在 contactCleanupTables 中（HasContactID 级联清理）")
	}

	// 再建一条挂该联系人，删联系人后按 contact_id 清干净。
	if _, err := CreateProject(db, CreateProjectInput{ContactID: cid, Title: "项目2"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM relationship_projects WHERE contact_id=?`, cid); err != nil {
		t.Fatal(err)
	}
	left, err := ListProjects(db, cid, "")
	if err != nil || len(left) != 0 {
		t.Errorf("按联系人清理后应无残留, got %d err=%v", len(left), err)
	}
}
