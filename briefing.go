package main

// 高阶洞察 · Phase D：主动人生简报（编排层）。
//
// 设计原则：减负的终极形态——把人生模拟器 + 网络/自我/干预四层，融成一页
//   「打开即行动」的每周摘要。本层不做新计算，只**只读**各层缓存并合成结论：
//   - 各层 GetCachedXxx 内部各自取锁并释放，这里顺序调用、绝不外持 dbMu（避免嵌套死锁）
//   - 缺哪层的缓存就优雅跳过那段，绝不因某层没算过而整页失败
//   - 模板化确定性文案，无 LLM
//   - 缓存 briefing_cache
//
// ComputeAdvancedInsights 是四层 + 简报的有序重算流水线，供周一调度与手动 recompute 复用；
//   它先确保人生状态基座新鲜（A/B/C 都依赖它），再依次算网络/自我/干预，最后合成简报。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// BriefingAction 一条本周行动建议。
type BriefingAction struct {
	Title    string `json:"title"`
	Who      string `json:"who,omitempty"`
	Detail   string `json:"detail"`
	Source   string `json:"source"` // network / life / projection / self / intervention
	Priority int    `json:"priority"`
}

// Briefing 一页主动人生简报。
type Briefing struct {
	GeneratedAt string           `json:"generatedAt"`
	Headline    string           `json:"headline"`
	HealthLine  string           `json:"healthLine"`
	TopActions  []BriefingAction `json:"topActions"`
	Signals     []string         `json:"signals"`
	Notes       []string         `json:"notes"`
}

const briefingMaxActions = 3

// ComputeAdvancedInsights 有序重算四件套 + 简报（供调度与手动重算复用）。
func ComputeAdvancedInsights(db *sql.DB, now time.Time) error {
	// 0) 基座：人生状态/推演缺失或过期则先补算（网络/自我/干预都依赖它）
	if st, genAt, _ := GetCachedLifeState(db); st == nil || IsLifeStateStale(genAt, now) {
		if err := ComputeLifeState(db, now); err != nil {
			return err
		}
	}
	if pj, genAt, _ := GetCachedLifeProjection(db); pj == nil || IsLifeStateStale(genAt, now) {
		if err := ProjectLifeForward(db, now, 90); err != nil {
			return err
		}
	}
	// 1) 结构层
	if err := ComputeNetworkInsights(db, now); err != nil {
		return err
	}
	// 2) 镜像层
	if err := ComputeSelfPortrait(db, now); err != nil {
		return err
	}
	// 3) 因果层
	if err := ComputeInterventionLearning(db, now); err != nil {
		return err
	}
	// 4) 编排层
	return GenerateBriefing(db, now)
}

// GenerateBriefing 只读各层缓存合成简报并写入缓存（不触发各层重算）。
func GenerateBriefing(db *sql.DB, now time.Time) error {
	br := buildBriefing(now,
		getCachedNoRecomputeState(db),
		getCachedNoRecomputeProjection(db),
		getCachedNoRecomputeNetwork(db),
		getCachedNoRecomputeSelf(db),
		getCachedNoRecomputeIntervention(db),
	)
	return saveBriefingCache(db, now, br)
}

// buildBriefing 纯合成，供单测直接喂入各层快照调用。
func buildBriefing(now time.Time,
	state *LifeState, proj *LifeProjection, net *NetworkInsight,
	self *SelfPortrait, learn *InterventionInsight) *Briefing {

	br := &Briefing{
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
		TopActions:  []BriefingAction{},
		Signals:     []string{},
		Notes:       []string{},
	}

	var acts []BriefingAction

	// —— 来自结构层：高风险桥梁人物最该先保 ——
	if net != nil {
		for _, b := range net.Bridges {
			if !b.Fragile {
				continue
			}
			conn := ""
			if len(b.ConnectsClusters) >= 2 {
				conn = fmt.Sprintf("连接 %s 与 %s", b.ConnectsClusters[0], b.ConnectsClusters[1])
			} else if len(b.ConnectsClusters) == 1 {
				conn = "连着你的" + b.ConnectsClusters[0]
			}
			acts = append(acts, BriefingAction{
				Title: "先联系 " + b.Name, Who: b.Name, Source: "network", Priority: 1,
				Detail: fmt.Sprintf("他是你网络的%s，且正处在高风险断联——一句话就能稳住", conn),
			})
			break
		}
	}

	// —— 来自人生状态：风险最高的资产 ——
	if state != nil && len(state.HighRisk) > 0 {
		a := state.HighRisk[0]
		acts = append(acts, BriefingAction{
			Title: "给 " + a.Name + " 发条消息", Who: a.Name, Source: "life", Priority: 2,
			Detail: fmt.Sprintf("%s 风险分 %.0f%%（%s），已经 %.0f 天没互动", riskLevelCn(a.RiskLevel), a.RiskScore*100, a.Category, float64(a.DecayDays)),
		})
	}

	// —— 来自推演：最快将断的一段 ——
	if proj != nil {
		for _, p := range proj.Projections {
			if !p.WillBreak || !p.SufficientData {
				continue
			}
			acts = append(acts, BriefingAction{
				Title: "别让 " + p.Name + " 淡出", Who: p.Name, Source: "projection", Priority: 3,
				Detail: fmt.Sprintf("照当前趋势约 %d 天后亲密度只剩 %d，本周花一分钟问候就能保住", proj.HorizonDays, p.FutureBalance),
			})
			break
		}
	}

	// —— 来自镜像层：单向关系 / 精力迁移警示 ——
	if self != nil {
		if self.OneWayCount > 0 {
			acts = append(acts, BriefingAction{
				Title: "看看那几段单向关系", Source: "self", Priority: 5,
				Detail: fmt.Sprintf("有 %d 段几乎只有你在主动——值得想想是放手，还是找机会深谈一次", self.OneWayCount),
			})
		} else if len(self.Migration) > 0 {
			acts = append(acts, BriefingAction{
				Title: "留意精力的去向", Source: "self", Priority: 6,
				Detail: self.Migration[0],
			})
		}
	}

	// —— 来自因果层：已被验证有效的动作 ——
	if learn != nil && len(learn.BestActions) > 0 {
		acts = append(acts, BriefingAction{
			Title: "多做被验证有效的动作", Source: "intervention", Priority: 4,
			Detail: learn.BestActions[0],
		})
	}

	sort.SliceStable(acts, func(i, j int) bool { return acts[i].Priority < acts[j].Priority })
	if len(acts) > briefingMaxActions {
		acts = acts[:briefingMaxActions]
	}
	br.TopActions = acts

	// —— 全局健康度一行 ——
	br.HealthLine = healthLine(state)

	// —— 最强信号（各层各取最刺眼的一条） ——
	br.Signals = briefingSignals(state, proj, net, self, learn)

	// —— 备注：数据量 / 降级说明 ——
	br.Notes = briefingNotes(net, self, learn)

	// —— 大标题 ——
	br.Headline = briefingHeadline(br, state)

	return br
}

func healthLine(state *LifeState) string {
	if state == nil || state.Portfolio.ContactCount == 0 {
		return "还没有可汇总的人际资产数据，等画像与互动攒够就会自动成形。"
	}
	p := state.Portfolio
	quarter := "基本持平"
	if p.QuarterDelta > 0.5 {
		quarter = fmt.Sprintf("本季在升温（+%.0f）", p.QuarterDelta)
	} else if p.QuarterDelta < -0.5 {
		quarter = fmt.Sprintf("本季在降温（%.0f）", p.QuarterDelta)
	}
	return fmt.Sprintf("人际总财富约 %.0f，%s；覆盖 %d 段关系，其中 %d 段处于高风险。",
		p.TotalWealth, quarter, p.ContactCount, p.HighRiskCount)
}

func briefingSignals(state *LifeState, proj *LifeProjection, net *NetworkInsight,
	self *SelfPortrait, learn *InterventionInsight) []string {
	out := []string{}
	if proj != nil && proj.BreakCount > 0 {
		out = append(out, fmt.Sprintf("什么都不做的话，约 %d 天内会有 %d 段关系自然断掉", proj.HorizonDays, proj.BreakCount))
	}
	if net != nil && net.FragilityScore >= 0.4 && net.FragilityNote != "" {
		out = append(out, net.FragilityNote)
	}
	if state != nil && len(state.Time.Insights) > 0 {
		out = append(out, state.Time.Insights[0])
	}
	if self != nil && self.InitiationRate >= selfProactiveThreshold && self.OneWayCount > 0 {
		out = append(out, fmt.Sprintf("你近 30 天 %.0f%% 都是先开口的那个，主动得很辛苦", self.InitiationRate))
	}
	if learn != nil && learn.TotalResolved >= interventionMinSample {
		out = append(out, fmt.Sprintf("从你自己的历史看，维护动作整体回暖率约 %.0f%%", learn.BaseRate))
	}
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}

func briefingNotes(net *NetworkInsight, self *SelfPortrait, learn *InterventionInsight) []string {
	out := []string{}
	if learn != nil && learn.DataNote != "" {
		out = append(out, learn.DataNote)
	}
	if net != nil && net.NodeCount == 0 {
		out = append(out, "社交网络还没成形：等画像里出现共同城市/兴趣/职业，关系会自动连成网。")
	}
	return out
}

func briefingHeadline(br *Briefing, state *LifeState) string {
	if len(br.TopActions) == 0 {
		if state == nil || state.Portfolio.ContactCount == 0 {
			return "本周没有需要特别做的事——一切都在自动观察中"
		}
		return "本周关系整体平稳，没有紧急事项"
	}
	return fmt.Sprintf("本周最值得你花时间的 %d 件事", len(br.TopActions))
}

func riskLevelCn(level string) string {
	switch level {
	case "high":
		return "高风险"
	case "mid":
		return "中风险"
	case "low":
		return "低风险"
	default:
		return level
	}
}

// ---------- 各层只读缓存（不重算，出错即 nil） ----------

func getCachedNoRecomputeState(db *sql.DB) *LifeState {
	st, _, err := GetCachedLifeState(db)
	if err != nil {
		return nil
	}
	return st
}

func getCachedNoRecomputeProjection(db *sql.DB) *LifeProjection {
	pj, _, err := GetCachedLifeProjection(db)
	if err != nil {
		return nil
	}
	return pj
}

func getCachedNoRecomputeNetwork(db *sql.DB) *NetworkInsight {
	v, _, err := GetCachedNetwork(db)
	if err != nil {
		return nil
	}
	return v
}

func getCachedNoRecomputeSelf(db *sql.DB) *SelfPortrait {
	v, _, err := GetCachedSelfPortrait(db)
	if err != nil {
		return nil
	}
	return v
}

func getCachedNoRecomputeIntervention(db *sql.DB) *InterventionInsight {
	v, _, err := GetCachedIntervention(db)
	if err != nil {
		return nil
	}
	return v
}

// ---------- 缓存读写（镜像 life_state.go） ----------

func saveBriefingCache(db *sql.DB, now time.Time, br *Briefing) error {
	data, err := json.Marshal(br)
	if err != nil {
		return fmt.Errorf("序列化人生简报失败: %w", err)
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err = db.Exec(
		`INSERT INTO briefing_cache (id, generated_at, brief_json) VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET generated_at=excluded.generated_at, brief_json=excluded.brief_json`,
		now.Format(time.RFC3339), string(data))
	return err
}

// GetCachedBriefing 读取缓存；无行返回 nil,nil。
func GetCachedBriefing(db *sql.DB) (*Briefing, time.Time, error) {
	dbMu.Lock()
	var genAt, raw string
	err := db.QueryRow(`SELECT generated_at, brief_json FROM briefing_cache WHERE id = 1`).Scan(&genAt, &raw)
	dbMu.Unlock()
	if err == sql.ErrNoRows {
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	var br Briefing
	if err := json.Unmarshal([]byte(raw), &br); err != nil {
		return nil, time.Time{}, fmt.Errorf("人生简报缓存解析失败: %w", err)
	}
	t, _ := time.Parse(time.RFC3339, genAt)
	return &br, t, nil
}

// IsBriefingStale 缓存是否超过 7 天。
func IsBriefingStale(generatedAt time.Time, now time.Time) bool {
	if generatedAt.IsZero() {
		return true
	}
	return now.Sub(generatedAt) > 7*24*time.Hour
}
