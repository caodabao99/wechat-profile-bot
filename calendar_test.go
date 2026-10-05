package main

// calendar.go 的独立单元测试：RFC-5545 文案转义/折行/UID 等易错纯函数，
// 以及 BuildCalendarICS 的 DB 聚合与「2 月 29 日生日在平年不被静默丢弃」回归。
// 全部确定性、不调模型、不改生产代码。

import (
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestICSEscape(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"  首尾空白  ", "首尾空白"}, // TrimSpace
		{`反斜杠\`, `反斜杠\\`},
		{`分;号`, `分\;号`},
		{`逗,号`, `逗\,号`},
		{"换\r\n行", `换\n行`},
		{"换\n行", `换\n行`},
		{"换\r行", `换\n行`},
		{"制表\t符", "制表 符"}, // \t → 空格
	}
	for _, c := range cases {
		if got := icsEscape(c.in); got != c.want {
			t.Errorf("icsEscape(%q)=%q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestICSFold(t *testing.T) {
	// 折行不切断多字节 rune、不丢内容：续行以 " \r\n"（空格）起，剥离后应还原原串。
	long := strings.Repeat("测", 100) // 每字 3 字节，远超 70 字节阈值
	folded := icsFold(long)
	if !utf8.ValidString(folded) {
		t.Fatalf("折行结果非合法 UTF-8，可能在字节边界切断了 rune: %q", folded)
	}
	restored := strings.ReplaceAll(folded, "\r\n ", "")
	if restored != long {
		t.Fatalf("剥离折行标记后未还原原文，len(restored)=%d len(long)=%d", len([]rune(restored)), len([]rune(long)))
	}
	// 每个物理行字节数不超过 70 内容 + 1 前导空格 = 71。
	for _, seg := range strings.Split(folded, "\r\n") {
		if len(seg) > 71 {
			t.Errorf("物理行超过 RFC5545 上限：%d 字节，段=%q", len(seg), seg)
		}
	}
	// 短行（<=70 字节）不应被折。
	short := "SUMMARY:张三 生日"
	if icsFold(short) != short {
		t.Errorf("短行不应折行，got %q", icsFold(short))
	}
}

func TestICSUID(t *testing.T) {
	// 稳定：同输入两次全等
	a := icsUID(42, "生日 5月20日")
	b := icsUID(42, "生日 5月20日")
	if a != b {
		t.Fatalf("icsUID 不确定：%q != %q", a, b)
	}
	if !strings.HasPrefix(a, "42-") || !strings.HasSuffix(a, "@wechat-profile-bot") {
		t.Errorf("icsUID 格式异常：%q", a)
	}
	// 区分：contact 或 raw 不同都应得到不同 UID
	if icsUID(43, "生日 5月20日") == a {
		t.Error("不同 contactID 却得到相同 UID")
	}
	if icsUID(42, "生日 6月1日") == a {
		t.Error("不同 raw 却得到相同 UID")
	}
}

func TestNewCalendarKey(t *testing.T) {
	k1 := newCalendarKey()
	k2 := newCalendarKey()
	if len(k1) != calendarKeyBytes*2 {
		t.Errorf("密钥长度=%d，期望 %d", len(k1), calendarKeyBytes*2)
	}
	if !regexp.MustCompile(`^[0-9a-f]+$`).MatchString(k1) {
		t.Errorf("密钥非十六进制：%q", k1)
	}
	if k1 == k2 {
		t.Error("两次生成的订阅密钥相同，随机性不足")
	}
}

func TestBuildBlessingEmailHTML(t *testing.T) {
	if got := buildBlessingEmailHTML(nil); got != "" {
		t.Errorf("空列表应返回空串，got %q", got)
	}
	html := buildBlessingEmailHTML([]string{"第一条<b>", "第二条"})
	if !strings.Contains(html, "<li>第一条&lt;b&gt;</li>") {
		t.Errorf("条目未做 HTML 转义：%q", html)
	}
	if !strings.Contains(html, "<li>第二条</li>") {
		t.Errorf("缺少第二个条目：%q", html)
	}
	if strings.Count(html, "<li>") != 2 {
		t.Errorf("应有 2 个 <li>：%q", html)
	}
}

func TestBuildCalendarICSWithDates(t *testing.T) {
	db := regressionDB(t)
	// 一个普通生日（5月20日）在一年内应出现；一个 2/29 闰日生日在平年不得被静默丢弃。
	c1 := regressionContact(t, db, "阿五月")
	c2 := regressionContact(t, db, "闰闰")
	if _, err := db.Exec(`UPDATE contacts SET profile_json=? WHERE id=?`,
		`{"basic_info":{"important_dates":["生日 5月20日"]}}`, c1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE contacts SET profile_json=? WHERE id=?`,
		`{"basic_info":{"important_dates":["生日 2月29日"]}}`, c2); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local) // 2026 非闰年
	ics, err := BuildCalendarICS(db, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{"BEGIN:VCALENDAR", "END:VCALENDAR", "RRULE:FREQ=YEARLY", "BEGIN:VEVENT", "END:VEVENT"} {
		if !strings.Contains(ics, must) {
			t.Errorf("ICS 缺少 %q", must)
		}
	}
	// 两条日期都应各生成一个 VEVENT（按 UID 前缀区分两个联系人）。
	if !strings.Contains(ics, icsUID(c1, "生日 5月20日")) {
		t.Errorf("ICS 未包含 5/20 生日事件的 UID")
	}
	if !strings.Contains(ics, icsUID(c2, "生日 2月29日")) {
		t.Errorf("闰日回归失败：2/29 生日在平年被静默丢弃，未出现在 ICS 中")
	}
	// 平年 2/29 回退到 2/28：下一次发生应为 2027-02-28（2026 的 2/28 已在今日之前）。
	if !strings.Contains(ics, "DTSTART;VALUE=DATE:20270228") {
		t.Errorf("2/29 生日在平年应回退到 2/28，未在 ICS 中找到 DTSTART 20270228")
	}
}

func TestBuildCalendarICSEmpty(t *testing.T) {
	db := regressionDB(t)
	// 无重要日子时仍应产出合法的日历头，但不含任何 VEVENT。
	regressionContact(t, db, "无日期")
	ics, err := BuildCalendarICS(db, time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ics, "BEGIN:VCALENDAR\r\n") || !strings.HasSuffix(ics, "END:VCALENDAR\r\n") {
		t.Errorf("空日历头不完整：%q", ics)
	}
	if strings.Contains(ics, "BEGIN:VEVENT") {
		t.Errorf("无重要日子时不应有 VEVENT：%q", ics)
	}
}

// countByKind 统计日历事件里各 kind 的条数（供断言）。
func countByKind(events []CalendarEvent) map[string]int {
	m := map[string]int{}
	for _, e := range events {
		m[e.Kind]++
	}
	return m
}

// findFirst 按 kind 取第一条事件（测试可控环境里每 kind 只种一条）。
func findFirst(events []CalendarEvent, kind string) (CalendarEvent, bool) {
	for _, e := range events {
		if e.Kind == kind {
			return e, true
		}
	}
	return CalendarEvent{}, false
}

// v4.7.0：buildCalendarEvents 三源聚合 + 窗口边界 + 确定性排序。
func TestBuildCalendarEventsAggregate(t *testing.T) {
	db := vaDB(t) // 含 tag/timeline/followup 全部增值表

	// 1) 生日：10 月 15 日（年度重复）
	bday := regressionContact(t, db, "小寿")
	if _, err := db.Exec(`UPDATE contacts SET profile_json=? WHERE id=?`,
		`{"basic_info":{"important_dates":["生日 10月15日"]}}`, bday); err != nil {
		t.Fatal(err)
	}
	// 2) 手动大事记：2026-10-10（用内部记录函数，不受“不能晚于明天”的手动录入校验）
	big := regressionContact(t, db, "大事")
	RecordContactEvent(db, big, "custom", "升职了", "", time.Date(2026, 10, 10, 9, 0, 0, 0, time.Local))
	// 3) 跟进截止：2026-10-20 open
	fu := regressionContact(t, db, "待办")
	if _, err := AddFollowup(db, fu, "promise", "还书", "", "2026-10-20"); err != nil {
		t.Fatal(err)
	}
	// 4) 越界项：生日 11 月 11 日 + 跟进 2026-12-01（都不应落入 10 月窗口）
	nov := regressionContact(t, db, "十一月")
	if _, err := db.Exec(`UPDATE contacts SET profile_json=? WHERE id=?`,
		`{"basic_info":{"important_dates":["生日 11月11日"]}}`, nov); err != nil {
		t.Fatal(err)
	}
	lateFu := regressionContact(t, db, "迟")
	if _, err := AddFollowup(db, lateFu, "custom", "迟到的截止", "", "2026-12-01"); err != nil {
		t.Fatal(err)
	}

	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)
	to := time.Date(2026, 10, 31, 0, 0, 0, 0, time.Local)
	events, err := buildCalendarEvents(db, from, to)
	if err != nil {
		t.Fatal(err)
	}

	// 三源各命中一条，kind 正确
	cnt := countByKind(events)
	if cnt["birthday"] != 1 || cnt["timeline"] != 1 || cnt["followup"] != 1 {
		t.Fatalf("三源应各 1 条，实际 %+v（events=%+v）", cnt, events)
	}

	// 落位日期正确
	if e, _ := findFirst(events, "birthday"); e.Date != "2026-10-15" || e.ContactID != bday {
		t.Errorf("生日事件异常: %+v", e)
	}
	if e, _ := findFirst(events, "timeline"); e.Date != "2026-10-10" || e.Title != "升职了" || e.ContactID != big {
		t.Errorf("大事记事件异常: %+v", e)
	}
	if e, _ := findFirst(events, "followup"); e.Date != "2026-10-20" || e.Title != "还书" || e.ContactID != fu {
		t.Errorf("跟进事件异常: %+v", e)
	}

	// 越界项不出现：11/11 生日、12-01 跟进都不在结果里
	for _, e := range events {
		if e.ContactID == nov || e.ContactID == lateFu {
			t.Errorf("越界事件不应落入窗口: %+v", e)
		}
	}

	// 确定性：日期升序
	for i := 1; i < len(events); i++ {
		if events[i].Date < events[i-1].Date {
			t.Fatalf("事件未按 date 升序: %v", datesOf(events))
		}
	}
	// 可复现：同输入两次逐字段全等
	again, err := buildCalendarEvents(db, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != len(events) {
		t.Fatalf("两次聚合条数不一致: %d vs %d", len(again), len(events))
	}
	for i := range again {
		if again[i] != events[i] {
			t.Fatalf("第 %d 条事件不确定: %+v vs %+v", i, again[i], events[i])
		}
	}
}

func datesOf(events []CalendarEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Date)
	}
	return out
}

// v4.7.0：2 月 29 日生日在平年逐年展开时回退到 2/28，不被静默丢弃。
func TestBuildCalendarEventsLeapDayFallback(t *testing.T) {
	db := vaDB(t)
	cid := regressionContact(t, db, "闰闰")
	if _, err := db.Exec(`UPDATE contacts SET profile_json=? WHERE id=?`,
		`{"basic_info":{"important_dates":["生日 2月29日"]}}`, cid); err != nil {
		t.Fatal(err)
	}
	// 2027 非闰年：窗口盖住整个 2 月
	from := time.Date(2027, 2, 1, 0, 0, 0, 0, time.Local)
	to := time.Date(2027, 2, 28, 0, 0, 0, 0, time.Local)
	events, err := buildCalendarEvents(db, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("平年 2/29 生日应回退展开为 1 条，实际 %d 条: %+v", len(events), events)
	}
	if events[0].Date != "2027-02-28" || events[0].Kind != "birthday" {
		t.Errorf("2/29 应回退到 2027-02-28，实际 %+v", events[0])
	}
}

// v4.7.0：已完成的跟进（status!=open）不进日历。
func TestBuildCalendarEventsFollowupStatusFilter(t *testing.T) {
	db := vaDB(t)
	cid := regressionContact(t, db, "待办")
	id, err := AddFollowup(db, cid, "promise", "办完的事", "", "2026-10-05")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetFollowupStatus(db, id, "done"); err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)
	to := time.Date(2026, 10, 31, 0, 0, 0, 0, time.Local)
	events, err := buildCalendarEvents(db, from, to)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == "followup" {
			t.Fatalf("已完成跟进不应出现在日历: %+v", e)
		}
	}
}
