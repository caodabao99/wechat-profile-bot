package main

import (
	"database/sql"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// Memory Review 待确认记忆（蓝图 §8 P4）。
//
// 不是给所有事实都排队确认，而是确定性挑出「值得让用户核对」的记忆：中等置信 / 证据弱 /
// 最近发生冲突 / 长期未确认 / 可能过时。只读选取、无副作用；用户动作复用既有事实原语：
//   确认 = ConfirmFact(source_type=user, confidence=1)
//   否定 = RejectFact(status=rejected)
//   暂不处理 = 无操作（保持原状态，下次仍按启发式重新入选）
//
// §8.3 更新规则由既有派生逻辑保证：source_type='user' 的事实永不自动降级；AI 只能对其
// 提出 conflict 证据（Phase4b 的 evidence_type='conflict'），不改写 status。全链路确定性，
// 不引入任何新 LLM 调用。

// 触发原因码（越大优先级越靠前）。
const (
	reviewReasonConflict   = "recent_conflict"
	reviewReasonStale      = "possibly_stale"
	reviewReasonWeak       = "weak_evidence"
	reviewReasonMedium     = "medium_confidence"
	reviewReasonUnverified = "long_unconfirmed"

	reviewPerContactMax = 3  // §8.1 每联系人最多 Top 3
	reviewSystemDefault = 10 // §8.1 整个系统默认最多 10 条
	reviewStaleDays     = 90 // 长期未确认：last_seen 距今 ≥ 90 天且从未确认
)

// MemoryReviewItem 一条待确认记忆 + 触发原因与确定性优先级。
type MemoryReviewItem struct {
	FactID           int64   `json:"fact_id"`
	ContactID        int64   `json:"contact_id"`
	ContactName      string  `json:"contact_name"`
	FactType         string  `json:"fact_type"`
	FactKey          string  `json:"fact_key"`
	FactValue        string  `json:"fact_value"`
	Status           string  `json:"status"`
	Confidence       float64 `json:"confidence"`
	EvidenceStrength float64 `json:"evidence_strength"`
	HasConflict      bool    `json:"has_conflict"`
	LastSeen         string  `json:"last_seen"`
	LastConfirmedAt  string  `json:"last_confirmed_at"`
	Reason           string  `json:"reason"`
	Priority         int     `json:"priority"`
}

// classifyReview 依据确定性阈值给出该事实的触发原因与优先级；无触发返回 ok=false。
// 优先级：冲突 > 过时/失效 > 证据弱 > 中等置信 > 长期未确认。
func classifyReview(it MemoryReviewItem, now time.Time) (string, int, bool) {
	switch {
	case it.HasConflict:
		return reviewReasonConflict, 100, true
	case it.Status == "stale" || it.Status == "conflict":
		return reviewReasonStale, 80, true
	case it.Confidence > 0.5 && it.EvidenceStrength < 0.34:
		return reviewReasonWeak, 60, true
	case it.Confidence >= 0.55 && it.Confidence <= 0.75:
		return reviewReasonMedium, 40, true
	case it.LastConfirmedAt == "" && olderThanDays(it.LastSeen, now, reviewStaleDays):
		return reviewReasonUnverified, 20, true
	}
	return "", 0, false
}

// olderThanDays 判断时间戳是否早于 now-days；无法解析或空则保守视为陈旧（宁可纳入待确认）。
func olderThanDays(ts string, now time.Time, days int) bool {
	if ts == "" {
		return true
	}
	var t time.Time
	switch parsed, err := time.Parse(time.RFC3339, ts); err {
	case nil:
		t = parsed
	default:
		if p2, e2 := time.Parse("2006-01-02 15:04:05", ts); e2 == nil {
			t = p2
		} else if p3, e3 := time.Parse("2006-01-02", ts); e3 == nil {
			t = p3
		} else {
			return true
		}
	}
	return now.Sub(t) >= time.Duration(days)*24*time.Hour
}

// BuildMemoryReviewQueue 构建待确认记忆队列（§8.1）：只读、确定性排序、每联系人 Top3、全局 TopN。
// systemLimit<=0 时回落默认 10。派生视图：不建表、无副作用；调用自锁（顶层 API 入口）。
func BuildMemoryReviewQueue(db *sql.DB, now time.Time, systemLimit int) ([]MemoryReviewItem, error) {
	if systemLimit <= 0 {
		systemLimit = reviewSystemDefault
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "profile_facts") {
		return []MemoryReviewItem{}, nil
	}
	rows, err := db.Query(`
		SELECT f.id, f.contact_id, COALESCE(NULLIF(c.remark,''), c.name, ''),
		       f.fact_type, f.fact_key, f.fact_value, f.status, f.confidence, f.evidence_strength,
		       f.last_seen, f.last_confirmed_at,
		       EXISTS(SELECT 1 FROM profile_fact_evidence e WHERE e.fact_id=f.id AND e.evidence_type='conflict')
		FROM profile_facts f JOIN contacts c ON c.id=f.contact_id
		WHERE f.source_type!='user' AND f.status IN ('active','inferred','stale','conflict')
		ORDER BY f.contact_id, f.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []MemoryReviewItem
	for rows.Next() {
		var it MemoryReviewItem
		var conflict int
		if err := rows.Scan(&it.FactID, &it.ContactID, &it.ContactName, &it.FactType, &it.FactKey,
			&it.FactValue, &it.Status, &it.Confidence, &it.EvidenceStrength, &it.LastSeen,
			&it.LastConfirmedAt, &conflict); err != nil {
			continue
		}
		it.HasConflict = conflict == 1
		reason, prio, ok := classifyReview(it, now)
		if !ok {
			continue
		}
		it.Reason = reason
		it.Priority = prio
		candidates = append(candidates, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 确定性排序：优先级降序 → 同优先级按 (contact_id, fact_id) 升序，稳定可复现。
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority > candidates[j].Priority
		}
		if candidates[i].ContactID != candidates[j].ContactID {
			return candidates[i].ContactID < candidates[j].ContactID
		}
		return candidates[i].FactID < candidates[j].FactID
	})
	// §8.1 节流：每联系人 Top3、全局 TopN。
	perContact := map[int64]int{}
	out := []MemoryReviewItem{}
	for _, it := range candidates {
		if perContact[it.ContactID] >= reviewPerContactMax {
			continue
		}
		if len(out) >= systemLimit {
			break
		}
		perContact[it.ContactID]++
		out = append(out, it)
	}
	return out, nil
}

// routeMemory 顶层路由：GET /api/memory/review?limit=N 返回待确认记忆队列。
func (s *apiServer) routeMemory(w http.ResponseWriter, r *http.Request, sub []string) {
	if len(sub) == 1 && sub[0] == "review" && r.Method == http.MethodGet {
		limit := reviewSystemDefault
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		list, err := BuildMemoryReviewQueue(s.db, time.Now(), limit)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "构建待确认记忆失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "count": len(list), "items": list})
		return
	}
	writeErr(w, http.StatusNotFound, "未知接口")
}
