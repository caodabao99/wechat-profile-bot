package main

// v4.5.0 C：彻底删除联系人的级联清理（被遗忘权）回归。
// 断言删除后隐私残留表再无该 contact 的行：relationship_daily_metrics / profile_facts /
// profile_fact_evidence / contact_connections / suggestion_outcomes / relationship_action_suggestions。

import (
	"database/sql"
	"testing"
)

func countWhere(t *testing.T, db *sql.DB, table string, cid int64) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE contact_id = ?`, cid).Scan(&n); err != nil {
		t.Fatalf("统计 %s 失败: %v", table, err)
	}
	return n
}

func TestDeleteContactCascadesPrivacyResidue(t *testing.T) {
	db := regressionAssistantDB(t)
	cid := regressionContact(t, db, "待遗忘对象")
	peer := regressionContact(t, db, "连线对面")

	// 1) 互动度量
	seedDailyMetric(t, db, cid, "2026-10-01", 3, 2)
	// 2) 关系连线（双列）
	seedConn(t, db, cid, peer)
	// 3) 画像事实 + 证据
	var factID int64
	if err := db.QueryRow(`INSERT INTO profile_facts (contact_id, fact_type, fact_key, fact_value)
		VALUES (?, 'city', 'city', '上海') RETURNING id`, cid).Scan(&factID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO profile_fact_evidence (fact_id, contact_id, message_id, snippet)
		VALUES (?, ?, 999, '我在上海')`, factID, cid); err != nil {
		t.Fatal(err)
	}
	// 4) 建议 + 回测结果
	suggID := insertOpenSuggestion(t, db, cid, "cooling", "cooling:2026-10", "很久没联系", 5)
	seedOutcome(t, db, suggID, cid, "improved")

	// 种完先确认各行都在（否则测试自身无效）。
	for tbl, want := range map[string]int{
		"relationship_daily_metrics":      1,
		"profile_facts":                   1,
		"profile_fact_evidence":           1,
		"relationship_action_suggestions": 1,
		"suggestion_outcomes":             1,
	} {
		if got := countWhere(t, db, tbl, cid); got != want {
			t.Fatalf("删除前 %s 应有 %d 行, got %d", tbl, want, got)
		}
	}
	var connN int
	if err := db.QueryRow(`SELECT COUNT(*) FROM contact_connections WHERE contact_a=? OR contact_b=?`, cid, cid).Scan(&connN); err != nil || connN == 0 {
		t.Fatalf("删除前应有关系连线: got %d err=%v", connN, err)
	}

	// 执行删除
	if err := DeleteContactByID(db, cid); err != nil {
		t.Fatal(err)
	}

	// 删除后所有残留表都应归零（peer 的行不受影响，证明只删目标人）。
	for _, tbl := range []string{
		"relationship_daily_metrics", "profile_facts", "profile_fact_evidence",
		"relationship_action_suggestions", "suggestion_outcomes",
	} {
		if got := countWhere(t, db, tbl, cid); got != 0 {
			t.Errorf("删除后 %s 仍残留 %d 行 contact=%d", tbl, got, cid)
		}
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM contact_connections WHERE contact_a=? OR contact_b=?`, cid, cid).Scan(&connN); err != nil {
		t.Fatal(err)
	}
	if connN != 0 {
		t.Errorf("删除后 contact_connections 仍残留 %d 条涉及该人的边", connN)
	}
	// 联系人本体消失
	var selfN int
	db.QueryRow(`SELECT COUNT(*) FROM contacts WHERE id=?`, cid).Scan(&selfN)
	if selfN != 0 {
		t.Error("联系人应已被删除")
	}
	// peer 未被误伤
	if countWhere(t, db, "profile_facts", peer) != 0 {
		t.Error("不该误删无关联系人的数据")
	}
}
