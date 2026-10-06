package main

// V7 §16 历史消息仓储门面验收。
//
// 钉死四件事：
//  1. HistoricalMessages 必须并归档（§16.1 Historical 语义），归档行不丢。
//  2. RecentMessages 只取近窗口——归档出去的老消息本就落不进近窗口。
//  3. AggregateMessages 的按日分桶总数必须与明细并表口径一致（列表看到的=聚合算出来的）。
//  4. §16 要求的 SearchMessages 由 search.go 单一入口承担，门面不重复定义——测其含归档。

import (
	"testing"
	"time"
)

// TestFacadeRecentAndHistoricalWindow 验证近窗口与历史并表语义。
func TestFacadeRecentAndHistoricalWindow(t *testing.T) {
	db := regressionDB(t)
	if err := ensureArchiveTables(db); err != nil {
		t.Fatalf("建归档表: %v", err)
	}
	id := regressionContact(t, db, "门面窗口")
	old := time.Now().AddDate(0, 0, -200).Format("2006-01-02 15:04:05")
	near := time.Now().Add(-time.Hour).Format("2006-01-02 15:04:05")
	insertRawMessage(t, db, "messages", id, "other", "归档的老话", old, nil)
	insertRawMessage(t, db, "messages", id, "other", "近来的话", near, nil)
	if _, err := db.Exec(
		`INSERT INTO messages_archive (contact_id, sender, content, msg_hash, msg_time)
		 SELECT contact_id, sender, content, msg_hash, msg_time FROM messages WHERE content = '归档的老话'`); err != nil {
		t.Fatalf("搬归档: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM messages WHERE content = '归档的老话'`); err != nil {
		t.Fatalf("删原行: %v", err)
	}

	now := time.Now()
	// Recent：30 天窗口，只应看到近来的那条（归档老话落在窗口外）
	rec, err := RecentMessages(db, id, 30, 0, now)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if len(rec) != 1 || rec[0].Content != "近来的话" {
		t.Fatalf("Recent 应只含近窗口 1 条，实得 %d 条 %+v", len(rec), rec)
	}

	// Historical：260 天窗口，必须并上归档，看到 2 条
	his, err := HistoricalMessages(db, HistoryFilter{
		ContactID: id,
		SinceUnix: now.AddDate(0, 0, -260).Unix(),
	}, 0)
	if err != nil {
		t.Fatalf("HistoricalMessages: %v", err)
	}
	if len(his) != 2 {
		t.Fatalf("Historical 应并表见 2 条，实得 %d", len(his))
	}
	if !his[0].Archived || his[1].Archived {
		t.Fatalf("归档行应排在前面且带来源标记，实得 archived[0]=%v archived[1]=%v", his[0].Archived, his[1].Archived)
	}
}

func TestFacadeAggregateMatchesDetail(t *testing.T) {
	db := regressionDB(t)
	if err := ensureArchiveTables(db); err != nil {
		t.Fatalf("建归档表: %v", err)
	}
	id := regressionContact(t, db, "门面聚合")
	d1 := time.Now().AddDate(0, 0, -1).Format("2006-01-02 15:04:05")
	d2 := time.Now().Add(-time.Hour).Format("2006-01-02 15:04:05")
	d3 := time.Now().AddDate(0, 0, -200).Format("2006-01-02 15:04:05") // 将被归档
	insertRawMessage(t, db, "messages", id, "other", "昨天的话", d1, nil)
	insertRawMessage(t, db, "messages", id, "other", "今天的话", d2, nil)
	insertRawMessage(t, db, "messages", id, "other", "归档那天的话", d3, nil)
	if _, err := db.Exec(
		`INSERT INTO messages_archive (contact_id, sender, content, msg_hash, msg_time)
		 SELECT contact_id, sender, content, msg_hash, msg_time FROM messages WHERE content = '归档那天的话'`); err != nil {
		t.Fatalf("搬归档: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM messages WHERE content = '归档那天的话'`); err != nil {
		t.Fatalf("删原行: %v", err)
	}

	f := HistoryFilter{ContactID: id, SinceUnix: time.Now().AddDate(0, 0, -260).Unix(), WithTimeOnly: true}
	stats, err := AggregateMessages(db, f)
	if err != nil {
		t.Fatalf("AggregateMessages: %v", err)
	}
	var sum int64
	for _, s := range stats {
		if s.Day == "" {
			t.Fatalf("日桶不应出现空日期")
		}
		sum += s.Count
	}
	// 聚合总数必须等于明细并表条数——一致性不变量
	detail, err := HistoricalMessages(db, f, 0)
	if err != nil {
		t.Fatalf("HistoricalMessages: %v", err)
	}
	if sum != int64(len(detail)) || sum != 3 {
		t.Fatalf("聚合总数应与明细一致且为 3，实得 sum=%d detail=%d", sum, len(detail))
	}
	// 3 天各 1 条 → 3 个日桶
	if len(stats) != 3 {
		t.Fatalf("应有 3 个日桶（含归档那天），实得 %d：%+v", len(stats), stats)
	}
}

// 验证 §16 的 SearchMessages 能力已由 search.go 单一入口承担，且覆盖归档。
func TestFacadeSearchServedBySingleSource(t *testing.T) {
	db := regressionDB(t)
	if err := ensureArchiveTables(db); err != nil {
		t.Fatalf("建归档表: %v", err)
	}
	id := regressionContact(t, db, "门面检索")
	old := time.Now().AddDate(0, 0, -200).Format("2006-01-02 15:04:05")
	near := time.Now().Add(-time.Hour).Format("2006-01-02 15:04:05")
	insertRawMessage(t, db, "messages", id, "other", "检索令牌甲近话", near, nil)
	insertRawMessage(t, db, "messages", id, "other", "检索令牌乙老话", old, nil)
	if _, err := db.Exec(
		`INSERT INTO messages_archive (contact_id, sender, content, msg_hash, msg_time)
		 SELECT contact_id, sender, content, msg_hash, msg_time FROM messages WHERE content = '检索令牌乙老话'`); err != nil {
		t.Fatalf("搬归档: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM messages WHERE content = '检索令牌乙老话'`); err != nil {
		t.Fatalf("删原行: %v", err)
	}

	res, err := SearchMessages(db, SearchOptions{Query: "检索令牌", ContactID: id, IncludeArchive: true, Limit: 10})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(res.List) != 2 {
		t.Fatalf("检索应含归档共 2 条，实得 %d：%+v", len(res.List), res.List)
	}
}
