package main

// Action Ledger 行动账本（蓝图 §5 P1）。
//
// 复用现有 relationship_action_suggestions / suggestion_outcomes 不重造，本文件只新增一张
// 「跨来源 · 全生命周期」的行动账本 relationship_action_log，承载：
//   - §5.2 六来源行动记录：decision / coach / goal / project / calendar / manual；
//   - §5.1 八态生命周期：generated → viewed → accepted →（deferred）→ acted → completed，
//     旁支 dismissed / expired；
//   - §5.3 结果与来源分离：outcome 记极性（positive/neutral/negative/unknown），
//     outcome_provenance 记来源（''=未定 / estimated=系统估算 / confirmed=用户手工确认），
//     系统估算永不冒充用户确认。
//
// 本表记录的是「真实发生过的事」，不可从 messages/profile 派生重建，故属 audit：参与备份恢复、
// 按 contact_id 级联清理（见 registry.go / cleanup.go 同步登记）。
//
// 锁纪律：与 ai_cache.go / followup.go 一致——自锁、单层 dbMu、持锁内先查表存在再操作；
// 调用点必须处于锁外，绝不嵌套 dbMu。

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// 生命周期状态（§5.1）。
const (
	ActionStatusGenerated = "generated"
	ActionStatusViewed    = "viewed"
	ActionStatusAccepted  = "accepted"
	ActionStatusDeferred  = "deferred"
	ActionStatusDismissed = "dismissed"
	ActionStatusActed     = "acted"
	ActionStatusCompleted = "completed"
	ActionStatusExpired   = "expired"
)

// 结果极性（§5.3）。
const (
	ActionOutcomePositive = "positive"
	ActionOutcomeNeutral  = "neutral"
	ActionOutcomeNegative = "negative"
	ActionOutcomeUnknown  = "unknown"
)

// 结果来源（§5.3，「不能混淆」）：系统估算 vs 用户确认。
const (
	ActionProvenanceEstimated = "estimated"
	ActionProvenanceConfirmed = "confirmed"
)

const (
	actionTextMaxLen = 500 // action_text 按 rune 截断上限
	actionRefMaxLen  = 120 // source_ref 按 rune 截断上限
)

var (
	validActionSources = map[string]bool{
		"decision": true, "coach": true, "goal": true,
		"project": true, "calendar": true, "manual": true,
	}
	validActionStatuses = map[string]bool{
		ActionStatusGenerated: true, ActionStatusViewed: true, ActionStatusAccepted: true,
		ActionStatusDeferred: true, ActionStatusDismissed: true, ActionStatusActed: true,
		ActionStatusCompleted: true, ActionStatusExpired: true,
	}
	validActionOutcomes = map[string]bool{
		ActionOutcomePositive: true, ActionOutcomeNeutral: true,
		ActionOutcomeNegative: true, ActionOutcomeUnknown: true,
	}
)

// ActionLogEntry 行动账本的一条记录（读出视图，供 context / API 消费）。
type ActionLogEntry struct {
	ID                int64  `json:"id"`
	ContactID         int64  `json:"contact_id"`
	Source            string `json:"source"`
	SourceRef         string `json:"source_ref"`
	ActionType        string `json:"action_type"`
	ActionText        string `json:"action_text"`
	Status            string `json:"status"`
	DeferredUntil     string `json:"deferred_until"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
	ActedAt           string `json:"acted_at"`
	Outcome           string `json:"outcome"`
	OutcomeProvenance string `json:"outcome_provenance"`
	OutcomeObservedAt string `json:"outcome_observed_at"`
	OutcomeDays       int    `json:"outcome_days"`
	OutcomeNote       string `json:"outcome_note"`
}

// LogAction 新增一条行动记录（生命周期起点 status=generated）。
// 校验 source 合法；action_text/source_ref 按 rune 截断；联系人必须存在。返回新行 id。
func LogAction(db *sql.DB, contactID int64, source, sourceRef, actionType, actionText string, now time.Time) (int64, error) {
	if contactID <= 0 {
		return 0, fmt.Errorf("contact_id 非法")
	}
	if !validActionSources[source] {
		return 0, fmt.Errorf("无效的行动来源: %q", source)
	}
	sourceRef = clipRunes(strings.TrimSpace(sourceRef), actionRefMaxLen)
	actionType = clipRunes(strings.TrimSpace(actionType), 80)
	actionText = clipRunes(strings.TrimSpace(actionText), actionTextMaxLen)
	nowStr := now.Format(time.RFC3339)

	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "relationship_action_log") {
		return 0, fmt.Errorf("行动账本表尚未就绪")
	}
	var exists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM contacts WHERE id = ?`, contactID).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, fmt.Errorf("联系人不存在")
	}
	res, err := db.Exec(`
		INSERT INTO relationship_action_log
			(contact_id, source, source_ref, action_type, action_text, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'generated', ?, ?)`,
		contactID, source, sourceRef, actionType, actionText, nowStr, nowStr)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, nil
}

// TransitionActionStatus 迁移一条行动记录的生命周期状态（§5.1）。
// 校验目标状态合法；进入 acted/completed 时若 acted_at 为空则补记；进入 deferred 时可携带
// deferred_until（YYYY-MM-DD）。不回退已 acted 的记录到 acted 之前的状态（防时间线倒错）。
func TransitionActionStatus(db *sql.DB, id int64, to, deferredUntil string, now time.Time) error {
	if id <= 0 {
		return fmt.Errorf("行动记录 id 非法")
	}
	if !validActionStatuses[to] {
		return fmt.Errorf("无效的生命周期状态: %q", to)
	}
	deferredUntil = strings.TrimSpace(deferredUntil)
	if deferredUntil != "" && !isValidYMD(deferredUntil) {
		return fmt.Errorf("延期截止日期格式应为 YYYY-MM-DD")
	}
	nowStr := now.Format(time.RFC3339)

	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "relationship_action_log") {
		return fmt.Errorf("行动账本表尚未就绪")
	}
	var cur, actedAt string
	if err := db.QueryRow(`SELECT status, acted_at FROM relationship_action_log WHERE id = ?`, id).
		Scan(&cur, &actedAt); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("行动记录不存在")
		}
		return err
	}
	// 已进入 acted/completed 的记录，禁止回退到更早的状态（ acted 之前 的四态）。
	if (cur == ActionStatusActed || cur == ActionStatusCompleted) &&
		(to == ActionStatusGenerated || to == ActionStatusViewed ||
			to == ActionStatusAccepted || to == ActionStatusDeferred) {
		return fmt.Errorf("已执行的行动不可回退到执行前状态")
	}
	// acted_at：首次进入 acted/completed 时补记，之后保持不变（记录真实执行时刻）。
	newActed := actedAt
	if (to == ActionStatusActed || to == ActionStatusCompleted) && actedAt == "" {
		newActed = nowStr
	}
	du := deferredUntil
	if to != ActionStatusDeferred {
		du = "" // 离开 deferred 态即清空延期标记
	}
	if _, err := db.Exec(`
		UPDATE relationship_action_log
		SET status = ?, acted_at = ?, deferred_until = ?, updated_at = ?
		WHERE id = ?`,
		to, newActed, du, nowStr, id); err != nil {
		return err
	}
	return nil
}

// SetActionOutcome 记录结果（§5.3）。outcome=极性、provenance=来源，二者不可混淆：
//   - provenance=confirmed 表示用户手工确认，estimated 表示系统估算（Phase 2b 自动观察）；
//   - 铁律：一旦某条已被用户确认（confirmed），禁止用系统估算（estimated）覆盖——估算永不
//     冒充用户的事实确认；同 provenance 允许刷新。
//
// 记 outcome_observed_at，并按 acted_at→now 计算 outcome_days（观察窗口天数）。
func SetActionOutcome(db *sql.DB, id int64, outcome, provenance, note string, now time.Time) error {
	if id <= 0 {
		return fmt.Errorf("行动记录 id 非法")
	}
	if !validActionOutcomes[outcome] {
		return fmt.Errorf("无效的结果极性: %q", outcome)
	}
	if provenance != ActionProvenanceEstimated && provenance != ActionProvenanceConfirmed {
		return fmt.Errorf("无效的结果来源（须为 estimated 或 confirmed）: %q", provenance)
	}
	note = clipRunes(strings.TrimSpace(note), actionTextMaxLen)
	nowStr := now.Format(time.RFC3339)

	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "relationship_action_log") {
		return fmt.Errorf("行动账本表尚未就绪")
	}
	var curProv, actedAt string
	if err := db.QueryRow(`SELECT outcome_provenance, acted_at FROM relationship_action_log WHERE id = ?`, id).
		Scan(&curProv, &actedAt); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("行动记录不存在")
		}
		return err
	}
	// 不能混淆：系统估算不得覆盖用户确认。
	if curProv == ActionProvenanceConfirmed && provenance == ActionProvenanceEstimated {
		return fmt.Errorf("该结果已由用户确认，系统估算不得覆盖")
	}
	days := 0
	if actedAt != "" {
		if t, err := time.Parse(time.RFC3339, actedAt); err == nil {
			if d := int(now.Sub(t).Hours() / 24); d > 0 {
				days = d
			}
		}
	}
	if _, err := db.Exec(`
		UPDATE relationship_action_log
		SET outcome = ?, outcome_provenance = ?, outcome_observed_at = ?, outcome_days = ?, outcome_note = ?, updated_at = ?
		WHERE id = ?`,
		outcome, provenance, nowStr, days, note, nowStr, id); err != nil {
		return err
	}
	return nil
}

// ListActionLog 读取某联系人的行动账本（按 id 逆序，最近在前），最多 limit 条（<=0 则不限）。
func ListActionLog(db *sql.DB, contactID int64, limit int) ([]ActionLogEntry, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "relationship_action_log") {
		return nil, nil
	}
	q := `SELECT id, contact_id, source, source_ref, action_type, action_text, status,
		deferred_until, created_at, updated_at, acted_at, outcome, outcome_provenance,
		outcome_observed_at, outcome_days, outcome_note
		FROM relationship_action_log WHERE contact_id = ? ORDER BY id DESC`
	args := []any{contactID}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ActionLogEntry{}
	for rows.Next() {
		var e ActionLogEntry
		if err := rows.Scan(&e.ID, &e.ContactID, &e.Source, &e.SourceRef, &e.ActionType,
			&e.ActionText, &e.Status, &e.DeferredUntil, &e.CreatedAt, &e.UpdatedAt,
			&e.ActedAt, &e.Outcome, &e.OutcomeProvenance, &e.OutcomeObservedAt,
			&e.OutcomeDays, &e.OutcomeNote); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetActionLog 按 id 读回单条行动记录（不存在返回 nil,nil）。供 API 越权校验与回显刷新后的行。
func GetActionLog(db *sql.DB, id int64) (*ActionLogEntry, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "relationship_action_log") {
		return nil, nil
	}
	var e ActionLogEntry
	err := db.QueryRow(`SELECT id, contact_id, source, source_ref, action_type, action_text, status,
		deferred_until, created_at, updated_at, acted_at, outcome, outcome_provenance,
		outcome_observed_at, outcome_days, outcome_note
		FROM relationship_action_log WHERE id = ?`, id).Scan(&e.ID, &e.ContactID, &e.Source, &e.SourceRef,
		&e.ActionType, &e.ActionText, &e.Status, &e.DeferredUntil, &e.CreatedAt, &e.UpdatedAt,
		&e.ActedAt, &e.Outcome, &e.OutcomeProvenance, &e.OutcomeObservedAt, &e.OutcomeDays, &e.OutcomeNote)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// clipRunes 按 rune 截断（保中文不产生半个字符 / U+FFFD），超长保留前 max 个 rune。
func clipRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if r := []rune(s); len(r) > max {
		return string(r[:max])
	}
	return s
}
