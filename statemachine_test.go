package main

// Relationship State Machine（OS 2.0 Phase 3）专项测试。
// 覆盖：6.1 base/dynamic 阈值映射（纯函数）、6.2 仅跨阈值才产历史事件（幂等去重）、
// reconnecting 借状态记忆判定、单联系人/看板视图与历史。

import (
	"testing"
	"time"
)

func TestDeriveBaseState(t *testing.T) {
	cases := []struct {
		intimacy int
		trend    string
		want     string
	}{
		{0, "new", baseUnknown},
		{0, "dormant", baseIntroduced}, // 有历史但亲密度 0 → 至少 introduced（非 new 才不 unknown）
		{10, "stable", baseIntroduced},
		{15, "stable", baseFamiliar},
		{39, "warming", baseFamiliar},
		{40, "stable", baseStable},
		{60, "stable", baseClose},
		{80, "warming", baseCore},
		{100, "cooling", baseCore},
	}
	for _, c := range cases {
		if got := deriveBaseState(c.intimacy, c.trend); got != c.want {
			t.Errorf("deriveBaseState(%d,%q)=%s，期望 %s", c.intimacy, c.trend, got, c.want)
		}
	}
}

func TestDeriveDynamicStatePrecedence(t *testing.T) {
	cases := []struct {
		trend, alert, prev string
		want               string
	}{
		{"dormant", "none", "", dynDormant},
		{"dormant", "urgent", "", dynDormant},            // dormant 优先级最高
		{"warming", "none", dynDormant, dynReconnecting}, // 曾 dormant 现升温 → 重连
		{"stable", "none", dynDormant, dynReconnecting},  // 曾 dormant 现恢复互动 → 重连
		{"stable", "watching", "", dynAtRisk},            // 前瞻预警优先于 stable
		{"cooling", "urgent", "", dynAtRisk},             // urgent 覆盖 cooling
		{"cooling", "none", "", dynCooling},              //
		{"warming", "none", "", dynWarming},              //
		{"stable", "none", "", dynStable},                //
		{"new", "none", "", dynStable},                   // 无数据动态态回落 stable
		{"warming", "none", dynWarming, dynWarming},      // prev 非 dormant → 不判重连
	}
	for _, c := range cases {
		if got := deriveDynamicState(c.trend, c.alert, c.prev); got != c.want {
			t.Errorf("deriveDynamicState(%q,%q,%q)=%s，期望 %s", c.trend, c.alert, c.prev, got, c.want)
		}
	}
}

func TestRelationshipStateTransitionOnlyOnThreshold(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "状态对象")
	if err := SaveProfile(db, cid, `{"interests":["跑步"],"summary":"s"}`, "s", "i"); err != nil {
		t.Fatal(err)
	}
	regressionMessages(t, db, cid, "今天跑步", "周末跑步", "一起跑步")

	// 首次评估：每个联系人都产生一次“进入观测”变迁（prev 空 → base/dynamic）。
	changed, total, err := RefreshRelationshipStates(db, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if total < 1 {
		t.Fatalf("total 应 ≥1, got %d", total)
	}
	if changed < 1 {
		t.Fatalf("首次评估应产生状态写入, changed=%d", changed)
	}

	// 记录当前快照与 changed_at。
	var base, dyn, changedAt string
	if err := db.QueryRow(`SELECT base_state, dynamic_state, changed_at FROM relationship_state WHERE contact_id=?`, cid).
		Scan(&base, &dyn, &changedAt); err != nil {
		t.Fatal(err)
	}
	var hist1 int
	if err := db.QueryRow(`SELECT COUNT(*) FROM relationship_state_history WHERE contact_id=?`, cid).Scan(&hist1); err != nil {
		t.Fatal(err)
	}

	// 立即再评估一次：输入未跨越阈值 → changed=0、不新增历史、changed_at 不变（规格 6.2）。
	changed2, _, err := RefreshRelationshipStates(db, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if changed2 != 0 {
		t.Fatalf("二次刷新不应跨越阈值, changed=%d", changed2)
	}
	var hist2 int
	if err := db.QueryRow(`SELECT COUNT(*) FROM relationship_state_history WHERE contact_id=?`, cid).Scan(&hist2); err != nil {
		t.Fatal(err)
	}
	if hist2 != hist1 {
		t.Errorf("二次刷新不应新增历史事件: %d → %d", hist1, hist2)
	}
	var changedAt2 string
	if err := db.QueryRow(`SELECT changed_at FROM relationship_state WHERE contact_id=?`, cid).Scan(&changedAt2); err != nil {
		t.Fatal(err)
	}
	if changedAt2 != changedAt {
		t.Errorf("未跨阈值时 changed_at 不应前进: %q → %q", changedAt, changedAt2)
	}
}

func TestRelationshipStateReconnecting(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "重连对象")
	if err := SaveProfile(db, cid, `{"interests":["咖啡"],"summary":"s"}`, "s", "i"); err != nil {
		t.Fatal(err)
	}
	// 近期有互动 → 趋势 stable/warming（非 dormant）。regressionMessages 时间戳较旧会判 dormant，
	// 故补插一条“刚刚”的消息把 DaysSinceLast 压到 0，确保进入非 dormant 分支。
	if _, err := SaveMessages(db, cid, []Message{{Sender: "other", Content: "最近也喝咖啡", Timestamp: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RefreshRelationshipStates(db, time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	// 伪造“上一动态态曾为 dormant” → 再评估应据状态记忆判为 reconnecting。
	if _, err := db.Exec(`UPDATE relationship_state SET dynamic_state='dormant' WHERE contact_id=?`, cid); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RefreshRelationshipStates(db, time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	var dyn string
	if err := db.QueryRow(`SELECT dynamic_state FROM relationship_state WHERE contact_id=?`, cid).Scan(&dyn); err != nil {
		t.Fatal(err)
	}
	if dyn != dynReconnecting {
		t.Fatalf("曾沉寂后恢复互动应判 reconnecting, got %s", dyn)
	}
	// 历史里应有一条 dormant → reconnecting 的变迁。
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM relationship_state_history WHERE contact_id=? AND prev_dynamic='dormant' AND new_dynamic='reconnecting'`, cid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Error("应记录一条 dormant→reconnecting 的变迁事件")
	}
}

func TestRelationshipStateViews(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "视图对象")
	if err := SaveProfile(db, cid, `{"basic_info":{"location":"深圳"}}`, "s", "i"); err != nil {
		t.Fatal(err)
	}
	regressionMessages(t, db, cid, "在深圳工作", "深圳下雨了")

	v, err := GetRelationshipState(db, cid)
	if err != nil {
		t.Fatal(err)
	}
	if v.ContactID != cid || v.Name != "视图对象" {
		t.Errorf("视图身份不符: %+v", v)
	}
	if v.BaseState == "" || v.DynamicState == "" {
		t.Errorf("视图状态字段不应为空: %+v", v)
	}

	list, err := ListRelationshipStates(db)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, s := range list {
		if s.ContactID == cid {
			found = true
		}
	}
	if !found {
		t.Error("看板应含该联系人")
	}
}
