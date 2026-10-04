package main

// 逻辑缺陷修复的回归测试，覆盖四处：
//   #1 助手调度改为「到点即触发」——测 atOrAfter 判定
//   #2 daysUntilNext 按民用日历日计算——闰年 2/29 与不存在的 2/30
//   #3 闰年生日在平年回退到 2/28，不再被静默丢弃
//   #4 messageHash：无时间戳消息重复粘贴不再重复入库、不再虚增 other_msg_count
//
// checkAssistantSchedule 会拉起后台 goroutine 触达 DB/邮件，与 t.Cleanup 关库存在
// 竞态，故这里只测其纯判定函数 atOrAfter，不测整条调度链。

import (
	"testing"
	"time"
)

// ---------- #2 / #3 ----------

func TestDaysUntilNextLeapDay(t *testing.T) {
	// 平年 2026：2/29 不存在，应回退到 2/28，而不是返回 -1 把人整段丢弃
	nonLeap := time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC)
	d, next := daysUntilNext(nonLeap, 2, 29)
	if d < 0 {
		t.Fatalf("平年 2/29 不应返回 -1，得到 %d", d)
	}
	if got := next.Format("2006-01-02"); got != "2026-02-28" {
		t.Fatalf("平年 2/29 应回退到 2/28，得到 %s", got)
	}

	// 闰年 2028：应正好落在 2/29
	leap := time.Date(2028, 2, 1, 9, 0, 0, 0, time.UTC)
	if _, next := daysUntilNext(leap, 2, 29); next.Format("2006-01-02") != "2028-02-29" {
		t.Fatalf("闰年 2/29 应为 2/29，得到 %s", next.Format("2006-01-02"))
	}

	// 2/30 任何年份都不存在：保持 -1（与既有 TestDaysUntilNext 一致）
	if d, _ := daysUntilNext(nonLeap, 2, 30); d != -1 {
		t.Fatalf("2/30 应为 -1，得到 %d", d)
	}
}

// daysUntilNext 的天数必须是精确的日历日差，不受时区/时刻影响。
func TestDaysUntilNextIsCalendarDayBased(t *testing.T) {
	morning := time.Date(2026, 3, 10, 0, 30, 0, 0, time.Local)
	evening := time.Date(2026, 3, 10, 23, 30, 0, 0, time.Local)
	d1, _ := daysUntilNext(morning, 3, 12)
	d2, _ := daysUntilNext(evening, 3, 12)
	if d1 != 2 || d2 != 2 {
		t.Fatalf("3/12 相对 3/10 恒为 2 天后，得到 %d / %d", d1, d2)
	}
}

// ---------- #1 ----------

func TestAtOrAfter(t *testing.T) {
	now := time.Date(2026, 3, 10, 8, 0, 30, 0, time.Local) // 08:00:30
	cases := []struct {
		hhmm string
		want bool
	}{
		{"08:00", true}, // 已过触发点 → 到点即触发（含"错过后重启当天补跑"语义）
		{"08:01", false},
		{"00:00", true},
		{"23:59", false},
		{"99:99", false}, // 非法
		{"8", false},     // 非法
		{"", false},      // 非法
	}
	for _, c := range cases {
		if got := atOrAfter(now, c.hhmm); got != c.want {
			t.Errorf("atOrAfter(08:00:30, %q) = %v, want %v", c.hhmm, got, c.want)
		}
	}
}

// ---------- #4 ----------

func TestSaveMessagesDedupsWithoutTimestamp(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "无时间戳甲")

	// Timestamp 保持零值 = 聊天记录没带可解析的时间
	paste := func() []Message {
		return []Message{{Sender: "other", Content: "今晚八点老地方见"}}
	}

	n1, err := SaveMessages(db, cid, paste())
	if err != nil {
		t.Fatal(err)
	}
	if n1 != 1 {
		t.Fatalf("首次应新增 1 条，得到 %d", n1)
	}

	n2, err := SaveMessages(db, cid, paste())
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Fatalf("重复粘贴无时间戳的同一内容应去重为 0，得到 %d", n2)
	}

	var otherCount int
	if err := db.QueryRow(`SELECT other_msg_count FROM contacts WHERE id = ?`, cid).Scan(&otherCount); err != nil {
		t.Fatal(err)
	}
	if otherCount != 1 {
		t.Fatalf("other_msg_count 不应因重复粘贴虚增，期望 1，得到 %d", otherCount)
	}
}

func TestSaveMessagesSameContentDifferentTime(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "有时间戳乙")

	base := time.Date(2025, 6, 10, 10, 0, 0, 0, time.Local)
	n1, _ := SaveMessages(db, cid, []Message{{Sender: "other", Content: "好", Timestamp: base}})
	n2, _ := SaveMessages(db, cid, []Message{{Sender: "other", Content: "好", Timestamp: base.Add(time.Hour)}})
	// 带不同真实时间的相同内容视为两条不同消息，都应入库
	if n1 != 1 || n2 != 1 {
		t.Fatalf("同内容不同真实时间应各自入库，得到 %d / %d", n1, n2)
	}
}
