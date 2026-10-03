package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// 联系人时间线（网页端增值功能）：把首次聊天、改名、合并、画像变更、手动记录的重要事件
// 串成一条时间轴，在联系人详情页新增「时间线」标签展示。
//
// 设计原则——对现有功能零侵入：
//   - 绝大部分节点是从现有表**实时派生**的（contacts / messages / profile_history / merge_log），
//     不改动任何既有写入路径，老数据一上来就有完整时间线
//   - 只有用户手动添加的事件、以及改名/改备注这类"库里没留痕"的动作写新表 contact_events
//   - RecordContactEvent 全程容错：写失败只记日志，绝不影响调用方主流程

const (
	timelineMaxItems       = 300
	timelineTitleMaxRunes  = 60
	timelineDetailMaxRunes = 500
)

// TimelineItem 时间线节点
type TimelineItem struct {
	Kind      string `json:"kind"` // created/first_message/last_message/profile/merge/merged_into/renamed/remark/custom
	Title     string `json:"title"`
	Detail    string `json:"detail"`
	EventTime string `json:"eventTime"` // 已格式化：2006-01-02 15:04:05
	RawTime   string `json:"rawTime"`   // 原始时间串，前端排序/展示用
	Source    string `json:"source"`    // derived = 从现有数据派生；record = contact_events 表记录
	ID        int64  `json:"id,omitempty"`
}

func ensureTimelineTables(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS contact_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			contact_id INTEGER NOT NULL,
			kind TEXT NOT NULL,
			title TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT '',
			event_time TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_events_cid ON contact_events(contact_id, event_time)`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("建时间线表失败: %w", err)
		}
	}
	return nil
}

// RecordContactEvent 记一条联系人事件。任何失败都只记日志、不返回错误：
// 这是增值信息，不能因为它写不进去就让改名、改备注等主操作失败。
func RecordContactEvent(db *sql.DB, contactID int64, kind, title, detail string, eventTime time.Time) {
	if contactID <= 0 || strings.TrimSpace(title) == "" {
		return
	}
	if eventTime.IsZero() {
		eventTime = time.Now()
	}
	now := time.Now().Format(time.RFC3339)
	dbMu.Lock()
	defer dbMu.Unlock()
	if _, err := db.Exec(`INSERT INTO contact_events (contact_id, kind, title, detail, event_time, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		contactID, kind, truncateRunes(title, timelineTitleMaxRunes), truncateRunes(detail, timelineDetailMaxRunes),
		eventTime.Format(time.RFC3339), now); err != nil {
		if !strings.Contains(err.Error(), "no such table") {
			slog.Warn("记录联系人事件失败", "contactId", contactID, "kind", kind, "err", err)
		}
	}
}

// AddContactEvent 用户手动添加重要事件（网页端），返回事件 id
func AddContactEvent(db *sql.DB, contactID int64, title, detail string, eventTime time.Time) (int64, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return 0, fmt.Errorf("事件内容不能为空")
	}
	if eventTime.IsZero() {
		eventTime = time.Now()
	}
	if eventTime.After(time.Now().Add(24 * time.Hour)) {
		return 0, fmt.Errorf("事件时间不能晚于明天")
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM contacts WHERE id = ?`, contactID).Scan(&n); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, fmt.Errorf("联系人不存在")
	}
	now := time.Now().Format(time.RFC3339)
	res, err := db.Exec(`INSERT INTO contact_events (contact_id, kind, title, detail, event_time, created_at)
		VALUES (?, 'custom', ?, ?, ?, ?)`,
		contactID, truncateRunes(title, timelineTitleMaxRunes), truncateRunes(detail, timelineDetailMaxRunes),
		eventTime.Format(time.RFC3339), now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// DeleteContactEvent 删除一条手动记录的事件（派生节点不可删）
func DeleteContactEvent(db *sql.DB, id int64) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	res, err := db.Exec(`DELETE FROM contact_events WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("事件不存在")
	}
	return nil
}

// GetContactTimeline 汇总某联系人的完整时间线（最新在前）
func GetContactTimeline(db *sql.DB, contactID int64, limit int) ([]TimelineItem, error) {
	if limit <= 0 || limit > timelineMaxItems {
		limit = timelineMaxItems
	}
	dbMu.Lock()
	defer dbMu.Unlock()

	items := []TimelineItem{}
	add := func(kind, title, detail, raw string, source string, id int64) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		items = append(items, TimelineItem{
			Kind: kind, Title: title, Detail: strings.TrimSpace(detail),
			EventTime: displayTime(raw), RawTime: raw, Source: source, ID: id,
		})
	}

	// 1. 联系人基础信息：创建时间 + 首末条聊天
	var createdAt string
	var firstMsg, lastMsg sql.NullString
	var msgCount int64
	err := db.QueryRow(`SELECT c.created_at,
			(SELECT MIN(m.msg_time) FROM messages m WHERE m.contact_id = c.id AND COALESCE(m.msg_time,'') != ''),
			(SELECT MAX(m.msg_time) FROM messages m WHERE m.contact_id = c.id AND COALESCE(m.msg_time,'') != ''),
			(SELECT COUNT(*) FROM messages m WHERE m.contact_id = c.id)
		FROM contacts c WHERE c.id = ?`, contactID).Scan(&createdAt, &firstMsg, &lastMsg, &msgCount)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("联系人不存在")
		}
		return nil, err
	}
	add("created", "联系人创建", "", createdAt, "derived", 0)
	if firstMsg.Valid {
		add("first_message", "第一次聊天", "", firstMsg.String, "derived", 0)
	}
	if lastMsg.Valid {
		add("last_message", "最近一次聊天", fmt.Sprintf("累计 %d 条消息记录", msgCount), lastMsg.String, "derived", 0)
	}

	// 2. 画像变更历史（含手动编辑、手动补充、合并快照）
	rows, err := db.Query(`SELECT COALESCE(change_summary,''), COALESCE(created_at,'') FROM profile_history
		WHERE contact_id = ? ORDER BY id DESC LIMIT 60`, contactID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var summary, ts string
		if err := rows.Scan(&summary, &ts); err != nil {
			rows.Close()
			return nil, err
		}
		title := "画像更新"
		if s := strings.TrimSpace(summary); s != "" {
			if strings.HasPrefix(s, "手动编辑") {
				title = "手动编辑画像"
			} else if strings.HasPrefix(s, "手动补充") {
				title = "手动补充信息"
			} else if strings.HasPrefix(s, "合并联系人") {
				title = "合并联系人"
			}
		}
		add("profile", title, summary, ts, "derived", 0)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 3. 合并记录（作为目标 / 作为源）
	rows, err = db.Query(`SELECT source_name, COALESCE(undone_at,''), COALESCE(created_at,'')
		FROM merge_log WHERE target_id = ? ORDER BY id DESC LIMIT 30`, contactID)
	if err == nil {
		for rows.Next() {
			var srcName, undoneAt, ts string
			if err := rows.Scan(&srcName, &undoneAt, &ts); err != nil {
				continue
			}
			detail := fmt.Sprintf("「%s」并入本联系人", srcName)
			if undoneAt != "" {
				detail += "（已撤销）"
			}
			add("merge", "合并联系人", detail, ts, "derived", 0)
		}
		rows.Close()
	}
	rows, err = db.Query(`SELECT target_name, COALESCE(undone_at,''), COALESCE(created_at,'')
		FROM merge_log WHERE source_id = ? ORDER BY id DESC LIMIT 30`, contactID)
	if err == nil {
		for rows.Next() {
			var tgtName, undoneAt, ts string
			if err := rows.Scan(&tgtName, &undoneAt, &ts); err != nil {
				continue
			}
			detail := fmt.Sprintf("本联系人并入「%s」", tgtName)
			if undoneAt != "" {
				detail += "（已撤销）"
			}
			add("merged_into", "被合并", detail, ts, "derived", 0)
		}
		rows.Close()
	}

	// 4. 手动记录的事件（改名、改备注、用户自定义）；表不存在时静默跳过
	rows, err = db.Query(`SELECT id, kind, title, detail, event_time FROM contact_events
		WHERE contact_id = ? ORDER BY id DESC LIMIT 100`, contactID)
	if err == nil {
		for rows.Next() {
			var id int64
			var kind, title, detail, ts string
			if err := rows.Scan(&id, &kind, &title, &detail, &ts); err != nil {
				continue
			}
			add(kind, title, detail, ts, "record", id)
		}
		rows.Close()
	}

	// 排序：时间倒序，时间无法解析的排最后（保持派生顺序）
	sort.SliceStable(items, func(i, j int) bool {
		ti, oki := parseTimeLoose(items[i].RawTime)
		tj, okj := parseTimeLoose(items[j].RawTime)
		if !oki || !okj {
			return oki && !okj
		}
		return ti.After(tj)
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

// parseTimeLoose 宽松解析库里的各种时间格式
func parseTimeLoose(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// truncateRunes 按 rune 截断，超长补省略号
func truncateRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}
