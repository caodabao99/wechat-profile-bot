package main

import (
	"database/sql"
	"testing"
	"time"
)

// listIDs 按 id 升序列出某联系人全部消息 id。
func listIDs(t *testing.T, db *sql.DB, contactID int64) []int64 {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM messages WHERE contact_id=? ORDER BY id ASC`, contactID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// TestPhase3SaveMessagesBatchCounts 校验批量事务写入：新增计数正确、other_msg_count
// 一次性按“对方新增条数”累加（而不是每条一次 UPDATE），批内重复被去重。
func TestPhase3SaveMessagesBatchCounts(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "批量人")

	base, _ := otherCount(t, db, id)

	ts := time.Date(2026, 4, 1, 9, 0, 0, 0, time.Local)
	batch := []Message{
		{Sender: "other", Content: "第一条", Timestamp: ts},
		{Sender: "other", Content: "第二条", Timestamp: ts.Add(time.Minute)},
		{Sender: "me", Content: "我回复", Timestamp: ts.Add(2 * time.Minute)},
		{Sender: "other", Content: "第一条", Timestamp: ts}, // 与首条完全相同 → 去重
	}
	n, err := SaveMessages(db, id, batch)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("应新增 3 条（第 4 条重复被忽略）, got %d", n)
	}
	got, _ := otherCount(t, db, id)
	if got-base != 2 {
		t.Fatalf("other_msg_count 应 +2, got +%d", got-base)
	}
}

func otherCount(t *testing.T, db *sql.DB, id int64) (int64, error) {
	t.Helper()
	var c int64
	err := db.QueryRow(`SELECT other_msg_count FROM contacts WHERE id=?`, id).Scan(&c)
	return c, err
}

// TestPhase3KeysetPagination 校验 keyset 分页：首屏取最新一页、以游标翻下一页，
// 页间不重叠不遗漏，覆盖全部消息，且与 offset 分页取到的内容一致。
func TestPhase3KeysetPagination(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "翻页人")
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)
	for i := 0; i < 7; i++ {
		saveAt(t, db, id, "other", "消息"+time.Duration(i).String(), base.Add(time.Duration(i)*time.Hour))
	}
	ids := listIDs(t, db, id)
	if len(ids) != 7 {
		t.Fatalf("应有 7 条, got %d", len(ids))
	}

	// 首屏：最新 3 条（id 降序）
	p1, err := GetMessagesPageBefore(db, id, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1) != 3 {
		t.Fatalf("首屏应 3 条, got %d", len(p1))
	}
	if !(p1[0].ID > p1[1].ID && p1[1].ID > p1[2].ID) {
		t.Fatal("首屏应按 id 降序")
	}
	// 游标翻页：取严格早于 p1 最旧一条的下一页
	p2, err := GetMessagesPageBefore(db, id, p1[2].ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2) != 3 {
		t.Fatalf("第二页应 3 条, got %d", len(p2))
	}
	if p2[0].ID >= p1[2].ID {
		t.Fatal("第二页必须严格早于游标")
	}
	// 汇总覆盖：三页拼起来应恰好 7 条且不重复
	seen := map[int64]bool{}
	for _, m := range append(append(p1, p2...), func() []Message {
		p3, _ := GetMessagesPageBefore(db, id, p2[2].ID, 3)
		return p3
	}()...) {
		if seen[m.ID] {
			t.Fatalf("翻页出现重复 id=%d", m.ID)
		}
		seen[m.ID] = true
	}
	if len(seen) != 7 {
		t.Fatalf("三页应覆盖 7 条, got %d", len(seen))
	}
}

// TestPhase3KeysetEmptyIsNotNil 空结果返回非 nil 切片（避免前端白屏）。
func TestPhase3KeysetEmptyIsNotNil(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "空号")
	got, err := GetMessagesPageBefore(db, id, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("空结果应为 [] 而非 nil")
	}
	if len(got) != 0 {
		t.Fatalf("应为空, got %d", len(got))
	}
}
