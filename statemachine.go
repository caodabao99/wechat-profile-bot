package main

// Relationship State Machine（Personal Relationship OS 2.0 · Phase 3 / P2）。
//
// 在既有 intimacy / trend / alert / health 之上加一层「统一、可解释」的关系状态：
//   base_state（亲疏阶梯）× dynamic_state（当下动态），二者正交。
//
// 铁律落实：
//   - 复用 ComputeHealth 的批量产出（其内部已复用 daily-metrics/亲密度/趋势/情绪聚合），
//     绝不新建第二套消息统计（规格 6.1）。
//   - 只有真实跨越阈值才改变状态并写 relationship_state_history（规格 6.2）；
//     每次刷新只更新快照与 computed_at，不产事件、不动 changed_at。
//   - 数据不足诚实回落 unknown，绝不把「猜」当成事实。
//
// relationship_state / relationship_state_history 均为派生缓存：懒 ensure* 建表、不 bump
// user_version、入 backup.go derivedTables（不入备份、恢复末尾清空、访问时缺则自愈重建）。

import (
	"database/sql"
	"fmt"
	"time"
)

// 状态枚举（与规格六一致）。
const (
	baseUnknown    = "unknown"
	baseIntroduced = "introduced"
	baseFamiliar   = "familiar"
	baseStable     = "stable"
	baseClose      = "close"
	baseCore       = "core"

	dynStable       = "stable"
	dynWarming      = "warming"
	dynCooling      = "cooling"
	dynAtRisk       = "at_risk"
	dynDormant      = "dormant"
	dynReconnecting = "reconnecting"
)

// deriveBaseState 纯函数：亲密度(0-100)+趋势 → 基础状态阶梯（确定性、可脱库单测）。
// 无任何互动数据（趋势 new 且亲密度 0）诚实回落 unknown。
func deriveBaseState(intimacy int, trendState string) string {
	if intimacy <= 0 && trendState == "new" {
		return baseUnknown
	}
	switch {
	case intimacy >= 80:
		return baseCore
	case intimacy >= 60:
		return baseClose
	case intimacy >= 40:
		return baseStable
	case intimacy >= 15:
		return baseFamiliar
	default:
		return baseIntroduced
	}
}

// deriveDynamicState 纯函数：趋势 + 前瞻预警 + 上一动态态 → 当下动态态。
// 优先级：dormant > reconnecting > at_risk > cooling > warming > stable。
// reconnecting 借状态机自身记忆（上一态曾 dormant、当前恢复互动）判定，不引入新统计。
func deriveDynamicState(trendState, alert, prevDynamic string) string {
	if trendState == "dormant" {
		return dynDormant
	}
	if prevDynamic == dynDormant && (trendState == "warming" || trendState == "stable") {
		return dynReconnecting
	}
	if alert == "urgent" || alert == "watching" {
		return dynAtRisk
	}
	switch trendState {
	case "cooling":
		return dynCooling
	case "warming":
		return dynWarming
	default:
		return dynStable
	}
}

// stateReason 纯函数：拼接可读转态理由（供解释与前端展示）。
func stateReason(intimacy int, base, trendState, alert string) string {
	s := fmt.Sprintf("亲密度 %d → %s", intimacy, base)
	switch trendState {
	case "warming":
		s += "；互动升温"
	case "cooling":
		s += "；互动降温"
	case "dormant":
		s += "；长期沉寂"
	case "new":
		s += "；尚无互动"
	default:
		s += "；互动平稳"
	}
	if alert == "urgent" {
		s += "；断点风险高"
	} else if alert == "watching" {
		s += "；需留意降温"
	}
	return s
}

// ensureRelationshipState 懒建两张派生表（幂等 DDL，自持 dbMu，不 bump user_version）。
func ensureRelationshipState(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS relationship_state (
		contact_id INTEGER PRIMARY KEY,
		base_state TEXT NOT NULL DEFAULT '',
		dynamic_state TEXT NOT NULL DEFAULT '',
		intimacy INTEGER NOT NULL DEFAULT 0,
		trend_state TEXT NOT NULL DEFAULT '',
		alert TEXT NOT NULL DEFAULT 'none',
		health INTEGER NOT NULL DEFAULT 0,
		reason TEXT NOT NULL DEFAULT '',
		changed_at TEXT NOT NULL DEFAULT '',
		computed_at TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS relationship_state_history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		contact_id INTEGER NOT NULL,
		prev_base TEXT NOT NULL DEFAULT '',
		prev_dynamic TEXT NOT NULL DEFAULT '',
		new_base TEXT NOT NULL,
		new_dynamic TEXT NOT NULL,
		reason TEXT NOT NULL DEFAULT '',
		changed_at TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return err
	}
	_, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_relationship_state_history_contact ON relationship_state_history(contact_id, changed_at DESC)`)
	return err
}

// RefreshRelationshipStates 依据 ComputeHealth 重算全体联系人状态并落库。
// 返回 (本轮跨越阈值的状态变更数, 评估的联系人总数, err)。
// ComputeHealth 内部自锁，必须在取本函数锁之前调用（dbMu 不可重入）。
func RefreshRelationshipStates(db *sql.DB, now time.Time, windowDays int) (changed, total int, err error) {
	dash, err := ComputeHealth(db, now, windowDays)
	if err != nil {
		return 0, 0, err
	}
	if err = ensureRelationshipState(db); err != nil {
		return 0, 0, err
	}
	nowStr := now.Format(time.RFC3339)

	dbMu.Lock()
	defer dbMu.Unlock()
	for _, it := range dash.Items {
		var prevBase, prevDyn string
		if err := db.QueryRow(`SELECT base_state, dynamic_state FROM relationship_state WHERE contact_id=?`, it.ContactID).
			Scan(&prevBase, &prevDyn); err != nil && err != sql.ErrNoRows {
			return changed, total, err
		}
		base := deriveBaseState(it.Intimacy, it.TrendState)
		dyn := deriveDynamicState(it.TrendState, it.Alert, prevDyn)
		reason := stateReason(it.Intimacy, base, it.TrendState, it.Alert)
		total++

		// 仅当 (base,dynamic) 相对存量真实跨越阈值才产历史事件。
		if prevBase != base || prevDyn != dyn {
			changed++
			if _, err := db.Exec(`INSERT INTO relationship_state_history
				(contact_id, prev_base, prev_dynamic, new_base, new_dynamic, reason, changed_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				it.ContactID, prevBase, prevDyn, base, dyn, reason, nowStr); err != nil {
				return changed, total, err
			}
		}
		// upsert 当前快照；changed_at 仅在状态跨越阈值时前进（用 excluded 与存量比较自证），
		// 否则保留旧 changed_at——即使每次刷新都跑到这里。
		if _, err := db.Exec(`INSERT INTO relationship_state
			(contact_id, base_state, dynamic_state, intimacy, trend_state, alert, health, reason, changed_at, computed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(contact_id) DO UPDATE SET
			  base_state=excluded.base_state, dynamic_state=excluded.dynamic_state,
			  intimacy=excluded.intimacy, trend_state=excluded.trend_state,
			  alert=excluded.alert, health=excluded.health, reason=excluded.reason, computed_at=excluded.computed_at,
			  changed_at=CASE WHEN relationship_state.base_state!=excluded.base_state
			                     OR relationship_state.dynamic_state!=excluded.dynamic_state
			                  THEN excluded.changed_at ELSE relationship_state.changed_at END`,
			it.ContactID, base, dyn, it.Intimacy, it.TrendState, it.Alert, it.Health, reason, nowStr, nowStr); err != nil {
			return changed, total, err
		}
	}
	return changed, total, nil
}

// RelationshipStateView 状态展示体（供 API/前端）。
type RelationshipStateView struct {
	ContactID    int64  `json:"contactId"`
	Name         string `json:"name"`
	BaseState    string `json:"baseState"`
	DynamicState string `json:"dynamicState"`
	Intimacy     int    `json:"intimacy"`
	TrendState   string `json:"trendState"`
	Alert        string `json:"alert"`
	Health       int    `json:"health"`
	Reason       string `json:"reason"`
	ChangedAt    string `json:"changedAt"`
	ComputedAt   string `json:"computedAt"`
}

const stateSelectSQL = `SELECT rs.contact_id, COALESCE(c.name,''), rs.base_state, rs.dynamic_state,
		rs.intimacy, rs.trend_state, rs.alert, rs.health, rs.reason, rs.changed_at, rs.computed_at
		FROM relationship_state rs LEFT JOIN contacts c ON c.id=rs.contact_id `

func scanStates(rows *sql.Rows) ([]RelationshipStateView, error) {
	out := []RelationshipStateView{}
	for rows.Next() {
		var v RelationshipStateView
		if err := rows.Scan(&v.ContactID, &v.Name, &v.BaseState, &v.DynamicState,
			&v.Intimacy, &v.TrendState, &v.Alert, &v.Health, &v.Reason, &v.ChangedAt, &v.ComputedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// readStateLocked 读单联系人存量（无则返回 nil）。
func readStateLocked(db *sql.DB, contactID int64) (*RelationshipStateView, error) {
	rows, err := db.Query(stateSelectSQL+`WHERE rs.contact_id=?`, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scanStates(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	return &list[0], nil
}

// GetRelationshipState 读取某联系人的当前状态；缺失时先整板刷新一次再读（自愈）。
func GetRelationshipState(db *sql.DB, contactID int64) (*RelationshipStateView, error) {
	if err := ensureRelationshipState(db); err != nil {
		return nil, err
	}
	dbMu.Lock()
	v, err := readStateLocked(db, contactID)
	dbMu.Unlock()
	if err != nil {
		return nil, err
	}
	if v != nil {
		return v, nil
	}
	if _, _, err := RefreshRelationshipStates(db, time.Now(), 0); err != nil {
		return nil, err
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	if v, err := readStateLocked(db, contactID); err != nil {
		return nil, err
	} else if v != nil {
		return v, nil
	}
	return nil, sql.ErrNoRows
}

// ListRelationshipStates 返回全体联系人当前状态看板；为空时先整板刷新一次再读（自愈）。
func ListRelationshipStates(db *sql.DB) ([]RelationshipStateView, error) {
	if err := ensureRelationshipState(db); err != nil {
		return nil, err
	}
	dbMu.Lock()
	rows, err := db.Query(stateSelectSQL + `ORDER BY rs.health ASC, rs.intimacy DESC, rs.contact_id ASC`)
	if err != nil {
		dbMu.Unlock()
		return nil, err
	}
	list, serr := scanStates(rows)
	rows.Close()
	dbMu.Unlock()
	if serr != nil {
		return nil, serr
	}
	if len(list) == 0 {
		if _, _, err := RefreshRelationshipStates(db, time.Now(), 0); err != nil {
			return nil, err
		}
		dbMu.Lock()
		defer dbMu.Unlock()
		rows, err := db.Query(stateSelectSQL + `ORDER BY rs.health ASC, rs.intimacy DESC, rs.contact_id ASC`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return scanStates(rows)
	}
	return list, nil
}

// GetRelationshipStateHistory 返回某联系人的状态变迁历史（按时间倒序），供 Memory Replay / 趋势回看。
func GetRelationshipStateHistory(db *sql.DB, contactID int64, limit int) ([]map[string]interface{}, error) {
	if err := ensureRelationshipState(db); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	rows, err := db.Query(`SELECT prev_base, prev_dynamic, new_base, new_dynamic, reason, changed_at
		FROM relationship_state_history WHERE contact_id=? ORDER BY id DESC LIMIT ?`, contactID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var pb, pd, nb, nd, reason, changed string
		if err := rows.Scan(&pb, &pd, &nb, &nd, &reason, &changed); err != nil {
			return nil, err
		}
		out = append(out, map[string]interface{}{
			"prevBase": pb, "prevDynamic": pd, "newBase": nb, "newDynamic": nd,
			"reason": reason, "changedAt": changed,
		})
	}
	return out, rows.Err()
}
