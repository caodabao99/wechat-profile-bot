package main

// 蓝图 Phase 15「完整回归」+ §31 场景 A 的核心闭环钉。
//
// 各 Phase 的分段测试已覆盖每一环（关系会话 / 行动账本 / 观察 / 策略学习 / 决策集成），
// 但没有一个测试证明「用户主流程闭环」真的贯通——§33 明令「不能用代码存在代替功能闭环」。
//
// 本回归用真实 DB、零 LLM，走一遍 PERCEIVE→…→ACT→OBSERVE→LEARN→下一次推荐改变：
//   ACT      LogAction（经 seedActionOutcome 落统一行动账本）
//   OBSERVE  SetActionOutcome（写入已确认正向结果，等价 7/14/30 天观察后的用户确认）
//   LEARN    ComputeStrategyHistory（只读聚合出有界软加分）
//   推荐      annotateStrategyScore（下一次同类候选的 strategy_score 因学习而上移）
//
// 并钉死 §32 铁律：学习是「有界软信号」，绝不翻转确定性 Priority、绝不宣称因果。

import (
	"testing"
	"time"
)

func TestRegressionV7ScenarioAClosedLoop(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()
	cid := regressionContact(t, db, "闭环老王")

	// —— 基线：尚无策略历史时，推荐评分必须纯确定性（score==priority，无软说明）。——
	base := []DecisionCandidate{{ContactID: cid, Priority: 60, ReasonCodes: []string{reasonReconnecting}}}
	annotateStrategyScore(base, nil)
	if base[0].StrategyType != StrategyLowPressureCheckin {
		t.Fatalf("低压力问候候选应归类为 %q，实得 %q", StrategyLowPressureCheckin, base[0].StrategyType)
	}
	if base[0].StrategyScore != base[0].Priority || base[0].StrategyNote != "" {
		t.Fatalf("无历史时不应有任何策略偏移：score=%d note=%q", base[0].StrategyScore, base[0].StrategyNote)
	}

	// —— ACT→OBSERVE：为该联系人写入足量「已确认正向」的低压力问候行动。——
	for i := 0; i < interventionMinSample+1; i++ {
		seedActionOutcome(t, db, cid, reasonReconnecting, ActionOutcomePositive, ActionProvenanceConfirmed, now)
	}

	// —— LEARN：只读聚合出可参考的策略历史。——
	hist, err := ComputeStrategyHistory(db, now)
	if err != nil {
		t.Fatalf("ComputeStrategyHistory: %v", err)
	}
	var stat *StrategyStat
	for i := range hist.Stats {
		if hist.Stats[i].StrategyType == StrategyLowPressureCheckin {
			stat = &hist.Stats[i]
		}
	}
	if stat == nil || stat.LowSample || stat.Positive != interventionMinSample+1 {
		t.Fatalf("应聚合出足量正向低压力问候历史，实得 %+v", hist.Stats)
	}

	// —— 下一次推荐因学习而改变（闭环达成）。——
	next := []DecisionCandidate{{ContactID: cid, Priority: 60, ReasonCodes: []string{reasonReconnecting}}}
	annotateStrategyScore(next, hist.Stats)

	if next[0].Priority != 60 {
		t.Fatalf("§32 学习绝不改动确定性 Priority，实得 %d", next[0].Priority)
	}
	delta := next[0].StrategyScore - next[0].Priority
	if delta <= 0 {
		t.Fatalf("正向历史应使 strategy_score 上移，实得 +%d（score=%d）", delta, next[0].StrategyScore)
	}
	if delta > strategyScoreMaxBonus {
		t.Fatalf("§32 软加分必须有界，实得 +%d > 上限 %d", delta, strategyScoreMaxBonus)
	}
	if next[0].StrategyNote == "" {
		t.Fatal("学习后的推荐应附带可解释的历史表现说明")
	}
}
