package main

// ═══════════════════════════════════════════════════════════════════════════
// Memory Replay「重新认识 TA」（PERSONAL RELATIONSHIP OS 2.0 · Phase 7，规格「十、P6」）
//
// 定位：不是普通 summary，而是一份**Relationship Replay**——沿时间把这段关系重新讲一遍：
// 首次认识 → 发展阶段 → 重要转折 → 兴趣/职业变化 → 升温降温 → 共同事件 → 主题变化 →
// 当前状态/关注点 → 长期目标 → 未完成事项。
//
// 铁律（规格第十章）：
//   - **优先确定性时间线**：所有段落由既有结构化数据（timeline/state_history/facts/topics/
//     projects/followups/metrics）确定性拼装，LLM 只负责「组织语言」，**绝不让模型创造事实**。
//     因此本层核心 BuildRelationshipReplay 不依赖 LLM，天然可测、可复现、永不 503。
//   - 10.1 Replay Evidence：每条重要结论都带**来源层**（timeline/fact/topic/metric/message）
//     与可回溯引用（ref_id/label/time），前端点击可回到具体数据。
//   - 单连接池分层锁：全部输入取自自锁顶层 reader，逐个顺序调用、绝不嵌套 dbMu；任一块缺失即优雅跳过。
// ═══════════════════════════════════════════════════════════════════════════

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ReplayEvidence 一条结论的来源证据（规格 10.1）。Source∈timeline/fact/topic/metric/message/state。
type ReplayEvidence struct {
	Source string `json:"source"`
	RefID  int64  `json:"ref_id,omitempty"` // 可回溯的主键（fact.id / 事件 id 等），派生项为 0
	Label  string `json:"label,omitempty"`  // 展示用出处
	Time   string `json:"time,omitempty"`
}

// ReplayStatement 一条结论 + 其证据集。
type ReplayStatement struct {
	Text     string           `json:"text"`
	Evidence []ReplayEvidence `json:"evidence"`
}

// ReplaySection 一个叙事段落（对应规格第十章的一种结构块）。
type ReplaySection struct {
	Key   string            `json:"key"`
	Title string            `json:"title"`
	Items []ReplayStatement `json:"items"`
}

// RelationshipReplay 一份完整的关系回放。
type RelationshipReplay struct {
	ContactID   int64           `json:"contact_id"`
	Name        string          `json:"name"`
	GeneratedAt string          `json:"generated_at"`
	Sections    []ReplaySection `json:"sections"`
}

// 段落键（顺序即叙事顺序）。
const (
	replayFirstMeeting  = "first_meeting"
	replayStages        = "stages"
	replayTurningPoints = "turning_points"
	replayInterests     = "interest_evolution"
	replayCareer        = "career_changes"
	replayHeating       = "heating_cooling"
	replaySharedEvents  = "shared_events"
	replayThemes        = "theme_changes"
	replayCurrentState  = "current_state"
	replayCurrentFocus  = "current_focus"
	replayGoals         = "long_term_goals"
	replayOpenItems     = "open_items"
)

func replaySectionTitles() map[string]string {
	return map[string]string{
		replayFirstMeeting: "首次认识", replayStages: "关系发展阶段", replayTurningPoints: "重要转折",
		replayInterests: "长期兴趣变化", replayCareer: "职业变化", replayHeating: "关系升温/降温",
		replaySharedEvents: "共同事件", replayThemes: "关键主题变化", replayCurrentState: "当前状态",
		replayCurrentFocus: "当前关注点", replayGoals: "长期目标", replayOpenItems: "未完成事项",
	}
}

// replayBuilder 累积各段结论，最后按固定顺序产出非空段落。
type replayBuilder struct {
	byKey map[string][]ReplayStatement
}

func newReplayBuilder() *replayBuilder {
	return &replayBuilder{byKey: map[string][]ReplayStatement{}}
}

func (b *replayBuilder) add(key, text string, ev ...ReplayEvidence) {
	if strings.TrimSpace(text) == "" {
		return
	}
	b.byKey[key] = append(b.byKey[key], ReplayStatement{Text: text, Evidence: ev})
}

// BuildRelationshipReplay 确定性拼装关系回放（不依赖 LLM，永不因缺 LLM 报错）。
func BuildRelationshipReplay(db *sql.DB, contactID int64, now time.Time) (*RelationshipReplay, error) {
	contact, err := GetContactByID(db, contactID)
	if err != nil {
		return nil, err
	}
	b := newReplayBuilder()

	// 时间线：首次认识 + 共同事件。
	events, _ := GetContactTimeline(db, contactID, 60)
	if ev := firstMeetingStatement(events); ev != nil {
		b.add(replayFirstMeeting, ev.text, ev.evidence...)
	}
	for _, e := range events {
		if e.Kind == "custom" || e.Kind == "first_message" || e.Kind == "last_message" {
			b.add(replaySharedEvents, fmt.Sprintf("%s：%s", e.EventTime, e.Title),
				ReplayEvidence{Source: "timeline", RefID: e.ID, Label: e.Kind, Time: e.EventTime})
		}
	}

	// 状态历史（新→旧）：反转成时间正序，用于发展阶段 / 转折 / 升降温。
	hist, _ := GetRelationshipStateHistory(db, contactID, 200)
	chrono := reverseHistory(hist)
	for _, h := range chrono {
		nb, _ := h["newBase"].(string)
		pb, _ := h["prevBase"].(string)
		nd, _ := h["newDynamic"].(string)
		pd, _ := h["prevDynamic"].(string)
		at, _ := h["changedAt"].(string)
		reason, _ := h["reason"].(string)
		ev := ReplayEvidence{Source: "state", Label: "relationship_state_history", Time: at}
		if nb != "" && nb != pb {
			b.add(replayStages, fmt.Sprintf("%s：关系进入「%s」阶段", at, nb), ev)
		}
		switch nd {
		case dynAtRisk, dynDormant, dynReconnecting:
			if nd != pd {
				txt := fmt.Sprintf("%s：%s", at, replayDynamicCn(nd))
				if reason != "" {
					txt += "（" + reason + "）"
				}
				b.add(replayTurningPoints, txt, ev)
			}
		}
		if nd != pd && (nd == dynWarming || nd == dynCooling || pd == dynWarming || pd == dynCooling) {
			b.add(replayHeating, fmt.Sprintf("%s：%s → %s", at, replayDynamicCn(pd), replayDynamicCn(nd)), ev)
		}
	}

	// 事实：职业变化（含被取代链）+ 兴趣演变（现态 vs 已退役/被取代）。
	facts, _ := GetFacts(db, contactID, true)
	buildFactReplay(b, facts)

	// 主题：关键主题变化 + 当前关注点。
	if tp, err := GetContactTopics(db, contactID, 12); err == nil && tp != nil {
		buildTopicReplay(b, tp)
	}

	// 当前状态。
	if st, err := GetRelationshipState(db, contactID); err == nil && st != nil {
		b.add(replayCurrentState, fmt.Sprintf("当前：%s / %s，亲密度 %d，趋势 %s，预警 %s",
			st.BaseState, st.DynamicState, st.Intimacy, st.TrendState, st.Alert),
			ReplayEvidence{Source: "state", Label: "relationship_state"})
	}

	// 长期目标（活跃项目）+ 未完成事项（open 待办 + paused 项目）。
	if ps, err := ListProjects(db, contactID, "open"); err == nil {
		for _, p := range ps {
			line := p.Title + "［" + p.Stage + "］"
			if p.NextAction != "" {
				line += " 下一步：" + p.NextAction
			}
			b.add(replayGoals, line, ReplayEvidence{Source: "fact", RefID: p.ID, Label: "relationship_project"})
			if p.Status == projectStatusPaused {
				b.add(replayOpenItems, p.Title+"（已暂停）", ReplayEvidence{Source: "fact", RefID: p.ID, Label: "project:paused"})
			}
		}
	}
	if fus, err := ListFollowups(db, "open", 0); err == nil {
		for _, f := range fus {
			if f.ContactID != contactID {
				continue
			}
			b.add(replayOpenItems, f.KindLabel+"："+strings.TrimSpace(f.Content),
				ReplayEvidence{Source: "message", RefID: f.ID, Label: "followup"})
		}
	}

	// 指标佐证升降温（近况）。
	if m, err := GetAggregatedMetrics(db, int(contactID), 90); err == nil && m != nil {
		b.add(replayHeating, fmt.Sprintf("近 90 天：我发 %d 条 / 对方发 %d 条", m.MeCount, m.OtherCount),
			ReplayEvidence{Source: "metric", Label: "aggregated_metrics"})
	}

	sections := []ReplaySection{}
	for _, key := range []string{replayFirstMeeting, replayStages, replayTurningPoints, replayInterests,
		replayCareer, replayHeating, replaySharedEvents, replayThemes, replayCurrentState, replayCurrentFocus,
		replayGoals, replayOpenItems} {
		items := b.byKey[key]
		if len(items) == 0 {
			continue // 无据不造段（宁可缺段，绝不编造）
		}
		sections = append(sections, ReplaySection{Key: key, Title: replaySectionTitles()[key], Items: items})
	}
	return &RelationshipReplay{
		ContactID:   contactID,
		Name:        contact.Name,
		GeneratedAt: now.Format(time.RFC3339),
		Sections:    sections,
	}, nil
}

// replayDynamicCn 动态态中文展示名。
func replayDynamicCn(dyn string) string {
	switch dyn {
	case dynStable:
		return "平稳"
	case dynWarming:
		return "升温"
	case dynCooling:
		return "降温"
	case dynAtRisk:
		return "风险（快凉）"
	case dynDormant:
		return "沉寂"
	case dynReconnecting:
		return "重连"
	}
	return dyn
}

// firstMeetingStatement 从时间线取最早的「第一次聊天/创建」作为首次认识。
func firstMeetingStatement(events []TimelineItem) *struct {
	text     string
	evidence []ReplayEvidence
} {
	var pick *TimelineItem
	for i := range events {
		e := events[i]
		if e.Kind != "first_message" && e.Kind != "created" {
			continue
		}
		if pick == nil || e.RawTime < pick.RawTime {
			pick = &e
		}
	}
	if pick == nil {
		return nil
	}
	label := "第一次聊天"
	if pick.Kind == "created" {
		label = "联系人创建"
	}
	return &struct {
		text     string
		evidence []ReplayEvidence
	}{
		text:     fmt.Sprintf("首次认识可追溯到 %s（%s）", pick.EventTime, label),
		evidence: []ReplayEvidence{{Source: "timeline", RefID: pick.ID, Label: pick.Kind, Time: pick.EventTime}},
	}
}

// reverseHistory 把（新→旧）历史反转成时间正序，供叙事按时间推进。
func reverseHistory(hist []map[string]interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, len(hist))
	for i, h := range hist {
		out[len(hist)-1-i] = h
	}
	return out
}

// buildFactReplay 职业演变链 + 兴趣演变。
func buildFactReplay(b *replayBuilder, facts []FactView) {
	evOf := func(f FactView) ReplayEvidence {
		return ReplayEvidence{Source: "fact", RefID: f.ID, Label: f.Type + "/" + f.Status, Time: f.LastSeen}
	}
	// 职业：现值 + 被取代的历史值（superseded/retired 的同类型事实构成演变链）。
	var curOcc []FactView
	var pastOcc []FactView
	for _, f := range facts {
		if f.Type != "occupation" {
			continue
		}
		if f.Status == "active" || f.Status == "confirmed" || f.Status == "verified" {
			curOcc = append(curOcc, f)
		} else if f.Status == "superseded" || f.Status == "retired" {
			pastOcc = append(pastOcc, f)
		}
	}
	sort.SliceStable(pastOcc, func(i, j int) bool { return pastOcc[i].ValidFrom < pastOcc[j].ValidFrom })
	for _, f := range pastOcc {
		b.add(replayCareer, "曾任/曾任职："+f.Value, evOf(f))
	}
	for _, f := range curOcc {
		b.add(replayCareer, "现职："+f.Value, evOf(f))
	}
	// 兴趣演变：现役 vs 已退役/被取代。
	var liveInt, goneInt []FactView
	for _, f := range facts {
		if f.Type != "interest" {
			continue
		}
		if f.Status == "active" || f.Status == "confirmed" || f.Status == "verified" {
			liveInt = append(liveInt, f)
		} else {
			goneInt = append(goneInt, f)
		}
	}
	for _, f := range liveInt {
		b.add(replayInterests, "持续兴趣："+f.Value, evOf(f))
	}
	for _, f := range goneInt {
		b.add(replayInterests, "曾热衷（"+f.Status+"）："+f.Value, evOf(f))
	}
}

// buildTopicReplay 主题变化（emergent/fading）+ 当前关注点（最新周）。
func buildTopicReplay(b *replayBuilder, tp *ContactTopics) {
	for _, w := range tp.Weeks {
		for _, t := range w.Topics {
			switch t.Status {
			case "emergent":
				b.add(replayThemes, fmt.Sprintf("%s 新出现话题：%s（占比 %d%%）", w.WeekStart, t.Name, t.Weight),
					ReplayEvidence{Source: "topic", Label: t.Status, Time: w.WeekStart})
			case "fading":
				b.add(replayThemes, fmt.Sprintf("%s 话题淡出：%s", w.WeekStart, t.Name),
					ReplayEvidence{Source: "topic", Label: t.Status, Time: w.WeekStart})
			}
		}
	}
	// 当前关注点 = 最新一周的活跃话题。
	if n := len(tp.Weeks); n > 0 {
		latest := tp.Weeks[n-1]
		for _, t := range latest.Topics {
			b.add(replayCurrentFocus, fmt.Sprintf("%s（%s，%d%%）", t.Name, t.Status, t.Weight),
				ReplayEvidence{Source: "topic", Label: "week:" + latest.WeekStart})
		}
	}
}

// RenderReplayText 确定性渲染回放为纯文本（LLM 缺席时的最终产物，亦是 LLM 的输入蓝本）。
func RenderReplayText(r *RelationshipReplay) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("# 重新认识 TA：" + r.Name + "\n\n")
	if len(r.Sections) == 0 {
		b.WriteString("（尚无足够可回溯的数据形成回放。）\n")
		return b.String()
	}
	for _, s := range r.Sections {
		b.WriteString("## " + s.Title + "\n")
		for _, it := range s.Items {
			b.WriteString("- " + it.Text)
			if srcs := evidenceSources(it.Evidence); srcs != "" {
				b.WriteString(" 〔来源：" + srcs + "〕")
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// evidenceSources 去重合并证据来源层（供展示「来自哪些源」，规格 7.4/10.1）。
func evidenceSources(evs []ReplayEvidence) string {
	seen := map[string]bool{}
	out := []string{}
	for _, e := range evs {
		if e.Source == "" || seen[e.Source] {
			continue
		}
		seen[e.Source] = true
		out = append(out, e.Source)
	}
	return strings.Join(out, ",")
}
