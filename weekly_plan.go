package main

// 每周维护计划 + 隐式反馈闭环。
//
// 设计原则：系统主动做，人只看结果。
//   - GenerateWeeklyPlan 按周计算 top-N 联系人（评分 = 建议优先级 + 亲密度权重）并缓存
//   - autoMarkSuggestionActed 在消息入库时隐式检测"是否已联系"，自动标记建议为 done
//   - checkPendingOutcomes 对已标记 14 天的建议自动回测互动趋势变化
//   - 结果仅网页看板展示，不发邮件
//
// 与现有代码的交互：
//   - 复用 relationship_action_suggestions（由 GenerateActionSuggestions 产出）
//   - 复用 computeIntimacy（亲密度排行）
//   - 复用 fillDrafts（LLM 开场白草稿）
//   - 不修改 dbMu 锁模式，所有 LLM 调用在锁外

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

// WeeklyPlanItem 单条周计划项。
type WeeklyPlanItem struct {
	ContactID   int64   `json:"contactId"`
	ContactName string  `json:"contactName"`
	Kind        string  `json:"kind"`
	Reason      string  `json:"reason"`
	Draft       string  `json:"draft"`
	Score       float64 `json:"score"`
}

// WeeklyPlanStats 反馈统计。
type WeeklyPlanStats struct {
	Total    int `json:"total"`
	Acted    int `json:"acted"`
	Improved int `json:"improved"`
	Stable   int `json:"stable"`
	Worsened int `json:"worsened"`
}

// ---------- 生成 ----------

// GenerateWeeklyPlan 计算本周维护计划并写入缓存。
// 调用前会刷新全量建议（GenerateActionSuggestions），然后按分数取 top 5。
func GenerateWeeklyPlan(db *sql.DB, llm *LLMClient, now time.Time) error {
	// 先刷新建议
	if _, err := GenerateActionSuggestions(db, llm, 0); err != nil {
		slog.Warn("周计划：刷新建议失败", "err", err)
		// 不中止，继续用已有建议
	}

	// 获取 open 建议（ListSuggestions 内部取锁）
	suggestions, err := ListSuggestions(db, false)
	if err != nil {
		return fmt.Errorf("读取建议列表失败: %w", err)
	}
	if len(suggestions) == 0 {
		// 无建议 → 存空列表
		return saveWeeklyPlanCache(db, now, []WeeklyPlanItem{})
	}

	// 获取亲密度排行（computeIntimacy 内部取锁）
	intimacy, err := computeIntimacy(db, now, 30)
	if err != nil {
		slog.Warn("周计划：亲密度计算失败，降级为无加成", "err", err)
		intimacy = []AssistantIntimacyItem{}
	}
	// 建立 intimacy 查找表
	intimacyMap := make(map[int64]int, len(intimacy))
	maxScore := 1
	for _, it := range intimacy {
		intimacyMap[it.ContactID] = it.Score
		if it.Score > maxScore {
			maxScore = it.Score
		}
	}

	// 评分并排序
	type scored struct {
		sug   SuggestionView
		score float64
	}
	var ranked []scored
	for _, s := range suggestions {
		sc := float64(s.Priority)
		// 亲密度加成：分数越高说明这个联系人越活跃/重要 → 更值得维护
		if intim, ok := intimacyMap[s.ContactID]; ok {
			sc += float64(intim) / float64(maxScore) * 5.0
		}
		ranked = append(ranked, scored{sug: s, score: sc})
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].score > ranked[j].score
	})

	// 取 top 5
	limit := 5
	if len(ranked) < limit {
		limit = len(ranked)
	}
	items := make([]WeeklyPlanItem, 0, limit)
	for _, r := range ranked[:limit] {
		items = append(items, WeeklyPlanItem{
			ContactID:   r.sug.ContactID,
			ContactName: r.sug.ContactName,
			Kind:        r.sug.Kind,
			Reason:      r.sug.Reason,
			Draft:       r.sug.Draft,
			Score:       r.score,
		})
	}

	// 对 draft 为空者补一段 LLM 开场白（在 dbMu 锁外）
	if llm != nil {
		for i := range items {
			if items[i].Draft == "" {
				items[i].Draft = generateOutreachDraft(db, llm, items[i].ContactID, items[i].Kind)
			}
		}
	}

	return saveWeeklyPlanCache(db, now, items)
}

// saveWeeklyPlanCache 把计划序列化并 UPSERT 到缓存表。
func saveWeeklyPlanCache(db *sql.DB, now time.Time, items []WeeklyPlanItem) error {
	data, err := json.Marshal(items)
	if err != nil {
		return fmt.Errorf("序列化周计划失败: %w", err)
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err = db.Exec(
		`INSERT INTO weekly_plan_cache (id, generated_at, items_json) VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET generated_at=excluded.generated_at, items_json=excluded.items_json`,
		now.Format(time.RFC3339), string(data))
	return err
}

// ---------- 读取 ----------

// GetCachedWeeklyPlan 从缓存读取周计划项与生成时间。
func GetCachedWeeklyPlan(db *sql.DB) ([]WeeklyPlanItem, time.Time, error) {
	dbMu.Lock()
	row := db.QueryRow(`SELECT generated_at, items_json FROM weekly_plan_cache WHERE id = 1`)
	var genAt, itemsRaw string
	err := row.Scan(&genAt, &itemsRaw)
	dbMu.Unlock()

	if err == sql.ErrNoRows {
		return []WeeklyPlanItem{}, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	var items []WeeklyPlanItem
	if err := json.Unmarshal([]byte(itemsRaw), &items); err != nil {
		return nil, time.Time{}, fmt.Errorf("周计划缓存解析失败: %w", err)
	}
	t, _ := time.Parse(time.RFC3339, genAt)
	return items, t, nil
}

// IsWeeklyPlanStale 判断缓存是否超过 7 天。
func IsWeeklyPlanStale(generatedAt time.Time, now time.Time) bool {
	if generatedAt.IsZero() {
		return true
	}
	return now.Sub(generatedAt) > 7*24*time.Hour
}

// ---------- 反馈统计 ----------

// getOutcomeStats 查询最近 90 天的反馈统计。
func getOutcomeStats(db *sql.DB, now time.Time) WeeklyPlanStats {
	stats := WeeklyPlanStats{}
	dbMu.Lock()
	defer dbMu.Unlock()
	since := now.AddDate(0, 0, -90).Format(time.RFC3339)
	// 最近 90 天内产生的建议总数。扫过的行一律用 QueryRow（自带释放连接）；
	// 单连接池下，不 Close 的 db.Query 会把后续写操作饿死在连接等待上。
	db.QueryRow(`SELECT COUNT(*) FROM relationship_action_suggestions WHERE created_at >= ?`, since).Scan(&stats.Total)
	// 已执行（acted）
	db.QueryRow(`SELECT COUNT(*) FROM suggestion_outcomes WHERE acted_at >= ?`, since).Scan(&stats.Acted)
	// 回暖/平稳/恶化
	db.QueryRow(`SELECT COUNT(*) FROM suggestion_outcomes WHERE acted_at >= ? AND outcome='improved'`, since).Scan(&stats.Improved)
	db.QueryRow(`SELECT COUNT(*) FROM suggestion_outcomes WHERE acted_at >= ? AND outcome='stable'`, since).Scan(&stats.Stable)
	db.QueryRow(`SELECT COUNT(*) FROM suggestion_outcomes WHERE acted_at >= ? AND outcome='worsened'`, since).Scan(&stats.Worsened)
	return stats
}

// ---------- Phase 2: 隐式反馈 ----------

// autoMarkSuggestionActed 在用户给某联系人发消息后自动标记其 open 建议为 done，
// 并记录 suggestion_outcomes 供回测。幂等：已标记的不重复。
func autoMarkSuggestionActed(db *sql.DB, contactID int64, now time.Time) {
	dbMu.Lock()
	defer dbMu.Unlock()

	// 先查该联系人的 open 建议并一次性收完：单连接池下
	// 边迭代 rows 边 Exec 会死锁。
	var sugIDs []int64
	rows, err := db.Query(
		`SELECT id FROM relationship_action_suggestions WHERE contact_id = ? AND status = 'open'`,
		contactID)
	if err != nil {
		return
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			sugIDs = append(sugIDs, id)
		}
	}
	rows.Close()
	if len(sugIDs) == 0 {
		return
	}

	// 查当前 recent30 互动数作为 trend_before
	var recent30 int
	today := now.Format("2006-01-02")
	from := now.AddDate(0, 0, -29).Format("2006-01-02")
	if err := db.QueryRow(
		`SELECT COALESCE(SUM(me_count+other_count),0) FROM relationship_daily_metrics
		 WHERE contact_id=? AND day>=? AND day<=?`,
		contactID, from, today).Scan(&recent30); err != nil {
		slog.Warn("隐式反馈：读取互动趋势失败", "contactId", contactID, "err", err)
	}

	nowStr := now.Format(time.RFC3339)
	for _, sid := range sugIDs {
		// 幂等：若已有该 suggestion_id 的 outcome 记录则跳过
		var existing int
		db.QueryRow(`SELECT COUNT(*) FROM suggestion_outcomes WHERE suggestion_id=?`, sid).Scan(&existing)
		if existing > 0 {
			continue
		}
		if _, err := db.Exec(
			`INSERT INTO suggestion_outcomes (suggestion_id, contact_id, acted_at, trend_before) VALUES (?, ?, ?, ?)`,
			sid, contactID, nowStr, recent30); err != nil {
			slog.Warn("隐式反馈：写入回测记录失败", "suggestionId", sid, "err", err)
			continue
		}
		if _, err := db.Exec(
			`UPDATE relationship_action_suggestions SET status='done', updated_at=? WHERE id=?`,
			nowStr, sid); err != nil {
			slog.Warn("隐式反馈：标记建议完成失败", "suggestionId", sid, "err", err)
		}
	}
}

// checkPendingOutcomes 对 acted 超过 14 天且 outcome='pending' 的记录自动回测。
func checkPendingOutcomes(db *sql.DB, now time.Time) {
	dbMu.Lock()
	defer dbMu.Unlock()

	cutoff := now.AddDate(0, 0, -14).Format(time.RFC3339)
	// 一次性收完再处理（单连接池下边迭代边写会死锁）
	type pending struct {
		id, cid     int64
		trendBefore int
	}
	var items []pending
	rows, err := db.Query(
		`SELECT id, contact_id, trend_before FROM suggestion_outcomes
		 WHERE outcome='pending' AND acted_at != '' AND acted_at <= ?`, cutoff)
	if err != nil {
		return
	}
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.cid, &p.trendBefore); err != nil {
			continue
		}
		items = append(items, p)
	}
	rows.Close()

	if len(items) == 0 {
		return
	}

	today := now.Format("2006-01-02")
	from := now.AddDate(0, 0, -29).Format("2006-01-02")
	nowStr := now.Format(time.RFC3339)

	for _, p := range items {
		var trendAfter int
		if err := db.QueryRow(
			`SELECT COALESCE(SUM(me_count+other_count),0) FROM relationship_daily_metrics
			 WHERE contact_id=? AND day>=? AND day<=?`,
			p.cid, from, today).Scan(&trendAfter); err != nil {
			slog.Warn("反馈回测：读取互动趋势失败", "contactId", p.cid, "err", err)
			continue
		}

		outcome := "stable"
		if p.trendBefore > 0 {
			ratio := float64(trendAfter) / float64(p.trendBefore)
			if ratio > 1.3 {
				outcome = "improved"
			} else if ratio < 0.7 {
				outcome = "worsened"
			}
		} else if trendAfter > 0 {
			outcome = "improved"
		}

		if _, err := db.Exec(
			`UPDATE suggestion_outcomes SET outcome=?, trend_after=?, checked_at=? WHERE id=?`,
			outcome, trendAfter, nowStr, p.id); err != nil {
			slog.Warn("反馈回测：写入结果失败", "outcomeId", p.id, "err", err)
		}
	}
}

// ---------- LLM 开场白生成 ----------

// generateOutreachDraft 为单个联系人生成一段 AI 开场白（锁外调用 LLM）。
func generateOutreachDraft(db *sql.DB, llm *LLMClient, contactID int64, kind string) string {
	ctx := context.Background()

	// 读取联系人画像（锁内）
	dbMu.Lock()
	var name, summary string
	db.QueryRow(`SELECT name, COALESCE(profile_summary,'') FROM contacts WHERE id=?`, contactID).Scan(&name, &summary)
	dbMu.Unlock()

	if name == "" || !llm.configured() {
		return ""
	}

	reasonHint := "对方近期互动减少，关系有降温趋势"
	if kind == "silence" {
		reasonHint = "已很久没有互动"
	} else if kind == "no_reply" {
		reasonHint = "对方发了消息你还没回"
	}

	prompt := fmt.Sprintf(
		"你是一位社交顾问。用户想维护与「%s」的关系（画像：%s）。原因：%s。"+
			"请为用户生成一句简短自然的微信开场白（不超过50字），像朋友之间随口发的那种，不要写得太正式。只输出这句话，不要解释。",
		name, summary, reasonHint)

	raw, err := llm.CallContext(ctx, prompt)
	if err != nil || raw == "" {
		return ""
	}
	// 截断到合理长度
	if len(raw) > 200 {
		raw = raw[:200]
	}
	return raw
}
