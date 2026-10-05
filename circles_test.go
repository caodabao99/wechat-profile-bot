package main

// v5.3.0 #6 关系圈层：纯函数 assignTier 边界阈值与频率组合；ComputeCircles 分层/排序确定性与幂等。

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestAssignTierBoundaries(t *testing.T) {
	cases := []struct {
		intim, days int
		want        string
	}{
		{75, 12, circleCore},      // 核心圈下界（且活跃达阈）
		{100, 12, circleCore},     // 核心
		{75, 11, circleIntimate},  // 亲密但活跃不足 → 亲密圈
		{74, 100, circleIntimate}, // 未达核心亲密度 → 亲密圈
		{40, 3, circleIntimate},   // 亲密圈下界
		{39, 20, circleSocial},    // 社交圈上界
		{15, 0, circleSocial},     // 社交圈下界
		{14, 30, circleWeak},      // 弱联系
		{0, 0, circleWeak},        // 空
	}
	for _, c := range cases {
		if got := assignTier(c.intim, c.days); got != c.want {
			t.Errorf("assignTier(%d,%d)=%s, want %s", c.intim, c.days, got, c.want)
		}
	}
}

func TestCircleRankOrder(t *testing.T) {
	if !(circleRank(circleCore) < circleRank(circleIntimate) &&
		circleRank(circleIntimate) < circleRank(circleSocial) &&
		circleRank(circleSocial) < circleRank(circleWeak)) {
		t.Error("圈层排序应为核心<亲密<社交<弱联系")
	}
	if circleRank("nonsense") != len(circleMetas) {
		t.Errorf("未知圈层键应落到末尾, got %d", circleRank("nonsense"))
	}
}

func TestComputeCirclesTiersFixedAndDeterministic(t *testing.T) {
	db := regressionAssistantDB(t)
	now := time.Date(2025, 6, 20, 12, 0, 0, 0, time.Local)
	// 高活跃高亲密 → 应能进较靠前圈层；纯孤立联系人 → 弱联系。
	core := regressionContact(t, db, "核心的人")
	for d := 0; d < 20; d++ {
		day := now.AddDate(0, 0, -d)
		if _, err := SaveMessages(db, core, []Message{
			{Sender: "me", Content: fmt.Sprintf("m%d", d), Timestamp: day},
			{Sender: "other", Content: fmt.Sprintf("o%d-long enough content here", d), Timestamp: day},
		}); err != nil {
			t.Fatal(err)
		}
	}
	weak := regressionContact(t, db, "几乎不说话的人")
	if _, err := SaveMessages(db, weak, []Message{{Sender: "me", Content: "在?", Timestamp: now.AddDate(0, 0, -60)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildDailyMetrics(db, 0); err != nil {
		t.Fatal(err)
	}

	first, err := ComputeCircles(db, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ComputeCircles(db, now)
	if err != nil {
		t.Fatal(err)
	}

	// tiers 固定 4 层、顺序稳定。
	if len(first.Tiers) != len(circleMetas) {
		t.Fatalf("tiers 应固定 %d 层, got %d", len(circleMetas), len(first.Tiers))
	}
	for i, tr := range first.Tiers {
		if tr.Key != circleMetas[i].Key {
			t.Errorf("tiers[%d] 顺序错: got %s want %s", i, tr.Key, circleMetas[i].Key)
		}
	}
	// 每层 count 求和 == 成员数。
	total := 0
	for _, tr := range first.Tiers {
		total += tr.Count
	}
	if total != len(first.Members) {
		t.Errorf("各层人数求和 %d ≠ 成员数 %d", total, len(first.Members))
	}
	// 排序不变量：圈层靠前 → 亲密度降序。
	lastRank, lastScore := -1, 1<<31
	for _, m := range first.Members {
		r := circleRank(m.Tier)
		if r < lastRank || (r == lastRank && m.Score > lastScore) {
			t.Errorf("members 排序违例: tier=%s score=%d", m.Tier, m.Score)
		}
		if r != lastRank {
			lastScore = 1 << 31
			lastRank = r
		}
		lastScore = m.Score
	}
	// 幂等。
	strip := func(d *CircleDashboard) string {
		cp := *d
		cp.GeneratedAt = ""
		b, _ := json.Marshal(cp)
		return string(b)
	}
	if strip(first) != strip(second) {
		t.Errorf("ComputeCircles 应确定性可复现")
	}
}
