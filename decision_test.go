package main

// Decision Engine（OS 2.0 Phase 4，规格第七章）专项测试。
// 覆盖：7.2 确定性打分（纯函数、可复现、不依赖 LLM）、九种 reason_codes 的触发、
// Top-N 排序与去噪（无紧迫信号者不入决策）、Today 视图补最佳触达时段。

import (
	"database/sql"
	"testing"
	"time"
)

func TestScoreDecisionDeterministic(t *testing.T) {
	// 同输入必同结果（可复现，规格 7.2「不要直接让 LLM 排序」）。
	codes := []string{reasonHighRisk, reasonOpenFollowup}
	a := scoreDecision(50, "urgent", codes)
	b := scoreDecision(50, "urgent", codes)
	if a != b {
		t.Fatalf("打分不确定: %d != %d", a, b)
	}
	// 理由码越多分越高（紧迫度累加）。
	if scoreDecision(50, "none", []string{reasonCooling, reasonImportantDate}) <=
		scoreDecision(50, "none", []string{reasonCooling}) {
		t.Error("叠加紧迫理由码应提升优先级")
	}
	// 风险高于「升温」这类正向变化：urgent 场景应显著抬分。
	if scoreDecision(50, "urgent", nil) <= scoreDecision(50, "none", nil) {
		t.Error("urgent 风险应贡献额外优先级")
	}
	// 重复码只计一次（去重防刷分）。
	if scoreDecision(0, "none", []string{reasonCooling, reasonCooling}) !=
		scoreDecision(0, "none", []string{reasonCooling}) {
		t.Error("重复理由码不应重复计分")
	}
}

func TestDecisionReasonWeightOrdering(t *testing.T) {
	// 权重次序体现「现在」的紧迫度：风险 > 重要日子 > 目标 > 待办 > 降温 > 重连 > 机会 > 升温 > 话题。
	seq := []string{reasonHighRisk, reasonImportantDate, reasonGoalDeadline, reasonOpenFollowup,
		reasonCooling, reasonReconnecting, reasonStrongOpportunity, reasonRecentPositiveChange, reasonUnresolvedTopic}
	for i := 0; i+1 < len(seq); i++ {
		if decisionReasonWeight(seq[i]) <= decisionReasonWeight(seq[i+1]) {
			t.Errorf("权重次序违反: %s(%d) 应 > %s(%d)",
				seq[i], decisionReasonWeight(seq[i]), seq[i+1], decisionReasonWeight(seq[i+1]))
		}
	}
	if decisionReasonWeight("unknown_code") != 0 {
		t.Error("未知理由码不应贡献分数")
	}
}

// setForcedState 把某联系人已持久化的状态改写为期望值（仅 UPDATE，不重刷），
// 供决策按确定性输入判定。调用前须先做一次 RefreshRelationshipStates 铺底。
func setForcedState(t *testing.T, db *sql.DB, cid int64, dyn, alert string, intimacy int) {
	t.Helper()
	if _, err := db.Exec(`UPDATE relationship_state SET dynamic_state=?, alert=?, intimacy=? WHERE contact_id=?`,
		dyn, alert, intimacy, cid); err != nil {
		t.Fatal(err)
	}
}

func TestBuildDecisionCandidatesCodesAndDenoise(t *testing.T) {
	db := regressionDB(t)
	if err := ensureFollowupTables(db); err != nil {
		t.Fatal(err)
	}
	cidRisk := regressionContact(t, db, "风险人")
	cidQuiet := regressionContact(t, db, "无信号人")
	cidOpp := regressionContact(t, db, "机会人")
	for _, c := range []int64{cidRisk, cidQuiet, cidOpp} {
		if err := SaveProfile(db, c, `{"interests":["跑步"],"summary":"s"}`, "s", "i"); err != nil {
			t.Fatal(err)
		}
	}

	// 先铺底一次（表空时 ListRelationshipStates 会自愈整刷），再分别固定各自状态。
	// 注意：多次整刷会用派生值覆盖前一次强制值，故铺底只一次、后续仅 UPDATE。
	if _, _, err := RefreshRelationshipStates(db, time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	setForcedState(t, db, cidRisk, dynAtRisk, "urgent", 50)
	setForcedState(t, db, cidQuiet, dynStable, "none", 10) // 低亲密 + stable + 无信号 → 应被去噪剔除
	setForcedState(t, db, cidOpp, dynStable, "none", 80)   // 高亲密 stable → strong_opportunity

	// 风险人挂一个未闭合待办 + 一个临期目标。
	if _, err := AddFollowup(db, cidRisk, "promise", "答应还书", "", ""); err != nil {
		t.Fatal(err)
	}
	pe := time.Now().Add(5 * 24 * time.Hour).Format("2006-01-02")
	if _, err := CreateGoal(db, CreateGoalInput{ContactID: cidRisk, Title: "每周联系一次", TargetCount: 4, PeriodEnd: pe}, time.Now()); err != nil {
		t.Fatal(err)
	}

	cands, err := BuildDecisionCandidates(db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]DecisionCandidate{}
	for _, c := range cands {
		byID[c.ContactID] = c
	}

	// 风险人：应同时命中 high_risk / cooling / open_followup / goal_deadline。
	r := byID[cidRisk]
	if r.ContactID != cidRisk {
		t.Fatal("风险人应进入决策")
	}
	for _, want := range []string{reasonHighRisk, reasonCooling, reasonOpenFollowup, reasonGoalDeadline} {
		if !hasCode(r.ReasonCodes, want) {
			t.Errorf("风险人缺少理由码 %s, got %v", want, r.ReasonCodes)
		}
	}
	if r.Risk != 2 {
		t.Errorf("urgent 应映射风险档 2, got %d", r.Risk)
	}
	if r.Priority != scoreDecision(50, "urgent", r.ReasonCodes) {
		t.Errorf("优先级应等于确定性打分, got %d want %d", r.Priority, scoreDecision(50, "urgent", r.ReasonCodes))
	}
	if r.Followup == "" || r.Goal == "" {
		t.Errorf("决策卡应带相关待办/目标明细: %+v", r)
	}

	// 机会人：命中 strong_opportunity，机会档=1。
	o := byID[cidOpp]
	if o.ContactID != cidOpp || !hasCode(o.ReasonCodes, reasonStrongOpportunity) {
		t.Errorf("高亲密 stable 应命中 strong_opportunity, got %+v", o)
	}
	if o.Opportunity != 1 {
		t.Errorf("strong_opportunity 应置机会档 1, got %d", o.Opportunity)
	}

	// 无信号人：被去噪，绝不进入决策（系统只呈现值得行动的人）。
	if _, ok := byID[cidQuiet]; ok {
		t.Error("无任何紧迫信号的低亲密联系人不应进入决策（去噪）")
	}

	// 确定性排序：优先级降序。
	for i := 0; i+1 < len(cands); i++ {
		if cands[i].Priority < cands[i+1].Priority {
			t.Fatalf("候选应按 priority 降序, %d < %d", cands[i].Priority, cands[i+1].Priority)
		}
	}
}

func TestTodayDecisionsTopNAndBestTime(t *testing.T) {
	db := regressionDB(t)
	if err := ensureFollowupTables(db); err != nil {
		t.Fatal(err)
	}
	a := regressionContact(t, db, "决策甲")
	b := regressionContact(t, db, "决策乙")
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
	// 甲再挂待办，确保其严格高于乙、稳居 Top1。
	if _, err := AddFollowup(db, a, "question", "等他回复方案", "", ""); err != nil {
		t.Fatal(err)
	}

	top, err := TodayDecisions(db, time.Now(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 1 {
		t.Fatalf("top=1 应只返回 1 条, got %d", len(top))
	}
	if top[0].ContactID != a {
		t.Errorf("Top1 应为信号更强的甲, got %d", top[0].ContactID)
	}

	// 非法 top（<=0）回落默认 3，不应报错。
	if all, err := TodayDecisions(db, time.Now(), 0); err != nil || len(all) < 1 {
		t.Errorf("top<=0 应回落默认并返回结果, len=%d err=%v", len(all), err)
	}
}

func TestPeakHourLabel(t *testing.T) {
	if got := peakHourLabel(map[int]int{}); got != "" {
		t.Errorf("空直方应返回空串, got %q", got)
	}
	got := peakHourLabel(map[int]int{9: 1, 21: 7, 13: 3})
	if got == "" || got[0:2] != "21" {
		t.Errorf("应取峰值小时 21, got %q", got)
	}
}
