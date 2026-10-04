package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// 联系人标签分组（网页端增值功能）：自定义标签 + 按标签筛选 + 批量打标。
//
// 设计原则——对现有功能零侵入：
//   - 数据只写新表 contact_tags / contact_tag_links，不改 contacts 表结构
//   - 现有查询路径（GetAllContacts / GetContactsPage 不带标签时）行为完全不变
//   - 标签不参与画像生成、合并匹配、提醒等任何既有逻辑，纯粹是给人看的分组

const (
	maxTagNameRunes   = 20
	maxTagsPerContact = 100  // 单个联系人最多挂多少标签
	maxBatchTagIDs    = 50   // 批量打标一次最多带多少标签
	maxBatchContacts  = 2000 // 批量打标一次最多带多少联系人
)

// ContactTag 标签及其关联的联系人数
type ContactTag struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func ensureTagTables(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS contact_tags (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			created_at TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS contact_tag_links (
			contact_id INTEGER NOT NULL,
			tag_id INTEGER NOT NULL,
			created_at TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (contact_id, tag_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tag_links_tag ON contact_tag_links(tag_id)`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("建标签表失败: %w", err)
		}
	}
	return nil
}

// tagTableReadyLocked 标签表是否已建好（调用方必须已持有 dbMu，本函数不再加锁）
func tagTableReadyLocked(db *sql.DB) bool {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='contact_tag_links'`).Scan(&n)
	return err == nil && n > 0
}

// normalizeTagName 统一标签名：去首尾空白与内部多余空格，限长
func normalizeTagName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "", fmt.Errorf("标签名不能为空")
	}
	if utf8.RuneCountInString(name) > maxTagNameRunes {
		return "", fmt.Errorf("标签名最多 %d 个字", maxTagNameRunes)
	}
	return name, nil
}

// ListTags 全部标签，按关联人数倒序、名称升序
func ListTags(db *sql.DB) ([]ContactTag, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	rows, err := db.Query(`SELECT t.id, t.name,
		(SELECT COUNT(*) FROM contact_tag_links l WHERE l.tag_id = t.id) AS cnt
		FROM contact_tags t ORDER BY cnt DESC, t.name ASC, t.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ContactTag{}
	for rows.Next() {
		var t ContactTag
		if err := rows.Scan(&t.ID, &t.Name, &t.Count); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateTag 新建标签；已存在同名标签时直接返回该标签（幂等，便于前端"输入即创建"）
func CreateTag(db *sql.DB, name string) (*ContactTag, error) {
	name, err := normalizeTagName(name)
	if err != nil {
		return nil, err
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	now := time.Now().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT OR IGNORE INTO contact_tags (name, created_at) VALUES (?, ?)`, name, now); err != nil {
		return nil, err
	}
	// 无条件按唯一名回查 id：INSERT 被 IGNORE 时 LastInsertId 不会更新，
	// 在 MaxOpenConns(1) 下它会返回同连接上一次插入的陈旧 rowid，用 id == 0 判断永远不成立
	var id int64
	if err := db.QueryRow(`SELECT id FROM contact_tags WHERE name = ?`, name).Scan(&id); err != nil {
		return nil, err
	}
	return &ContactTag{ID: id, Name: name}, nil
}

// RenameTag 改标签名
func RenameTag(db *sql.DB, id int64, name string) error {
	name, err := normalizeTagName(name)
	if err != nil {
		return err
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	res, err := db.Exec(`UPDATE contact_tags SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return fmt.Errorf("已存在同名标签")
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("标签不存在")
	}
	return nil
}

// DeleteTag 删除标签及其全部关联（联系人本身不受影响）
func DeleteTag(db *sql.DB, id int64) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM contact_tag_links WHERE tag_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM contact_tags WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("标签不存在")
	}
	return tx.Commit()
}

// GetContactTags 某联系人的标签列表
func GetContactTags(db *sql.DB, contactID int64) ([]ContactTag, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	return contactTagsLocked(db, contactID)
}

func contactTagsLocked(db *sql.DB, contactID int64) ([]ContactTag, error) {
	rows, err := db.Query(`SELECT t.id, t.name FROM contact_tag_links l
		JOIN contact_tags t ON t.id = l.tag_id
		WHERE l.contact_id = ? ORDER BY t.name ASC, t.id ASC`, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ContactTag{}
	for rows.Next() {
		var t ContactTag
		if err := rows.Scan(&t.ID, &t.Name); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TagsForContacts 批量取多个联系人的标签（列表页一次查询，避免 N+1）
func TagsForContacts(db *sql.DB, ids []int64) (map[int64][]ContactTag, error) {
	out := make(map[int64][]ContactTag, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	// 分块查询：全量联系人列表可能上千条，一次拼进 IN(...) 会撞上 SQLite 的绑定参数上限
	const chunk = 400
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		part := ids[start:end]
		ph := make([]string, len(part))
		args := make([]interface{}, len(part))
		for i, id := range part {
			ph[i] = "?"
			args[i] = id
		}
		rows, err := db.Query(`SELECT l.contact_id, t.id, t.name FROM contact_tag_links l
			JOIN contact_tags t ON t.id = l.tag_id
			WHERE l.contact_id IN (`+strings.Join(ph, ",")+`) ORDER BY t.name ASC, t.id ASC`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var cid int64
			var t ContactTag
			if err := rows.Scan(&cid, &t.ID, &t.Name); err != nil {
				rows.Close()
				return nil, err
			}
			out[cid] = append(out[cid], t)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SetContactTags 覆盖式设置某联系人的标签（tagIDs 里不存在的 id 会被忽略）
func SetContactTags(db *sql.DB, contactID int64, tagIDs []int64) error {
	if len(tagIDs) > maxTagsPerContact {
		return fmt.Errorf("一个联系人最多挂 %d 个标签", maxTagsPerContact)
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM contacts WHERE id = ?`, contactID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("联系人不存在")
	}
	if _, err := tx.Exec(`DELETE FROM contact_tag_links WHERE contact_id = ?`, contactID); err != nil {
		return err
	}
	now := time.Now().Format(time.RFC3339)
	seen := make(map[int64]bool, len(tagIDs))
	for _, id := range tagIDs {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		res, err := tx.Exec(`INSERT OR IGNORE INTO contact_tag_links (contact_id, tag_id, created_at)
			SELECT ?, id, ? FROM contact_tags WHERE id = ?`, contactID, now, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("标签不存在: %d", id)
		}
	}
	return tx.Commit()
}

// BatchTag 给多个联系人批量加/减标签，返回实际影响的关联行数
func BatchTag(db *sql.DB, contactIDs, tagIDs []int64, remove bool) (int, error) {
	if len(contactIDs) == 0 || len(tagIDs) == 0 {
		return 0, fmt.Errorf("请先选择联系人和标签")
	}
	// 双层循环是逐对执行 SQL，不设上限的话一次请求就能把整库锁死几十分钟
	if len(contactIDs) > maxBatchContacts {
		return 0, fmt.Errorf("一次最多处理 %d 个联系人", maxBatchContacts)
	}
	if len(tagIDs) > maxBatchTagIDs {
		return 0, fmt.Errorf("一次最多操作 %d 个标签", maxBatchTagIDs)
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now().Format(time.RFC3339)
	affected := 0
	for _, cid := range contactIDs {
		for _, tid := range tagIDs {
			if cid <= 0 || tid <= 0 {
				continue
			}
			var res sql.Result
			if remove {
				res, err = tx.Exec(`DELETE FROM contact_tag_links WHERE contact_id = ? AND tag_id = ?`, cid, tid)
			} else {
				// 只给真实存在的联系人打标，避免留下悬空关联
				res, err = tx.Exec(`INSERT OR IGNORE INTO contact_tag_links (contact_id, tag_id, created_at)
					SELECT c.id, t.id, ? FROM contacts c, contact_tags t WHERE c.id = ? AND t.id = ?`, now, cid, tid)
			}
			if err != nil {
				return 0, err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				affected += int(n)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return affected, nil
}

// transferTagsLocked 合并时把源联系人的标签并到目标联系人（调用方须已持 dbMu 且在事务内）
func transferTagsLocked(tx *sql.Tx, sourceID, targetID int64) error {
	if sourceID == targetID {
		return nil
	}
	now := time.Now().Format(time.RFC3339)
	if _, err := tx.Exec(`INSERT OR IGNORE INTO contact_tag_links (contact_id, tag_id, created_at)
		SELECT ?, tag_id, ? FROM contact_tag_links WHERE contact_id = ?`, targetID, now, sourceID); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM contact_tag_links WHERE contact_id = ?`, sourceID)
	return err
}

// attachContactTags 给一批联系人输出附加标签（一次批量查询，避免 N+1）。
// 标签属于纯增值信息：查询失败只记日志，绝不让联系人列表跟着 500。
func attachContactTags(db *sql.DB, list []contactJSON) {
	if len(list) == 0 {
		return
	}
	ids := make([]int64, 0, len(list))
	for i := range list {
		ids = append(ids, list[i].ID)
	}
	tagMap, err := TagsForContacts(db, ids)
	if err != nil {
		slog.Warn("读取联系人标签失败", "err", err)
		return
	}
	for i := range list {
		// 没标签的也要给空数组：JSON 里少了 tags 键，网页端读 c.tags.length 会直接抛异常
		tags := tagMap[list[i].ID]
		if tags == nil {
			tags = []ContactTag{}
		}
		list[i].Tags = tags
	}
}

// parseTagIDs 解析 "1,2,3" 形式的标签 id 串
func parseTagIDs(raw string) []int64 {
	out := []int64{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// 用 ParseInt 而不是 Sscanf：Sscanf("%d") 对 "12abc" 也会返回成功并吃掉 12
		if id, err := strconv.ParseInt(part, 10, 64); err == nil && id > 0 {
			out = append(out, id)
		}
	}
	return out
}
