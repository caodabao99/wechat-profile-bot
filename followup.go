package main

// 待跟进追踪（增值功能）。
//
// 用 LLM 从最近聊天记录里挖出三类容易被忘掉的事：
//   - question：对方问了但我一直没回
//   - promise ：我答应过要做的事
//   - money   ：借钱 / 还钱往来
//
// 另外支持用户在网页上手动加一条（kind=custom）。
// 结果落在独立的 followup_items 表里，每日提醒邮件追加一个「待跟进」板块，
// 关掉开关后这张表就完全不会被读写，对既有逻辑零影响。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

const (
	followupMaxPerContact = 5   // 单个联系人一次最多抽取几条
	followupMaxMsgs       = 80  // 抽取时最多看多少条历史消息
	followupContentMax    = 200 // 单条内容长度上限
	followupEmailMax      = 15  // 邮件里最多列几条
)

// FollowupItem 一条待跟进事项
type FollowupItem struct {
	ID         int64  `json:"id"`
	ContactID  int64  `json:"contactId"`
	Name       string `json:"name"`
	Kind       string `json:"kind"` // question / promise / money / custom
	KindLabel  string `json:"kindLabel"`
	Content    string `json:"content"`
	Amount     string `json:"amount"`
	SourceTime string `json:"sourceTime"`
	Status     string `json:"status"`  // open / done / ignored
	DueDate    string `json:"dueDate"` // 截止日期 YYYY-MM-DD（空=无截止），v4.7.0 供维护日历聚合
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
}

func followupKindLabel(kind string) string {
	switch kind {
	case "question":
		return "待回复"
	case "promise":
		return "我答应的事"
	case "money":
		return "钱款往来"
	case "custom":
		return "手动记录"
	}
	return "其他"
}

func validFollowupKind(kind string) bool {
	switch kind {
	case "question", "promise", "money", "custom":
		return true
	}
	return false
}

func validFollowupStatus(st string) bool {
	switch st {
	case "open", "done", "ignored":
		return true
	}
	return false
}

// isValidYMD 校验严格 YYYY-MM-DD（零填充、月/日合法），空串不在此判定。
func isValidYMD(s string) bool {
	if len(s) != 10 {
		return false
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return false
	}
	return t.Format("2006-01-02") == s
}

// ensureFollowupTables 建待跟进表（幂等，不进 migrate）
func ensureFollowupTables(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS followup_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			contact_id INTEGER NOT NULL,
			kind TEXT NOT NULL DEFAULT 'custom',
			content TEXT NOT NULL,
			amount TEXT NOT NULL DEFAULT '',
			source_msg_time TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'open',
			due_date TEXT NOT NULL DEFAULT '',
			dedup_key TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_followup_status ON followup_items(status, contact_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_followup_dedup ON followup_items(contact_id, dedup_key)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	// v4.7.0：给既有库补 due_date 列（幂等）。本表懒建、不在版本化 migrate 内，
	// 故不 bump user_version、不动 backup.go（与 tag/timeline 表同一治理方式）。
	hasDue, err := followupHasDueDate(db)
	if err != nil {
		return err
	}
	if !hasDue {
		if _, err := db.Exec(`ALTER TABLE followup_items ADD COLUMN due_date TEXT NOT NULL DEFAULT ''`); err != nil {
			if !strings.Contains(err.Error(), "duplicate column") {
				return err
			}
		}
	}
	return nil
}

// followupHasDueDate 查 followup_items 是否已有 due_date 列（供幂等 ALTER 判定）。
func followupHasDueDate(db *sql.DB) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(followup_items)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == "due_date" {
			return true, nil
		}
	}
	return false, rows.Err()
}

// followupDedupKey 同一联系人下，内容归一化后相同就算同一条，避免每天重复抽取
func followupDedupKey(kind, content string) string {
	norm := normalizeForMatch(content)
	rs := []rune(norm)
	if len(rs) > 40 {
		rs = rs[:40]
	}
	return kind + "|" + string(rs)
}

// ListFollowups 按状态列出待跟进事项（status 传 "all" 表示不限）
func ListFollowups(db *sql.DB, status string, limit int) ([]FollowupItem, error) {
	out := []FollowupItem{}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// 已合并掉的联系人不再展示：否则列表和每日邮件里会冒出"张三#merged-17"这种用户没建过的名字。
	// LEFT JOIN 下联系人已被彻底删除的孤儿行 COALESCE 成 0，仍然保留（与旧行为一致）。
	query := `
		SELECT f.id, f.contact_id, f.kind, f.content, COALESCE(f.amount, ''),
		       COALESCE(f.source_msg_time, ''), f.status, f.created_at, f.updated_at,
		       COALESCE(c.remark, ''), COALESCE(c.name, ''), COALESCE(f.due_date, '')
		FROM followup_items f LEFT JOIN contacts c ON c.id = f.contact_id
		WHERE COALESCE(c.merged_into, 0) = 0`
	args := []interface{}{}
	switch status {
	case "", "all":
		query += ` ORDER BY (f.status = 'open') DESC, strftime('%s', f.created_at) DESC LIMIT ?`
	case "open", "done", "ignored":
		query += ` AND f.status = ? ORDER BY strftime('%s', f.created_at) DESC LIMIT ?`
		args = append(args, status)
	default:
		return nil, fmt.Errorf("无效的待跟进状态")
	}
	args = append(args, limit)

	dbMu.Lock()
	defer dbMu.Unlock()
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var it FollowupItem
		var remark, name string
		if err := rows.Scan(&it.ID, &it.ContactID, &it.Kind, &it.Content, &it.Amount,
			&it.SourceTime, &it.Status, &it.CreatedAt, &it.UpdatedAt, &remark, &name, &it.DueDate); err != nil {
			return nil, err
		}
		it.Name = name
		if strings.TrimSpace(remark) != "" {
			it.Name = remark + "（" + name + "）"
		}
		it.KindLabel = followupKindLabel(it.Kind)
		out = append(out, it)
	}
	return out, rows.Err()
}

// AddFollowup 手动添加一条待跟进。dueDate 为可选截止日期 YYYY-MM-DD（空=无截止）。
func AddFollowup(db *sql.DB, contactID int64, kind, content, amount, dueDate string) (int64, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return 0, fmt.Errorf("内容不能为空")
	}
	if !validFollowupKind(kind) {
		kind = "custom"
	}
	if r := []rune(content); len(r) > followupContentMax {
		content = string(r[:followupContentMax])
	}
	amount = strings.TrimSpace(amount)
	if len([]rune(amount)) > 40 {
		amount = string([]rune(amount)[:40])
	}
	dueDate = strings.TrimSpace(dueDate)
	if dueDate != "" && !isValidYMD(dueDate) {
		return 0, fmt.Errorf("截止日期格式应为 YYYY-MM-DD")
	}
	now := time.Now().Format(time.RFC3339)

	dbMu.Lock()
	defer dbMu.Unlock()
	var exists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM contacts WHERE id = ?`, contactID).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, fmt.Errorf("联系人不存在")
	}
	// 同内容重复添加视为"把这件事重新提上来"：状态复位成 open 并刷新内容/金额/截止日
	if _, err := db.Exec(`
		INSERT INTO followup_items
			(contact_id, kind, content, amount, source_msg_time, status, due_date, dedup_key, created_at, updated_at)
		VALUES (?, ?, ?, ?, '', 'open', ?, ?, ?, ?)
		ON CONFLICT(contact_id, dedup_key) DO UPDATE SET
			status = 'open', content = excluded.content, amount = excluded.amount, due_date = excluded.due_date, updated_at = excluded.updated_at`,
		contactID, kind, content, amount, dueDate, followupDedupKey(kind, content), now, now); err != nil {
		return 0, err
	}
	// 一律按唯一键回查 id：走 ON CONFLICT 的 UPDATE 分支时 LastInsertId 不会更新，
	// 单连接下它返回的是上一次插入的陈旧 rowid，前端拿它会去改另一条毫不相干的记录
	var id int64
	if err := db.QueryRow(`SELECT id FROM followup_items WHERE contact_id = ? AND dedup_key = ?`,
		contactID, followupDedupKey(kind, content)).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// SetFollowupStatus 勾掉 / 忽略 / 恢复
func SetFollowupStatus(db *sql.DB, id int64, status string) error {
	if !validFollowupStatus(status) {
		return fmt.Errorf("无效的状态")
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	res, err := db.Exec(`UPDATE followup_items SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("待跟进事项不存在")
	}
	return nil
}

// DeleteFollowup 彻底删除一条
func DeleteFollowup(db *sql.DB, id int64) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err := db.Exec(`DELETE FROM followup_items WHERE id = ?`, id)
	return err
}

// followupTargets 最近 withinDays 天有消息的联系人，按最近活跃排序，最多 limit 个
func followupTargets(db *sql.DB, now time.Time, withinDays, limit int) []int64 {
	if limit <= 0 {
		return nil
	}
	since := now.AddDate(0, 0, -withinDays).Format(time.RFC3339)
	dbMu.Lock()
	defer dbMu.Unlock()
	rows, err := db.Query(`
		SELECT m.contact_id FROM messages m
		JOIN contacts c ON c.id = m.contact_id AND COALESCE(c.merged_into, 0) = 0
		WHERE strftime('%s', m.msg_time) >= strftime('%s', ?)
		GROUP BY m.contact_id
		ORDER BY MAX(strftime('%s', m.msg_time)) DESC
		LIMIT ?`, since, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	return ids
}

type followupLLMItem struct {
	Kind       string `json:"kind"`
	Content    string `json:"content"`
	Amount     string `json:"amount"`
	SourceTime string `json:"sourceTime"`
}

// extractFollowups 让 LLM 从一个联系人的近期消息里挖待跟进事项，写入 followup_items，返回新增条数。
func extractFollowups(db *sql.DB, llm *LLMClient, contactID int64, now time.Time, windowDays int) (int, error) {
	c, err := GetContactByID(db, contactID)
	if err != nil {
		return 0, err
	}
	msgs, err := GetRecentMessages(db, contactID, followupMaxMsgs)
	if err != nil {
		return 0, err
	}
	cutoff := now.AddDate(0, 0, -windowDays)
	lines := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if m.Timestamp.Before(cutoff) {
			continue
		}
		who := "我"
		if m.Sender == "other" {
			who = "对方"
		}
		lines = append(lines, fmt.Sprintf("%s[%s]: %s", who,
			m.Timestamp.Format("2006-01-02 15:04"), preview(m.Content, 160)))
	}
	if len(lines) < 4 {
		return 0, fmt.Errorf("近 %d 天消息不足 4 条，暂不抽取", windowDays)
	}

	prompt, err := RenderPrompt(db, "followup_extract", map[string]string{
		"name":          displayName(c),
		"maxPerContact": strconv.Itoa(followupMaxPerContact),
		"messages":      strings.Join(lines, "\n"),
	})
	if err != nil {
		return 0, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	raw, err := callLLMCached(ctx, db, llm, contactID, TaskFollowup, "", prompt)
	if err != nil {
		return 0, err
	}
	var out struct {
		Items []followupLLMItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(ExtractJSON(raw)), &out); err != nil {
		return 0, fmt.Errorf("解析待跟进结果失败: %w", err)
	}

	added := 0
	nowStr := now.Format(time.RFC3339)
	for _, it := range out.Items {
		if added >= followupMaxPerContact {
			break
		}
		content := strings.TrimSpace(it.Content)
		if content == "" || !validFollowupKind(it.Kind) || it.Kind == "custom" {
			continue
		}
		if r := []rune(content); len(r) > followupContentMax {
			content = string(r[:followupContentMax])
		}
		amount := strings.TrimSpace(it.Amount)
		if len([]rune(amount)) > 40 {
			amount = string([]rune(amount)[:40])
		}
		src := strings.TrimSpace(it.SourceTime)
		if len([]rune(src)) > 32 {
			src = string([]rune(src)[:32])
		}
		n, err := insertFollowup(db, contactID, it.Kind, content, amount, src, nowStr)
		if err != nil {
			slog.Info("待跟进：写入跳过", "contact", contactID, "err", err)
			continue
		}
		added += n
	}
	return added, nil
}

// insertFollowup 幂等写入：dedup_key 撞了就跳过（说明之前已经抽取过同一条）。
// 返回 1 表示新增，0 表示已存在。
func insertFollowup(db *sql.DB, contactID int64, kind, content, amount, srcTime, now string) (int, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	res, err := db.Exec(`
		INSERT OR IGNORE INTO followup_items
			(contact_id, kind, content, amount, source_msg_time, status, dedup_key, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'open', ?, ?, ?)`,
		contactID, kind, content, amount, srcTime, followupDedupKey(kind, content), now, now)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return 1, nil
	}
	return 0, nil
}

// RefreshFollowups 每日扫描：挑最近活跃的几个联系人做抽取。
// 全程容错，任何单个联系人失败都不影响其余的，也不影响调用方（每日邮件）。
func RefreshFollowups(db *sql.DB, llm *LLMClient, now time.Time, maxContacts, windowDays int) (scanned, added int) {
	if llm == nil || maxContacts <= 0 {
		return 0, 0
	}
	if windowDays <= 0 || windowDays > 180 {
		windowDays = 30
	}
	targets := followupTargets(db, now, windowDays, maxContacts)
	for _, id := range targets {
		scanned++
		n, err := extractFollowups(db, llm, id, now, windowDays)
		if err != nil {
			slog.Info("待跟进：抽取跳过", "contact", id, "err", err)
			continue
		}
		added += n
	}
	return scanned, added
}

// buildFollowupEmailSection 每日邮件里的「待跟进」板块
func buildFollowupEmailSection(items []FollowupItem) string {
	if len(items) == 0 {
		return ""
	}
	b := &strings.Builder{}
	b.WriteString(fmt.Sprintf(`<h3>✅ 待跟进（%d 项）</h3><ul style="line-height:1.9;">`, len(items)))
	for _, it := range items {
		amount := ""
		if strings.TrimSpace(it.Amount) != "" {
			amount = fmt.Sprintf(` <span style="color:#e64340;">[%s]</span>`, html.EscapeString(it.Amount))
		}
		src := ""
		if strings.TrimSpace(it.SourceTime) != "" {
			src = fmt.Sprintf(`<br><span style="color:#888;font-size:12px;">来源：%s</span>`, html.EscapeString(it.SourceTime))
		}
		b.WriteString(fmt.Sprintf(`<li><b>%s</b> · <span style="color:#07c160;">%s</span>：%s%s%s</li>`,
			html.EscapeString(it.Name), html.EscapeString(it.KindLabel),
			html.EscapeString(it.Content), amount, src))
	}
	b.WriteString(`</ul>`)
	b.WriteString(`<p style="color:#999;font-size:12px;">在网页端「关系助手 → 待跟进」里可以勾掉或忽略。</p>`)
	return b.String()
}

// insertEmailSection 把增值板块插到邮件页脚之前（页脚找不到就追加到末尾）
func insertEmailSection(body, section string) string {
	if section == "" {
		return body
	}
	if i := strings.LastIndex(body, emailFooter); i >= 0 {
		return body[:i] + section + body[i:]
	}
	return body + section
}
