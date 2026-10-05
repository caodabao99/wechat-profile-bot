package main

import (
	"database/sql"
	"testing"
	"time"
)

// 对话质量历史趋势（contact_quality_history）离线单测：确定性、不联网、临时库隔离。

// insertQualityWeek 手工塞一行历史快照（绕过按周幂等逻辑，专测裁剪/排序）。
func insertQualityWeek(t *testing.T, db *sql.DB, id int64, week string, score int) {
	t.Helper()
	if _, err := db.Exec(`INSERT OR REPLACE INTO contact_quality_history (contact_id, week_start, score, dims_json, window_days, generated_at) VALUES (?,?,?,?,?,?)`,
		id, week, score, "[]", 90, "2026-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}
}

func TestQualityHistoryUpsertIdempotent(t *testing.T) {
	db := regressionDB(t)
	if err := ensureQualityHistory(db); err != nil {
		t.Fatal(err)
	}
	id := regressionContact(t, db, "qhist")
	q := &ContactQuality{ContactID: id, Score: 77, WindowDays: 90, GeneratedAt: "2026-01-01 12:00:00", Dims: []QualityDim{{Key: "depth", Value: 60}}}

	upsertQualitySnapshot(db, q)
	// 同周再写一次（分数变化）：应覆盖同一行，不新增。
	q.Score = 88
	upsertQualitySnapshot(db, q)

	var cnt int
	var score int
	if err := db.QueryRow(`SELECT COUNT(*), MAX(score) FROM contact_quality_history WHERE contact_id = ?`, id).Scan(&cnt, &score); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Fatalf("同周应幂等覆盖为 1 行，实得 %d", cnt)
	}
	if score != 88 {
		t.Fatalf("期望覆盖后分数 88，实得 %d", score)
	}
}

func TestQualityHistoryReturnsAscending(t *testing.T) {
	db := regressionDB(t)
	if err := ensureQualityHistory(db); err != nil {
		t.Fatal(err)
	}
	id := regressionContact(t, db, "qhist")
	// 乱序插入三个不同周，getQualityHistory 应翻回升序（老→新）供画线。
	insertQualityWeek(t, db, id, "2026-03-02", 60)
	insertQualityWeek(t, db, id, "2026-01-05", 40)
	insertQualityWeek(t, db, id, "2026-02-02", 50)

	pts, err := getQualityHistory(db, id, 52)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 3 {
		t.Fatalf("期望 3 条，实得 %d", len(pts))
	}
	for i := 1; i < len(pts); i++ {
		if pts[i].WeekStart < pts[i-1].WeekStart {
			t.Fatalf("未按升序：%v", pts)
		}
	}
	if pts[0].WeekStart != "2026-01-05" || pts[0].Score != 40 {
		t.Fatalf("最老一周应 2026-01-05/40，实得 %s/%d", pts[0].WeekStart, pts[0].Score)
	}
}

func TestQualityHistoryCapTrim(t *testing.T) {
	db := regressionDB(t)
	if err := ensureQualityHistory(db); err != nil {
		t.Fatal(err)
	}
	id := regressionContact(t, db, "qhist")
	now := time.Now()
	// 灌入远超上限的周历史（130 周），再触发一次 upsert —— 应裁剪到仅留近 qualityHistoryMaxWeeks 周。
	for i := 0; i < 130; i++ {
		ws := weekStartOf(now.AddDate(0, 0, -7*i))
		insertQualityWeek(t, db, id, ws, 50+i%50)
	}
	q := &ContactQuality{ContactID: id, Score: 70, WindowDays: 90, GeneratedAt: now.Format("2006-01-02 15:04:05")}
	upsertQualitySnapshot(db, q)

	var cnt int
	if err := db.QueryRow(`SELECT COUNT(*) FROM contact_quality_history WHERE contact_id = ?`, id).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt > qualityHistoryMaxWeeks+1 {
		t.Fatalf("裁剪后应 ≪ %d 周（含当前周边界），实得 %d", qualityHistoryMaxWeeks+1, cnt)
	}
	if cnt < qualityHistoryMaxWeeks/2 {
		t.Fatalf("裁剪过度：仅剩 %d 周", cnt)
	}
}
