package main

import (
	"fmt"
	"strconv"
	"testing"
	"time"
)

// 关系成就 / 里程碑（achievements.go）离线单测：纯函数确定性 + DB 集成幂等。

func TestLongestStreak(t *testing.T) {
	cases := []struct {
		name string
		days []string
		want int
	}{
		{"empty", nil, 0},
		{"single", []string{"2026-01-01"}, 1},
		{"contiguous", []string{"2026-01-01", "2026-01-02", "2026-01-03"}, 3},
		{"gap_resets", []string{"2026-01-01", "2026-01-02", "2026-01-05"}, 2},
		{"unsorted_input", []string{"2026-01-03", "2026-01-01", "2026-01-02"}, 3},
		{"two_runs_take_max", []string{"2026-02-01", "2026-02-02", "2026-02-03", "2026-02-10", "2026-02-11", "2026-02-12", "2026-02-13"}, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := longestStreak(c.days); got != c.want {
				t.Fatalf("days=%v want %d got %d", c.days, c.want, got)
			}
		})
	}
}

func TestWholeYearsSince(t *testing.T) {
	now := time.Date(2026, 6, 15, 9, 0, 0, 0, time.Local)
	cases := []struct {
		name string
		from time.Time
		want int
	}{
		{"zero_when_now", now, 0},
		{"future_before_from", time.Date(2027, 1, 1, 0, 0, 0, 0, time.Local), 0},
		{"two_years", now.AddDate(-2, 0, 0), 2},
		{"one_year_minus_a_day", now.AddDate(-2, 0, 1), 1}, // 2年前多1天 → 还差1天满2周年
		{"exact_one", now.AddDate(-1, 0, 0), 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := wholeYearsSince(c.from, now); got != c.want {
				t.Fatalf("from=%v want %d got %d", c.from.Format("2006-01-02"), c.want, got)
			}
		})
	}
}

func TestEvaluateAchievementsUnlockAndIdempotency(t *testing.T) {
	now := time.Date(2026, 6, 15, 9, 0, 0, 0, time.Local)
	firstTime := now.AddDate(-2, 0, 0) // 相识满 2 年
	metrics := achMetrics{msgTotal: 1000, maxStreak: 8, firstTime: firstTime}

	// 空 recorded：msg 100/500/1000（3）+ streak 7（1）+ years 1（1）= 5 个新跨越。
	items, unlocks := evaluateAchievements(metrics, map[string]recAchieve{}, now)
	if len(unlocks) != 5 {
		t.Fatalf("期望 5 个新跨越，实得 %d：%+v", len(unlocks), unlocks)
	}
	// 相识周年的跨越时间应为确定性的 firstTime+1 年，而非 now。
	var yearUnlock *pendingUnlock
	for i := range unlocks {
		if unlocks[i].Key == "years_known" {
			yearUnlock = &unlocks[i]
		}
	}
	if yearUnlock == nil {
		t.Fatal("years_known 档位未被检出")
	}
	if !yearUnlock.AchievedAt.Equal(firstTime.AddDate(1, 0, 0)) {
		t.Fatalf("周年跨越时间应=首条+N年，got %v", yearUnlock.AchievedAt)
	}
	// 未达成档位 pct 正确夹取：msg 5000 档 current=1000 → 20%、met=false。
	var found5000 bool
	for _, it := range items {
		if it.Key == "msg_total" && it.Tier == 5000 {
			found5000 = true
			if it.Met || it.Pct != 20 {
				t.Fatalf("msg5000 期望未达成/20%%，got met=%v pct=%d", it.Met, it.Pct)
			}
		}
	}
	if !found5000 {
		t.Fatal("未遍历到 msg_total 5000 档")
	}

	// 全部已记录：unlocks 应为空，展示项沿用库内解锁时间（幂等）。
	recorded := map[string]recAchieve{}
	for _, u := range unlocks {
		recorded[u.Key+"|"+strconv.Itoa(u.Tier)] = recAchieve{achievedAt: u.AchievedAt.Format(time.RFC3339)}
	}
	items2, unlocks2 := evaluateAchievements(metrics, recorded, now)
	if len(unlocks2) != 0 {
		t.Fatalf("已记录后不应再有新跨越，实得 %d", len(unlocks2))
	}
	for _, it := range items2 {
		if it.Met && it.AchievedAt == "" {
			t.Fatalf("达成项应有解锁时间：%+v", it)
		}
	}
}

func TestDetectAndRecordAchievementsIdempotent(t *testing.T) {
	db := regressionDB(t)
	if err := ensureAchievements(db); err != nil {
		t.Fatal(err)
	}
	// contact_events 正常由启动期建立，测试库需显式兜底（里程碑事件落此表）。
	if err := ensureTimelineTables(db); err != nil {
		t.Fatal(err)
	}
	id := regressionContact(t, db, "achive")

	// 7 个连续自然日、每天 15 条 → 累计 105 条（≥100）+ 连续 7 天（≥7），且都落在同一年（周年=0）。
	now := time.Now()
	var msgs []Message
	for d := 0; d < 7; d++ {
		day := now.AddDate(0, 0, -(6 - d))
		for j := 0; j < 15; j++ {
			msgs = append(msgs, Message{Sender: "other", Content: fmt.Sprintf("d%d-m%d", d, j), Timestamp: day.Add(time.Duration(j) * time.Minute)})
		}
	}
	if _, err := SaveMessages(db, id, msgs); err != nil {
		t.Fatal(err)
	}

	countRows := func() (achCnt, evCnt int) {
		if err := db.QueryRow(`SELECT COUNT(*) FROM contact_achievements WHERE contact_id=?`, id).Scan(&achCnt); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM contact_events WHERE contact_id=? AND kind='milestone'`, id).Scan(&evCnt); err != nil {
			t.Fatal(err)
		}
		return
	}

	resp, err := DetectAndRecordAchievements(db, id, now)
	if err != nil {
		t.Fatal(err)
	}
	achCnt, evCnt := countRows()
	if achCnt != 2 || evCnt != 2 {
		t.Fatalf("首次应落 2 成就 + 2 时间线事件，实得 ach=%d ev=%d", achCnt, evCnt)
	}
	met := map[string]int{}
	for _, it := range resp.Items {
		if it.Met {
			met[it.Key+"/"+strconv.Itoa(it.Tier)] = 1
		}
	}
	if met["msg_total/100"] != 1 || met["streak_days/7"] != 1 {
		t.Fatalf("期望达成 msg100 与 streak7，got %v", met)
	}
	if met["msg_total/500"] == 1 || met["years_known/1"] == 1 {
		t.Fatalf("不应达成 msg500 或周年1，got %v", met)
	}

	// 重复检测：不应新增任何成就或时间线事件（幂等）。
	if _, err := DetectAndRecordAchievements(db, id, now.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	achCnt2, evCnt2 := countRows()
	if achCnt2 != achCnt || evCnt2 != evCnt {
		t.Fatalf("重复调用应幂等：ach %d→%d ev %d→%d", achCnt, achCnt2, evCnt, evCnt2)
	}
}
