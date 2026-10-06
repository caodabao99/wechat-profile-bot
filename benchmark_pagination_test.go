package main

// Phase 14 性能 Benchmark（蓝图 §23 · 场景5）：证明联系人深分页的 keyset 游标
// 单位页成本不随页深增长（O(limit)），而旧 OFFSET 分页会随页深线性劣化（O(offset+limit)）。
//
// 说明：Benchmark 仅在 `go test -bench` 下执行，普通 `go test`/`-race` 不会运行，
// 因此可安全提交、不拖慢 CI。数据集刻意保持中等规模（benchPageSize 条），既能在秒级完成，
// 又足以拉开 offset 与 cursor 在深页处的成本差；更大规模（100K~10M）可经 -benchtime 复现。

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

const benchPageSize = 3000 // 播种联系人条数（够深页对比，又保证 benchmark 秒级完成）

// pageBenchDB 仅接 *testing.B 的建库器（regressionDB 只接 *testing.T；benchmark 需 B 版）。
func pageBenchDB(b *testing.B) *sql.DB {
	b.Helper()
	if config == nil {
		config = &Config{}
	}
	db, err := InitDB(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	return db
}

// seedContactsBench 建库并写入 n 条 last_updated 互异的联系人（触发器同步 last_updated_unix），
// 供深分页 benchmark 复用。返回的游标锚点 (unix,id) 指向第 n/2 行附近（降序）。
func seedContactsBench(b *testing.B, n int) (*sql.DB, int64, int64) {
	b.Helper()
	db := pageBenchDB(b)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		id, err := GetOrCreateContact(db, fmt.Sprintf("bench-%06d", i))
		if err != nil {
			b.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE contacts SET last_updated=? WHERE id=?`,
			base.Add(time.Duration(i)*time.Minute).Format(time.RFC3339), id); err != nil {
			b.Fatal(err)
		}
	}
	// 深页锚点：降序第 n/2 行的 (last_updated_unix, id)，游标将从此继续取下一页。
	var beforeUnix, beforeID int64
	if err := db.QueryRow(
		`SELECT last_updated_unix, id FROM contacts ORDER BY last_updated_unix DESC, id DESC LIMIT 1 OFFSET ?`,
		n/2).Scan(&beforeUnix, &beforeID); err != nil {
		b.Fatal(err)
	}
	return db, beforeUnix, beforeID
}

// BenchmarkContactsOffsetDeepPage 旧 OFFSET 深分页：每页成本 ∝ offset+limit。
func BenchmarkContactsOffsetDeepPage(b *testing.B) {
	db, _, _ := seedContactsBench(b, benchPageSize)
	offset := benchPageSize / 2
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := GetContactsPageFiltered(db, false, "", nil, offset, 50); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkContactsCursorDeepPage keyset 游标深分页：每页成本仅 ∝ limit，与页深无关。
func BenchmarkContactsCursorDeepPage(b *testing.B) {
	db, beforeUnix, beforeID := seedContactsBench(b, benchPageSize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, _, _, _, err := GetContactsPageCursor(db, false, "", nil, beforeUnix, beforeID, 50, false); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkContactsCursorFirstPageNoCount 首屏游标但不 COUNT（includeTotal=false）：验证默认省掉全表计数。
func BenchmarkContactsCursorFirstPageNoCount(b *testing.B) {
	db, _, _ := seedContactsBench(b, benchPageSize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, _, _, _, err := GetContactsPageCursor(db, false, "", nil, 0, 0, 50, false); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkContactsCursorFirstPageWithCount 首屏游标且 COUNT（includeTotal=true）：对照 §11.1 显式 opt-in 的计数开销。
func BenchmarkContactsCursorFirstPageWithCount(b *testing.B) {
	db, _, _ := seedContactsBench(b, benchPageSize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, _, _, _, err := GetContactsPageCursor(db, false, "", nil, 0, 0, 50, true); err != nil {
			b.Fatal(err)
		}
	}
}
