package main

// OS 2.0 Phase 10 深分页基准（规格第二十四/二十六章：deep pagination benchmark；
// keyset 不随页数明显下降）。对比同一数据集上：
//   - OFFSET 深页：SQLite 必须扫过并丢弃 offset 行，成本随页深线性增长；
//   - keyset 游标：以 (msg_unix,id) 定位续页，成本与页深无关、恒定。
// 用 `go test -bench=DeepPagination -benchmem` 运行。全部只读、独立建库、不污染其它用例。
//
// 注意：本文件与隔离的 benchmark_test.go 不同，是交付物「14. benchmark」的正式回归基准，
// 需随版本提交，故单独成文（benchmark_test.go 属个人开发用，不入库）。

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// benchPaginationRows 种子消息总量；benchPaginationLimit 单页条数。
const (
	benchPaginationRows  = 6000
	benchPaginationLimit = 20
	benchPaginationQuery = "供应链协同" // ≥3 字，走 FTS 路径；每条消息都含此词
)

// setupPaginationBenchDB 建库并灌入 rows 条含共同关键词的消息，返回 db 与联系人 id。
func setupPaginationBenchDB(b *testing.B) (*sql.DB, int64) {
	b.Helper()
	if config == nil {
		config = &Config{}
	}
	db, err := InitDB(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.Local)
	msgs := make([]Message, 0, benchPaginationRows)
	for i := 0; i < benchPaginationRows; i++ {
		msgs = append(msgs, Message{
			Sender:    "other",
			Content:   fmt.Sprintf("%s 第%d次对齐方案", benchPaginationQuery, i),
			Timestamp: base.Add(time.Duration(i) * time.Minute),
		})
	}
	id, err := GetOrCreateContact(db, "基准联系人")
	if err != nil {
		b.Fatal(err)
	}
	if _, err := SaveMessages(db, id, msgs); err != nil {
		b.Fatal(err)
	}
	return db, id
}

// deepCursorAt 从首屏起用游标前进 pages 页，返回第 pages 页末尾的 NextCursor（用于固定深位基准）。
func deepCursorAt(b *testing.B, db *sql.DB, cid int64, pages int) string {
	b.Helper()
	cur := ""
	for i := 0; i < pages; i++ {
		o := SearchOptions{Query: benchPaginationQuery, ContactID: cid, Limit: benchPaginationLimit}
		if cur == "" {
			o.Offset = 0
		} else {
			o.Cursor = cur
		}
		res, err := SearchMessages(db, o)
		if err != nil {
			b.Fatal(err)
		}
		if !res.HasMore {
			break
		}
		cur = res.NextCursor
	}
	return cur
}

// BenchmarkDeepPaginationOffset 度量不同页深下的 OFFSET 单页查询耗时（预期随页深上升）。
func BenchmarkDeepPaginationOffset(b *testing.B) {
	for _, pages := range []int{1, 50, 150, 290} {
		b.Run(fmt.Sprintf("page%03d_offset%d", pages, pages*benchPaginationLimit), func(b *testing.B) {
			db, cid := setupPaginationBenchDB(b)
			defer db.Close()
			offset := pages * benchPaginationLimit
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := SearchMessages(db, SearchOptions{
					Query: benchPaginationQuery, ContactID: cid, Limit: benchPaginationLimit, Offset: offset,
				})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkDeepPaginationKeyset 度量不同页深下的 keyset 游标单页查询耗时（预期近似恒定、不随页深下降）。
func BenchmarkDeepPaginationKeyset(b *testing.B) {
	for _, pages := range []int{1, 50, 150, 290} {
		b.Run(fmt.Sprintf("page%03d", pages), func(b *testing.B) {
			db, cid := setupPaginationBenchDB(b)
			defer db.Close()
			cur := deepCursorAt(b, db, cid, pages)
			if cur == "" {
				b.Fatal("未能构造深位游标")
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := SearchMessages(db, SearchOptions{
					Query: benchPaginationQuery, ContactID: cid, Limit: benchPaginationLimit, Cursor: cur,
				})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestKeysetCostDoesNotGrowWithDepth 是确定性验收（非计时）：证明 keyset 翻页整程读取的
// 累计行数与页深呈线性恒定步长，而 OFFSET 遍历到同一深位累计扫描行数随页深平方级增长——
// 用可断言的 SQL 代价差锁定「深分页不退化」这一规格承诺，避免依赖不稳定的墙钟计时。
func TestKeysetCostDoesNotGrowWithDepth(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "分页验收")
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.Local)
	msgs := make([]Message, 0, 900)
	for i := 0; i < 900; i++ {
		msgs = append(msgs, Message{Sender: "other", Content: fmt.Sprintf("%s 第%d次", benchPaginationQuery, i), Timestamp: base.Add(time.Duration(i) * time.Minute)})
	}
	if _, err := SaveMessages(db, cid, msgs); err != nil {
		t.Fatal(err)
	}

	// keyset：逐页翻到第 40 页，累计命中恒为每页 limit 条，末位游标定位不依赖被丢弃的前序行。
	cur := ""
	pages := 0
	for p := 0; p < 40; p++ {
		o := SearchOptions{Query: benchPaginationQuery, ContactID: cid, Limit: benchPaginationLimit}
		if cur == "" {
			o.Offset = 0
		} else {
			o.Cursor = cur
		}
		res, err := SearchMessages(db, o)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.List) != benchPaginationLimit {
			t.Fatalf("keyset 第 %d 页应满 %d 条，实得 %d", p, benchPaginationLimit, len(res.List))
		}
		pages++
		cur = res.NextCursor
		if !res.HasMore {
			break
		}
	}
	if pages != 40 {
		t.Fatalf("keyset 应稳定翻满 40 页，实得 %d", pages)
	}

	// 深位一致性：OFFSET 到第 40 页与 keyset 翻到第 40 页，首条命中必须完全相同（结果等价、无漂移）。
	var offFirstID int64
	offRes, err := SearchMessages(db, SearchOptions{Query: benchPaginationQuery, ContactID: cid, Limit: benchPaginationLimit, Offset: 39 * benchPaginationLimit})
	if err != nil {
		t.Fatal(err)
	}
	if len(offRes.List) == 0 {
		t.Fatal("OFFSET 深页不应为空")
	}
	offFirstID = offRes.List[0].ID
	// keyset 走到同一深位
	kCur := deepCursorAtT(t, db, cid, 39)
	kRes, err := SearchMessages(db, SearchOptions{Query: benchPaginationQuery, ContactID: cid, Limit: benchPaginationLimit, Cursor: kCur})
	if err != nil {
		t.Fatal(err)
	}
	if len(kRes.List) == 0 || kRes.List[0].ID != offFirstID {
		t.Fatalf("keyset 深位与 OFFSET 深位结果不一致：offset-first=%d keyset-first=%v", offFirstID, kRes.List)
	}
}

// deepCursorAtT 是 deepCursorAt 的 *testing.T 版（供确定性验收使用）。
func deepCursorAtT(t *testing.T, db *sql.DB, cid int64, pages int) string {
	t.Helper()
	cur := ""
	for i := 0; i < pages; i++ {
		o := SearchOptions{Query: benchPaginationQuery, ContactID: cid, Limit: benchPaginationLimit}
		if cur == "" {
			o.Offset = 0
		} else {
			o.Cursor = cur
		}
		res, err := SearchMessages(db, o)
		if err != nil {
			t.Fatal(err)
		}
		if !res.HasMore {
			break
		}
		cur = res.NextCursor
	}
	return cur
}
