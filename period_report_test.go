package main

import (
	"strings"
	"testing"
	"time"
)

// TestResolvePeriodWindowBoundaries 校验各周期的自然区间、分桶数与桶边界。
func TestResolvePeriodWindowBoundaries(t *testing.T) {
	loc := time.Local

	// 日报：锚点当天 0 点起 24 个小时桶
	w, ok := resolvePeriodWindow("day", time.Date(2026, 3, 4, 15, 30, 0, 0, loc))
	if !ok {
		t.Fatal("day 解析失败")
	}
	if w.NumBuckets != 24 {
		t.Fatalf("day 应有 24 桶, got %d", w.NumBuckets)
	}
	if !w.From.Equal(time.Date(2026, 3, 4, 0, 0, 0, 0, loc)) || !w.To.Equal(time.Date(2026, 3, 5, 0, 0, 0, 0, loc)) {
		t.Fatalf("day 区间错误: %v ~ %v", w.From, w.To)
	}
	if w.bucketOf(time.Date(2026, 3, 4, 23, 59, 0, 0, loc)) != 23 {
		t.Fatal("day 桶边界错误")
	}

	// 月报：3 月有 31 天
	w, _ = resolvePeriodWindow("month", time.Date(2026, 3, 20, 0, 0, 0, 0, loc))
	if w.NumBuckets != 31 || !w.From.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, loc)) ||
		!w.To.Equal(time.Date(2026, 4, 1, 0, 0, 0, 0, loc)) {
		t.Fatalf("month 区间/桶数错误: n=%d %v~%v", w.NumBuckets, w.From, w.To)
	}
	if w.bucketOf(time.Date(2026, 3, 31, 0, 0, 0, 0, loc)) != 30 {
		t.Fatal("month 月末桶边界错误")
	}

	// 季报：3 月属于第 1 季度（1~3 月）
	w, _ = resolvePeriodWindow("quarter", time.Date(2026, 3, 15, 0, 0, 0, 0, loc))
	if w.NumBuckets != 3 || !w.From.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, loc)) ||
		!w.To.Equal(time.Date(2026, 4, 1, 0, 0, 0, 0, loc)) {
		t.Fatalf("quarter 区间错误: %v~%v", w.From, w.To)
	}
	if w.bucketOf(time.Date(2026, 3, 1, 0, 0, 0, 0, loc)) != 2 {
		t.Fatal("quarter 3 月应落在第 3 个桶")
	}

	// 半年报：8 月属下半年（7~12 月）
	w, _ = resolvePeriodWindow("half", time.Date(2026, 8, 1, 0, 0, 0, 0, loc))
	if w.NumBuckets != 6 || !w.From.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, loc)) {
		t.Fatalf("half 区间错误: %v", w.From)
	}
	if w.bucketOf(time.Date(2026, 8, 10, 0, 0, 0, 0, loc)) != 1 {
		t.Fatal("half 8 月应落在第 2 个桶")
	}

	// 年报：整年 12 个月
	w, _ = resolvePeriodWindow("year", time.Date(2026, 12, 31, 0, 0, 0, 0, loc))
	if w.NumBuckets != 12 || !w.From.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, loc)) ||
		!w.To.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, loc)) {
		t.Fatalf("year 区间错误: %v~%v", w.From, w.To)
	}
	if w.bucketOf(time.Date(2026, 12, 31, 23, 0, 0, 0, loc)) != 11 {
		t.Fatal("year 12 月应落在第 12 个桶")
	}

	// 周报：锚点所在周应为周一起、周日止，共 7 桶
	w, ok = resolvePeriodWindow("week", time.Date(2026, 3, 4, 12, 0, 0, 0, loc)) // 2026-03-04 周三
	if !ok {
		t.Fatal("week 解析失败")
	}
	if w.From.Weekday() != time.Monday || w.NumBuckets != 7 {
		t.Fatalf("周报应周一起算且 7 桶: %v(%v) n=%d", w.From, w.From.Weekday(), w.NumBuckets)
	}
	if !w.To.Equal(w.From.AddDate(0, 0, 7)) {
		t.Fatalf("周报区间长度应为 7 天: %v~%v", w.From, w.To)
	}
	if w.bucketOf(w.From) != 0 || w.bucketOf(w.From.AddDate(0, 0, 6)) != 6 {
		t.Fatal("week 桶边界错误")
	}
	// 区间外时间点应被夹紧到 [0,6]，不越界
	if idx := w.bucketOf(w.To.AddDate(0, 0, 3)); idx < 0 || idx >= w.NumBuckets {
		t.Fatalf("week 越界未夹紧: %d", idx)
	}

	if _, ok := resolvePeriodWindow("decade", time.Now()); ok {
		t.Fatal("未知周期应返回 false")
	}
}

// TestBuildPeriodReportInvalidPeriod 非法周期应直接报错。
func TestBuildPeriodReportInvalidPeriod(t *testing.T) {
	db := regressionDB(t)
	if _, err := BuildPeriodReport(db, "fortnight", time.Now()); err == nil {
		t.Fatal("非法周期应返回 error")
	}
}

// TestBuildPeriodReportWeeklyAggregation 校验周报告的总量、收发拆分、分桶与 Top 联系人。
func TestBuildPeriodReportWeeklyAggregation(t *testing.T) {
	db := regressionDB(t)
	anchor := time.Date(2026, 3, 4, 12, 0, 0, 0, time.Local) // 周三
	w, ok := resolvePeriodWindow("week", anchor)
	if !ok {
		t.Fatal("week 解析失败")
	}
	mondayNoon := w.From.Add(12 * time.Hour) // 桶 0
	tuesdayAM := w.From.Add(25 * time.Hour)  // 桶 1（次日 01:00）
	outside := w.To.Add(24 * time.Hour)      // 下周，应被排除

	a := regressionContact(t, db, "阿明")
	b := regressionContact(t, db, "小红")

	save := func(cid int64, sender, content string, ts time.Time) {
		if _, err := SaveMessages(db, cid, []Message{{Sender: sender, Content: content, Timestamp: ts}}); err != nil {
			t.Fatal(err)
		}
	}
	save(a, "me", "周一我发的话", mondayNoon)
	save(a, "other", "周一对方回的", mondayNoon.Add(time.Minute))
	save(b, "other", "周二对方的消息", tuesdayAM)
	save(a, "me", "下周不该被算进来", outside)

	rep, err := BuildPeriodReport(db, "week", anchor)
	if err != nil {
		t.Fatal(err)
	}
	if rep.TotalMessages != 3 {
		t.Fatalf("区间内应统计 3 条, got %d", rep.TotalMessages)
	}
	if rep.MyMessages != 1 || rep.TheirMessages != 2 {
		t.Fatalf("收发拆分错误: me=%d other=%d", rep.MyMessages, rep.TheirMessages)
	}
	if rep.ActiveContacts != 2 {
		t.Fatalf("活跃联系人应为 2, got %d", rep.ActiveContacts)
	}
	if rep.ActiveDays != 2 {
		t.Fatalf("活跃天数应为 2, got %d", rep.ActiveDays)
	}
	if len(rep.Buckets) != 7 {
		t.Fatalf("周报告应有 7 桶, got %d", len(rep.Buckets))
	}
	if rep.Buckets[0].Total != 2 || rep.Buckets[1].Total != 1 {
		t.Fatalf("分桶统计错误: b0=%d b1=%d", rep.Buckets[0].Total, rep.Buckets[1].Total)
	}
	if rep.BusiestLabel != "周一" || rep.BusiestCount != 2 {
		t.Fatalf("最忙碌桶错误: %s(%d)", rep.BusiestLabel, rep.BusiestCount)
	}
	if len(rep.Top) == 0 || rep.Top[0].Name != "阿明" || rep.Top[0].Total != 2 {
		t.Fatalf("Top 联系人排序错误: %+v", rep.Top)
	}
	if rep.NewContacts != 0 {
		t.Fatalf("这些联系人并非本周创建, NewContacts=%d", rep.NewContacts)
	}
}

// TestRenderPeriodHTMLEscapesAndRenders 分享长页应包含标题、KPI 并转义内容。
func TestRenderPeriodHTMLEscapesAndRenders(t *testing.T) {
	rep := &PeriodReport{
		Period: "week", Title: "2026-03-02 ~ 03-08", From: "2026-03-02T00:00:00+08:00",
		To: "2026-03-09T00:00:00+08:00", GeneratedAt: "2026-03-09 10:00:00",
		TotalMessages: 3, MyMessages: 1, TheirMessages: 2, ActiveContacts: 2,
		ActiveDays: 2, BusiestLabel: "周一", BusiestCount: 2,
		Buckets: []PeriodBucket{{Label: "周一", Total: 2}, {Label: "周二", Total: 1}},
		Top:     []ContactMsgStat{{ContactID: 1, Name: "<b>危险名字</b>", Total: 2}},
	}
	out := RenderPeriodHTML(rep)
	if out == "" {
		t.Fatal("渲染结果为空")
	}
	if !strings.Contains(out, "周报") || !strings.Contains(out, "周一") {
		t.Fatal("分享页缺少周期标题或分桶标签")
	}
	if strings.Contains(out, "<b>危险名字</b>") || !strings.Contains(out, "&lt;b&gt;") {
		t.Fatal("联系人姓名未做 HTML 转义")
	}
}
