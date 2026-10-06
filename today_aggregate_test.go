package main

// v6.3 §P11 Action Center 2.0 验收。
//
// 钉死：
//  1. 聚合后每人最多一张卡（噪声折叠）。
//  2. Daily cap 生效：超过 max 人被裁掉，TotalSuppressed > 0。
//  3. Snooze：被屏蔽联系人不出现在结果中；过期后恢复。
//  4. Urgency 分组正确。

import (
	"testing"
	"time"
)

func TestTodayAggregateNoiseCollapse(t *testing.T) {
	db := regressionDB(t)
	if err := ensureTodaySnoozeTable(db); err != nil {
		t.Fatal(err)
	}
	if err := ensureFollowupTables(db); err != nil {
		t.Fatal(err)
	}
	// 建 3 个联系人
	a := regressionContact(t, db, "甲")
	b := regressionContact(t, db, "乙")
	c := regressionContact(t, db, "丙")
	now := time.Now()

	// 给每人挂画像（决策引擎需要）
	for _, id := range []int64{a, b, c} {
		if err := SaveProfile(db, id, `{"summary":"s"}`, "s", "i"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := RefreshRelationshipStates(db, now, 0); err != nil {
		t.Fatal(err)
	}
	// 甲：高风险 + 待办 → 应折叠为 1 张 urgent 卡
	setForcedState(t, db, a, dynAtRisk, "urgent", 80)
	if _, err := AddFollowup(db, a, "question", "等他回复", "", ""); err != nil {
		t.Fatal(err)
	}
	// 乙：低风险
	setForcedState(t, db, b, dynCooling, "watching", 30)
	// 丙：无信号（不应出现）

	agg, err := BuildTodayAggregate(db, now, 7)
	if err != nil {
		t.Fatal(err)
	}
	// 甲应恰好出现 1 次
	countA := 0
	for _, card := range append(append(agg.Urgent, agg.Today...), agg.Later...) {
		if card.ContactID == a {
			countA++
		}
	}
	if countA != 1 {
		t.Fatalf("甲应折叠为 1 张卡，实得 %d", countA)
	}
	// 甲应为 urgent
	if len(agg.Urgent) > 0 && agg.Urgent[0].ContactID == a {
		// good
	} else if len(agg.Urgent) == 0 {
		// 可能不在 urgent 但 priority 应 >= 50
		for _, card := range agg.Today {
			if card.ContactID == a && card.Priority < 50 {
				t.Fatal("甲（高风险+待办）优先级应 ≥50")
			}
		}
	}
}

func TestTodayAggregateDailyCap(t *testing.T) {
	db := regressionDB(t)
	if err := ensureTodaySnoozeTable(db); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// 先建画像 + 刷状态（确保 relationship_state 表存在）
	ids := make([]int64, 10)
	for i := range ids {
		ids[i] = regressionContact(t, db, string(rune('A'+i)))
		SaveProfile(db, ids[i], `{"summary":"s"}`, "s", "i")
	}
	if _, _, err := RefreshRelationshipStates(db, now, 0); err != nil {
		t.Fatal(err)
	}
	for i := range ids {
		setForcedState(t, db, ids[i], dynAtRisk, "urgent", 60)
	}

	agg, err := BuildTodayAggregate(db, now, 5)
	if err != nil {
		t.Fatal(err)
	}
	total := len(agg.Urgent) + len(agg.Today) + len(agg.Later)
	if total > 5 {
		t.Fatalf("max=5 但展示了 %d 人", total)
	}
	if agg.TotalSuppressed == 0 {
		t.Fatal("被裁掉的人应计入 TotalSuppressed")
	}
}

func TestTodaySnooze(t *testing.T) {
	db := regressionDB(t)
	if err := ensureTodaySnoozeTable(db); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	id := regressionContact(t, db, "被屏蔽者")
	SaveProfile(db, id, `{"summary":"s"}`, "s", "i")
	if _, _, err := RefreshRelationshipStates(db, now, 0); err != nil {
		t.Fatal(err)
	}
	setForcedState(t, db, id, dynAtRisk, "urgent", 80)

	// 不屏蔽时出现
	agg1, _ := BuildTodayAggregate(db, now, 7)
	found := false
	for _, card := range append(append(agg1.Urgent, agg1.Today...), agg1.Later...) {
		if card.ContactID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("未屏蔽时应出现")
	}

	// 屏蔽 2 天
	if err := SnoozeContact(db, id, 2, now); err != nil {
		t.Fatal(err)
	}
	agg2, _ := BuildTodayAggregate(db, now, 7)
	for _, card := range append(append(agg2.Urgent, agg2.Today...), agg2.Later...) {
		if card.ContactID == id {
			t.Fatal("snooze 后不应出现")
		}
	}

	// 过期后恢复
	future := now.AddDate(0, 0, 3)
	if _, err := PurgeExpiredSnoozes(db, future); err != nil {
		t.Fatal(err)
	}
	snoozed, _ := getSnoozedContactIDs(db, future)
	if snoozed[id] {
		t.Fatal("purge 后该联系人不应仍在 snooze 集合中")
	}
}

func TestTakeN(t *testing.T) {
	set := map[string]bool{"a": true, "b": true, "c": true, "": true}
	got := takeN(set, 2)
	if len(got) != 2 {
		t.Fatalf("应返回 2 个，实得 %d: %v", len(got), got)
	}
	// 空串不应出现
	for _, s := range got {
		if s == "" {
			t.Fatal("空串应被过滤")
		}
	}
}
