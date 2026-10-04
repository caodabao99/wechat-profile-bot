package main

import (
	"database/sql"
	"testing"
	"time"
)

// idxExists 判断某个索引是否已建立。
func idxExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// TestPhase1MsgUnixWritePath 校验 SaveMessages 写入时同步填充 msg_unix。
func TestPhase1MsgUnixWritePath(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "写入人")
	ts := time.Date(2026, 5, 20, 13, 45, 30, 0, time.Local)
	if _, err := SaveMessages(db, id, []Message{{Sender: "other", Content: "带时间的消息", Timestamp: ts}}); err != nil {
		t.Fatal(err)
	}
	var got sql.NullInt64
	if err := db.QueryRow(`SELECT msg_unix FROM messages WHERE content='带时间的消息'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Valid || got.Int64 != ts.Unix() {
		t.Fatalf("msg_unix 应等于 %d, got %+v", ts.Unix(), got)
	}
}

// TestPhase1MigrationBackfillAndIdempotent 校验：老库（无 msg_unix 值、user_version=7）
// 迁移后自动回填 msg_unix、建立复合索引、版本升到 8，且重复迁移幂等不报错。
func TestPhase1MigrationBackfillAndIdempotent(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "老数据")
	// 先按新版写入若干条（会带 msg_unix），再抹掉值 + 降级版本号，模拟 v7 老库。
	for i := 0; i < 3; i++ {
		ts := time.Date(2025, 3, 10+i, 9, 0, 0, 0, time.Local)
		if _, err := SaveMessages(db, id, []Message{{Sender: "me", Content: time.Now().String() + string(rune('a'+i)), Timestamp: ts}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE messages SET msg_unix = NULL`); err != nil {
		t.Fatal(err)
	}
	// 删掉已建索引、降版本号，完整模拟一个尚未跑过 v8 迁移的老库。
	for _, idx := range []string{"idx_messages_contact_id", "idx_messages_contact_unix"} {
		if _, err := db.Exec(`DROP INDEX IF EXISTS ` + idx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 7`); err != nil {
		t.Fatal(err)
	}

	// 迁移前应看不到新索引。
	if idxExists(t, db, "idx_messages_contact_unix") {
		t.Fatal("迁移前不应存在 idx_messages_contact_unix（回归前置条件不成立）")
	}

	if err := migrate(db); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	// 版本应已完成整链迁移到当前最高版（含 v9 FTS 及后续派生表 v10-v13）。
	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil || ver != backupCurrentDBVer {
		t.Fatalf("user_version 应为 %d, got %d err=%v", backupCurrentDBVer, ver, err)
	}
	// 复合索引应存在。
	if !idxExists(t, db, "idx_messages_contact_id") || !idxExists(t, db, "idx_messages_contact_unix") {
		t.Fatal("messages 复合索引未建立")
	}
	// 所有可解析 msg_time 的行都应被回填。
	var bad int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages
		WHERE msg_unix IS NULL AND msg_time IS NOT NULL AND msg_time != ''`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("仍有 %d 行 msg_unix 未回填", bad)
	}

	// 幂等：再跑一次不得报错、版本保持最高版、不重复破坏数据。
	if err := migrate(db); err != nil {
		t.Fatalf("重复迁移不应报错: %v", err)
	}
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil || ver != backupCurrentDBVer {
		t.Fatalf("重复迁移后 user_version 应仍为 %d, got %d", backupCurrentDBVer, ver)
	}
}

// TestPhase1ArchivePreservesMsgUnix 校验归档/恢复往返保持 msg_unix 不丢失。
func TestPhase1ArchivePreservesMsgUnix(t *testing.T) {
	db := regressionDB(t)
	if err := ensureArchiveTables(db); err != nil {
		t.Fatal(err)
	}
	id := regressionContact(t, db, "归档对象")
	old := time.Date(2020, 1, 2, 8, 0, 0, 0, time.Local) // 远早于归档截止
	if _, err := SaveMessages(db, id, []Message{{Sender: "other", Content: "很久以前的消息", Timestamp: old}}); err != nil {
		t.Fatal(err)
	}
	wantUnix := old.Unix()

	// days=1 → cutoff≈昨天，2020 年的消息符合归档条件。
	moved, err := RunArchive(db, 1)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 1 {
		t.Fatalf("应归档 1 条, got %d", moved)
	}
	var archUnix sql.NullInt64
	if err := db.QueryRow(`SELECT msg_unix FROM messages_archive WHERE content='很久以前的消息'`).Scan(&archUnix); err != nil {
		t.Fatal(err)
	}
	if !archUnix.Valid || archUnix.Int64 != wantUnix {
		t.Fatalf("归档后 msg_unix 应=%d, got %+v", wantUnix, archUnix)
	}

	res, err := RestoreArchive(db, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Restored != 1 {
		t.Fatalf("应恢复 1 条, got %+v", res)
	}
	var backUnix sql.NullInt64
	if err := db.QueryRow(`SELECT msg_unix FROM messages WHERE content='很久以前的消息'`).Scan(&backUnix); err != nil {
		t.Fatal(err)
	}
	if !backUnix.Valid || backUnix.Int64 != wantUnix {
		t.Fatalf("恢复后 msg_unix 应=%d, got %+v", wantUnix, backUnix)
	}
}

// TestPhase1ArchiveMsgUnixColumnAddedOnUpgrade 校验旧归档表（无 msg_unix）在
// ensureArchiveTables 里会被补列并回填。
func TestPhase1ArchiveMsgUnixColumnAddedOnUpgrade(t *testing.T) {
	db := regressionDB(t)
	// 构造一张缺 msg_unix 的旧归档表 + 一条历史数据。
	if _, err := db.Exec(`CREATE TABLE messages_archive (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		contact_id INTEGER NOT NULL,
		sender TEXT NOT NULL,
		content TEXT NOT NULL,
		msg_hash TEXT NOT NULL,
		msg_time DATETIME,
		captured_at DATETIME,
		archived_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(contact_id, msg_hash))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages_archive (contact_id,sender,content,msg_hash,msg_time)
		VALUES (1,'other','老归档消息','h1','2021-06-01T10:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := ensureArchiveTables(db); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('messages_archive') WHERE name='msg_unix'`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("ensureArchiveTables 应补 msg_unix 列, n=%d err=%v", n, err)
	}
	var unix sql.NullInt64
	if err := db.QueryRow(`SELECT msg_unix FROM messages_archive WHERE content='老归档消息'`).Scan(&unix); err != nil {
		t.Fatal(err)
	}
	if !unix.Valid || unix.Int64 == 0 {
		t.Fatalf("旧归档行 msg_unix 应被回填, got %+v", unix)
	}
	if !idxExists(t, db, "idx_messages_archive_contact_unix") {
		t.Fatal("归档表复合索引未建立")
	}
}
