package main

// v6.3 §P5 Memory Consolidation（记忆整合）。
//
// 问题：画像经多轮 AI 迭代后，profile_facts 可能堆积逻辑冲突/陈旧/近重复的记录：
//   - 冲突：同一联系人 occupation 有两条 active（"律师" vs "创业者"），因为 source_type='user'
//     的记录不参与自动 supersede，用户确认后画像又改，两条都留着。
//   - 陈旧：兴趣/性格/口头禅等集合型事实长期不再出现在画像中，但 status 仍是 active
//     （只有当次画像重建才把消失的降为 retired，如果画像没重建就一直挂着）。
//   - 近重复："跑步" 与 "喜欢跑步"、"篮球" 与 "打篮球"——UNIQUE 约束只看精确文本，
//     语义相同/包含关系不会被去重。
//
// P5 做三件事：
//   1. 确定性检测（不调 LLM），产出 ConsolidationProposal 列表。
//   2. 暴露 API 给前端 / 用户审阅。
//   3. 用户选择后原子执行：保留高可信 / 标 superseded / 合并近重复。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ---------- 数据结构 ----------

// ConsolidationKind 标识一条整合建议的类型。
type ConsolidationKind string

const (
	KindConflict  ConsolidationKind = "conflict" // 同 (type,key) 存在多条 active 且值矛盾
	KindStale     ConsolidationKind = "stale"    // 长时间未再确认
	KindDuplicate ConsolidationKind = "near_dup" // 文本近重复（子串/包含关系）
	// v7.0 §10.1 新增两类确定性检测（不自动删除，只生成 Memory Proposal）：
	KindLowEvidence    ConsolidationKind = "low_evidence"    // active 但证据薄弱（低 evidence_strength 且未确认）
	KindNeverConfirmed ConsolidationKind = "never_confirmed" // 长期从未被用户确认（last_confirmed_at 空）
)

// ConsolidationItem 一条具体的整合建议。
type ConsolidationItem struct {
	Kind        ConsolidationKind `json:"kind"`
	ContactID   int64             `json:"contact_id"`
	ContactName string            `json:"contact_name"`
	FactType    string            `json:"fact_type"`
	FactKey     string            `json:"fact_key"`
	Description string            `json:"description"`
	// 涉及的事实列表（按 id 升序）
	Facts []ConsolidationFact `json:"facts"`
	// 建议操作
	Suggestion string `json:"suggestion"`
}

// ConsolidationFact 建议中涉及的一条事实摘要。
type ConsolidationFact struct {
	ID         int64   `json:"id"`
	Value      string  `json:"fact_value"`
	Status     string  `json:"status"`
	Confidence float64 `json:"confidence"`
	SourceType string  `json:"source_type"`
	LastSeen   string  `json:"last_seen"`
}

// ConsolidationProposal 系统级别的整合提案（一次扫描的完整产出）。
type ConsolidationProposal struct {
	GeneratedAt string              `json:"generated_at"`
	Summary     ProposalSummary     `json:"summary"`
	Items       []ConsolidationItem `json:"items"`
}

type ProposalSummary struct {
	Conflicts      int `json:"conflicts"`
	Stales         int `json:"stales"`
	Duplicates     int `json:"duplicates"`
	LowEvidence    int `json:"low_evidence"`    // §10.1 证据薄弱
	NeverConfirmed int `json:"never_confirmed"` // §10.1 从未确认
}

// ---------- 阈值常量 ----------

const (
	// 单值型事实：同一 (contact_id, fact_type, fact_key) 只应有一条 active。
	singleValuedTypes = "occupation,location,closeness"
	// 陈旧判定天数：集合型事实 last_seen 距今 ≥ 此天数且从未确认。
	staleThresholdDays = 120
	// 近重复字符 bigram Jaccard 阈值（≥此值认为重复）。
	dupJaccardThreshold = 0.6
	// §10.1 证据薄弱：active 但 evidence_strength 低于此值（且未被用户确认）视为低证据。
	lowEvidenceThreshold = 0.34
	// §10.1 从未确认：last_confirmed_at 为空且 last_seen 距今 ≥ 此天数，视为长期未核实。
	neverConfirmedDays = 90
)

// ---------- 核心检测 ----------

// BuildConsolidationProposal 扫描所有联系人的 active 事实，检测三类问题。
// 确定性、无 LLM 调用，可安全在后台调度中执行。
func BuildConsolidationProposal(db *sql.DB, now time.Time) (*ConsolidationProposal, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	p := &ConsolidationProposal{GeneratedAt: now.Format(time.RFC3339)}

	// 冲突检测
	conflicts, err := detectConflictsLocked(db, now)
	if err != nil {
		return nil, fmt.Errorf("检测冲突: %w", err)
	}
	p.Items = append(p.Items, conflicts...)
	p.Summary.Conflicts = len(conflicts)

	// 陈旧检测
	stales, err := detectStaleLocked(db, now)
	if err != nil {
		return nil, fmt.Errorf("检测陈旧: %w", err)
	}
	p.Items = append(p.Items, stales...)
	p.Summary.Stales = len(stales)

	// 近重复检测
	dups, err := detectNearDuplicatesLocked(db, now)
	if err != nil {
		return nil, fmt.Errorf("检测近重复: %w", err)
	}
	p.Items = append(p.Items, dups...)
	p.Summary.Duplicates = len(dups)

	// 证据薄弱检测（§10.1）
	lowEv, err := detectLowEvidenceLocked(db, now)
	if err != nil {
		return nil, fmt.Errorf("检测低证据: %w", err)
	}
	p.Items = append(p.Items, lowEv...)
	p.Summary.LowEvidence = len(lowEv)

	// 从未确认检测（§10.1）
	never, err := detectNeverConfirmedLocked(db, now)
	if err != nil {
		return nil, fmt.Errorf("检测未确认: %w", err)
	}
	p.Items = append(p.Items, never...)
	p.Summary.NeverConfirmed = len(never)

	return p, nil
}

// detectConflictsLocked 找单值型事实中存在多条 active 且值不同的情况。
func detectConflictsLocked(db *sql.DB, now time.Time) ([]ConsolidationItem, error) {
	types := strings.Split(singleValuedTypes, ",")
	var items []ConsolidationItem
	for _, t := range types {
		rows, err := db.Query(`
			SELECT f.contact_id, c.name, f.fact_type, f.fact_key, COUNT(DISTINCT f.fact_value) AS n
			FROM profile_facts f JOIN contacts c ON c.id = f.contact_id
			WHERE f.status='active' AND f.fact_type=?
			GROUP BY f.contact_id, f.fact_key
			HAVING n > 1`, t)
		if err != nil {
			return nil, err
		}
		type conflict struct {
			cid  int64
			name string
			typ  string
			key  string
		}
		var found []conflict
		for rows.Next() {
			var cf conflict
			var n int
			if rows.Scan(&cf.cid, &cf.name, &cf.typ, &cf.key, &n) == nil {
				found = append(found, cf)
			}
		}
		rows.Close()
		for _, cf := range found {
			facts, err := scanFactsLocked(db, cf.cid, cf.typ, cf.key, "active")
			if err != nil {
				continue
			}
			items = append(items, ConsolidationItem{
				Kind:        KindConflict,
				ContactID:   cf.cid,
				ContactName: cf.name,
				FactType:    cf.typ,
				FactKey:     cf.key,
				Description: fmt.Sprintf("%s 的 %s 存在 %d 条互相矛盾的 active 事实", cf.name, cf.typ, len(facts)),
				Facts:       facts,
				Suggestion:  "保留最高置信的一条，其余标 superseded",
			})
		}
	}
	return items, nil
}

// detectStaleLocked 找集合型事实中 last_seen 过旧的 active 记录。
func detectStaleLocked(db *sql.DB, now time.Time) ([]ConsolidationItem, error) {
	cutoff := now.AddDate(0, 0, -staleThresholdDays).Format(time.RFC3339)
	rows, err := db.Query(`
		SELECT f.id, f.contact_id, c.name, f.fact_type, f.fact_key, f.fact_value,
		       f.confidence, f.source_type, f.last_seen
		FROM profile_facts f JOIN contacts c ON c.id = f.contact_id
		WHERE f.status='active' AND f.source_type != 'user'
		  AND f.last_seen != '' AND f.last_seen < ?
		ORDER BY f.last_seen ASC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 按 (contact_id, fact_type) 归组
	type groupKey struct {
		cid int64
		typ string
	}
	grouped := map[groupKey][]ConsolidationFact{}
	names := map[int64]string{}
	for rows.Next() {
		var id, cid int64
		var name, typ, key, val, srcType, lastSeen string
		var conf float64
		if rows.Scan(&id, &cid, &name, &typ, &key, &val, &conf, &srcType, &lastSeen) != nil {
			continue
		}
		gk := groupKey{cid, typ}
		grouped[gk] = append(grouped[gk], ConsolidationFact{
			ID: id, Value: val, Status: "active",
			Confidence: conf, SourceType: srcType, LastSeen: lastSeen,
		})
		names[cid] = name
	}

	var items []ConsolidationItem
	for gk, facts := range grouped {
		items = append(items, ConsolidationItem{
			Kind:        KindStale,
			ContactID:   gk.cid,
			ContactName: names[gk.cid],
			FactType:    gk.typ,
			Description: fmt.Sprintf("%s 的 %d 条 %s 已超过 %d 天未被画像再确认", names[gk.cid], len(facts), gk.typ, staleThresholdDays),
			Facts:       facts,
			Suggestion:  "标记为 stale，等待下次画像更新时自动清除或用户确认",
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].ContactID != items[j].ContactID {
			return items[i].ContactID < items[j].ContactID
		}
		return items[i].FactType < items[j].FactType
	})
	return items, nil
}

// detectNearDuplicatesLocked 找同一联系人同类型的事实里文本近重复的记录。
func detectNearDuplicatesLocked(db *sql.DB, now time.Time) ([]ConsolidationItem, error) {
	// 一次性拉全部 active 非用户事实，按 (contact_id, fact_type, fact_key) 分组
	rows, err := db.Query(`
		SELECT f.id, f.contact_id, c.name, f.fact_type, f.fact_key, f.fact_value,
		       f.confidence, f.source_type, f.last_seen
		FROM profile_facts f JOIN contacts c ON c.id = f.contact_id
		WHERE f.status='active' AND f.source_type != 'user'
		ORDER BY f.contact_id, f.fact_type, f.fact_key, f.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type groupKey struct {
		cid int64
		typ string
		key string
	}
	type factEntry struct {
		ConsolidationFact
		name string
	}
	grouped := map[groupKey][]factEntry{}
	for rows.Next() {
		var id, cid int64
		var name, typ, key, val, srcType, lastSeen string
		var conf float64
		if rows.Scan(&id, &cid, &name, &typ, &key, &val, &conf, &srcType, &lastSeen) != nil {
			continue
		}
		gk := groupKey{cid, typ, key}
		grouped[gk] = append(grouped[gk], factEntry{
			ConsolidationFact: ConsolidationFact{
				ID: id, Value: val, Status: "active",
				Confidence: conf, SourceType: srcType, LastSeen: lastSeen,
			},
			name: name,
		})
	}

	var items []ConsolidationItem
	for gk, facts := range grouped {
		if len(facts) < 2 {
			continue
		}
		// O(n²) 对每个分组内做配对比较
		found := map[int]bool{} // 标记已配对的 index
		for i := 0; i < len(facts); i++ {
			if found[i] {
				continue
			}
			var dupes []factEntry
			for j := i + 1; j < len(facts); j++ {
				if found[j] {
					continue
				}
				if isNearDuplicate(facts[i].Value, facts[j].Value) {
					dupes = append(dupes, facts[j])
					found[j] = true
				}
			}
			if len(dupes) > 0 {
				all := append([]factEntry{facts[i]}, dupes...)
				var cf []ConsolidationFact
				for _, e := range all {
					cf = append(cf, e.ConsolidationFact)
				}
				items = append(items, ConsolidationItem{
					Kind:        KindDuplicate,
					ContactID:   gk.cid,
					ContactName: facts[i].name,
					FactType:    gk.typ,
					FactKey:     gk.key,
					Description: fmt.Sprintf("%s 的 %s 有 %d 条近重复（%q ≈ %q）",
						facts[i].name, gk.typ, len(all), facts[i].Value, dupes[0].Value),
					Facts:      cf,
					Suggestion: "保留最短表述（或最高置信），其余标 superseded",
				})
				found[i] = true
			}
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].ContactID != items[j].ContactID {
			return items[i].ContactID < items[j].ContactID
		}
		return items[i].FactType < items[j].FactType
	})
	return items, nil
}

// detectLowEvidenceLocked 找 active 但证据薄弱的事实（§10.1 low evidence）。
// 条件：未被用户确认（source_type!='user'）且 evidence_strength 低于阈值。逐条产出建议。
func detectLowEvidenceLocked(db *sql.DB, now time.Time) ([]ConsolidationItem, error) {
	rows, err := db.Query(`
		SELECT f.id, f.contact_id, COALESCE(NULLIF(c.remark,''), c.name, ''), f.fact_type, f.fact_key, f.fact_value,
		       f.confidence, f.source_type, f.last_seen
		FROM profile_facts f JOIN contacts c ON c.id = f.contact_id
		WHERE f.status='active' AND f.source_type != 'user' AND f.evidence_strength < ?
		ORDER BY f.evidence_strength ASC, f.id`, lowEvidenceThreshold)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []ConsolidationItem
	for rows.Next() {
		var id, cid int64
		var name, typ, key, val, srcType, lastSeen string
		var conf float64
		if rows.Scan(&id, &cid, &name, &typ, &key, &val, &conf, &srcType, &lastSeen) != nil {
			continue
		}
		items = append(items, ConsolidationItem{
			Kind:        KindLowEvidence,
			ContactID:   cid,
			ContactName: name,
			FactType:    typ,
			FactKey:     key,
			Description: fmt.Sprintf("%s 的 %s=%q 证据薄弱（置信 %.2f、无足够直接支撑）", name, typ, val, conf),
			Facts:       []ConsolidationFact{{ID: id, Value: val, Status: "active", Confidence: conf, SourceType: srcType, LastSeen: lastSeen}},
			Suggestion:  "建议到 Memory Review 逐条核对：确认、否定或暂不处理（不自动删除）",
		})
	}
	return items, rows.Err()
}

// detectNeverConfirmedLocked 找长期从未被用户确认的事实（§10.1 never confirmed）。
// 条件：active、非用户来源、last_confirmed_at 为空且 last_seen 距今 ≥ neverConfirmedDays。
func detectNeverConfirmedLocked(db *sql.DB, now time.Time) ([]ConsolidationItem, error) {
	cutoff := now.AddDate(0, 0, -neverConfirmedDays).Format(time.RFC3339)
	rows, err := db.Query(`
		SELECT f.id, f.contact_id, COALESCE(NULLIF(c.remark,''), c.name, ''), f.fact_type, f.fact_key, f.fact_value,
		       f.confidence, f.source_type, f.last_seen
		FROM profile_facts f JOIN contacts c ON c.id = f.contact_id
		WHERE f.status='active' AND f.source_type != 'user'
		  AND (f.last_confirmed_at = '' OR f.last_confirmed_at IS NULL)
		  AND f.last_seen != '' AND f.last_seen < ?
		ORDER BY f.last_seen ASC, f.id`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []ConsolidationItem
	for rows.Next() {
		var id, cid int64
		var name, typ, key, val, srcType, lastSeen string
		var conf float64
		if rows.Scan(&id, &cid, &name, &typ, &key, &val, &conf, &srcType, &lastSeen) != nil {
			continue
		}
		items = append(items, ConsolidationItem{
			Kind:        KindNeverConfirmed,
			ContactID:   cid,
			ContactName: name,
			FactType:    typ,
			FactKey:     key,
			Description: fmt.Sprintf("%s 的 %s=%q 自录入以来从未被确认（末次见于 %s）", name, typ, val, lastSeen),
			Facts:       []ConsolidationFact{{ID: id, Value: val, Status: "active", Confidence: conf, SourceType: srcType, LastSeen: lastSeen}},
			Suggestion:  "建议核对后确认（升为用户权威）或否定（退出当前态）",
		})
	}
	return items, rows.Err()
}

// ---------- 辅助函数 ----------

// scanFactsLocked 读取某 (contact_id, fact_type, fact_key, status) 的全部事实。
func scanFactsLocked(db *sql.DB, cid int64, typ, key, status string) ([]ConsolidationFact, error) {
	rows, err := db.Query(`
		SELECT id, fact_value, status, confidence, source_type, last_seen
		FROM profile_facts
		WHERE contact_id=? AND fact_type=? AND fact_key=? AND status=?
		ORDER BY confidence DESC, id ASC`, cid, typ, key, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConsolidationFact
	for rows.Next() {
		var f ConsolidationFact
		if rows.Scan(&f.ID, &f.Value, &f.Status, &f.Confidence, &f.SourceType, &f.LastSeen) == nil {
			out = append(out, f)
		}
	}
	return out, nil
}

// isNearDuplicate 确定性判断两条文本是否近重复。
// 规则：(1) 去空格后互为子串；(2) 字符 bigram Jaccard ≥ dupJaccardThreshold。
func isNearDuplicate(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == b {
		return true // 精确重复（理论上被 UNIQUE 约束排除，但 key 为空时多值可能绕过）
	}
	// 子串包含
	if strings.Contains(a, b) || strings.Contains(b, a) {
		return true
	}
	// bigram Jaccard
	ja := bigrams(a)
	jb := bigrams(b)
	return jaccard(ja, jb) >= dupJaccardThreshold
}

func bigrams(s string) map[string]bool {
	m := make(map[string]bool, len(s))
	r := []rune(s)
	for i := 0; i+1 < len(r); i++ {
		m[string(r[i:i+2])] = true
	}
	return m
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	var inter int
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// ---------- 执行操作 ----------

// ApplyConsolidation 按用户决策执行一条整合建议。
// action: "supersede_low" — 保留最高置信的，其余标 superseded + valid_until
func ApplyConsolidation(db *sql.DB, factIDs []int64, keepID int64) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	now := time.Now().Format(time.RFC3339)
	for _, id := range factIDs {
		if id == keepID {
			continue
		}
		if _, err := db.Exec(`
			UPDATE profile_facts
			SET status='superseded', valid_until=?, superseded_by=?, updated_at=?
			WHERE id=? AND status='active'`, now, keepID, now, id); err != nil {
			return fmt.Errorf("supersede fact %d: %w", id, err)
		}
	}
	return nil
}

// MarkFactsStale 批量把指定 id 的 active 事实标 stale（不删除，仅降级可见性）。
func MarkFactsStale(db *sql.DB, ids []int64) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	now := time.Now().Format(time.RFC3339)
	for _, id := range ids {
		if _, err := db.Exec(`UPDATE profile_facts SET status='stale', updated_at=? WHERE id=? AND status='active'`, now, id); err != nil {
			return err
		}
	}
	return nil
}

// MergeFacts 把一组近重复事实合并到一条 canonical（keepID）：
//  1. 将 loser 的证据迁移到 keep（跳过与 keep 已有 (message_id,archived) 重复的，避免撞唯一约束）；
//  2. loser 事实本身不删除——标 superseded、写 valid_until、回填 superseded_by=keep（溯源不断层）。
//
// 确定性、无 LLM。keepID 必须在 ids 内或至少存在于库；loser 中等于 keepID 的会被跳过。
func MergeFacts(db *sql.DB, keepID int64, loserIDs []int64) error {
	if keepID == 0 {
		return fmt.Errorf("merge 操作必须指定 keep_id")
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	var keepOwner int64
	if err := db.QueryRow(`SELECT contact_id FROM profile_facts WHERE id=?`, keepID).Scan(&keepOwner); err != nil {
		return fmt.Errorf("保留事实 %d 不存在: %w", keepID, err)
	}
	ids := make([]int64, 0, len(loserIDs))
	for _, id := range loserIDs {
		if id != keepID {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	now := time.Now().Format(time.RFC3339)
	// 1) 先删掉与 keep 重复的 loser 证据（同一 message_id+archived 已被 keep 支撑），再迁移其余。
	if _, err := db.Exec(
		`DELETE FROM profile_fact_evidence
		 WHERE fact_id IN (`+ph+`)
		   AND EXISTS(SELECT 1 FROM profile_fact_evidence k
		     WHERE k.fact_id=? AND k.message_id=profile_fact_evidence.message_id AND k.archived=profile_fact_evidence.archived)`,
		append(args, keepID)...); err != nil {
		return fmt.Errorf("去重证据: %w", err)
	}
	if _, err := db.Exec(
		`UPDATE profile_fact_evidence SET fact_id=? WHERE fact_id IN (`+ph+`)`,
		append([]interface{}{keepID}, args...)...); err != nil {
		return fmt.Errorf("迁移证据: %w", err)
	}
	// 2) loser 事实标 superseded（保留行，不删除）。
	if _, err := db.Exec(
		`UPDATE profile_facts SET status='superseded', valid_until=?, superseded_by=?, updated_at=?
		 WHERE id IN (`+ph+`) AND status='active'`,
		append([]interface{}{now, keepID, now}, args...)...); err != nil {
		return fmt.Errorf("supersede loser: %w", err)
	}
	return nil
}

// ---------- API Handler ----------

// applyRequest 前端 POST /api/memory/consolidation 的 body。
type applyRequest struct {
	Action  string  `json:"action"`   // "supersede" | "stale"
	FactIDs []int64 `json:"fact_ids"` // 要处理的事实 id 列表
	KeepID  int64   `json:"keep_id"`  // supersede 时保留哪一条
}

func hApplyConsolidation(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	var req applyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}
	if len(req.FactIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "fact_ids 不得为空")
		return
	}
	switch req.Action {
	case "supersede":
		if req.KeepID == 0 {
			writeErr(w, http.StatusBadRequest, "supersede 操作必须指定 keep_id")
			return
		}
		if err := ApplyConsolidation(db, req.FactIDs, req.KeepID); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	case "stale":
		if err := MarkFactsStale(db, req.FactIDs); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	case "merge":
		// §10.2 合并：将多条近重复事实合并到 keep_id（迁移证据 + 其余标 superseded，绝不删除）。
		if req.KeepID == 0 {
			writeErr(w, http.StatusBadRequest, "merge 操作必须指定 keep_id")
			return
		}
		if err := MergeFacts(db, req.KeepID, req.FactIDs); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	case "defer":
		// §10.2 暂不处理：无任何副作用（与 confirm/reject 于事实级动作对称）。
	default:
		writeErr(w, http.StatusBadRequest, "不支持的 action: "+req.Action)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "applied": len(req.FactIDs)})
}
