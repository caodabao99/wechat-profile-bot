package main

// v6.3 §P12 Smart Paste 去重统计验收。

import (
	"testing"
	"time"
)

func TestIngestStatsRecordAndQuery(t *testing.T) {
	db := regressionDB(t)
	if err := ensureIngestStatsTable(db); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	id := regressionContact(t, db, "粘贴测试")

	// 模拟两次 ingest：第一次 10 条全新增，第二次 10 条只有 3 新增（7 重复）
	RecordIngestStat(db, id, 10, 10, now.Add(-time.Hour))
	RecordIngestStat(db, id, 10, 3, now)

	st, err := GetIngestStats(db, now, 90)
	if err != nil {
		t.Fatal(err)
	}
	if st.TotalPastes != 2 {
		t.Fatalf("TotalPastes=%d want 2", st.TotalPastes)
	}
	if st.TotalParsed != 20 {
		t.Fatalf("TotalParsed=%d want 20", st.TotalParsed)
	}
	if st.TotalNew != 13 {
		t.Fatalf("TotalNew=%d want 13", st.TotalNew)
	}
	if st.TotalDup != 7 {
		t.Fatalf("TotalDup=%d want 7", st.TotalDup)
	}
	if st.DedupRate < 0.34 || st.DedupRate > 0.36 {
		t.Fatalf("DedupRate=%.3f want ~0.35", st.DedupRate)
	}
	if len(st.TopDuplicates) != 1 || st.TopDuplicates[0].ContactID != id {
		t.Fatalf("TopDuplicates 应含该联系人，实得 %+v", st.TopDuplicates)
	}
}

func TestPurgeOldIngestStats(t *testing.T) {
	db := regressionDB(t)
	if err := ensureIngestStatsTable(db); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	id := regressionContact(t, db, "清理测试")
	// 100 天前的记录
	RecordIngestStat(db, id, 5, 2, now.AddDate(0, 0, -100))
	// 今天的记录
	RecordIngestStat(db, id, 5, 3, now)

	// 保留 90 天
	n, err := PurgeOldIngestStats(db, now, 90)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应清理 1 条过期行，实得 %d", n)
	}
	st, _ := GetIngestStats(db, now, 90)
	if st.TotalPastes != 1 {
		t.Fatalf("清理后应剩 1 条粘贴记录，实得 %d", st.TotalPastes)
	}
}
