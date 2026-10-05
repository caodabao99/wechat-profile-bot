package main

// ═══════════════════════════════════════════════════════════════════════════
// Decision Engine（PERSONAL RELATIONSHIP OS 2.0 · Phase 4，规格「七、Decision Engine」）
//
// 定位（本次升级最核心）：系统第一次拥有统一「判断层」。此前各推荐模块
// （Briefing Top-3 / Health alert / Coach suggestion / Followup / Goal）各自为战、
// 彼此不可见。本层不再生产第四套推荐，而是把它们已有的产出**收敛成一个可解释的
// 最终决策**：回答「今天谁最值得投入时间，为什么，做什么」。
//
// 设计红线（对齐规格 7.2 / 协作约束）：
//   - **优先确定性计算，不让 LLM 排序**：priority 由 scoreDecision 纯函数按固定权重算出，
//     同输入必同结果、可复现、可测（LLM 只作可选增强，缺失即确定性降级，永不 503）。
//   - **不另建事实源**：所有输入取自既有产出——Phase3 relationship_state（状态）、
//     ComputeHealth（亲密度/风险）、ListFollowups（待办）、collectUpcomingDates（重要日子）、
//     relationship_goals（目标 deadline）、ListSuggestions（行动建议=未闭合话题）。
//   - **不加并行新表**：决策按需计算，派生数据无持久化必要（避免与既有模块重复）。
//   - 单连接池分层锁：本层顺序调用各「自锁」顶层函数（取完即放锁），绝不嵌套 dbMu；
//     自身对 relationship_goals 的直读单独自锁。
//
// 决策候选结构（规格 7.1）：contact / state / priority / risk / opportunity / goal /
// followup / reason_codes[]。reason_codes 取定九种，保证「为什么现在推荐」可解释。
// ═══════════════════════════════════════════════════════════════════════════

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// 九种理由码（规格 7.1 原样）：每个候选附带命中的码，前端可解释来源。
const (
	reasonCooling              = "cooling"                // 关系正在降温
	reasonImportantDate        = "important_date"         // 重要日子临近
	reasonOpenFollowup         = "open_followup"          // 有未闭合的待办/承诺
	reasonGoalDeadline         = "goal_deadline"          // 关系维护目标到期临近
	reasonStrongOpportunity    = "strong_opportunity"     // 高亲密度且势头向好，值得加码
	reasonRecentPositiveChange = "recent_positive_change" // 近期正向变化（升温）
	reasonUnresolvedTopic      = "unresolved_topic"       // 有未闭合的对话线索（行动建议积压）
	reasonHighRisk             = "high_risk"              // 高风险（快流失）
	reasonReconnecting         = "reconnecting"           // 断联后重新活跃
)

// decisionImportanceCap：亲密度折算进优先级的上限（关系越重要基础分越高，规格 7.2 因素①）。
const decisionImportanceCap = 30

// decisionReasonWeight 每个理由码对优先级的固定贡献（确定性、可测）。
// 权重体现「现在」的紧迫度：风险/日子/目标/待办 > 状态变化 > 机会。
func decisionReasonWeight(code string) int {
	switch code {
	case reasonHighRisk:
		return 40
	case reasonImportantDate:
		return 35
	case reasonGoalDeadline:
		return 30
	case reasonOpenFollowup:
		return 28
	case reasonCooling:
		return 24
	case reasonReconnecting:
		return 22
	case reasonStrongOpportunity:
		return 16
	case reasonRecentPositiveChange:
		return 14
	case reasonUnresolvedTopic:
		return 10
	}
	return 0
}

// riskPriorityBonus 风险等级对优先级的附加分（规格 7.2 因素②风险）。
func riskPriorityBonus(alert string) int {
	switch alert {
	case "urgent":
		return 20
	case "watching":
		return 10
	}
	return 0
}

// scoreDecision 是纯函数：由亲密度、风险、理由码确定性地算出优先级分数。
// 相同输入必得相同输出——这是 Decision Engine 可复现、可测、不依赖 LLM 的根（规格 7.2）。
func scoreDecision(intimacy int, alert string, codes []string) int {
	score := intimacy * decisionImportanceCap / 100 // 因素①关系重要性（0..30）
	score += riskPriorityBonus(alert)               // 因素②风险
	seen := map[string]bool{}
	for _, c := range codes {
		if seen[c] {
			continue // 同码只计一次，去重防刷分
		}
		seen[c] = true
		score += decisionReasonWeight(c) // 因素③紧迫信号
	}
	return score
}

// DecisionCandidate 一个联系人的决策卡（规格 7.1 结构 + 7.3 展示字段）。
type DecisionCandidate struct {
	ContactID    int64    `json:"contact_id"`
	Name         string   `json:"name"`
	State        string   `json:"state"` // 「base/dynamic」复合展示
	BaseState    string   `json:"base_state"`
	DynamicState string   `json:"dynamic_state"`
	Priority     int      `json:"priority"`
	Risk         int      `json:"risk"`        // 0 低 / 1 关注 / 2 高
	Opportunity  int      `json:"opportunity"` // 0 无 / 1 有（强机会）
	Intimacy     int      `json:"intimacy"`
	Goal         string   `json:"goal,omitempty"`      // 相关维护目标
	Followup     string   `json:"followup,omitempty"`  // 相关待办
	Topic        string   `json:"topic,omitempty"`     // 相关话题/对话线索
	Action       string   `json:"action,omitempty"`    // 建议行动（确定性来源：已有草稿）
	BestTime     string   `json:"best_time,omitempty"` // 最佳触达时段（对方活跃小时）
	Source       string   `json:"source"`              // 来源（FACT/INFERENCE 汇聚说明）
	Confidence   string   `json:"confidence"`          // 置信度档
	ReasonCodes  []string `json:"reason_codes"`
	WhyNow       []string `json:"why_now"` // 人类可读「为什么是现在」
}

// decisionRiskLevel 把 alert 归一到 0/1/2 风险档。
func decisionRiskLevel(alert string) int {
	switch alert {
	case "urgent":
		return 2
	case "watching":
		return 1
	}
	return 0
}

// decisionReasonText 理由码 → 一句「为什么现在」（可解释，规格 7.4）。
func decisionReasonText(code string, detail string) string {
	switch code {
	case reasonCooling:
		return "关系正在降温" + suffixDetail(detail)
	case reasonHighRisk:
		return "高流失风险，需要尽快触达"
	case reasonReconnecting:
		return "断联后重新活跃，是加温的好时机"
	case reasonRecentPositiveChange:
		return "近期互动升温，趁热推进"
	case reasonStrongOpportunity:
		return "亲密度高且势头好，值得投入"
	case reasonImportantDate:
		return "重要日子临近：" + detail
	case reasonOpenFollowup:
		return "有未完成的待办/承诺：" + detail
	case reasonGoalDeadline:
		return "关系维护目标临期：" + detail
	case reasonUnresolvedTopic:
		return "有未闭合的对话线索：" + detail
	}
	return code
}

func suffixDetail(detail string) string {
	if detail == "" {
		return ""
	}
	return "（" + detail + "）"
}

// decisionGoalRow 活跃目标的精简读入（contact_id / title / period_end）。
type decisionGoalRow struct {
	contactID int64
	title     string
	periodEnd string
}

// activeGoalsForDecision 自锁读活跃关系维护目标（表缺失返回空，决策优雅降级）。
func activeGoalsForDecision(db *sql.DB) ([]decisionGoalRow, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "relationship_goals") {
		return nil, nil
	}
	rows, err := db.Query(`SELECT contact_id, title, period_end FROM relationship_goals WHERE status=? ORDER BY id ASC`, goalStatusActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []decisionGoalRow{}
	for rows.Next() {
		var g decisionGoalRow
		if err := rows.Scan(&g.contactID, &g.title, &g.periodEnd); err != nil {
			continue
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// daysUntilDate 解析 YYYY-MM-DD 相对 now 的剩余天数（无/非法 → -1）。
func daysUntilDate(now time.Time, dateStr string) int {
	dateStr = first8Date(dateStr)
	if dateStr == "" {
		return -1
	}
	t, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return -1
	}
	d := int(t.Sub(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())).Hours() / 24)
	return d
}

// first8Date 从可能含时间的串里取前 10 位日期；仅当形如 YYYY-MM-DD 才返回。
func first8Date(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 10 && s[4] == '-' && s[7] == '-' {
		return s[:10]
	}
	return ""
}

// goalDeadlineSoonDays 目标临期判定窗口（天）。
const goalDeadlineSoonDays = 7

// importantDateWindowDays 重要日子前瞻窗口（天）。
const importantDateWindowDays = 14

// BuildDecisionCandidates 汇聚各既有产出，为每个有状态的联系人生成可解释决策候选，
// 按确定性 priority 降序排序（规格 7.1 / 7.2）。顺序调用自锁顶层函数，不嵌套 dbMu。
func BuildDecisionCandidates(db *sql.DB, now time.Time) ([]DecisionCandidate, error) {
	// 懒建表防御：followup_items 等非 migrate 建表在某些环境尚未初始化时，
	// 决策板仍应可用（ensureFollowupTables 幂等、自锁，此处不持外层锁）。
	if err := ensureFollowupTables(db); err != nil {
		return nil, err
	}
	states, err := ListRelationshipStates(db)
	if err != nil {
		return nil, err
	}
	followups, err := ListFollowups(db, "open", 0)
	if err != nil {
		return nil, err
	}
	dates, _, err := collectUpcomingDates(db, now, importantDateWindowDays)
	if err != nil {
		return nil, err
	}
	goals, err := activeGoalsForDecision(db)
	if err != nil {
		return nil, err
	}
	suggs, err := ListSuggestions(db, false)
	if err != nil {
		return nil, err
	}

	// 按联系人归集紧迫信号（取每个联系人最突出的一条作展示明细）。
	type fuSig struct {
		has    bool
		detail string
	}
	fuByContact := map[int64]fuSig{}
	for _, f := range followups {
		cur := fuByContact[f.ContactID]
		if !cur.has {
			cur.has = true
			cur.detail = strings.TrimSpace(f.Content)
		}
		fuByContact[f.ContactID] = cur
	}
	dateByContact := map[int64]string{}
	for _, d := range dates {
		if _, ok := dateByContact[d.ContactID]; !ok {
			dateByContact[d.ContactID] = fmt.Sprintf("%s（%d 天后）", d.Kind, d.DaysUntil)
		}
	}
	goalByContact := map[int64]string{}
	for _, g := range goals {
		if d := daysUntilDate(now, g.periodEnd); d >= 0 && d <= goalDeadlineSoonDays {
			if _, ok := goalByContact[g.contactID]; !ok {
				goalByContact[g.contactID] = fmt.Sprintf("%s（%d 天后到期）", g.title, d)
			}
		}
	}
	suggByContact := map[int64]SuggestionView{}
	for _, s := range suggs {
		if _, ok := suggByContact[s.ContactID]; !ok { // ListSuggestions 已按 priority 降序 → 首个即最优
			suggByContact[s.ContactID] = s
		}
	}

	out := []DecisionCandidate{}
	for _, st := range states {
		codes := []string{}
		why := []string{}
		add := func(code, detail string) {
			codes = append(codes, code)
			why = append(why, decisionReasonText(code, detail))
		}

		switch st.DynamicState {
		case dynAtRisk:
			add(reasonHighRisk, "")
			add(reasonCooling, "")
		case dynCooling:
			add(reasonCooling, "")
		case dynReconnecting:
			add(reasonReconnecting, "")
		case dynWarming:
			add(reasonRecentPositiveChange, "")
		}
		if st.Intimacy >= 75 && (st.DynamicState == dynWarming || st.DynamicState == dynStable) {
			add(reasonStrongOpportunity, "")
		}
		if detail, ok := dateByContact[st.ContactID]; ok {
			add(reasonImportantDate, detail)
		}
		if sig, ok := fuByContact[st.ContactID]; ok && sig.has {
			add(reasonOpenFollowup, sig.detail)
		}
		if detail, ok := goalByContact[st.ContactID]; ok {
			add(reasonGoalDeadline, detail)
		}
		var topSugg SuggestionView
		if s, ok := suggByContact[st.ContactID]; ok {
			topSugg = s
			add(reasonUnresolvedTopic, s.Draft)
		}

		priority := scoreDecision(st.Intimacy, st.Alert, codes)
		// 无任何紧迫信号且非高风险 → 不进入决策（系统只呈现「值得行动」的人，不制造噪音）。
		if len(codes) == 0 {
			continue
		}

		cand := DecisionCandidate{
			ContactID:    st.ContactID,
			Name:         st.Name,
			State:        st.BaseState + " / " + st.DynamicState,
			BaseState:    st.BaseState,
			DynamicState: st.DynamicState,
			Priority:     priority,
			Risk:         decisionRiskLevel(st.Alert),
			Opportunity:  b2i(hasCode(codes, reasonStrongOpportunity)),
			Intimacy:     st.Intimacy,
			ReasonCodes:  codes,
			WhyNow:       why,
			Source:       "state+health+followup+goal+date+suggestion",
			Confidence:   decisionConfidence(len(codes)),
		}
		if detail, ok := fuByContact[st.ContactID]; ok && detail.has {
			cand.Followup = detail.detail
		}
		if detail, ok := goalByContact[st.ContactID]; ok {
			cand.Goal = detail
		}
		if topSugg.ContactID != 0 {
			cand.Action = topSugg.Draft
			cand.Topic = topSugg.Reason
		}
		out = append(out, cand)
	}

	// 确定性排序：priority 降序 → 亲密度降序 → contact_id 升序（稳定可复现）。
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		if out[i].Intimacy != out[j].Intimacy {
			return out[i].Intimacy > out[j].Intimacy
		}
		return out[i].ContactID < out[j].ContactID
	})
	return out, nil
}

// TodayDecisions 返回「今天最值得做的关系行动」Top N（规格 7.3），并对入选者补最佳触达时段。
// topN<=0 用默认 3；时段来自 GetAggregatedMetrics 对方发消息峰值小时（确定性复用，缺失则留空）。
func TodayDecisions(db *sql.DB, now time.Time, topN int) ([]DecisionCandidate, error) {
	if topN <= 0 || topN > 20 {
		topN = 3
	}
	cands, err := BuildDecisionCandidates(db, now)
	if err != nil {
		return nil, err
	}
	if len(cands) > topN {
		cands = cands[:topN]
	}
	for i := range cands {
		if m, err := GetAggregatedMetrics(db, int(cands[i].ContactID), decisionBestTimeWindowDays); err == nil && m != nil {
			cands[i].BestTime = peakHourLabel(m.OtherHourHist)
		}
	}
	return cands, nil
}

// decisionBestTimeWindowDays 补「最佳触达时段」时读对方活跃小时的时间窗（天）。
const decisionBestTimeWindowDays = 90

// peakHourLabel 从「对方发消息」小时分布取峰值小时，产「~HH:00 更可能在线」标签（无数据返回空）。
func peakHourLabel(hourHist map[int]int) string {
	best := -1
	bestN := 0
	for h, n := range hourHist {
		if n > bestN {
			bestN = n
			best = h
		}
	}
	if best < 0 || bestN == 0 {
		return ""
	}
	return fmt.Sprintf("%02d:00 前后（对方历史活跃峰值）", best)
}

// decisionConfidence 依据紧迫信号数量给粗粒度置信度档（信号越多越可信）。
func decisionConfidence(n int) string {
	switch {
	case n >= 3:
		return "high"
	case n == 2:
		return "medium"
	default:
		return "low"
	}
}

func hasCode(codes []string, want string) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
