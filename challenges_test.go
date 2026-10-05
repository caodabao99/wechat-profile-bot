package main

// v5.3.0 #8 维护挑战/游戏化：levelForXP/challengeTitle 纯函数；BuildChallenges 幂等
//   （跨周去重、完成检测随本周我方互动置 achieved、XP 不重复累加）。

import (
	"testing"
	"time"
)

func TestLevelForXP(t *testing.T) {
	cases := map[int]int{0: 1, 99: 1, 100: 2, 199: 2, 250: 3, -10: 1}
	for xp, want := range cases {
		if got := levelForXP(xp); got != want {
			t.Errorf("levelForXP(%d)=%d, want %d", xp, got, want)
		}
	}
}

func TestChallengeTitleKnown(t *testing.T) {
	if challengeTitle(chalReachDormant) == "" || challengeTitle(chalReplyPending) == "" || challengeTitle(chalGreetCore) == "" {
		t.Error("三种已知挑战类型都应有中文标题")
	}
	if challengeTitle("bogus") == "" {
		t.Error("未知类型也应有兜底标题")
	}
}

func TestBuildChallengesDeterministicAndIdempotent(t *testing.T) {
	db := regressionAssistantDB(t)
	if err := ensureTimelineTables(db); err != nil { // RecordContactEvent 落时间线需时表就绪
		t.Fatal(err)
	}
	now := time.Now()

	// 一个近期高频互动的联系人（我方本周也发过言）→ 大概率成为核心圈挑战靶点并可本周达成。
	id := regressionContact(t, db, "常联系的人")
	for d := 0; d < 10; d++ {
		day := now.AddDate(0, 0, -d)
		if _, err := SaveMessages(db, id, []Message{
			{Sender: "me", Content: "在忙吗", Timestamp: day},
			{Sender: "other", Content: "还好还好呢", Timestamp: day},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RebuildDailyMetrics(db, 0); err != nil {
		t.Fatal(err)
	}

	first, err := BuildChallenges(db, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.WeekStart != weekStartOf(now) {
		t.Errorf("weekStart 应为本周一起始, got %s want %s", first.WeekStart, weekStartOf(now))
	}
	if len(first.Challenges) > challengeMaxPerWeek {
		t.Errorf("每周最多 %d 条, got %d", challengeMaxPerWeek, len(first.Challenges))
	}
	for _, c := range first.Challenges {
		if c.DoneAt != "" && !c.Achieved {
			t.Errorf("有 done_at 却未 achieved: %+v", c)
		}
	}

	// 幂等：再次构建不应重复生成挑战、不应重复累加 XP。
	second, err := BuildChallenges(db, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Challenges) != len(first.Challenges) {
		t.Errorf("跨调用挑战条数应一致（幂等）: %d vs %d", len(first.Challenges), len(second.Challenges))
	}
	if second.XP != first.XP {
		t.Errorf("XP 不应重复累加: %d vs %d", first.XP, second.XP)
	}
	if second.Level != levelForXP(second.XP) {
		t.Errorf("等级应与 XP 自洽: level=%d xp=%d", second.Level, second.XP)
	}
}

func TestDetectCompletionsMarksAchievedOnce(t *testing.T) {
	db := regressionAssistantDB(t)
	if err := ensureGamification(db); err != nil {
		t.Fatal(err)
	}
	if err := ensureTimelineTables(db); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ws := weekStartOf(now)

	id := regressionContact(t, db, "待达成")
	// 用真实消息 + 重建日聚合，保证本周我方发言量 >0（与完成检测口径一致）。
	if _, err := SaveMessages(db, id, []Message{
		{Sender: "me", Content: "本周我发了言", Timestamp: now},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildDailyMetrics(db, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO weekly_challenges (week_start, contact_id, kind, target, achieved, xp, done_at, created_at)
		VALUES (?,?,?,1,0,0,'',?)`, ws, id, chalGreetCore, now.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	events, err := detectCompletions(db, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("应达成 1 条, got %d", len(events))
	}
	var ach, xp int
	db.QueryRow(`SELECT achieved, xp FROM weekly_challenges WHERE week_start=? AND kind=? AND contact_id=?`, ws, chalGreetCore, id).Scan(&ach, &xp)
	if ach != 1 || xp != challengeXPPerDone {
		t.Errorf("应 achieved=1 且 xp=%d, got ach=%d xp=%d", challengeXPPerDone, ach, xp)
	}

	// 再跑一次：已 achieved 的行不应再次计入、不再加 XP（幂等）。
	events2, err := detectCompletions(db, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(events2) != 0 {
		t.Errorf("重复检测不应再达成, got %d", len(events2))
	}
	var totalXP int
	db.QueryRow(`SELECT xp FROM gamification_state WHERE id=1`).Scan(&totalXP)
	if totalXP != challengeXPPerDone {
		t.Errorf("终身 XP 应只加一次 = %d, got %d", challengeXPPerDone, totalXP)
	}
}
