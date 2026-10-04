package main

// Phase 11：备份 / 恢复兼容回归。
// 验证可信画像派生表（事实/证据/日标指标/行动建议）不参与备份拷贝，恢复后为空，
// 且下次访问能「缺则重建」自愈；同时保证老数据（profile_json + messages）恢复后不损坏。

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestPhase11BackupExcludesDerivedTablesAndSelfHeals(t *testing.T) {
	// 源库：一个有画像 + 消息的联系人，并生成全部派生表数据
	src := regressionDB(t)
	sid := regressionContact(t, src, "备份的人")
	pj := `{"basic_info":{"occupation":"工程师","location":"杭州"},"interests":["徒步"],"summary":"工程师"}`
	if err := SaveProfile(src, sid, pj, "工程师", "init"); err != nil {
		t.Fatal(err)
	}
	regressionMessages(t, src, sid, "我是工程师", "在杭州上班", "周末去徒步")
	active, ev, err := RebuildFactsAndEvidence(src, sid)
	if err != nil || active == 0 {
		t.Fatalf("源库事实重建失败 active=%d ev=%d err=%v", active, ev, err)
	}
	if _, err := RebuildDailyMetrics(src, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateActionSuggestions(src, nil, sid); err != nil {
		t.Fatal(err)
	}
	assertDerivedCounts(t, src, "源库", true /*expectNonZero*/)

	// 目标库：预置一份将被恢复覆盖的旧派生数据
	dst := regressionDB(t)
	did := regressionContact(t, dst, "要被覆盖的旧人")
	if err := SaveProfile(dst, did, `{"interests":["钓鱼"],"summary":"旧"}`, "旧", "old"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RebuildFactsAndEvidence(dst, did); err != nil {
		t.Fatal(err)
	}

	zipPath, cleanup, err := BuildBackupZip(src, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if _, err := RestoreBackupZip(dst, zipPath, filepath.Dir(zipPath), false); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}

	// 恢复后派生表必须为空（既不来自备份，也清掉了目标库旧数据）
	assertDerivedCounts(t, dst, "恢复后的目标库", false /*expectNonZero*/)

	// 源数据完好：联系人 + 消息都在
	var rid int64
	if err := dst.QueryRow(`SELECT id FROM contacts WHERE name='备份的人'`).Scan(&rid); err != nil {
		t.Fatalf("恢复后联系人丢失: %v", err)
	}
	var msgN int
	if err := dst.QueryRow(`SELECT COUNT(*) FROM messages WHERE contact_id=?`, rid).Scan(&msgN); err != nil || msgN < 3 {
		t.Fatalf("恢复后消息丢失: n=%d err=%v", msgN, err)
	}

	// 自愈：事实从恢复的 profile_json 重建
	a2, e2, err := RebuildFactsAndEvidence(dst, rid)
	if err != nil || a2 == 0 {
		t.Fatalf("恢复后事实自愈失败 active=%d ev=%d err=%v", a2, e2, err)
	}
	// 自愈：趋势读到时指标为空则就地重建
	if _, err := GetRelationshipTrend(dst, rid); err != nil {
		t.Fatalf("恢复后趋势自愈失败: %v", err)
	}
	var metricN int
	if err := dst.QueryRow(`SELECT COUNT(*) FROM relationship_daily_metrics WHERE contact_id=?`, rid).Scan(&metricN); err != nil || metricN == 0 {
		t.Fatalf("趋势读后日指标应被自愈重建: n=%d err=%v", metricN, err)
	}
}

// assertDerivedCounts 断言四张派生表是否有数据。
func assertDerivedCounts(t *testing.T, db *sql.DB, label string, expectNonZero bool) {
	t.Helper()
	tables := []string{"profile_facts", "profile_fact_evidence", "relationship_daily_metrics", "relationship_action_suggestions"}
	for _, tb := range tables {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + tb).Scan(&n); err != nil {
			t.Fatalf("[%s] 统计 %s 失败: %v", label, tb, err)
		}
		if expectNonZero && n == 0 {
			t.Fatalf("[%s] 派生表 %s 应有数据", label, tb)
		}
		if !expectNonZero && n != 0 {
			t.Fatalf("[%s] 派生表 %s 应为空, got %d", label, tb, n)
		}
	}
}
