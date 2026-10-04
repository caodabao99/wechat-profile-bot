package main

// 跨联系人关系图谱（Phase 3）。
//
// 从已有 profile_facts 纯 SQL 派生人际关联：
//   - shared_location: 两人在画像里出现相同城市/地区
//   - shared_interest: 两人在画像里出现相同兴趣
//   - shared_occupation: 两人在画像里出现相同职业
//   - mentioned_name:  A 的画像事实中提到了 B 的名字
//
// 设计要点：
//   - 无 LLM 调用，零额外成本
//   - 派生表模式：先 DELETE 全表再 INSERT（幂等自愈，同 daily_metrics）
//   - contact_a < contact_b 去对称化（每对只存一条）
//   - 上限 500 条防爆（大量同名 fact_value 会爆炸）

import (
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// ConnectionView 一条关系连线（API 返回用）。
type ConnectionView struct {
	ID             int64   `json:"id"`
	ContactA       int64   `json:"contactA"`
	NameA          string  `json:"nameA"`
	ContactB       int64   `json:"contactB"`
	NameB          string  `json:"nameB"`
	ConnectionType string  `json:"connectionType"`
	Detail         string  `json:"detail"`
	Confidence     float64 `json:"confidence"`
}

// RebuildAllConnections 重建全量关系图谱。返回写入的总条数。
func RebuildAllConnections(db *sql.DB) (int, error) {
	now := time.Now().Format(time.RFC3339)

	type connRow struct {
		a, b   int64
		ctype  string
		detail string
		conf   float64
	}
	seen := make(map[string]bool)
	var rows []connRow
	add := func(a, b int64, ctype, detail string, conf float64) {
		if a > b {
			a, b = b, a
		}
		if a == b {
			return
		}
		key := fmt.Sprintf("%d-%d-%s", a, b, ctype)
		if seen[key] {
			return
		}
		seen[key] = true
		rows = append(rows, connRow{a, b, ctype, detail, conf})
	}

	// 采集阶段先把结果读进内存再写库：同一连接上边迭代 rows 边 Exec
	// 会死锁（连接池只有 1 个可用连接）。
	dbMu.Lock()
	defer dbMu.Unlock()

	// 1) 同类 fact_value 交叉匹配（location / interest / occupation）
	for _, factType := range []string{"location", "interest", "occupation"} {
		connType := "shared_" + factType
		confidence := 0.5
		if factType == "location" {
			confidence = 0.7
		}

		res, err := db.Query(
			`SELECT a.contact_id, b.contact_id, a.fact_value
			 FROM profile_facts a
			 JOIN profile_facts b ON a.fact_value = b.fact_value
			 WHERE a.contact_id < b.contact_id
			   AND a.fact_type = ? AND b.fact_type = ?
			   AND a.status = 'active' AND b.status = 'active'
			   AND TRIM(a.fact_value) != ''
			 LIMIT 200`, factType, factType)
		if err != nil {
			slog.Warn("关系图谱：查询 shared 事实失败", "type", connType, "err", err)
			continue
		}
		for res.Next() {
			var ca, cb int64
			var val string
			if err := res.Scan(&ca, &cb, &val); err != nil {
				continue
			}
			add(ca, cb, connType, val, confidence)
		}
		res.Close()
	}

	// 2) mentioned_name: A 的某条 fact_value 包含 B 的名字（或反之）
	//    加载活跃联系人名字列表（截断到 200 防爆炸）
	names := map[int64]string{}
	nameRows, err := db.Query(`SELECT id, name FROM contacts WHERE merged_into IS NULL LIMIT 200`)
	if err == nil {
		for nameRows.Next() {
			var id int64
			var name string
			if err := nameRows.Scan(&id, &name); err == nil && strings.TrimSpace(name) != "" {
				names[id] = strings.TrimSpace(name)
			}
		}
		nameRows.Close()
	}

	if len(names) > 0 {
		// 查所有 active facts
		var facts []struct {
			cid int64
			val string
		}
		factRows, err := db.Query(
			`SELECT contact_id, fact_value FROM profile_facts WHERE status='active' AND TRIM(fact_value) != ''`)
		if err == nil {
			for factRows.Next() {
				var f struct {
					cid int64
					val string
				}
				if err := factRows.Scan(&f.cid, &f.val); err == nil {
					facts = append(facts, f)
				}
			}
			factRows.Close()
		}

		// 在 Go 层做 substring check（比 SQL LIKE 灵活，支持名字长度过滤）
		for _, f := range facts {
			for otherID, otherName := range names {
				if otherID == f.cid {
					continue
				}
				// 名字至少 2 个字符才做匹配，避免单字误匹配
				if len([]rune(otherName)) < 2 {
					continue
				}
				if strings.Contains(f.val, otherName) {
					add(f.cid, otherID, "mentioned_name", otherName, 0.8)
				}
			}
		}
	}

	// 硬上限 500 防爆炸：按可信度降序截断，保住最有用的连线
	if len(rows) > 500 {
		slog.Info("关系图谱：超过 500 条，按可信度截断", "total", len(rows))
		sort.Slice(rows, func(i, j int) bool { return rows[i].conf > rows[j].conf })
		rows = rows[:500]
	}

	// 3) 幂等写入：先清空全表再全量插（派生表，自愈重建，同 daily_metrics 模式）
	if _, err := db.Exec(`DELETE FROM contact_connections`); err != nil {
		return 0, fmt.Errorf("清空 contact_connections 失败: %w", err)
	}
	total := 0
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT OR IGNORE INTO contact_connections (contact_a, contact_b, connection_type, detail, confidence, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			r.a, r.b, r.ctype, r.detail, r.conf, now); err == nil {
			total++
		}
	}
	return total, nil
}

// ListConnections 查询关系连线列表。contactID > 0 时只看该联系人。
func ListConnections(db *sql.DB, contactID int64, limit int) ([]ConnectionView, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	dbMu.Lock()
	defer dbMu.Unlock()

	query := `SELECT cc.id, cc.contact_a, COALESCE(ca.remark, ca.name, ''),
	                   cc.contact_b, COALESCE(cb.remark, cb.name, ''),
	                   cc.connection_type, cc.detail, cc.confidence
	            FROM contact_connections cc
	            LEFT JOIN contacts ca ON ca.id = cc.contact_a
	            LEFT JOIN contacts cb ON cb.id = cc.contact_b`
	var args []interface{}
	if contactID > 0 {
		query += ` WHERE cc.contact_a = ? OR cc.contact_b = ?`
		args = append(args, contactID, contactID)
	}
	query += ` ORDER BY cc.confidence DESC LIMIT ?`
	args = append(args, limit)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ConnectionView{}
	for rows.Next() {
		var v ConnectionView
		if err := rows.Scan(&v.ID, &v.ContactA, &v.NameA, &v.ContactB, &v.NameB,
			&v.ConnectionType, &v.Detail, &v.Confidence); err != nil {
			continue
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
