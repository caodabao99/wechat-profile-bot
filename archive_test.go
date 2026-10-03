package main

import (
	"database/sql"
	"testing"
	"time"
)

func archiveTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := regressionDB(t)
	if err := ensureArchiveTables(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func countRows(t *testing.T, db *sql.DB, query string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNormalizeArchiveSettings(t *testing.T) {
	cases := []struct {
		in   ArchiveSettings
		days int
	}{
		{ArchiveSettings{RetentionDays: 0}, defaultArchiveRetentionDays},  // 未配置 → 默认 730
		{ArchiveSettings{RetentionDays: -5}, defaultArchiveRetentionDays}, // 非法 → 默认
		{ArchiveSettings{RetentionDays: 10}, 30},                          // 低于下限 → 30
		{ArchiveSettings{RetentionDays: 365}, 365},                        // 正常值保留
	}
	for _, c := range cases {
		if got := normalizeArchiveSettings(c.in); got.RetentionDays != c.days {
			t.Errorf("normalize(%d) = %d, 期望 %d", c.in.RetentionDays, got.RetentionDays, c.days)
		}
	}
}

func TestArchiveSettingsRoundTrip(t *testing.T) {
	db := archiveTestDB(t)
	// 无记录时返回默认
	s, err := loadArchiveSettings(db)
	if err != nil || s.Enabled || s.RetentionDays != defaultArchiveRetentionDays {
		t.Fatalf("默认配置加载异常: %+v err=%v", s, err)
	}
	s.Enabled = true
	s.RetentionDays = 400
	if err := saveArchiveSettings(db, s); err != nil {
		t.Fatal(err)
	}
	got, err := loadArchiveSettings(db)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.RetentionDays != 400 {
		t.Errorf("配置回读不一致: %+v", got)
	}
	// 覆盖保存（UPSERT 单行）
	got.RetentionDays = 500
	if err := saveArchiveSettings(db, got); err != nil {
		t.Fatal(err)
	}
	again, _ := loadArchiveSettings(db)
	if again.RetentionDays != 500 {
		t.Errorf("覆盖保存失败: %+v", again)
	}
	if countRows(t, db, `SELECT COUNT(*) FROM archive_settings`) != 1 {
		t.Error("archive_settings 应保持单行")
	}
}

func TestRunArchiveMovesOnlyOldMessages(t *testing.T) {
	db := archiveTestDB(t)
	cid := seedContact(t, db, "老友", "", "{}")
	now := time.Now()
	seedMessage(t, db, cid, "other", "三年前的话", "h-old", now.AddDate(-3, 0, 0))
	seedMessage(t, db, cid, "me", "上个月的话", "h-new", now.AddDate(0, -1, 0))
	// msg_time 为空/非法的历史数据必须原地保留
	seedMessage(t, db, cid, "other", "没有时间的消息", "h-empty", time.Time{})
	if _, err := db.Exec(`UPDATE messages SET msg_time = '' WHERE msg_hash = 'h-empty'`); err != nil {
		t.Fatal(err)
	}

	moved, err := RunArchive(db, 730)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 1 {
		t.Errorf("应归档 1 条，实际 %d", moved)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM messages`); n != 2 {
		t.Errorf("活动消息应剩 2 条，实际 %d", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM messages_archive`); n != 1 {
		t.Errorf("归档表应有 1 条，实际 %d", n)
	}
	// 幂等：再跑一次不应重复搬移
	moved2, err := RunArchive(db, 730)
	if err != nil {
		t.Fatal(err)
	}
	if moved2 != 0 {
		t.Errorf("二次归档应为 0，实际 %d", moved2)
	}
	if archiveLastRunAt(db) == "" {
		t.Error("归档后应记录 last_run_at")
	}
}

func TestRunArchiveUsesConfiguredDays(t *testing.T) {
	db := archiveTestDB(t)
	cid := seedContact(t, db, "配置测试", "", "{}")
	seedMessage(t, db, cid, "other", "100 天前", "h-100", time.Now().AddDate(0, 0, -100))
	// days<=0 时走配置：保留 200 天 → 100 天前的消息不归档
	if err := saveArchiveSettings(db, ArchiveSettings{RetentionDays: 200}); err != nil {
		t.Fatal(err)
	}
	moved, err := RunArchive(db, 0)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 0 {
		t.Errorf("按配置 200 天不应归档，实际 moved=%d", moved)
	}
	// 显式 days=30 覆盖配置
	moved, err = RunArchive(db, 30)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 1 {
		t.Errorf("显式 30 天应归档 1 条，实际 %d", moved)
	}
}

func TestRestoreArchiveKeepsOrphans(t *testing.T) {
	db := archiveTestDB(t)
	alive := seedContact(t, db, "还在的朋友", "", "{}")
	gone := seedContact(t, db, "已删除的朋友", "", "{}")
	old := time.Now().AddDate(-3, 0, 0)
	seedMessage(t, db, alive, "other", "旧消息A", "h-a", old)
	seedMessage(t, db, gone, "other", "旧消息B", "h-b", old)
	if _, err := RunArchive(db, 730); err != nil {
		t.Fatal(err)
	}
	// 删除联系人（消息已归档，主表无残留）
	if _, err := db.Exec(`DELETE FROM contacts WHERE id = ?`, gone); err != nil {
		t.Fatal(err)
	}

	res, err := RestoreArchive(db, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Restored != 1 {
		t.Errorf("应恢复 1 条，实际 %d", res.Restored)
	}
	if res.Skipped != 1 {
		t.Errorf("孤儿应计 1 条，实际 %d", res.Skipped)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM messages WHERE msg_hash = 'h-a'`); n != 1 {
		t.Error("存活联系人的消息应写回 messages")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM messages WHERE msg_hash = 'h-b'`); n != 0 {
		t.Error("孤儿消息绝不能写回 messages")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM messages_archive WHERE msg_hash = 'h-b'`); n != 1 {
		t.Error("孤儿消息应留在归档表")
	}
}

func TestArchiveStatsAndByContact(t *testing.T) {
	db := archiveTestDB(t)
	cid := seedContact(t, db, "统计对象", "", "{}")
	now := time.Now()
	seedMessage(t, db, cid, "other", "旧", "h-s1", now.AddDate(-3, 0, 0))
	seedMessage(t, db, cid, "me", "新", "h-s2", now)
	if _, err := RunArchive(db, 730); err != nil {
		t.Fatal(err)
	}
	st, err := getArchiveStats(db)
	if err != nil {
		t.Fatal(err)
	}
	if st.ActiveMessages != 1 || st.ArchivedMessages != 1 || st.Eligible != 0 {
		t.Errorf("统计不符: %+v", st)
	}
	if st.RetentionDays != defaultArchiveRetentionDays {
		t.Errorf("默认保留天数应为 %d，实际 %d", defaultArchiveRetentionDays, st.RetentionDays)
	}
	rows, err := listArchiveByContact(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ContactID != cid || rows[0].Count != 1 || rows[0].Deleted || rows[0].Name != "统计对象" {
		t.Errorf("按联系人明细不符: %+v", rows)
	}
}
