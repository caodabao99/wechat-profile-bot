package main

// Bug2 回归：归档把老消息移出 messages 后，画像语料 / 统计 / 时间线必须并入 messages_archive，
// 不再出现「点一次立即归档，下次画像基于被截断语料、首聊日期突然跳变、总数骤减」的退化。

import (
	"database/sql"
	"testing"
	"time"
)

func TestArchivePreservesCorpusStatsAndTimeline(t *testing.T) {
	db := archiveTestDB(t)
	cid := seedContact(t, db, "归档不该让我失忆", "", "{}")
	now := time.Now()
	old1 := now.AddDate(-3, 0, 0) // 最早的聊天：3 年前
	old2 := now.AddDate(-2, 0, 0)
	recent := now.AddDate(0, -1, 0)
	seedMessage(t, db, cid, "other", "三年前的第一句", "h-c1", old1)
	seedMessage(t, db, cid, "other", "两年前的话", "h-c2", old2)
	seedMessage(t, db, cid, "me", "上个月的话", "h-c3", recent)

	beforeCorpus, err := GetAllMessages(db, cid)
	if err != nil {
		t.Fatal(err)
	}
	beforeStats, err := GetContactStats(db, cid)
	if err != nil {
		t.Fatal(err)
	}
	beforeFirstRaw := timelineFirstChat(t, db, cid)

	// 归档：把 >400 天的老消息搬走（前两条，均在 400 天以外；上个月的保留）
	moved, err := RunArchive(db, 400)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 2 {
		t.Fatalf("应归档 2 条, got %d", moved)
	}
	// 确认活跃表确实只剩 1 条（归档真的发生了，不是空跑）
	if n := countRows(t, db, `SELECT COUNT(*) FROM messages`); n != 1 {
		t.Fatalf("归档后活跃表应剩 1 条, got %d", n)
	}

	// 关键回归：画像语料条数不因归档而减少
	afterCorpus, err := GetAllMessages(db, cid)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterCorpus) != len(beforeCorpus) {
		t.Fatalf("归档后画像语料退化: 前 %d 条 后 %d 条", len(beforeCorpus), len(afterCorpus))
	}
	// 且被归档的那句原文仍在语料里
	var sawOldest bool
	for _, m := range afterCorpus {
		if m.Content == "三年前的第一句" {
			sawOldest = true
		}
	}
	if !sawOldest {
		t.Fatal("归档后画像语料应仍含最早那条消息")
	}

	// 统计不退化：总数不变、首聊日期不跳变
	afterStats, err := GetContactStats(db, cid)
	if err != nil {
		t.Fatal(err)
	}
	if afterStats.Total != beforeStats.Total || afterStats.Total != 3 {
		t.Fatalf("统计总数退化: 前 %d 后 %d（应为 3）", beforeStats.Total, afterStats.Total)
	}
	if afterStats.FirstTime != beforeStats.FirstTime {
		t.Fatalf("首聊日期跳变: 前 %q 后 %q", beforeStats.FirstTime, afterStats.FirstTime)
	}

	// 时间线「第一次聊天」日期不跳变
	afterFirstRaw := timelineFirstChat(t, db, cid)
	if afterFirstRaw != beforeFirstRaw || afterFirstRaw == "" {
		t.Fatalf("时间线首聊日期跳变: 前 %q 后 %q", beforeFirstRaw, afterFirstRaw)
	}
}

// timelineFirstChat 取时间线里「第一次聊天」条目的原始时间字符串。
func timelineFirstChat(t *testing.T, db *sql.DB, cid int64) string {
	t.Helper()
	items, err := GetContactTimeline(db, cid, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Kind == "first_message" {
			return it.RawTime
		}
	}
	return ""
}
