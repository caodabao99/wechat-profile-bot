package main

// ═══════════════════════════════════════════════════════════════════════════
// §7 P1：Relationship Session 编排层（PERSONAL RELATIONSHIP OS 3.0）
//
// 本版本最大的产品升级。但蓝图铁律明确：**不要新建第二套 decision / action /
// simulation——直接编排已有能力**。所以本文件只做「装配 + 投影 + 落账」，
// 零新表、零新评分真相、零第二套引擎：
//
//   7.1 Before Brief   ← Context Engine（BuildContactContext(TaskDecision)，唯一入口）
//   7.2 Strategy       ← Decision Engine（BuildDecisionCandidates 取本联系人候选）
//   7.3 Rehearsal      ← Conversation Simulation（LoadRehearsalContext + 既有预演端点）
//                         —— 结果必须显式标 SIMULATION，不得表示真实预测（铁律）
//   7.4 Action         ← Action Ledger（LogAction，source=relationship_session）
//   7.5 Outcome        ← SetActionOutcome(provenance=confirmed)：用户直接反馈 5 档 + 备注
//   7.6 长期观察        ← ObserveActionOutcomes（provenance=estimated，7/14/30 天）
//                         —— estimated 与 confirmed 绝不能混淆（既有铁律已守）
//
// 锁纪律：全程只调既有自锁函数（BuildContactContext / LogAction / SetActionOutcome…），
// 本文件自身绝不持 dbMu、绝不嵌套锁。
// ═══════════════════════════════════════════════════════════════════════════

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ActionSourceSession 是本编排层落账行动的来源标识（已登记进 validActionSources）。
const ActionSourceSession = "relationship_session"

// SessionOutcomeLabel §7.5 用户直接反馈的五档 UI 取值（前端展示用）。
const (
	OutSmooth    = "smooth"    // 顺利
	OutNormal    = "normal"    // 一般
	OutPoor      = "poor"      // 不理想
	OutFailed    = "failed"    // 失败
	OutUndecided = "undecided" // 无法判断
)

// simulationDisclaimer 预演/模拟铁律措辞：任何模拟结果只是排演，绝不冒充真实预测。
const simulationDisclaimer = "以下为 AI 对话预演（SIMULATION），仅用于排演话术，" +
	"不代表对方的真实反应，也不构成对结果的任何预测。"

// sessionObservationDays §7.6 长期观察窗口（天）：动作后按 7/14/30 天回看关系数据估结果。
var sessionObservationDays = []int{7, 14, 30}

// ---- 7.1 Before Brief ----

// BeforeBrief 「开始处理这段关系」时先看的事实全景，全部字段来自 Context Engine。
type BeforeBrief struct {
	RelationshipState string           `json:"relationship_state"`
	Health            int              `json:"health"`
	Change30d         string           `json:"change_30d"` // 最近 30 天变化（确定性摘要）
	ImportantFacts    []string         `json:"important_facts"`
	ConflictFacts     []string         `json:"conflict_facts"`
	OpenItems         []string         `json:"open_items"` // 未完成事项（待回复/承诺/借款…）
	Goals             []string         `json:"goals"`
	Projects          []string         `json:"projects"`
	Risks             []string         `json:"risks"`
	Opportunities     []string         `json:"opportunities"`
	LastAction        string           `json:"last_action"`
	LastActionAt      string           `json:"last_action_at"`
	SimilarOutcomes   []ContextOutcome `json:"similar_outcomes"` // 历史类似行动结果
}

// ---- 7.2 Strategy ----

// SessionStrategy 推荐策略，复用 Decision Engine 输出（不新造决策）。
type SessionStrategy struct {
	Recommended       string   `json:"recommended"`
	Backup            string   `json:"backup"`
	Why               []string `json:"why"`                // 推荐理由
	DataBasis         []string `json:"data_basis"`         // 数据依据
	Confidence        string   `json:"confidence"`         // 置信度档
	HistEffectiveness string   `json:"hist_effectiveness"` // 历史有效性摘要
	Priority          int      `json:"priority"`
	BestTime          string   `json:"best_time,omitempty"`
}

// ---- 7.3 Rehearsal ----

// RehearsalEntry 预演入口：只读装配扮演依据，并恒定标注 SIMULATION。
type RehearsalEntry struct {
	Available  bool              `json:"available"`
	Mode       string            `json:"mode"` // 恒为 "SIMULATION"
	Disclaimer string            `json:"disclaimer"`
	Context    *RehearsalContext `json:"context,omitempty"`
}

// RelationshipSession 「开始处理这段关系」的一屏编排结果：Brief + Strategy + Rehearsal + 观察计划。
type RelationshipSession struct {
	ContactID       int64           `json:"contact_id"`
	Name            string          `json:"name"`
	Now             string          `json:"now"`
	Brief           BeforeBrief     `json:"before_brief"`
	Strategy        SessionStrategy `json:"strategy"`
	Rehearsal       RehearsalEntry  `json:"rehearsal"`
	ObservationDays []int           `json:"observation_days"`
	ContextVersion  string          `json:"context_version"` // 供前端/审计核对装配依据
}

// BuildRelationshipSession 装配一屏 Relationship Session（7.1-7.3）。只读、无副作用。
// 联系人不存在时 BuildContactContext 返回错误 → 调用方转 404（绝不 500 于核心页之外的缺数据）。
func BuildRelationshipSession(db *sql.DB, contactID int64, now time.Time) (*RelationshipSession, error) {
	cc, err := BuildContactContext(db, contactID, TaskDecision, "", now)
	if err != nil {
		return nil, err
	}

	sess := &RelationshipSession{
		ContactID:       contactID,
		Name:            cc.Identity.Name,
		Now:             now.Format(time.RFC3339),
		Brief:           projectBrief(cc, now),
		Strategy:        projectStrategy(db, cc, contactID, now),
		Rehearsal:       projectRehearsal(db, contactID),
		ObservationDays: append([]int{}, sessionObservationDays...),
		ContextVersion:  cc.ContextVersion,
	}
	return sess, nil
}

// projectBrief 把 ContactContext 投影为 7.1 Before Brief（纯映射，不新增真相）。
func projectBrief(cc *ContactContext, now time.Time) BeforeBrief {
	b := BeforeBrief{
		ImportantFacts:  []string{},
		ConflictFacts:   []string{},
		OpenItems:       []string{},
		Goals:           []string{},
		Projects:        []string{},
		Risks:           cc.Risks,
		Opportunities:   cc.Opportunities,
		SimilarOutcomes: cc.PreviousOutcomes,
	}
	if st := cc.CurrentRelationshipState; st != nil {
		b.RelationshipState = strings.TrimSpace(st.BaseState+"/"+st.DynamicState) + " · " + st.TrendState
		b.Health = st.Health
	}
	b.Change30d = summarize30d(cc, now)

	for _, f := range cc.TrustedFacts {
		b.ImportantFacts = append(b.ImportantFacts, factLine(f))
	}
	for _, f := range cc.ConflictingFacts {
		b.ConflictFacts = append(b.ConflictFacts, factLine(f))
	}
	for _, fu := range cc.OpenFollowups {
		if s := strings.TrimSpace(fu.Content); s != "" {
			label := fu.KindLabel
			if label == "" {
				label = fu.Kind
			}
			b.OpenItems = append(b.OpenItems, label+"："+s)
		}
	}
	for _, g := range cc.ActiveGoals {
		if s := strings.TrimSpace(g.Title); s != "" {
			b.Goals = append(b.Goals, s)
		}
	}
	for _, p := range cc.Projects {
		if s := strings.TrimSpace(p.Title); s != "" {
			b.Projects = append(b.Projects, s)
		}
	}
	// 最近一次行动：ActionLog 已按 id 逆序（最近在前），取首条。
	for _, a := range cc.ActionLog {
		if at := firstNonBlankStr(a.ActedAt, a.CreatedAt); at != "" {
			b.LastAction = strings.TrimSpace(a.ActionText)
			b.LastActionAt = at
			break
		}
		b.LastAction = strings.TrimSpace(a.ActionText)
		break
	}
	return b
}

// projectStrategy 复用 Decision Engine：取本联系人的候选，最高优先级为主策略、次为备用。
func projectStrategy(db *sql.DB, cc *ContactContext, contactID int64, now time.Time) SessionStrategy {
	s := SessionStrategy{Why: []string{}, DataBasis: []string{}}
	cands, err := BuildDecisionCandidates(db, now)
	if err != nil {
		return s
	}
	var mine []DecisionCandidate
	for _, c := range cands {
		if c.ContactID == contactID {
			mine = append(mine, c)
		}
	}
	if len(mine) == 0 {
		// 无行动型候选：诚实说明，不硬编策略。
		s.Recommended = "暂无明确行动建议；可先补充最近聊天记录以刷新判断。"
		return s
	}
	top := mine[0]
	s.Recommended = firstNonBlankStr(top.Action, top.Topic, top.Followup, top.Goal, top.State)
	if len(mine) > 1 {
		s.Backup = firstNonBlankStr(mine[1].Action, mine[1].Topic, mine[1].State)
	}
	s.Why = append(s.Why, top.WhyNow...)
	s.Priority = top.Priority
	s.Confidence = top.Confidence
	if top.Source != "" {
		s.DataBasis = append(s.DataBasis, "来源："+top.Source)
	}
	for _, rc := range top.ReasonCodes {
		s.DataBasis = append(s.DataBasis, "依据："+rc)
	}
	s.HistEffectiveness = summarizeEffectiveness(cc.PreviousOutcomes)
	// 补最佳触达时段（与 TodayDecisions 同源，确定性读活跃小时）。
	if m, err := GetAggregatedMetrics(db, int(contactID), decisionBestTimeWindowDays); err == nil && m != nil {
		s.BestTime = peakHourLabel(m.OtherHourHist)
	}
	return s
}

// projectRehearsal 装配 7.3 预演入口：只读扮演依据，恒定标 SIMULATION + 免责。
func projectRehearsal(db *sql.DB, contactID int64) RehearsalEntry {
	e := RehearsalEntry{Mode: "SIMULATION", Disclaimer: simulationDisclaimer}
	rc, err := LoadRehearsalContext(db, contactID)
	if err != nil || rc == nil {
		return e
	}
	e.Available = true
	e.Context = rc
	return e
}

// ---- 7.4 Action ----

// ExecuteSessionAction 点「执行」→ 自动落 Action Ledger(source=relationship_session)。
// 复用 LogAction，不新建第二套 action 表；返回新行动 id（生命周期起点 generated）。
func ExecuteSessionAction(db *sql.DB, contactID int64, actionText, sourceRef string, now time.Time) (int64, error) {
	if strings.TrimSpace(actionText) == "" {
		return 0, fmt.Errorf("行动内容不能为空")
	}
	return LogAction(db, contactID, ActionSourceSession, sourceRef, ActionSourceSession, actionText, now)
}

// ---- 7.5 Outcome ----

// MapSessionOutcome 把用户反馈的 5 档 UI 归一到既有结果极性（不新建第二套 outcome 真相）：
// 顺利→positive 一般→neutral 不理想/失败→negative 无法判断→unknown。未知档返回 false。
func MapSessionOutcome(label string) (string, bool) {
	switch strings.TrimSpace(label) {
	case OutSmooth:
		return ActionOutcomePositive, true
	case OutNormal:
		return ActionOutcomeNeutral, true
	case OutPoor, OutFailed:
		return ActionOutcomeNegative, true
	case OutUndecided:
		return ActionOutcomeUnknown, true
	default:
		return "", false
	}
}

// RecordSessionOutcome §7.5 用户直接反馈（带备注）记为用户确认结果(provenance=confirmed)。
// 复用 SetActionOutcome——其铁律保证系统估算(§7.6)永不覆盖用户确认，二者绝不混淆。
func RecordSessionOutcome(db *sql.DB, actionID int64, label, note string, now time.Time) error {
	outcome, ok := MapSessionOutcome(label)
	if !ok {
		return fmt.Errorf("无效的反馈档位: %q", label)
	}
	return SetActionOutcome(db, actionID, outcome, ActionProvenanceConfirmed, note, now)
}

// ---- 纯函数小工具（无副作用、确定性）----

// summarize30d 用 Context 的 30 天指标 + 趋势给出确定性「最近 30 天变化」摘要。
func summarize30d(cc *ContactContext, now time.Time) string {
	if cc.Metrics == nil {
		return "（近 30 天无互动数据）"
	}
	trend := ""
	if st := cc.CurrentRelationshipState; st != nil && st.TrendState != "" {
		trend = "趋势：" + st.TrendState + "。"
	}
	msg := fmt.Sprintf("近 %d 天对方发言 %d 条、我方 %d 条。",
		cc.Metrics.WindowDay, cc.Metrics.OtherCount, cc.Metrics.MeCount)
	if ev := countRecentEvents(cc, now, 30); ev > 0 {
		msg += fmt.Sprintf("期间有 %d 条时间线事件。", ev)
	}
	return trend + msg
}

// countRecentEvents 统计 RecentEvents 里 eventTime 落在最近 days 天内的条数（解析失败不计）。
func countRecentEvents(cc *ContactContext, now time.Time, days int) int {
	cutoff := now.AddDate(0, 0, -days)
	n := 0
	for _, e := range cc.RecentEvents {
		if t := parseEventTime(e.EventTime); !t.IsZero() && t.After(cutoff) {
			n++
		}
	}
	return n
}

// parseEventTime 容错解析时间线条目的两种常见格式；失败返回零值（诚实跳过）。
func parseEventTime(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// summarizeEffectiveness 把历史行动结果聚成一句诚实的有效性说明。
func summarizeEffectiveness(outs []ContextOutcome) string {
	if len(outs) == 0 {
		return "尚无历史行动结果可参考。"
	}
	var improved, stable, worsened int
	for _, o := range outs {
		switch o.Outcome {
		case "improved":
			improved++
		case "stable":
			stable++
		case "worsened":
			worsened++
		}
	}
	return fmt.Sprintf("历史 %d 次类似行动：改善 %d、持平 %d、转差 %d。", len(outs), improved, stable, worsened)
}

// factLine 把一个事实渲染为「类型：值」单行。
func factLine(f FactView) string {
	key := strings.TrimSpace(f.Key)
	if key != "" && key != f.Type {
		return fmt.Sprintf("%s(%s)：%s", f.Type, key, f.Value)
	}
	return fmt.Sprintf("%s：%s", f.Type, f.Value)
}

// firstNonBlankStr 返回第一个非空白字符串（本地版，避免与其它同名工具冲突）。
func firstNonBlankStr(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// ---- HTTP 编排端点（§7 联系人页「开始处理这段关系」）----

// routeContactSession /api/contacts/{id}/session[/execute|/outcome]。
// 全程只读装配（不调 LLM），故与 LLM 是否配置无关——核心页绝不 500/503。
func (s *apiServer) routeContactSession(w http.ResponseWriter, r *http.Request, id int64, sub []string) {
	if _, err := GetContactByID(s.db, id); err != nil {
		writeErr(w, http.StatusNotFound, "联系人不存在")
		return
	}
	switch {
	case len(sub) == 0 && r.Method == http.MethodGet:
		s.hSessionBrief(w, r, id)
	case len(sub) == 1 && sub[0] == "execute" && r.Method == http.MethodPost:
		s.hSessionExecute(w, r, id)
	case len(sub) == 1 && sub[0] == "outcome" && r.Method == http.MethodPost:
		s.hSessionOutcome(w, r, id)
	default:
		writeErr(w, http.StatusNotFound, "未知接口: "+r.URL.Path)
	}
}

// hSessionBrief GET：一屏装配 Before Brief + Strategy + Rehearsal 入口。
func (s *apiServer) hSessionBrief(w http.ResponseWriter, r *http.Request, id int64) {
	sess, err := BuildRelationshipSession(s.db, id, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "装配关系处理会话失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "session": sess})
}

// hSessionExecute POST：点「执行」→ 自动落 Action Ledger(source=relationship_session)。
func (s *apiServer) hSessionExecute(w http.ResponseWriter, r *http.Request, id int64) {
	var req struct {
		ActionText string `json:"action_text"`
		SourceRef  string `json:"source_ref"`
	}
	if !readBody(w, r, &req) {
		return
	}
	actionID, err := ExecuteSessionAction(s.db, id, req.ActionText, req.SourceRef, time.Now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "action_log_id": actionID, "source": ActionSourceSession})
}

// hSessionOutcome POST：用户对已执行行动的直接反馈（5 档 + 备注）记为 confirmed 结果。
func (s *apiServer) hSessionOutcome(w http.ResponseWriter, r *http.Request, id int64) {
	var req struct {
		ActionID int64  `json:"action_log_id"`
		Label    string `json:"label"`
		Note     string `json:"note"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if err := RecordSessionOutcome(s.db, req.ActionID, req.Label, req.Note, time.Now()); err != nil {
		// 档位非法/记录不存在/已被确认覆盖被拒 → 400，不 500。
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	outcome, _ := MapSessionOutcome(req.Label)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "outcome": outcome, "provenance": ActionProvenanceConfirmed})
}
