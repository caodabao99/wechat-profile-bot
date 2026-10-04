package main

// 高阶洞察 · Phase D 主动人生简报（编排层）回归。
// 覆盖：buildBriefing 纯合成（缺层优雅跳过、Top-3 截断、优先级排序、确定性）、
//       只读编排不触发各层重算、整条流水线 ComputeAdvancedInsights + 缓存新鲜度。

import (
	"testing"
	"time"
)

func fixtureState() *LifeState {
	return &LifeState{
		Portfolio: LifePortfolio{ContactCount: 5, TotalWealth: 300, HighRiskCount: 1, QuarterDelta: -3},
		HighRisk: []LifeAsset{{
			ContactID: 11, Name: "阿伟", RiskLevel: "high", RiskScore: 0.8,
			Category: "朋友", DecayDays: 40,
		}},
		Time: TimeReflux{Insights: []string{"你把更多时间投给了工作"}},
	}
}

func fixtureNet() *NetworkInsight {
	return &NetworkInsight{
		NodeCount: 7, ClusterCount: 2, FragilityScore: 0.6,
		FragilityNote: "有 1 位关键人正处高风险断联",
		Bridges: []NetworkBridge{{
			ContactID: 21, Name: "桥桥", Articulation: true, Fragile: true, RiskLevel: "high",
			ConnectsClusters: []string{"家人圈", "同事圈"},
		}},
	}
}

func fixtureSelf() *SelfPortrait {
	return &SelfPortrait{
		InitiationRate: 80, OneWayCount: 3, ActiveContacts: 6,
		Migration: []string{"家人占比持续下降"},
	}
}

func TestBuildBriefingAllNilGraceful(t *testing.T) {
	now := time.Now()
	br := buildBriefing(now, nil, nil, nil, nil, nil)
	if br == nil {
		t.Fatal("不应返回 nil")
	}
	if len(br.TopActions) != 0 {
		t.Errorf("全缺层不该有行动, got %+v", br.TopActions)
	}
	if br.Headline == "" || br.HealthLine == "" {
		t.Errorf("应有兜底大标题/健康度行, headline=%q health=%q", br.Headline, br.HealthLine)
	}
}

func TestBuildBriefingPriorityOrderAndCap(t *testing.T) {
	now := time.Now()
	proj := &LifeProjection{
		HorizonDays: 90, BreakCount: 2,
		Projections: []ContactProjection{{
			ContactID: 31, Name: "老张", WillBreak: true, SufficientData: true, FutureBalance: 5,
		}},
	}
	learn := &InterventionInsight{TotalResolved: 30, BaseRate: 70, BestActions: []string{"破冰问候很有效"}}
	br := buildBriefing(now, fixtureState(), proj, fixtureNet(), fixtureSelf(), learn)

	if len(br.TopActions) == 0 || len(br.TopActions) > briefingMaxActions {
		t.Fatalf("行动数应在 1..%d, got %d: %+v", briefingMaxActions, len(br.TopActions), br.TopActions)
	}
	// 排序后优先级应非递减
	for i := 1; i < len(br.TopActions); i++ {
		if br.TopActions[i-1].Priority > br.TopActions[i].Priority {
			t.Errorf("未按优先级升序: %+v", br.TopActions)
		}
	}
	// 网络脆弱桥梁(优先级1)应排在最前
	if br.TopActions[0].Source != "network" {
		t.Errorf("最高优先级应来自 network, got %s: %+v", br.TopActions[0].Source, br.TopActions[0])
	}
	// 最强信号应汇聚多层
	if len(br.Signals) == 0 {
		t.Error("多层数据下应有最强信号")
	}
}

func TestBuildBriefingLifeActionWhenStatePresent(t *testing.T) {
	now := time.Now()
	br := buildBriefing(now, fixtureState(), nil, nil, nil, nil)
	var foundLife bool
	for _, a := range br.TopActions {
		if a.Source == "life" && a.Who == "阿伟" {
			foundLife = true
		}
	}
	if !foundLife {
		t.Errorf("高风险资产应产出 life 行动, got %+v", br.TopActions)
	}
	if br.HealthLine == "" {
		t.Error("有组合数据应产出健康度行")
	}
}

func TestBuildBriefingDeterministic(t *testing.T) {
	now := time.Now()
	proj := &LifeProjection{HorizonDays: 90, BreakCount: 1,
		Projections: []ContactProjection{{ContactID: 31, Name: "老张", WillBreak: true, SufficientData: true, FutureBalance: 5}}}
	learn := &InterventionInsight{TotalResolved: 30, BaseRate: 70, BestActions: []string{"破冰问候很有效"}}
	a := buildBriefing(now, fixtureState(), proj, fixtureNet(), fixtureSelf(), learn)
	b := buildBriefing(now, fixtureState(), proj, fixtureNet(), fixtureSelf(), learn)
	if a.Headline != b.Headline || len(a.TopActions) != len(b.TopActions) || len(a.Signals) != len(b.Signals) {
		t.Fatalf("同输入两次简报不一致")
	}
	for i := range a.TopActions {
		if a.TopActions[i] != b.TopActions[i] {
			t.Fatalf("第 %d 条行动不一致: %+v vs %+v", i, a.TopActions[i], b.TopActions[i])
		}
	}
}

func TestRiskLevelCn(t *testing.T) {
	if riskLevelCn("high") != "高风险" || riskLevelCn("mid") != "中风险" || riskLevelCn("low") != "低风险" {
		t.Error("风险中文映射不符")
	}
	if riskLevelCn("weird") != "weird" {
		t.Error("未知应原样返回")
	}
}

func TestGenerateBriefingIsReadOnlyNoRecompute(t *testing.T) {
	db := regressionAssistantDB(t)
	now := time.Now()
	// 直接只读编排：其它层缓存应保持为空（不触发重算）
	if err := GenerateBriefing(db, now); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := GetCachedNetwork(db); n != nil {
		t.Error("GenerateBriefing 不该触发网络层重算")
	}
	if sp, _, _ := GetCachedSelfPortrait(db); sp != nil {
		t.Error("GenerateBriefing 不该触发自我画像重算")
	}
	br, _, err := GetCachedBriefing(db)
	if err != nil || br == nil {
		t.Fatalf("简报缓存应写入: %v", err)
	}
}

func TestComputeAdvancedInsightsPipeline(t *testing.T) {
	db := regressionAssistantDB(t)
	cid := regressionContact(t, db, "流水线对象")
	now := time.Now()
	for d := 0; d < 6; d++ {
		day := now.AddDate(0, 0, -d)
		seedMessage(t, db, cid, "me", "在忙吗", uniqueHash("adv-me", d), day)
		seedMessage(t, db, cid, "other", "还好", uniqueHash("adv-other", d), day)
	}
	if err := ComputeAdvancedInsights(db, now); err != nil {
		t.Fatal(err)
	}
	// 五份缓存都应被写入
	for name, chk := range map[string]func() bool{
		"network":      func() bool { v, _, _ := GetCachedNetwork(db); return v != nil },
		"self":         func() bool { v, _, _ := GetCachedSelfPortrait(db); return v != nil },
		"intervention": func() bool { v, _, _ := GetCachedIntervention(db); return v != nil },
		"briefing":     func() bool { v, _, _ := GetCachedBriefing(db); return v != nil },
	} {
		if !chk() {
			t.Errorf("流水线后 %s 缓存应已生成", name)
		}
	}
	br, genAt, _ := GetCachedBriefing(db)
	if br == nil {
		t.Fatal("简报应存在")
	}
	if IsBriefingStale(genAt, now) || !IsBriefingStale(genAt, now.Add(8*24*time.Hour)) {
		t.Error("简报新鲜度判定不符")
	}
}

func uniqueHash(prefix string, i int) string {
	return prefix + "-" + string(rune('a'+i))
}
