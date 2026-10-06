package main

// 蓝图 §10 P6：Archive-aware correctness audit（历史统计归档一致性）。
//
// 审计结论：多数历史统计已并入 messages_archive（GetAllMessages/GetContactStats/GetAggregatedMetrics/
// rebuildDailyMetricsLocked/search/merge other_msg_count）。唯一残留缺陷是四处「指标表为空则重建」的
// 自愈门槛只数活跃表——一个人若消息已全部归档，门槛算出 0 而跳过重建，令依赖 relationship_daily_metrics
// 的历史统计（累计互动/热力图/趋势/主题/数据报告）读 0。本文件锁死该修复，并按 §10.3 做
// archive before / archive after / restore after 三阶段一致性回归。

import (
	"database/sql"
	"testing"
	"time"
)

// seedMsgUnix 直插一条带 msg_unix 的活跃消息（日聚合按 msg_unix>0 归日，seedMessage 不设该列）。
func seedMsgUnix(t *testing.T, db *sql.DB, cid int64, sender, content, hash string, ts time.Time) {
	t.Helper()
	mustExec(t, db,
		`INSERT INTO messages (contact_id, sender, content, msg_hash, msg_time, msg_unix) VALUES (?, ?, ?, ?, ?, ?)`,
		cid, sender, content, hash, ts.Format(time.RFC3339), ts.Unix())
}

// sumDailyMetrics 某人日聚合表的互动总量（历史累计互动，与是否归档无关，只看派生结果）。
func sumDailyMetrics(t *testing.T, db *sql.DB, cid int64) int {
	t.Helper()
	var s int
	if err := db.QueryRow(`SELECT COALESCE(SUM(me_count+other_count),0) FROM relationship_daily_metrics WHERE contact_id=?`, cid).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// forceTrendRebuild 清空该人指标表后读趋势，触发「指标空则重建」自愈门槛（据 hasMessagesForRebuildLocked）。
func forceTrendRebuild(t *testing.T, db *sql.DB, cid int64) int {
	t.Helper()
	mustExec(t, db, `DELETE FROM relationship_daily_metrics WHERE contact_id=?`, cid)
	if _, err := GetRelationshipTrend(db, cid); err != nil {
		t.Fatal(err)
	}
	return sumDailyMetrics(t, db, cid)
}

// 核心回归：一个人消息「全部」被归档后，活跃表为空但归档表有数据——门槛必须计入归档，
// 仍重建日聚合，使历史统计读到真实累计互动（修复前这里会是 0）。
func TestRebuildGateCountsArchiveWhenFullyArchived(t *testing.T) {
	db := archiveTestDB(t)
	cid := seedContact(t, db, "全线归档者", "", "{}")
	old := time.Now().AddDate(-5, 0, 0)
	for _, s := range []struct {
		sender, txt string
	}{
		{"other", "五年前的话一"}, {"me", "五年前的话二"}, {"other", "五年前的话三"}, {"me", "五年前的话四"},
	} {
		seedMsgUnix(t, db, cid, s.sender, s.txt, "gate-"+s.txt, old)
	}

	// 归档前：门槛已能重建
	if got := forceTrendRebuild(t, db, cid); got != 4 {
		t.Fatalf("归档前历史累计互动应为 4, got %d", got)
	}

	moved, err := RunArchive(db, 400) // 五年前 → 全部归档
	if err != nil {
		t.Fatal(err)
	}
	if moved != 4 {
		t.Fatalf("应归档 4 条, got %d", moved)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM messages`); n != 0 {
		t.Fatalf("归档后活跃表应空, got %d", n)
	}

	// 关键：清空指标后重建门槛必须计入归档（修复前此步会得 0）
	if got := forceTrendRebuild(t, db, cid); got != 4 {
		t.Fatalf("消息全部归档后历史累计互动仍应为 4（门槛须计入归档）, got %d", got)
	}

	// 热力图（年度报告按年聚合）同样不得因全部归档而空
	if hm := buildHeatmap(db, cid, old.Year(), time.Now()); hm.Total != 4 {
		t.Fatalf("全部归档后年度热力图总量应 4, got %d", hm.Total)
	}

	// 认识多久（首聊日期）跨归档不跳变
	stats, err := GetContactStats(db, cid)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 4 || stats.FirstTime == "" {
		t.Fatalf("全部归档后统计应 Total=4 且首聊非空, got %+v", stats)
	}
}

// §10.3：历史统计（认识多久 / 累计互动）在 archive before → archive after → restore after 三阶段一致。
func TestHistoricalStatsThreePhaseArchiveConsistency(t *testing.T) {
	db := archiveTestDB(t)
	cid := seedContact(t, db, "三阶段一致性", "", "{}")
	veryOld := time.Now().AddDate(-4, 0, 0)
	oldMsg := time.Now().AddDate(-2, 0, 0)
	recent := time.Now().AddDate(0, 0, -10) // 10 天前 → 400 天门槛内保留
	seedMsgUnix(t, db, cid, "other", "四年前", "p-"+t.Name()+"-a", veryOld)
	seedMsgUnix(t, db, cid, "me", "两年前", "p-"+t.Name()+"-b", oldMsg)
	seedMsgUnix(t, db, cid, "other", "最近", "p-"+t.Name()+"-c", recent)

	// —— archive before ——
	beforeStats, err := GetContactStats(db, cid)
	if err != nil {
		t.Fatal(err)
	}
	beforeTotal := forceTrendRebuild(t, db, cid)
	if beforeTotal != 3 || beforeStats.Total != 3 {
		t.Fatalf("阶段1基线应 Total=3, got trend=%d stats=%d", beforeTotal, beforeStats.Total)
	}

	// —— archive after（归档 >400 天的两条：四年前、两年前）——
	moved, err := RunArchive(db, 400)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 2 {
		t.Fatalf("应归档 2 条, got %d", moved)
	}
	afterStats, err := GetContactStats(db, cid)
	if err != nil {
		t.Fatal(err)
	}
	afterTotal := forceTrendRebuild(t, db, cid)
	if afterTotal != beforeTotal {
		t.Fatalf("归档后累计互动退化: 前 %d 后 %d", beforeTotal, afterTotal)
	}
	if afterStats.Total != beforeStats.Total || afterStats.FirstTime != beforeStats.FirstTime {
		t.Fatalf("归档后统计/首聊跳变: 前 %+v 后 %+v", beforeStats, afterStats)
	}

	// —— restore after（全部出档）——
	res, err := RestoreArchive(db, cid)
	if err != nil {
		t.Fatal(err)
	}
	if res.Restored == 0 {
		t.Fatal("恢复应取回消息")
	}
	restStats, err := GetContactStats(db, cid)
	if err != nil {
		t.Fatal(err)
	}
	restTotal := forceTrendRebuild(t, db, cid)
	if restTotal != beforeTotal {
		t.Fatalf("恢复后累计互动不一致: 基线 %d 恢复 %d", beforeTotal, restTotal)
	}
	if restStats.Total != beforeStats.Total || restStats.FirstTime != beforeStats.FirstTime {
		t.Fatalf("恢复后统计/首聊不一致: 基线 %+v 恢复 %+v", beforeStats, restStats)
	}
}

// 门槛判定本身的单元回归：活跃/归档任一有数据即为真，两表皆空为假；归档表缺失时不得报错。
func TestHasMessagesForRebuildLocked(t *testing.T) {
	db := archiveTestDB(t)
	cid := seedContact(t, db, "门槛判定", "", "{}")

	count := func(q string, args ...interface{}) int {
		t.Helper()
		var n int
		if err := db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	locked := func() bool {
		dbMu.Lock()
		defer dbMu.Unlock()
		return hasMessagesForRebuildLocked(db, cid)
	}

	// 两表皆无该人消息 → false
	if locked() {
		t.Fatal("无任何消息应判 false")
	}

	// 仅活跃表有消息 → true
	seedMsgUnix(t, db, cid, "me", "活跃一条", "h-active", time.Now())
	if !locked() {
		t.Fatal("活跃表有消息应判 true")
	}

	// 手动把活跃行搬到归档、清空活跃表 → 构造「活跃空、归档有」→ true（本次修复核心）
	if n := count(`SELECT COUNT(*) FROM messages_archive`); n != 0 {
		t.Fatalf("前置：归档表应为空, got %d", n)
	}
	mustExec(t, db,
		`INSERT OR IGNORE INTO messages_archive (id, contact_id, sender, content, msg_hash, msg_time, msg_unix, archived_at)
		 SELECT id, contact_id, sender, content, msg_hash, msg_time, msg_unix, ? FROM messages WHERE contact_id=?`,
		time.Now().Format(time.RFC3339), cid)
	mustExec(t, db, `DELETE FROM messages WHERE contact_id=?`, cid)
	if count(`SELECT COUNT(*) FROM messages WHERE contact_id=?`, cid) != 0 {
		t.Fatal("应已清空活跃表")
	}
	if count(`SELECT COUNT(*) FROM messages_archive WHERE contact_id=?`, cid) == 0 {
		t.Fatal("前置：归档表应有搬入的行")
	}
	if !locked() {
		t.Fatal("仅归档表有消息时门槛必须计入归档判 true")
	}
}
