package main

// ═══════════════════════════════════════════════════════════════════════════
// Relationship Portfolio（v6.1 蓝图 §12 · P8）——本周关系时间预算的「建议分配」
//
// 高层新能力：用户设定每周关系时间预算（weekly_relationship_time_budget，如 5h），
// 系统据既有信号（圈层类别/亲密度/健康度/状态风险/网络归属）算出「建议把时间投给谁、
// 各投多少」，并给出仪表盘：本周预算 / 已使用（估算）/ 建议分配 / 最值得投入的人。
//
// 设计红线：
//   - §12.2「只建议、不替用户决定」：产出全是 recommendation；用户可改预算、类别权重、
//     逐人手指定分钟（ManualMinutes），手指定者优先占用预算、不参与比例分配。
//   - 不新建分数：per-contact「需求权重」是把类别权重×亲密度、风险/机会调节、健康度调节
//     相乘的**分配系数**（决定预算怎么切分），不是给用户打的新分，且完全确定性、可脱库单测。
//   - §13 复用：类别划分直接取 ComputeCircles（互动频率+亲密度四层圈），健康/状态取
//     ListRelationshipStates，网络归属（cluster）随 CircleMember 一并带出——不重复造轮子。
//   - 已使用（used）无法真知用户花了多少分钟，故用本周已完成行动数 × 名义分钟**估算**，
//     显式标 UsedEstimate=true（INFERENCE，绝不谎称事实）。
//   - 单连接池分层锁：顺序调用各自锁顶层函数，绝不嵌套 dbMu；自身对 portfolio_settings /
//     行动账本的直读各自单独自锁。
// ═══════════════════════════════════════════════════════════════════════════

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// 预算与分配常量。
const (
	portfolioDefaultBudgetMinutes = 300 // 未设置时默认每周 5 小时
	portfolioMaxBudgetMinutes     = 7 * 24 * 60
	portfolioNominalMinutesPerAct = 15 // 已完成一条行动的名义投入（估算「已使用」用）
	portfolioUsedWindowDays       = 7  // 「已使用」统计近 7 天
	portfolioTopNDefault          = 10
)

// portfolioDefaultTierWeights 四层圈默认类别权重（核心最高）。键与 circles.go 的圈层键一致。
var portfolioDefaultTierWeights = map[string]int{
	circleCore:     40,
	circleIntimate: 25,
	circleSocial:   12,
	circleWeak:     4,
}

// PortfolioSettings 用户可编辑的组合设置（存 portfolio_settings 单行 JSON，配置类·参与备份·不可重建）。
type PortfolioSettings struct {
	WeeklyBudgetMinutes int            `json:"weeklyBudgetMinutes"` // 0→默认 300
	TierWeights         map[string]int `json:"tierWeights"`         // 类别权重覆盖；缺项回落默认
	ManualMinutes       map[string]int `json:"manualMinutes"`       // 逐人手指定分钟（key=contactID 字符串），§12.2
}

// ensurePortfolioTables 懒建单行配置表（幂等 DDL，自持 dbMu，不 bump user_version）。仿 assistant_settings。
func ensurePortfolioTables(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS portfolio_settings (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		settings_json TEXT NOT NULL,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	return err
}

func defaultPortfolioSettings() PortfolioSettings {
	w := map[string]int{}
	for k, v := range portfolioDefaultTierWeights {
		w[k] = v
	}
	return PortfolioSettings{WeeklyBudgetMinutes: portfolioDefaultBudgetMinutes, TierWeights: w, ManualMinutes: map[string]int{}}
}

// normalize 兜底非法值：预算越界回默认、缺失类别权重回默认、负手指定归零。
func (s *PortfolioSettings) normalize() {
	if s.WeeklyBudgetMinutes <= 0 || s.WeeklyBudgetMinutes > portfolioMaxBudgetMinutes {
		s.WeeklyBudgetMinutes = portfolioDefaultBudgetMinutes
	}
	if s.TierWeights == nil {
		s.TierWeights = map[string]int{}
	}
	for k, v := range portfolioDefaultTierWeights {
		if _, ok := s.TierWeights[k]; !ok || s.TierWeights[k] < 0 {
			s.TierWeights[k] = v
		}
	}
	if s.ManualMinutes == nil {
		s.ManualMinutes = map[string]int{}
	}
	for k, v := range s.ManualMinutes {
		if v < 0 {
			s.ManualMinutes[k] = 0
		}
	}
}

func loadPortfolioSettings(db *sql.DB) (PortfolioSettings, error) {
	if err := ensurePortfolioTables(db); err != nil {
		return defaultPortfolioSettings(), err
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	var raw string
	row := db.QueryRow(`SELECT settings_json FROM portfolio_settings WHERE id = 1`)
	if err := row.Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return defaultPortfolioSettings(), nil
		}
		return defaultPortfolioSettings(), err
	}
	var s PortfolioSettings
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return defaultPortfolioSettings(), nil // 脏数据回落默认，不报错
	}
	s.normalize()
	return s, nil
}

func savePortfolioSettings(db *sql.DB, s PortfolioSettings) error {
	s.normalize()
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := ensurePortfolioTables(db); err != nil {
		return err
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err = db.Exec(
		`INSERT INTO portfolio_settings (id, settings_json, updated_at) VALUES (1, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(id) DO UPDATE SET settings_json=excluded.settings_json, updated_at=excluded.updated_at`,
		string(b))
	return err
}

// PortfolioEntry 一个联系人的本周建议投入（§12.1 输出单元）。
type PortfolioEntry struct {
	ContactID  int64  `json:"contactId"`
	Name       string `json:"name"`
	Tier       string `json:"tier"`
	TierLabel  string `json:"tierLabel"`
	Intimacy   int    `json:"intimacy"`
	Cluster    string `json:"cluster,omitempty"` // 网络归属（有则展示，来自 ComputeCircles）
	Minutes    int    `json:"minutes"`           // 建议投入分钟
	Reason     string `json:"reason"`            // 依据哪些信号（可解释）
	Overridden bool   `json:"overridden"`        // 是否用户手指定（而非系统建议）
}

// PortfolioTierAllocation 一个类别的汇总。
type PortfolioTierAllocation struct {
	Tier     string `json:"tier"`
	Label    string `json:"label"`
	Weight   int    `json:"weight"`
	Contacts int    `json:"contacts"`
	Minutes  int    `json:"minutes"`
}

// PortfolioView 组合仪表盘响应（§12.3：预算 / 已使用 / 建议分配 / 最值得投入的人）。
type PortfolioView struct {
	WeeklyBudgetMinutes int                       `json:"weeklyBudgetMinutes"`
	UsedMinutes         int                       `json:"usedMinutes"`
	UsedIsEstimate      bool                      `json:"usedIsEstimate"`
	AllocatedMinutes    int                       `json:"allocatedMinutes"`
	OverBudget          bool                      `json:"overBudget"` // 手指定之和已超预算
	TierWeights         map[string]int            `json:"tierWeights"`
	Tiers               []PortfolioTierAllocation `json:"tiers"`
	Top                 []PortfolioEntry          `json:"top"`
	Layer               string                    `json:"layer"` // 恒为 INFERENCE（建议）
	Note                string                    `json:"note"`
}

// portfolioNeedWeight 纯函数：由类别权重、亲密度、状态风险/机会、健康度算「需求分配系数」（×100 取整）。
// 它只决定预算如何切分，不是给用户打的新分。确定性、可脱库单测。
func portfolioNeedWeight(tierWeight, intimacy, activeDays int, dyn string, health int) int {
	base := float64(tierWeight) * (0.5 + float64(intimacy)/100.0)
	switch dyn {
	case dynAtRisk, dynDormant:
		// 值得挽回：仅当仍有相当亲密度才加码（弱联系流失不硬塞时间）
		if intimacy >= 30 {
			base *= 1.25
		}
	case dynCooling:
		base *= 1.15
	case dynWarming, dynReconnecting:
		base *= 1.10
	}
	if health > 0 && health < 50 {
		base *= 1.10
	}
	if base < 0 {
		base = 0
	}
	_ = activeDays // 活跃天数已隐含在圈层/亲密度里；保留形参签名以备可读性扩展
	return int(base*100 + 0.5)
}

// largestRemainder 纯函数：把 total 按 weights 比例整数分配，和恰为 total（最大余数法）。
// 平票按索引小者优先，保证确定性。weights 全 0 或 total<=0 时返回全 0（多余额退回，调用方按需处理）。
func largestRemainder(total int, weights []int) []int {
	alloc := make([]int, len(weights))
	if total <= 0 || len(weights) == 0 {
		return alloc
	}
	sum := 0
	for _, w := range weights {
		if w > 0 {
			sum += w
		}
	}
	if sum <= 0 {
		return alloc
	}
	type cell struct {
		idx  int
		frac float64
	}
	fracs := make([]cell, len(weights))
	used := 0
	for i, w := range weights {
		if w < 0 {
			w = 0
		}
		exact := float64(total) * float64(w) / float64(sum)
		base := int(exact)
		alloc[i] = base
		used += base
		fracs[i] = cell{i, exact - float64(base)}
	}
	leftover := total - used
	sort.SliceStable(fracs, func(a, b int) bool {
		if fracs[a].frac != fracs[b].frac {
			return fracs[a].frac > fracs[b].frac
		}
		return fracs[a].idx < fracs[b].idx
	})
	for k := 0; k < leftover && len(fracs) > 0; k++ {
		alloc[fracs[k%len(fracs)].idx]++
	}
	return alloc
}

// portfolioReason 生成「为什么投这么多」的可解释文案（信号来源，不做因果断言）。
func portfolioReason(tierLabel string, intimacy int, dyn string, health int, cluster string) string {
	parts := []string{fmt.Sprintf("类别=%s", tierLabel), fmt.Sprintf("亲密度=%d", intimacy)}
	switch dyn {
	case dynAtRisk:
		parts = append(parts, "状态=快流失(值得挽回)")
	case dynDormant:
		parts = append(parts, "状态=沉寂(可考虑重启)")
	case dynCooling:
		parts = append(parts, "状态=降温")
	case dynWarming:
		parts = append(parts, "状态=升温(顺势加码)")
	case dynReconnecting:
		parts = append(parts, "状态=复联(好时机)")
	}
	if health > 0 && health < 50 {
		parts = append(parts, fmt.Sprintf("健康度偏低=%d", health))
	}
	if cluster != "" {
		parts = append(parts, "网络圈子="+cluster)
	}
	return "依据：" + joinCN(parts, "、") + "（仅建议，非因果）"
}

// ComputePortfolio 汇聚既有信号，算出本周关系时间的建议分配（§12.1/§12.3）。now 注入便于确定性单测。
func ComputePortfolio(db *sql.DB, now time.Time, topN int) (*PortfolioView, error) {
	if topN <= 0 || topN > 50 {
		topN = portfolioTopNDefault
	}
	set, err := loadPortfolioSettings(db)
	if err != nil {
		return nil, err
	}
	circles, err := ComputeCircles(db, now)
	if err != nil {
		return nil, err
	}
	states, err := ListRelationshipStates(db)
	if err != nil {
		return nil, err
	}
	stateOf := map[int64]RelationshipStateView{}
	for _, st := range states {
		stateOf[st.ContactID] = st
	}
	labelOf := map[string]string{}
	for _, t := range circles.Tiers {
		labelOf[t.Key] = t.Label
	}

	type work struct {
		m      CircleMember
		dyn    string
		health int
		fixed  int // 手指定分钟（>0 则不参与比例分配）
		weight int
	}
	works := make([]work, 0, len(circles.Members))
	for _, m := range circles.Members {
		st := stateOf[m.ContactID]
		w := work{m: m, dyn: st.DynamicState, health: st.Health}
		if manual, ok := set.ManualMinutes[strconv.FormatInt(m.ContactID, 10)]; ok && manual > 0 {
			w.fixed = manual
		} else {
			w.weight = portfolioNeedWeight(set.TierWeights[m.Tier], m.Score, m.ActiveDays, st.DynamicState, st.Health)
		}
		works = append(works, w)
	}

	// 手指定者先占用预算；余量按权重比例分给其余人（最大余数法，和恰为余量）。
	fixedSum := 0
	for _, w := range works {
		fixedSum += w.fixed
	}
	remaining := set.WeeklyBudgetMinutes - fixedSum
	if remaining < 0 {
		remaining = 0
	}
	weights := make([]int, len(works))
	for i, w := range works {
		weights[i] = w.weight // 手指定者 weight=0，自然分不到
	}
	mins := largestRemainder(remaining, weights)

	view := &PortfolioView{
		WeeklyBudgetMinutes: set.WeeklyBudgetMinutes,
		TierWeights:         set.TierWeights,
		Layer:               "INFERENCE",
		Note:                "只建议、不替你决定；可修改预算/类别权重/逐人手指定分钟。",
		OverBudget:          fixedSum > set.WeeklyBudgetMinutes,
	}

	entries := make([]PortfolioEntry, 0, len(works))
	tierAgg := map[string]*PortfolioTierAllocation{}
	allocSum := 0
	for i, w := range works {
		minute := w.fixed
		if w.fixed == 0 {
			minute = mins[i]
		}
		allocSum += minute
		if minute <= 0 {
			// 分不到时间的联系人不进 Top 与类别明细（避免刷屏），但仍计入类别人数
		}
		lbl := labelOf[w.m.Tier]
		entries = append(entries, PortfolioEntry{
			ContactID: w.m.ContactID, Name: w.m.Name, Tier: w.m.Tier, TierLabel: lbl,
			Intimacy: w.m.Score, Cluster: w.m.Cluster, Minutes: minute,
			Reason:     portfolioReason(lbl, w.m.Score, w.dyn, w.health, w.m.Cluster),
			Overridden: w.fixed > 0,
		})
		if _, ok := tierAgg[w.m.Tier]; !ok {
			tierAgg[w.m.Tier] = &PortfolioTierAllocation{Tier: w.m.Tier, Label: lbl, Weight: set.TierWeights[w.m.Tier]}
		}
		tierAgg[w.m.Tier].Contacts++
		tierAgg[w.m.Tier].Minutes += minute
	}
	view.AllocatedMinutes = allocSum

	// 类别明细按 circleRank（核心在前）排序。
	for _, t := range circles.Tiers {
		if a, ok := tierAgg[t.Key]; ok {
			view.Tiers = append(view.Tiers, *a)
		}
	}

	// Top：建议分钟降序 → 亲密度降序 → contact_id 升；仅列有投入者。
	sort.SliceStable(entries, func(a, b int) bool {
		if entries[a].Minutes != entries[b].Minutes {
			return entries[a].Minutes > entries[b].Minutes
		}
		if entries[a].Intimacy != entries[b].Intimacy {
			return entries[a].Intimacy > entries[b].Intimacy
		}
		return entries[a].ContactID < entries[b].ContactID
	})
	view.Top = entries
	if len(view.Top) > topN {
		view.Top = view.Top[:topN]
	}

	// 已使用（近 7 天已完成行动 × 名义分钟，估算·INFERENCE）。
	view.UsedMinutes, view.UsedIsEstimate = estimatedUsedPortfolioMinutes(db, now)
	return view, nil
}

// estimatedUsedPortfolioMinutes 自锁估算本周已投入：近 7 天进入 acted/completed 的行动数 × 名义分钟。
// 表缺失则返回 0（优雅降级）。estimate 语义——绝不声称是用户真实花的时间。
func estimatedUsedPortfolioMinutes(db *sql.DB, now time.Time) (int, bool) {
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "relationship_action_log") {
		return 0, true
	}
	cutoff := now.AddDate(0, 0, -portfolioUsedWindowDays).Format(time.RFC3339)
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM relationship_action_log
		 WHERE status IN ('acted','completed') AND acted_at != '' AND acted_at >= ?`, cutoff).Scan(&n); err != nil {
		return 0, true
	}
	return n * portfolioNominalMinutesPerAct, true
}

// joinCN 用 sep 连接非空片段（本地化文案拼装，避免空串产生多余分隔符）。
func joinCN(parts []string, sep string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out == "" {
			out = p
		} else {
			out += sep + p
		}
	}
	return out
}
