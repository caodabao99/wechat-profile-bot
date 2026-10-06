package main

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// seedWindow 在 [fromDay, throughDay]（含端点，逐日）为每个 day 插入一行互动，me/other 固定。
func seedWindow(t *testing.T, db *sql.DB, cid int64, fromDay string, days int, me, other int) {
	t.Helper()
	f, err := time.Parse("2006-01-02", fromDay)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < days; i++ {
		day := f.AddDate(0, 0, i).Format("2006-01-02")
		insertMetrics(t, db, cid, day, me, other)
	}
}

// runMeasure 建一条 running 实验（duration=7, start=2026-01-08）并按 now 测量，返回结论与 note。
func runMeasure(t *testing.T, now time.Time, seed func(t *testing.T, db *sql.DB, cid int64)) (string, string) {
	t.Helper()
	db := regressionDB(t)
	cid := regressionContact(t, db, "测量对象")
	seed(t, db, cid)
	eid, err := CreateExperiment(db, CreateExperimentInput{
		ContactID: cid, Goal: "改善关系", DurationDays: 7, StartDate: "2026-01-08",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetExperimentStatus(db, eid, ExpStatusRunning, now); err != nil {
		t.Fatal(err)
	}
	ok, err := MeasureExperiment(db, eid, now)
	if err != nil {
		t.Fatalf("测量失败: %v", err)
	}
	if !ok {
		t.Fatal("running 且已到开始日应被测量")
	}
	e, err := GetExperiment(db, eid)
	if err != nil || e == nil {
		t.Fatalf("读回失败 %+v %v", e, err)
	}
	if e.Status != ExpStatusCompleted {
		t.Errorf("测量后应 completed, got %s", e.Status)
	}
	return e.Conclusion, e.ConclusionNote
}

// start 2026-01-08, end 01-15, baseFrom 01-01。base 窗口 [01-01,01-08) 7 天；obs [01-08,01-15] 8 天。
func TestMeasureExperimentConclusions(t *testing.T) {
	future := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC) // today>end → obs 用满整窗

	// improved：base 日均 1.0，obs 日均 4.0 → ratio 4>1.3
	c, note := runMeasure(t, future, func(t *testing.T, db *sql.DB, cid int64) {
		seedWindow(t, db, cid, "2026-01-01", 7, 1, 0) // base sum=7 → 1.0/day
		seedWindow(t, db, cid, "2026-01-08", 8, 3, 1) // obs sum=32 → 4.0/day
	})
	if c != ExpConclusionImproved {
		t.Errorf("应 improved, got %s (%s)", c, note)
	}

	// negative：base 日均高，obs 归零（无 obs 行）→ base>0 && obs<=0
	c, _ = runMeasure(t, future, func(t *testing.T, db *sql.DB, cid int64) {
		seedWindow(t, db, cid, "2026-01-01", 7, 3, 1)
	})
	if c != ExpConclusionNegative {
		t.Errorf("obs 归零应 negative, got %s", c)
	}

	// no_change：前后日均持平 ratio≈1
	c, _ = runMeasure(t, future, func(t *testing.T, db *sql.DB, cid int64) {
		seedWindow(t, db, cid, "2026-01-01", 7, 1, 0)
		seedWindow(t, db, cid, "2026-01-08", 8, 1, 0)
	})
	if c != ExpConclusionNoChange {
		t.Errorf("持平应 no_change, got %s", c)
	}

	// insufficient：前后皆无数据
	c, _ = runMeasure(t, future, func(t *testing.T, db *sql.DB, cid int64) {})
	if c != ExpConclusionInsufficient {
		t.Errorf("无数据应 insufficient, got %s", c)
	}

	// insufficient：观察窗口 <3 天（today 刚过 start）
	cShort, _ := runMeasure(t, time.Date(2026, 1, 9, 0, 0, 0, 0, time.UTC), func(t *testing.T, db *sql.DB, cid int64) {
		seedWindow(t, db, cid, "2026-01-08", 2, 5, 0)
	})
	if cShort != ExpConclusionInsufficient {
		t.Errorf("观察窗口仅 2 天应 insufficient, got %s", cShort)
	}
}

// §9.3 铁律：任何极性结论的 note 必须显式声明非因果（绝不宣称「X 导致关系变好」）。
func TestMeasureConclusionNoteNeverClaimsCausality(t *testing.T) {
	future := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	_, note := runMeasure(t, future, func(t *testing.T, db *sql.DB, cid int64) {
		seedWindow(t, db, cid, "2026-01-01", 7, 1, 0)
		seedWindow(t, db, cid, "2026-01-08", 8, 3, 1)
	})
	if !strings.Contains(note, "非因果") {
		t.Fatalf("结论 note 必须声明非因果, got: %s", note)
	}
}

// draft（尚未开始）不测量：返回 (false,nil) 且不改状态/结论。
func TestMeasureDraftSkipped(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "草稿实验")
	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	eid, err := CreateExperiment(db, CreateExperimentInput{ContactID: cid, DurationDays: 7, StartDate: "2026-01-08"}, now)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := MeasureExperiment(db, eid, now)
	if err != nil || ok {
		t.Fatalf("draft 应跳过: ok=%v err=%v", ok, err)
	}
	e, _ := GetExperiment(db, eid)
	if e.Status != ExpStatusDraft || e.Conclusion != "" {
		t.Fatalf("draft 不应被改动: %+v", e)
	}
}

// running 但 today < start_date：诚实给 insufficient，不测量数据。
func TestMeasureNotYetStarted(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "未开始实验")
	createNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	eid, err := CreateExperiment(db, CreateExperimentInput{ContactID: cid, DurationDays: 7, StartDate: "2026-01-08"}, createNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetExperimentStatus(db, eid, ExpStatusRunning, createNow); err != nil {
		t.Fatal(err)
	}
	// 测量时刻仍在开始日之前
	ok, err := MeasureExperiment(db, eid, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil || !ok {
		t.Fatalf("未到开始日应给 insufficient 且视为已处理: ok=%v err=%v", ok, err)
	}
	e, _ := GetExperiment(db, eid)
	if e.Conclusion != ExpConclusionInsufficient || !strings.Contains(e.ConclusionNote, "尚未开始") {
		t.Fatalf("应为 insufficient/尚未开始: %+v", e)
	}
}

// concludeExperiment 纯函数级：只测阈值边界与措辞，不依赖 DB。
func TestConcludeExperimentPure(t *testing.T) {
	cor := "非因果"
	base := expSnapshot{InteractionsPerDay: 1.0, Days: 7}
	obs := expSnapshot{InteractionsPerDay: 1.2, Days: 10}
	c, note := concludeExperiment(base, obs)
	if c != ExpConclusionNoChange {
		t.Errorf("ratio1.2 应 no_change, got %s", c)
	}
	if !strings.Contains(note, cor) {
		t.Errorf("note 应含非因果措辞, got %s", note)
	}
	// 观察天数不足优先判 insufficient
	c2, _ := concludeExperiment(base, expSnapshot{InteractionsPerDay: 9, Days: 2})
	if c2 != ExpConclusionInsufficient {
		t.Errorf("days<3 应 insufficient, got %s", c2)
	}
}
