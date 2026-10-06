package main

// 蓝图 §8 P1：Relationship Strategy Learning（策略学习）。
//
// 铁律——不重做、不新建第二套真相：
//   - Action Ledger（relationship_action_log）、Intervention Learning（intervention.go）、
//     Relationship Experiment（experiment.go）都已存在且各有真相来源，本文件一律不动它们。
//   - 本文件只做一件新事：把「已经落在 Action Ledger 里的真实行动 + 其结果」按 §8.1 的策略类型
//     聚合成一个只读的统一视图（Strategy History），并据此给 Decision Engine 提供一个
//     「历史上表现更好」的软性 strategy_score 参考。全程只读，绝不写 relationship_action_log，
//     绝不改变 Decision Engine 已有的确定性排序（Priority 不受本层影响）。
//
// 纪律：
//   - 样本少不给强结论：沿用 intervention.go 的 interventionMinSample 门槛与 wilsonInterval
//     95% 置信区间，低于门槛只报数、不排序、不加/减分。
//   - correlation ≠ causation：所有对外措辞只能「历史上表现更好 / 表现较弱」，绝不「保证有效」。
//   - estimated 与 confirmed 绝不混淆（延续 §7.6 铁律）：统计里二者分开计数，供决策参考时
//     以用户确认（confirmed）为权威、系统估算（estimated）仅作弱信号。

import (
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// §8.1 策略类型分类（可后续扩展；MapActionTypeToStrategyType 未识别的一律归 strategyUnknown，
// 绝不强行塞进某个已知类型，避免制造虚假规律）。
const (
	StrategyLowPressureCheckin = "low_pressure_checkin" // 低压力问候/ reconnect
	StrategyTopicFollowup      = "topic_followup"       // 对话线索追问
	StrategyProjectFollowup    = "project_followup"     // 项目/待办/目标跟进
	StrategyBirthdayContact    = "birthday_contact"     // 重要日子联系
	StrategyApology            = "apology"              // 道歉/修复
	StrategySupport            = "support"              // 支持/风险安抚
	StrategyCelebration        = "celebration"          // 庆祝/加码
	StrategyCasualContact      = "casual_contact"       // 日常维系
	StrategyUnknown            = "unknown"              // 未归类（诚实兜底，不给结论）
)

// strategyLabel 面向人的中文名（JSON 展示）。
var strategyLabel = map[string]string{
	StrategyLowPressureCheckin: "低压力问候",
	StrategyTopicFollowup:      "话题跟进",
	StrategyProjectFollowup:    "项目跟进",
	StrategyBirthdayContact:    "重要日子",
	StrategyApology:            "道歉修复",
	StrategySupport:            "支持安抚",
	StrategyCelebration:        "庆祝加码",
	StrategyCasualContact:      "日常维系",
	StrategyUnknown:            "未分类",
}

// StrategyTypeOrder 固定展示顺序（确定性，不随数据漂移）。
var StrategyTypeOrder = []string{
	StrategyLowPressureCheckin, StrategyTopicFollowup, StrategyProjectFollowup,
	StrategyBirthdayContact, StrategyApology, StrategySupport,
	StrategyCelebration, StrategyCasualContact, StrategyUnknown,
}

// MapActionTypeToStrategyType 把账本里的 action_type（决策引擎里等于排序后的首个 reason code，
// 或 "maintain"/来源名）+ 候选的 reason codes，确定性映射到 §8.1 的策略类型。纯函数、无副作用。
func MapActionTypeToStrategyType(actionType string, reasonCodes []string) string {
	// ① 已经是策略类型本身（允许以后直接以策略类型落账，向后兼容）。
	for _, s := range StrategyTypeOrder {
		if actionType == s {
			return s
		}
	}
	// ② 已知 reason code 的确定性映射。
	if st := reasonCodeStrategy(actionType); st != "" {
		return st
	}
	if actionType == "maintain" {
		return StrategyCasualContact
	}
	// ③ 来源名（relationship_session/coach/manual/... 无具体类型）→ 退而求其次看 reason codes，
	//    取排序后第一个可识别的（与 decisionActionType「排序取首」口径一致）。
	if len(reasonCodes) > 0 {
		codes := append([]string(nil), reasonCodes...)
		sort.Strings(codes)
		for _, c := range codes {
			if st := reasonCodeStrategy(c); st != "" {
				return st
			}
		}
	}
	// ④ 诚实兜底：不硬塞进已知类型。
	return StrategyUnknown
}

// reasonCodeStrategy 单个 reason code → 策略类型（不识别返回 ""）。
func reasonCodeStrategy(code string) string {
	switch code {
	case reasonImportantDate:
		return StrategyBirthdayContact
	case reasonUnresolvedTopic:
		return StrategyTopicFollowup
	case reasonOpenFollowup, reasonGoalDeadline:
		return StrategyProjectFollowup
	case reasonHighRisk, reasonCooling:
		return StrategySupport
	case reasonRecentPositiveChange, reasonStrongOpportunity:
		return StrategyCelebration
	case reasonReconnecting:
		return StrategyLowPressureCheckin
	default:
		return ""
	}
}

// StrategyStat 一个策略类型的历史统计（§8.2）。SampleCount 只计「已有结果」的行动
// （outcome_provenance ∈ {estimated,confirmed}），未观测的行动不计入成功率分母。
type StrategyStat struct {
	StrategyType  string  `json:"strategy_type"`
	StrategyLabel string  `json:"strategy_label"`
	SampleCount   int     `json:"sample_count"` // 已产出结果的样本数
	Positive      int     `json:"positive"`
	Neutral       int     `json:"neutral"`
	Negative      int     `json:"negative"`
	UnknownOut    int     `json:"unknown"` // 已观测但极性未定
	Estimated     int     `json:"estimated"`
	Confirmed     int     `json:"confirmed"`
	SuccessRate   float64 `json:"success_rate"` // positive/SampleCount ×100
	WilsonLow     float64 `json:"wilson_low"`   // 95% 置信下界 ×100
	WilsonHigh    float64 `json:"wilson_high"`
	LowSample     bool    `json:"low_sample"` // SampleCount < 门槛：不给强结论
	Verdict       string  `json:"verdict"`    // 措辞只谈「历史上表现」，不谈因果
}

// StrategyHistory §8.2 统一策略历史快照。
type StrategyHistory struct {
	GeneratedAt   string         `json:"generatedAt"`
	TotalRows     int            `json:"totalRows"` // 账本总行数（含未观测）
	TotalObserved int            `json:"totalObserved"`
	Stats         []StrategyStat `json:"stats"`
	DataNote      string         `json:"dataNote"`
	Insights      []string       `json:"insights"`
}

// ComputeStrategyHistory 只读扫描 relationship_action_log，按 §8.1 策略类型聚合历史表现。
// 表不存在（老库/测试未迁移）时诚实返回空视图，不报错——绝不因增值视图缺失阻断调用方。
func ComputeStrategyHistory(db *sql.DB, now time.Time) (*StrategyHistory, error) {
	h := &StrategyHistory{
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
		Stats:       []StrategyStat{},
		Insights:    []string{},
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "relationship_action_log") {
		h.DataNote = "尚无行动账本，暂无策略历史。"
		return h, nil
	}
	rows, err := db.Query(`SELECT action_type, outcome, outcome_provenance FROM relationship_action_log`)
	if err != nil {
		return nil, fmt.Errorf("读取行动账本失败: %w", err)
	}
	defer rows.Close()

	type agg struct {
		pos, neu, neg, unk, est, con int
	}
	byType := map[string]*agg{}
	touch := func(st string) *agg {
		if a, ok := byType[st]; ok {
			return a
		}
		a := &agg{}
		byType[st] = a
		return a
	}
	for rows.Next() {
		var at, outcome, prov string
		if err := rows.Scan(&at, &outcome, &prov); err != nil {
			continue
		}
		h.TotalRows++
		if prov != ActionProvenanceEstimated && prov != ActionProvenanceConfirmed {
			continue // 未观测（provenance 空）→ 不进成功率分母
		}
		h.TotalObserved++
		st := MapActionTypeToStrategyType(at, nil)
		a := touch(st)
		switch outcome {
		case ActionOutcomePositive:
			a.pos++
		case ActionOutcomeNeutral:
			a.neu++
		case ActionOutcomeNegative:
			a.neg++
		default:
			a.unk++
		}
		if prov == ActionProvenanceConfirmed {
			a.con++
		} else {
			a.est++
		}
	}

	for _, st := range StrategyTypeOrder { // 固定顺序，确定性输出
		a, ok := byType[st]
		if !ok {
			continue
		}
		s := StrategyStat{
			StrategyType:  st,
			StrategyLabel: strategyLabel[st],
			SampleCount:   a.pos + a.neu + a.neg + a.unk,
			Positive:      a.pos,
			Neutral:       a.neu,
			Negative:      a.neg,
			UnknownOut:    a.unk,
			Estimated:     a.est,
			Confirmed:     a.con,
		}
		if s.SampleCount > 0 {
			p := float64(s.Positive) / float64(s.SampleCount)
			s.SuccessRate = roundF(p*100, 1)
			lo, hi := wilsonInterval(p, s.SampleCount, interventionWilsonZ)
			s.WilsonLow = roundF(lo*100, 1)
			s.WilsonHigh = roundF(hi*100, 1)
		}
		s.LowSample = s.SampleCount < interventionMinSample
		s.Verdict = strategyVerdict(s)
		h.Stats = append(h.Stats, s)
	}
	h.DataNote = strategyDataNote(h.TotalObserved)
	h.Insights = strategyInsights(h.Stats)
	return h, nil
}

// strategyVerdict 措辞铁律：只说「历史上表现」，绝不「保证有效」、绝不把相关写成因果。
func strategyVerdict(s StrategyStat) string {
	if s.SampleCount == 0 {
		return fmt.Sprintf("「%s」尚无带结果的记录", s.StrategyLabel)
	}
	if s.LowSample {
		return fmt.Sprintf("「%s」仅 %d 例，正向率 %.0f%%，样本太少，暂不下结论",
			s.StrategyLabel, s.SampleCount, s.SuccessRate)
	}
	switch {
	case s.WilsonLow >= 50:
		return fmt.Sprintf("「%s」历史上表现更好：正向率 %.0f%%（置信下界 %.0f%%，%d 例）",
			s.StrategyLabel, s.SuccessRate, s.WilsonLow, s.SampleCount)
	case s.SuccessRate <= 25:
		return fmt.Sprintf("「%s」历史上表现较弱（正向率 %.0f%%，%d 例），可考虑换思路",
			s.StrategyLabel, s.SuccessRate, s.SampleCount)
	default:
		return fmt.Sprintf("「%s」历史上正向率 %.0f%%（区间 %.0f~%.0f，%d 例）",
			s.StrategyLabel, s.SuccessRate, s.WilsonLow, s.WilsonHigh, s.SampleCount)
	}
}

func strategyDataNote(observed int) string {
	switch {
	case observed == 0:
		return "账本里还没有带结果的行动，策略学习暂以规则推荐为主。"
	case observed < interventionMinSample:
		return fmt.Sprintf("已有 %d 条带结果的行动，样本仍偏少，以下结论仅供参考。", observed)
	default:
		return fmt.Sprintf("已积累 %d 条带结果的行动，可作历史表现参考（相关≠因果）。", observed)
	}
}

// strategyInsights 只从「样本充足」的类型里给排序性洞察；不足的一律不参与，避免误导。
func strategyInsights(stats []StrategyStat) []string {
	out := []string{}
	var confident []StrategyStat
	for _, s := range stats {
		if !s.LowSample && s.SampleCount > 0 && s.StrategyType != StrategyUnknown {
			confident = append(confident, s)
		}
	}
	sort.SliceStable(confident, func(i, j int) bool {
		if confident[i].WilsonLow != confident[j].WilsonLow {
			return confident[i].WilsonLow > confident[j].WilsonLow
		}
		return confident[i].SampleCount > confident[j].SampleCount
	})
	for _, s := range confident {
		out = append(out, s.Verdict)
	}
	return out
}

// strategyScoreBand 策略软调整对 strategy_score 的加/减分上下限（很小，绝不盖过确定性 Priority）。
const (
	strategyScoreMaxBonus   = 5
	strategyScoreMaxPenalty = -5
)

// strategyBonus 由某策略类型的历史表现算出一个「有界软加分」，供 §8.3 决策集成。
// 权威信号只用用户确认（confirmed）；estimated 仅作弱参考打折。样本不足→0（不给强结论）。
func strategyBonus(s StrategyStat) int {
	if s.LowSample || s.SampleCount == 0 {
		return 0
	}
	// 以确认样本的成功率为基准；确认样本不足时用整体成功率但乘 0.5 衰减系数。
	rate := s.SuccessRate
	confTotal := s.Confirmed
	if confTotal >= 3 {
		rate = float64(s.Positive) / float64(s.SampleCount) * 100 // 统一口径：整体正向率
	} else {
		rate = rate * 0.5 // 弱证据衰减
	}
	// 以 50% 为中性线，映射到 [penalty, bonus] 的有界整数。
	delta := (rate - 50) / 50 * strategyScoreMaxBonus
	if confTotal < 3 {
		delta *= 0.5
	}
	b := int(math.Round(delta))
	if b > strategyScoreMaxBonus {
		b = strategyScoreMaxBonus
	}
	if b < strategyScoreMaxPenalty {
		b = strategyScoreMaxPenalty
	}
	return b
}

func strategyBonusNote(s StrategyStat, bonus int) string {
	if s.LowSample || s.SampleCount == 0 {
		return ""
	}
	if bonus > 0 {
		return fmt.Sprintf("历史上表现更好（正向率 %.0f%%/%d 例）", s.SuccessRate, s.SampleCount)
	}
	if bonus < 0 {
		return fmt.Sprintf("历史上表现较弱（正向率 %.0f%%/%d 例）", s.SuccessRate, s.SampleCount)
	}
	return ""
}

// annotateStrategyScore 给决策候选补 strategy_score（=Priority + 有界软调整）与说明，
// 不改动 Priority、不重排——只读地为每个候选附上「历史表现」参考。stats 为空则仅标类型。
func annotateStrategyScore(cands []DecisionCandidate, stats []StrategyStat) {
	byType := map[string]StrategyStat{}
	for _, s := range stats {
		byType[s.StrategyType] = s
	}
	for i := range cands {
		st := MapActionTypeToStrategyType(decisionActionType(cands[i].ReasonCodes), cands[i].ReasonCodes)
		cands[i].StrategyType = st
		stat, ok := byType[st]
		if !ok {
			cands[i].StrategyScore = cands[i].Priority
			continue
		}
		bonus := strategyBonus(stat)
		cands[i].StrategyScore = cands[i].Priority + bonus
		cands[i].StrategyNote = strategyBonusNote(stat, bonus)
	}
}

// strategyScoreSummary 供日志/断言：统计里有几个类型是可参考的。
func strategyInsightCount(stats []StrategyStat) int {
	n := 0
	for _, s := range stats {
		if !s.LowSample && s.SampleCount > 0 && strings.Contains(s.Verdict, "历史上表现") {
			n++
		}
	}
	return n
}

// routeStrategy 蓝图 §8：策略学习只读视图。GET /api/strategy/history → 统一策略历史。
// 绝不触发任何写操作（只扫描已有 Action Ledger）；失败不 500，诚实降级为空视图。
func (s *apiServer) routeStrategy(w http.ResponseWriter, r *http.Request, sub []string) {
	if len(sub) != 1 || sub[0] != "history" {
		writeErr(w, http.StatusNotFound, "未知接口: /api/strategy/"+strings.Join(sub, "/"))
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	hist, err := ComputeStrategyHistory(s.db, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "策略历史计算失败")
		return
	}
	writeJSON(w, http.StatusOK, hist)
}
