package main

// AI Context Engine（OS 2.0 Phase 6，规格第九章）专项测试。
// 覆盖：9.3 任务预算差异化、9.2 分层聚合各块、相关消息(FTS)臂、渲染 token 预算截断、
// 缺联系人 ErrNoRows、空数据渲染不 panic（优雅降级）。

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBudgetForDistinctPerTask(t *testing.T) {
	ask := budgetFor(TaskAsk)
	sim := budgetFor(TaskSimulation)
	dec := budgetFor(TaskDecision)
	// 各任务预算必须不同（杜绝「所有任务都送 100 条」）。
	if ask.MaxRelevant <= sim.MaxRelevant/2 && ask.MaxRelevant <= dec.MaxRelevant {
		t.Error("Ask 应相关消息优先（MaxRelevant 更大）")
	}
	if sim.MaxMessages <= dec.MaxMessages {
		t.Errorf("Simulation 最近消息应多于 Decision, got %d vs %d", sim.MaxMessages, dec.MaxMessages)
	}
	if dec.MaxMessages > 10 {
		t.Errorf("Decision 应轻量近期消息, got %d", dec.MaxMessages)
	}
	// 未知任务回落默认（非零）。
	if budgetFor(ContextTask("weird")).MaxTokens <= 0 {
		t.Error("未知任务应回落有效默认预算")
	}
}

func TestBuildContactContextSections(t *testing.T) {
	db := regressionDB(t)
	if err := ensureFollowupTables(db); err != nil {
		t.Fatal(err)
	}
	cid := regressionContact(t, db, "张医生")
	if err := SaveProfile(db, cid, `{"basic_info":{"occupation":"医生","location":"广州"},"interests":["潜水"],"summary":"爱运动"}`, "s", "i"); err != nil {
		t.Fatal(err)
	}
	regressionMessages(t, db, cid, "最近去潜水了", "广州天气热")
	// 挂一个项目 + 一个待办，验证相应块被聚合。
	if _, err := CreateProject(db, CreateProjectInput{ContactID: cid, Title: "合作计划", NextAction: "发方案"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := AddFollowup(db, cid, "question", "等他回复时间", "", ""); err != nil {
		t.Fatal(err)
	}

	cc, err := BuildContactContext(db, cid, TaskAsk, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if cc.ContactID != cid || cc.Identity.Name != "张医生" {
		t.Errorf("身份块不符: %+v", cc.Identity)
	}
	if cc.CurrentRelationshipState == nil {
		t.Error("应含当前关系状态（GetRelationshipState 自愈刷新）")
	}
	if cc.Metrics == nil {
		t.Error("应含互动指标")
	}
	if len(cc.RecentMessages) == 0 {
		t.Error("应含最近消息")
	}
	if len(cc.Projects) != 1 || cc.Projects[0].Title != "合作计划" {
		t.Errorf("应聚合该项目, got %+v", cc.Projects)
	}
	if len(cc.OpenFollowups) != 1 {
		t.Errorf("应聚合该未闭合待办, got %d", len(cc.OpenFollowups))
	}
	// 事实块：保存画像应产出可信事实（或至少非 nil 切片，优雅降级）。
	if cc.TrustedFacts == nil || cc.ConflictingFacts == nil {
		t.Error("事实切片应初始化为非 nil（即便为空）")
	}
	// 预算随任务落地。
	if cc.Budget.MaxRelevant != budgetFor(TaskAsk).MaxRelevant {
		t.Errorf("预算应按任务设置, got %+v", cc.Budget)
	}
}

func TestBuildContactContextRelevantMessages(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "签证人")
	if err := SaveProfile(db, cid, `{"summary":"s"}`, "s", "i"); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveMessages(db, cid, []Message{{Sender: "other", Content: "我的签证办好了", Timestamp: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	cc, err := BuildContactContext(db, cid, TaskAsk, "签证", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(cc.RelevantMessages) == 0 {
		t.Error("带 query 时应有 FTS/LIKE 相关消息命中")
	}
}

func TestBuildContactContextMissingContact(t *testing.T) {
	db := regressionDB(t)
	if _, err := BuildContactContext(db, 999999, TaskAsk, "", time.Now()); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("不存在联系人应 ErrNoRows, got %v", err)
	}
}

func TestRenderContextTextBudgetAndNoPanic(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "渲染人")
	if err := SaveProfile(db, cid, `{"basic_info":{"occupation":"工程师"},"summary":"话很多的一段摘要，用来测试渲染截断行为是否按 token 预算生效，这里填充足够多的中文内容以逼近预算上限重复重复重复"}`, "s", "i"); err != nil {
		t.Fatal(err)
	}
	regressionMessages(t, db, cid, "你好", "在忙吗")

	// Decision（小预算）：渲染应含分节标题，且不 panic。
	cc, err := BuildContactContext(db, cid, TaskDecision, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	text := RenderContextText(cc)
	if !strings.Contains(text, "## ") {
		t.Errorf("渲染应含 markdown 分节, got:\n%s", text)
	}

	// 极小预算强制截断：Truncated 置真、结果被裁剪。
	cc2, err := BuildContactContext(db, cid, TaskNarrative, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cc2.Budget.MaxTokens = 20 // 远小于内容量
	text2 := RenderContextText(cc2)
	if !cc2.Truncated {
		t.Error("极小预算应触发截断标记")
	}
	if contextTokenApprox(text2) > 60 { // 20*2 rune 头部 + 截断提示，粗略上界
		t.Errorf("截断后不应远超预算, approx=%d", contextTokenApprox(text2))
	}
}

func TestRenderContextTextNilSafe(t *testing.T) {
	if RenderContextText(nil) != "" {
		t.Error("nil 上下文应返回空串而非 panic")
	}
}
