package main

// ═══════════════════════════════════════════════════════════════════════════
// AI Context Engine（PERSONAL RELATIONSHIP OS 2.0 · Phase 6，规格「九、P5」）
//
// 定位（架构级）：把散落各处的画像读取收敛成**唯一、分层、带预算**的上下文构造器，
// 让所有新 AI 功能都基于同一份「联系人认知快照」喂模型，而不是各写各的查询、
// 各自把整段历史塞进 prompt。
//
// 设计红线（对齐规格 9.1 / 9.2 / 9.3 与协作约束）：
//   - 9.1 保留旧入口，但**新代码一律走本引擎**：Decision/Briefing 等新增调用统一经此。
//   - 9.2 分层构造，**绝不一把捞全部历史**：最近消息 + FTS 相关消息 + 事实证据 +
//     目标/项目 + 指标 分块取，各块按预算限量。
//   - 9.3 为不同 Task 定义 max messages / max evidence / max tokens 预算（budgetFor），
//     渲染文本再按 maxTokens 截断——不同任务侧重不同（Ask 偏相关消息、Narrative 偏时间线…）。
//   - 单连接池分层锁：所有输入取自「自锁」顶层 reader，逐个顺序调用、绝不嵌套 dbMu；
//     任一读者失败即优雅跳过该块（宁可缺块也不 overall 报错、永不 panic）。
//   - 不引入新表、不新增强依赖：纯读聚合 + 纯函数预算/渲染。
// ═══════════════════════════════════════════════════════════════════════════

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ContextTask AI 任务类型（决定上下文预算与侧重，规格 9.3）。
type ContextTask string

const (
	TaskProfile    ContextTask = "profile"    // 画像生成
	TaskAsk        ContextTask = "ask"        // 问答：相关消息优先
	TaskCoach      ContextTask = "coach"      // 教练建议
	TaskNarrative  ContextTask = "narrative"  // 叙事：时间线+主题+代表性对话
	TaskSimulation ContextTask = "simulation" // 模拟对话：最近消息+说话风格
	TaskDecision   ContextTask = "decision"   // 决策：状态+指标+最近变化+关键事实
	TaskBriefing   ContextTask = "briefing"   // 简报
	TaskReplay     ContextTask = "replay"     // 重新认识 TA（Phase 7）
)

// ContextBudget 单任务的上下文预算（规格 9.3）。
type ContextBudget struct {
	MaxMessages int // 最近消息条数上限
	MaxRelevant int // FTS 相关消息条数上限（Ask/Simulation 用）
	MaxEvidence int // 事实证据条数上限
	MaxEvents   int // 时间线事件条数上限
	MaxTopics   int // 主题条目上限
	MaxWeeks    int // 主题回溯周数
	MaxTokens   int // 渲染文本 token 预算（近似：rune 数 / 2）
}

// budgetFor 任务 → 预算。不同任务侧重不同，杜绝「所有任务都送 100 条消息」。
func budgetFor(task ContextTask) ContextBudget {
	switch task {
	case TaskAsk:
		return ContextBudget{MaxMessages: 8, MaxRelevant: 24, MaxEvidence: 12, MaxEvents: 10, MaxTopics: 6, MaxWeeks: 4, MaxTokens: 1400}
	case TaskNarrative, TaskReplay:
		return ContextBudget{MaxMessages: 24, MaxRelevant: 12, MaxEvidence: 14, MaxEvents: 40, MaxTopics: 12, MaxWeeks: 12, MaxTokens: 2000}
	case TaskSimulation:
		return ContextBudget{MaxMessages: 48, MaxRelevant: 8, MaxEvidence: 6, MaxEvents: 8, MaxTopics: 6, MaxWeeks: 4, MaxTokens: 1600}
	case TaskDecision:
		return ContextBudget{MaxMessages: 6, MaxRelevant: 6, MaxEvidence: 8, MaxEvents: 6, MaxTopics: 4, MaxWeeks: 2, MaxTokens: 900}
	case TaskCoach, TaskBriefing:
		return ContextBudget{MaxMessages: 12, MaxRelevant: 8, MaxEvidence: 8, MaxEvents: 10, MaxTopics: 6, MaxWeeks: 4, MaxTokens: 1000}
	default: // profile 及未知任务
		return ContextBudget{MaxMessages: 20, MaxRelevant: 10, MaxEvidence: 12, MaxEvents: 16, MaxTopics: 8, MaxWeeks: 8, MaxTokens: 1400}
	}
}

// ContactContext 一个联系人的分层认知快照（规格第九章 ContactContext 全字段）。
type ContactContext struct {
	ContactID int64         `json:"contact_id"`
	Task      ContextTask   `json:"task"`
	Budget    ContextBudget `json:"budget"`

	Identity                 ContextIdentity        `json:"identity"`
	CurrentRelationshipState *RelationshipStateView `json:"current_relationship_state,omitempty"`
	TrustedFacts             []FactView             `json:"trusted_facts"`
	ConflictingFacts         []FactView             `json:"conflicting_facts"`
	RecentEvents             []TimelineItem         `json:"recent_events"`
	ActiveGoals              []decisionGoalRowView  `json:"active_goals"`
	Projects                 []ProjectView          `json:"projects"`
	OpenFollowups            []FollowupItem         `json:"open_followups"`
	RecentTopics             *ContactTopics         `json:"recent_topics,omitempty"`
	Metrics                  *Metrics               `json:"metrics,omitempty"`
	RecentMessages           []Message              `json:"recent_messages"`
	RelevantMessages         []SearchHit            `json:"relevant_messages,omitempty"` // 仅当带 query 时填充
	RelevantEvidence         []ContextEvidence      `json:"relevant_evidence"`
	PreviousActions          []SuggestionView       `json:"previous_actions"`
	PreviousOutcomes         []ContextOutcome       `json:"previous_outcomes"`
	Risks                    []string               `json:"risks"`
	Opportunities            []string               `json:"opportunities"`

	// Truncated 标记渲染时是否因超预算被截断（可观测，供上层知晓上下文被裁剪）。
	Truncated bool `json:"truncated"`
}

// ContextIdentity 身份块（名字/备注/画像摘要 + 关键单值事实）。
type ContextIdentity struct {
	Name        string `json:"name"`
	Remark      string `json:"remark,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Occupation  string `json:"occupation,omitempty"`
	Location    string `json:"location,omitempty"`
	Closeness   string `json:"closeness,omitempty"`
	FirstSeenAt string `json:"first_seen_at,omitempty"`
}

// ContextEvidence 扁平化的证据引用（来自事实的 evidence，供 prompt 引用、可溯源）。
type ContextEvidence struct {
	FactType string  `json:"fact_type"`
	FactKey  string  `json:"fact_key"`
	Value    string  `json:"value"`
	Quote    string  `json:"quote"`
	Match    string  `json:"match_type"`
	Strength float64 `json:"support_strength"`
}

// ContextOutcome 历史行动结果（suggestion_outcomes，供「上次干预有没有效」的学习）。
type ContextOutcome struct {
	Outcome string `json:"outcome"` // pending/improved/stable/worsened
	ActedAt string `json:"acted_at"`
}

// decisionGoalRowView 目标块视图（contact 维度的活跃目标）。
type decisionGoalRowView struct {
	Title     string `json:"title"`
	Metric    string `json:"metric"`
	Target    int    `json:"target"`
	PeriodEnd string `json:"period_end"`
}

// BuildContactContext 分层构造指定联系人的认知快照（规格 9.2）。query 非空时附带 FTS 相关消息。
// 逐块自锁读取、任一失败优雅跳过；不 panic、不因单块缺失整体报错（除非联系人都不存在）。
func BuildContactContext(db *sql.DB, contactID int64, task ContextTask, query string, now time.Time) (*ContactContext, error) {
	budget := budgetFor(task)
	cc := &ContactContext{
		ContactID:        contactID,
		Task:             task,
		Budget:           budget,
		TrustedFacts:     []FactView{},
		ConflictingFacts: []FactView{},
		RecentEvents:     []TimelineItem{},
		ActiveGoals:      []decisionGoalRowView{},
		Projects:         []ProjectView{},
		OpenFollowups:    []FollowupItem{},
		RecentMessages:   []Message{},
		RelevantEvidence: []ContextEvidence{},
		PreviousActions:  []SuggestionView{},
		PreviousOutcomes: []ContextOutcome{},
		Risks:            []string{},
		Opportunities:    []string{},
	}

	// Identity：GetContactByID 不存在 → 直接报错（上层映射 404）。
	contact, err := GetContactByID(db, contactID)
	if err != nil {
		return nil, err
	}
	cc.Identity = ContextIdentity{Name: contact.Name, Remark: contact.Remark, Summary: contact.ProfileSummary}

	// 事实（含证据）：拆可信 / 冲突，并从可信事实抽单值进 Identity、证据扁平化。
	if facts, err := GetFacts(db, contactID, false); err == nil {
		for _, f := range facts {
			switch f.Status {
			case "conflict":
				cc.ConflictingFacts = append(cc.ConflictingFacts, f)
				continue
			}
			cc.TrustedFacts = append(cc.TrustedFacts, f)
			switch f.Type {
			case "occupation":
				cc.Identity.Occupation = pickFirst(f.Value, cc.Identity.Occupation)
			case "location":
				cc.Identity.Location = pickFirst(f.Value, cc.Identity.Location)
			case "closeness":
				cc.Identity.Closeness = pickFirst(f.Value, cc.Identity.Closeness)
			}
			for _, ev := range f.Evidence {
				if len(cc.RelevantEvidence) >= budget.MaxEvidence {
					break
				}
				cc.RelevantEvidence = append(cc.RelevantEvidence, ContextEvidence{
					FactType: f.Type, FactKey: f.Key, Value: f.Value,
					Quote: ev.Quote, Match: ev.MatchType, Strength: ev.SupportStrength,
				})
			}
		}
	}

	// 关系状态（Phase 3）。
	if st, err := GetRelationshipState(db, contactID); err == nil {
		cc.CurrentRelationshipState = st
		cc.Risks, cc.Opportunities = deriveRisksOpportunities(*st)
	}

	// 时间线事件。
	if ev, err := GetContactTimeline(db, contactID, budget.MaxEvents); err == nil {
		cc.RecentEvents = ev
	}

	// 活跃目标（轻量直读，表缺失自动空）。
	if gs, err := activeGoalsForContactView(db, contactID); err == nil {
		cc.ActiveGoals = gs
	}

	// 项目（Phase 5）。
	if ps, err := ListProjects(db, contactID, "open"); err == nil {
		cc.Projects = ps
	}

	// 未闭合待办（全量 open 后按联系人过滤，量大时受预算约束）。
	if fus, err := ListFollowups(db, "open", 0); err == nil {
		for _, f := range fus {
			if f.ContactID == contactID {
				cc.OpenFollowups = append(cc.OpenFollowups, f)
			}
		}
	}

	// 主题。
	if tp, err := GetContactTopics(db, contactID, budget.MaxWeeks); err == nil {
		cc.RecentTopics = tp
	}

	// 指标（时间窗取 90 天默认）。
	if m, err := GetAggregatedMetrics(db, int(contactID), 90); err == nil {
		cc.Metrics = m
	}

	// 最近消息（受预算限量，绝不整段历史）。
	if msgs, err := GetRecentMessages(db, contactID, budget.MaxMessages); err == nil {
		cc.RecentMessages = msgs
	}

	// 相关消息（FTS，仅当带 query：分层构造的「相关」臂，Ask 侧重）。
	if q := strings.TrimSpace(query); q != "" && budget.MaxRelevant > 0 {
		if res, err := SearchMessages(db, SearchOptions{Query: q, ContactID: contactID, Limit: budget.MaxRelevant}); err == nil && res != nil {
			cc.RelevantMessages = res.List
		}
	}

	// 行动建议 + 历史结果。
	if sg, err := ListSuggestions(db, false); err == nil {
		for _, s := range sg {
			if s.ContactID == contactID {
				cc.PreviousActions = append(cc.PreviousActions, s)
			}
		}
	}
	if oc, err := recentOutcomesForContact(db, contactID, 10); err == nil {
		cc.PreviousOutcomes = oc
	}

	return cc, nil
}

// pickFirst 返回首个非空值（优先已有则保留，否则取新值）。
func pickFirst(cand, cur string) string {
	if strings.TrimSpace(cur) != "" {
		return cur
	}
	return strings.TrimSpace(cand)
}

// deriveRisksOpportunities 从状态确定性派生风险/机会条目（与 Decision reason_codes 同源自洽）。
func deriveRisksOpportunities(st RelationshipStateView) (risks, opportunities []string) {
	risks, opportunities = []string{}, []string{}
	switch st.DynamicState {
	case dynAtRisk:
		risks = append(risks, "高流失风险（快凉）")
	case dynCooling:
		risks = append(risks, "互动降温")
	case dynDormant:
		risks = append(risks, "长期沉寂")
	}
	if st.Alert == "urgent" {
		risks = append(risks, "健康度告警：紧急")
	} else if st.Alert == "watching" {
		risks = append(risks, "健康度告警：关注")
	}
	if st.DynamicState == dynWarming {
		opportunities = append(opportunities, "近期升温，宜推进")
	}
	if st.DynamicState == dynReconnecting {
		opportunities = append(opportunities, "断联后重连窗口")
	}
	if st.Intimacy >= 75 && st.DynamicState != dynDormant {
		opportunities = append(opportunities, "高亲密度核心关系")
	}
	return risks, opportunities
}

// activeGoalsForContactView 自锁轻量读该联系人的活跃目标（表缺失返回空、不报错）。
func activeGoalsForContactView(db *sql.DB, contactID int64) ([]decisionGoalRowView, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "relationship_goals") {
		return []decisionGoalRowView{}, nil
	}
	rows, err := db.Query(`SELECT title, metric, target_count, period_end FROM relationship_goals WHERE status=? AND contact_id=? ORDER BY id ASC`,
		goalStatusActive, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []decisionGoalRowView{}
	for rows.Next() {
		var g decisionGoalRowView
		if err := rows.Scan(&g.Title, &g.Metric, &g.Target, &g.PeriodEnd); err != nil {
			continue
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// recentOutcomesForContact 自锁读该联系人最近的行动结果（表缺失返回空）。
func recentOutcomesForContact(db *sql.DB, contactID int64, limit int) ([]ContextOutcome, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "suggestion_outcomes") {
		return []ContextOutcome{}, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := db.Query(`SELECT outcome, acted_at FROM suggestion_outcomes WHERE contact_id=? ORDER BY acted_at DESC LIMIT ?`, contactID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ContextOutcome{}
	for rows.Next() {
		var o ContextOutcome
		if err := rows.Scan(&o.Outcome, &o.ActedAt); err != nil {
			continue
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ── 渲染：把结构化快照压成 token 预算内的提示词上下文块（供新 AI 代码使用）──

// contextTokenApprox 近似 token 数（CJK 混合保守按 rune/2 估算，确定性、可测）。
func contextTokenApprox(s string) int {
	return len([]rune(s)) / 2
}

// RenderContextText 按任务侧重渲染上下文为纯文本块，并截断到 Budget.MaxTokens（规格 9.3）。
// 侧重排序：Ask/Simulation 先相关+最近消息，Narrative/Replay 先时间线+主题，Decision 先状态+指标。
func RenderContextText(cc *ContactContext) string {
	if cc == nil {
		return ""
	}
	var b strings.Builder
	order := cc.Task

	writeSection := func(title string, lines []string) {
		if len(lines) == 0 {
			return
		}
		b.WriteString("## " + title + "\n")
		for _, l := range lines {
			b.WriteString("- " + l + "\n")
		}
		b.WriteString("\n")
	}

	// Identity 恒在最前。
	writeSection("联系人", []string{cc.Identity.Name})
	idLines := []string{}
	if cc.Identity.Occupation != "" {
		idLines = append(idLines, "职业: "+cc.Identity.Occupation)
	}
	if cc.Identity.Location != "" {
		idLines = append(idLines, "所在地: "+cc.Identity.Location)
	}
	if cc.Identity.Closeness != "" {
		idLines = append(idLines, "亲疏: "+cc.Identity.Closeness)
	}
	if cc.Identity.Summary != "" {
		idLines = append(idLines, "摘要: "+cc.Identity.Summary)
	}
	writeSection("身份画像", idLines)

	if cc.CurrentRelationshipState != nil {
		s := cc.CurrentRelationshipState
		writeSection("当前关系状态", []string{fmt.Sprintf("%s / %s，亲密度 %d，趋势 %s，预警 %s", s.BaseState, s.DynamicState, s.Intimacy, s.TrendState, s.Alert)})
	}

	// 任务侧重：靠前块优先保留（截断时靠后被丢弃）。
	switch order {
	case TaskAsk, TaskSimulation:
		writeSection("相关消息", hitsToLines(cc.RelevantMessages))
		writeSection("最近对话", msgsToLines(cc.RecentMessages))
	case TaskNarrative, TaskReplay:
		writeSection("关系时间线", eventsToLines(cc.RecentEvents))
		writeSection("近期主题", topicsToLines(cc.RecentTopics))
		writeSection("最近对话", msgsToLines(cc.RecentMessages))
	default: // Decision/Coach/Briefing/Profile：状态+事实优先
		writeSection("最近变化/风险", append(append([]string{}, cc.Risks...), cc.Opportunities...))
	}

	writeSection("关键事实", factsToLines(cc.TrustedFacts))
	writeSection("事实证据", evidenceToLines(cc.RelevantEvidence))
	writeSection("活跃目标", goalsToLines(cc.ActiveGoals))
	writeSection("关系项目", projectsToLines(cc.Projects))
	writeSection("未闭合待办", followupsToLines(cc.OpenFollowups))
	if cc.Metrics != nil {
		writeSection("互动指标", []string{fmt.Sprintf("我发 %d 条 / 对方发 %d 条，回复中位 %ds", cc.Metrics.MeCount, cc.Metrics.OtherCount, cc.Metrics.ReplyLatencyP50)})
	}
	// 其他任务补时间线（若前面没写过）。
	if order != TaskNarrative && order != TaskReplay {
		writeSection("关系时间线", eventsToLines(cc.RecentEvents))
	}
	writeSection("上次行动", suggestionsToLines(cc.PreviousActions))
	writeSection("行动结果", outcomesToLines(cc.PreviousOutcomes))

	out := b.String()
	// token 预算截断（近似按 rune；保留头部，靠后块优先舍弃）。
	if max := cc.Budget.MaxTokens; max > 0 {
		if approx := contextTokenApprox(out); approx > max {
			keepRunes := max * 2
			if keepRunes < len([]rune(out)) {
				out = string([]rune(out)[:keepRunes])
				cc.Truncated = true
				out += "\n…（上下文已按 token 预算截断）"
			}
		}
	}
	return out
}

func factsToLines(fs []FactView) []string {
	out := []string{}
	for _, f := range fs {
		k := f.Type
		if f.Key != "" {
			k += "/" + f.Key
		}
		out = append(out, fmt.Sprintf("%s: %s（置信 %.2f，%s）", k, f.Value, f.Confidence, f.ConfidenceType))
	}
	return out
}

func evidenceToLines(es []ContextEvidence) []string {
	out := []string{}
	for _, e := range es {
		if e.Quote != "" {
			out = append(out, fmt.Sprintf("[%s] 引文「%s」", e.FactType, e.Quote))
		}
	}
	return out
}

func msgsToLines(ms []Message) []string {
	out := []string{}
	for _, m := range ms {
		who := "对方"
		if m.Sender == "me" {
			who = "我"
		}
		out = append(out, fmt.Sprintf("%s: %s", who, strings.TrimSpace(m.Content)))
	}
	return out
}

func hitsToLines(hs []SearchHit) []string {
	out := []string{}
	for _, h := range hs {
		snip := h.Snippet
		if snip == "" {
			snip = h.Content
		}
		out = append(out, fmt.Sprintf("%s｜%s", h.MsgTime, strings.TrimSpace(snip)))
	}
	return out
}

func eventsToLines(ev []TimelineItem) []string {
	out := []string{}
	for _, e := range ev {
		out = append(out, fmt.Sprintf("%s｜%s", e.EventTime, e.Title))
	}
	return out
}

func topicsToLines(tp *ContactTopics) []string {
	out := []string{}
	if tp == nil {
		return out
	}
	for _, w := range tp.Weeks {
		for _, t := range w.Topics {
			out = append(out, fmt.Sprintf("%s｜%s", w.WeekStart, t.Name))
		}
	}
	return out
}

func goalsToLines(gs []decisionGoalRowView) []string {
	out := []string{}
	for _, g := range gs {
		out = append(out, fmt.Sprintf("%s（目标 %d，止 %s）", g.Title, g.Target, g.PeriodEnd))
	}
	return out
}

func projectsToLines(ps []ProjectView) []string {
	out := []string{}
	for _, p := range ps {
		line := p.Title + "［" + p.Stage + "］"
		if p.NextAction != "" {
			line += " 下一步：" + p.NextAction
		}
		if p.NextActionDue != "" {
			line += "（" + p.NextActionDue + "）"
		}
		out = append(out, line)
	}
	return out
}

func followupsToLines(fs []FollowupItem) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, fmt.Sprintf("%s｜%s", f.KindLabel, strings.TrimSpace(f.Content)))
	}
	return out
}

func suggestionsToLines(ss []SuggestionView) []string {
	out := []string{}
	for _, s := range ss {
		out = append(out, fmt.Sprintf("%s｜%s", s.Kind, s.Draft))
	}
	return out
}

func outcomesToLines(os []ContextOutcome) []string {
	out := []string{}
	for _, o := range os {
		out = append(out, fmt.Sprintf("%s｜%s", o.ActedAt, o.Outcome))
	}
	return out
}
