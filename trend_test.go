package main

// v4.5.0 A：高阶洞察趋势快照与周环比回归。
// 覆盖：weekStartOf 周一边界、computeWeeklyChange 纯函数（nil 降级/方向/确定性）、
//       appendTrendSnapshot 同周幂等 + 52 周 prune、GetTrendSeries 升序/截断、latestPrevSnapshot。

import (
	"database/sql"
	"testing"
	"time"
)

// insertTrendRow 直插一行趋势快照，绕开四层缓存，专测读写与 prune 逻辑。
func insertTrendRow(t *testing.T, db *sql.DB, weekStart string, fragility float64, clusters int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO insight_trend_history (week_start, generated_at, cluster_count, fragility_score)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(week_start) DO UPDATE SET cluster_count=excluded.cluster_count, fragility_score=excluded.fragility_score`,
		weekStart, weekStart+"T00:00:00Z", clusters, fragility)
	if err != nil {
		t.Fatal(err)
	}
}

func TestWeekStartOf(t *testing.T) {
	cases := []struct {
		day  time.Time
		want string
	}{
		{time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local), "2026-10-05"},   // 周一本身
		{time.Date(2026, 10, 11, 23, 59, 0, 0, time.Local), "2026-10-05"}, // 周日归属上一个周一
		{time.Date(2026, 10, 7, 8, 0, 0, 0, time.Local), "2026-10-05"},    // 周三
		{time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local), "2026-10-26"},    // 跨月周日
	}
	for _, c := range cases {
		if got := weekStartOf(c.day); got != c.want {
			t.Errorf("weekStartOf(%v)=%q want %q", c.day.Format("2006-01-02 Mon"), got, c.want)
		}
	}
}

func TestComputeWeeklyChangeNilPrev(t *testing.T) {
	cur := &TrendSnapshot{TotalWealth: 300, HighRiskCount: 3, ClusterCount: 4}
	if got := computeWeeklyChange(cur, nil); len(got) != 0 {
		t.Errorf("prev 为 nil 应返回空, got %+v", got)
	}
	if got := computeWeeklyChange(nil, &TrendSnapshot{}); len(got) != 0 {
		t.Errorf("cur 为 nil 应返回空, got %+v", got)
	}
}

func TestComputeWeeklyChangeDirections(t *testing.T) {
	prev := &TrendSnapshot{TotalWealth: 200, HighRiskCount: 5, ClusterCount: 4, FragilityScore: 0.3, InitiationRate: 71, OneWayCount: 1, NodeCount: 10}
	cur := &TrendSnapshot{TotalWealth: 260, HighRiskCount: 3, ClusterCount: 3, FragilityScore: 0.55, InitiationRate: 64, OneWayCount: 4, NodeCount: 10}
	got := computeWeeklyChange(cur, prev)
	if len(got) == 0 {
		t.Fatal("显著差异应产出变化")
	}
	byText := map[string]BriefingChange{}
	for _, c := range got {
		byText[c.Layer] = c
		if c.Text == "" || (c.Dir != "up" && c.Dir != "down" && c.Dir != "flat") {
			t.Errorf("变化项不完整: %+v", c)
		}
	}
	// 高风险 5→3 应 down；先开口率 71→64 应 down；脆弱度 0.3→0.55 应 up。
	var hrChange, initChange, fragChange BriefingChange
	for _, c := range got {
		switch {
		case len(c.Text) > 0 && c.Layer == "life":
			hrChange = c
		case c.Layer == "self":
			initChange = c
		case c.Layer == "network":
			fragChange = c
		}
	}
	if hrChange.Dir != "down" {
		t.Errorf("life 层(总财富升/高风险降)方向取决于首条命中, got %+v", hrChange)
	}
	if initChange.Dir != "down" {
		t.Errorf("你先开口率 71→64 应 down, got %+v", initChange)
	}
	if fragChange.Dir != "up" {
		t.Errorf("网络脆弱度 0.3→0.55 应 up, got %+v", fragChange)
	}
	// 确定性：同输入两次全等。
	got2 := computeWeeklyChange(cur, prev)
	if len(got) != len(got2) {
		t.Fatalf("两次数量不一致")
	}
	for i := range got {
		if got[i] != got2[i] {
			t.Fatalf("第 %d 条不一致: %+v vs %+v", i, got[i], got2[i])
		}
	}
}

func TestComputeWeeklyChangeCap5(t *testing.T) {
	prev := &TrendSnapshot{TotalWealth: 100, HighRiskCount: 10, ClusterCount: 8, FragilityScore: 0.1, InitiationRate: 90, OneWayCount: 0, NodeCount: 20}
	cur := &TrendSnapshot{TotalWealth: 500, HighRiskCount: 1, ClusterCount: 2, FragilityScore: 0.9, InitiationRate: 20, OneWayCount: 9, NodeCount: 20}
	got := computeWeeklyChange(cur, prev)
	if len(got) > 5 {
		t.Errorf("变化块应 cap 5, got %d", len(got))
	}
}

func TestTrendAppendIdempotentAndPrune(t *testing.T) {
	db := regressionAssistantDB(t)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local) // 周一

	// 同周二次调用应覆盖不新增行。
	if err := appendTrendSnapshot(db, now); err != nil {
		t.Fatal(err)
	}
	seedTrendCache(t, db, now) // 造四层缓存后重算，应仍是一行
	if err := appendTrendSnapshot(db, now); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM insight_trend_history WHERE week_start = ?`, weekStartOf(now)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("同周应幂等 1 行, got %d err=%v", n, err)
	}

	// 插一行 60 周前的旧快照，再 append 触发 prune（只留近 52 周）。
	old := now.AddDate(0, 0, -60*7)
	insertTrendRow(t, db, weekStartOf(old), 0.5, 3)
	if err := appendTrendSnapshot(db, now); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM insight_trend_history WHERE week_start = ?`, weekStartOf(old)).Scan(&n); err != nil || n != 0 {
		t.Fatalf("超 52 周旧行应被 prune, got %d err=%v", n, err)
	}
}

func TestGetTrendSeriesAndPrev(t *testing.T) {
	db := regressionAssistantDB(t)
	// 造 5 个连续周（升序插入）。
	base := time.Date(2026, 9, 7, 0, 0, 0, 0, time.Local) // 周一
	for i := 0; i < 5; i++ {
		wk := base.AddDate(0, 0, 7*i)
		insertTrendRow(t, db, weekStartOf(wk), float64(i)/10, i)
	}

	series, err := GetTrendSeries(db, 3)
	if err != nil || len(series) != 3 {
		t.Fatalf("应取最近 3 周, got %d err=%v", len(series), err)
	}
	// 升序：末行应是最新一周。
	for i := 1; i < len(series); i++ {
		if series[i-1].WeekStart >= series[i].WeekStart {
			t.Errorf("应升序: %v", series)
		}
	}
	if series[len(series)-1].WeekStart != weekStartOf(base.AddDate(0, 0, 7*4)) {
		t.Errorf("最新周不符, got %s", series[len(series)-1].WeekStart)
	}

	// latestPrevSnapshot：严格早于本周的最近一行。
	thisWeek := weekStartOf(base.AddDate(0, 0, 7*4))
	prev, err := latestPrevSnapshot(db, thisWeek)
	if err != nil || prev == nil {
		t.Fatalf("应有上周基准: %v", err)
	}
	if prev.WeekStart != weekStartOf(base.AddDate(0, 0, 7*3)) {
		t.Errorf("上周不符, got %s", prev.WeekStart)
	}

	// 无历史返回空切片、nil 基准。
	empty, err := GetTrendSeries(db, 12)
	if err != nil || empty == nil {
		t.Errorf("GetTrendSeries 应返回非 nil 切片")
	}
}

func TestLatestPrevSnapshotNone(t *testing.T) {
	db := regressionAssistantDB(t)
	prev, err := latestPrevSnapshot(db, "1999-01-04")
	if err != nil || prev != nil {
		t.Errorf("无更早历史应返回 nil, got %+v err=%v", prev, err)
	}
}

// seedTrendCache 写入四层缓存，供 appendTrendSnapshot 读回标量（验证非 nil 路径）。
func seedTrendCache(t *testing.T, db *sql.DB, now time.Time) {
	t.Helper()
	if err := saveLifeStateCache(db, now, fixtureState()); err != nil {
		t.Fatal(err)
	}
	if err := saveNetworkCache(db, now, fixtureNet()); err != nil {
		t.Fatal(err)
	}
	if err := saveSelfPortraitCache(db, now, fixtureSelf()); err != nil {
		t.Fatal(err)
	}
}
