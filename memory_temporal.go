package main

// ═══════════════════════════════════════════════════════════════════════════
// §10.3 P1：Temporal Memory 时间线（PERSONAL RELATIONSHIP OS 3.0）
//
// 蓝图：历史事实必须能看到 value / valid_from / valid_until / first_seen /
// last_confirmed / superseded_by——例如「职业：销售 2023-2025」→「职业：创业者 2025-now」。
//
// 现状：profile_facts 已具备全部时效列（v19 迁移），facts.go 的 supersede 路径已写
// valid_until/superseded_by。缺的是**把这些散行的历史按事实槽（type,key）串成时间线**的
// 只读视图。本文件做且仅做这一件事：把一个事实槽的当前值 + 全部被取代历史，按生效时间
// 确定性排序，输出可核对的时间线。不新增表、不改数据、不调 LLM、绝不自动删除。
//
// 单一来源纪律：时间线完全由 profile_facts 既有行推导，不另建事实存储；排序确定性
// （valid_from → first_seen → id 升序），同槽多 active 异常值也如实呈现、不静默。
// ═══════════════════════════════════════════════════════════════════════════

import (
	"database/sql"
	"sort"
	"strings"
)

// TemporalFact 时间线上的一个事实版本（一条 profile_facts 行的时效切片）。
type TemporalFact struct {
	FactID          int64  `json:"fact_id"`
	Value           string `json:"value"`
	Status          string `json:"status"` // active/confirmed（当前）| superseded/retired（历史）
	ValidFrom       string `json:"valid_from"`
	ValidUntil      string `json:"valid_until"` // 空=仍然生效（now）
	FirstSeen       string `json:"first_seen"`
	LastConfirmedAt string `json:"last_confirmed_at"`
	SupersededBy    *int64 `json:"superseded_by,omitempty"`
	Current         bool   `json:"current"` // 是否是该槽位的当前值
}

// FactTimeline 一个事实槽（contact + type + key）的完整时间线。
type FactTimeline struct {
	ContactID int64          `json:"contact_id"`
	FactType  string         `json:"fact_type"`
	FactKey   string         `json:"fact_key"`
	Slots     []TemporalFact `json:"slots"` // 按生效先后升序；当前值恒在末位
}

// BuildFactTimeline 组装某联系人某事实槽（type[,key]）的时间线：
// 当前态（active/confirmed/verified/inferred）+ 历史态（superseded/retired）全部纳入，
// 按 (valid_from→first_seen→id) 升序排列，当前值置于末位并标 Current=true。
// 只读、无副作用。表缺失或无数据返回空时间线（不报错、不 500）。
func BuildFactTimeline(db *sql.DB, contactID int64, factType, factKey string) (*FactTimeline, error) {
	if err := ensureFactsSchema(db); err != nil {
		return nil, err
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "profile_facts") {
		return &FactTimeline{ContactID: contactID, FactType: factType, FactKey: factKey}, nil
	}
	rows, err := db.Query(
		`SELECT id, fact_value, status, valid_from, valid_until, first_seen, last_confirmed_at, superseded_by
		 FROM profile_facts
		 WHERE contact_id=? AND fact_type=? AND fact_key=?
		 ORDER BY id ASC`, contactID, factType, factKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tl := &FactTimeline{ContactID: contactID, FactType: factType, FactKey: factKey}
	for rows.Next() {
		var tf TemporalFact
		var sup sql.NullInt64
		if err := rows.Scan(&tf.FactID, &tf.Value, &tf.Status, &tf.ValidFrom, &tf.ValidUntil,
			&tf.FirstSeen, &tf.LastConfirmedAt, &sup); err != nil {
			continue
		}
		if sup.Valid {
			s := sup.Int64
			tf.SupersededBy = &s
		}
		tf.Current = isCurrentStatus(tf.Status)
		tl.Slots = append(tl.Slots, tf)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortTimeline(tl.Slots)
	return tl, nil
}

// isCurrentStatus 判定一个 status 是否属于「当前态」（非历史）。
func isCurrentStatus(status string) bool {
	switch status {
	case "active", "confirmed", "verified", "inferred":
		return true
	default:
		return false
	}
}

// sortTimeline 确定性排序：当前值恒在末位；其余按生效先后升序。
// 次序键：valid_from（空视为最早）→ first_seen → id。稳定排序保证同键按 id 有序。
func sortTimeline(slots []TemporalFact) {
	sort.SliceStable(slots, func(i, j int) bool {
		a, b := slots[i], slots[j]
		// 当前值排到最后。
		if a.Current != b.Current {
			return !a.Current
		}
		if av, bv := effTime(a), effTime(b); av != bv {
			return av < bv
		}
		return a.FactID < b.FactID
	})
}

// effTime 取一个版本的可比较生效时刻：优先 valid_from，回落 first_seen。空串视为最小。
func effTime(t TemporalFact) string {
	if s := strings.TrimSpace(t.ValidFrom); s != "" {
		return s
	}
	return strings.TrimSpace(t.FirstSeen)
}

// ensureFactsSchema 保证 profile_facts 存在（懒建，幂等自持 dbMu）。
// 迁移已建表；此处仅为「测试回卷 user_version / 极端未迁移库」兜底，避免只读时间线视图报错。
// 只建 profile_facts（时间线唯一依赖），不碰证据表，以免与迁移版列集不一致。
func ensureFactsSchema(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS profile_facts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		contact_id INTEGER NOT NULL,
		fact_type TEXT NOT NULL,
		fact_key TEXT NOT NULL DEFAULT '',
		fact_value TEXT NOT NULL,
		source TEXT NOT NULL DEFAULT 'profile',
		confidence REAL NOT NULL DEFAULT 0.6,
		status TEXT NOT NULL DEFAULT 'active',
		first_seen TEXT NOT NULL DEFAULT '',
		last_seen TEXT NOT NULL DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		source_type TEXT NOT NULL DEFAULT 'ai',
		valid_from TEXT NOT NULL DEFAULT '',
		valid_until TEXT NOT NULL DEFAULT '',
		last_confirmed_at TEXT NOT NULL DEFAULT '',
		superseded_by INTEGER,
		confidence_type TEXT NOT NULL DEFAULT 'inferred',
		evidence_strength REAL NOT NULL DEFAULT 0
	)`)
	return err
}
