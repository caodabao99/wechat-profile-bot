package main

// 高阶洞察 · Phase C：证据化干预学习（因果层）。
//
// 设计原则：零操作、纯确定性、不调模型；且**样本稀疏时诚实降级**，绝不假装精确。
//   - 数据源：suggestion_outcomes（建议被执行 + 14 天后回测的 improved/stable/worsened）
//     JOIN relationship_action_suggestions（取干预类型 kind）+ 人生状态缓存（取联系人分段）
//   - 学的是"对哪类人做哪种干预真的让互动回暖"：分组经验成功率 + Wilson 95% 置信区间
//   - 已回测样本 n < 门槛时标记"样本不足"，只报基线率、不给高置信排序，避免误导
//   - 缓存 intervention_cache，沿用单行派生缓存模式
//
// 单连接池铁律：先取人生状态缓存（外层自取锁），再单独取锁把 outcomes 一次读尽 Close，
//   分组统计全在 Go 层做，最后 saveInterventionCache 再取锁写。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"
)

// InterventionLearning 一个 (干预类型 × 人群分段) 的经验效果。
type InterventionLearning struct {
	Kind        string  `json:"kind"`
	KindLabel   string  `json:"kindLabel"`
	Segment     string  `json:"segment"` // 人群分段（按类别）；"全部" 表示不细分
	N           int     `json:"n"`       // 已回测样本数
	Improved    int     `json:"improved"`
	Stable      int     `json:"stable"`
	Worsened    int     `json:"worsened"`
	SuccessRate float64 `json:"successRate"` // improved/N ×100
	WilsonLow   float64 `json:"wilsonLow"`   // 95% 置信下界 ×100
	WilsonHigh  float64 `json:"wilsonHigh"`
	LowSample   bool    `json:"lowSample"` // N < 门槛：置信区间不可靠，仅参考
	Verdict     string  `json:"verdict"`
}

// InterventionInsight 一次完整的干预学习快照。
type InterventionInsight struct {
	GeneratedAt   string                 `json:"generatedAt"`
	TotalResolved int                    `json:"totalResolved"` // 全局已回测总数
	BaseRate      float64                `json:"baseRate"`      // 全局回暖率 ×100
	ByKind        []InterventionLearning `json:"byKind"`        // 按干预类型（全部人群）
	ByCell        []InterventionLearning `json:"byCell"`        // 干预类型 × 类别细分
	BestActions   []string               `json:"bestActions"`   // 一句话 top 结论
	DataNote      string                 `json:"dataNote"`      // 数据量诚实说明
	Insights      []string               `json:"insights"`
}

const (
	interventionMinSample = 10   // 低于此已回测样本数 → 诚实标记"样本不足"
	interventionWilsonZ   = 1.96 // 95% 置信
)

// ComputeInterventionLearning 重算干预学习并写入缓存。
func ComputeInterventionLearning(db *sql.DB, now time.Time) error {
	ins, err := buildIntervention(db, now)
	if err != nil {
		return err
	}
	return saveInterventionCache(db, now, ins)
}

type outcomeRow struct {
	kind    string
	contact int64
	outcome string
}

func buildIntervention(db *sql.DB, now time.Time) (*InterventionInsight, error) {
	// 1) 先在外层取人生状态（自取锁），拿联系人 → 类别 的分段配色。
	segOf := map[int64]string{}
	if st, _, err := GetCachedLifeState(db); err == nil && st != nil {
		for _, a := range st.Assets {
			cat := a.Category
			if cat == "" {
				cat = "其他"
			}
			segOf[a.ContactID] = cat
		}
	}

	// 2) 单独取锁把已回测 outcomes + 建议 kind 一次读尽 Close。
	var rows []outcomeRow
	dbMu.Lock()
	qrows, err := db.Query(`
		SELECT s.kind, o.contact_id, o.outcome
		FROM suggestion_outcomes o
		JOIN relationship_action_suggestions s ON s.id = o.suggestion_id
		WHERE o.outcome IN ('improved','stable','worsened')`)
	if err != nil {
		dbMu.Unlock()
		return nil, fmt.Errorf("读取回测结果失败: %w", err)
	}
	for qrows.Next() {
		var r outcomeRow
		if qrows.Scan(&r.kind, &r.contact, &r.outcome) == nil {
			rows = append(rows, r)
		}
	}
	qrows.Close()
	dbMu.Unlock()

	ins := &InterventionInsight{
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
		ByKind:      []InterventionLearning{},
		ByCell:      []InterventionLearning{},
		BestActions: []string{},
		Insights:    []string{},
	}

	// 3) 分组累加：kind×"全部" 与 kind×category。
	type key struct{ kind, seg string }
	agg := map[key]*InterventionLearning{}
	touch := func(kind, seg string) *InterventionLearning {
		k := key{kind, seg}
		if a, ok := agg[k]; ok {
			return a
		}
		a := &InterventionLearning{Kind: kind, KindLabel: kindLabel(kind), Segment: seg}
		agg[k] = a
		return a
	}
	totalImp, totalN := 0, 0
	for _, r := range rows {
		a := touch(r.kind, "全部")
		seg := segOf[r.contact]
		if seg == "" {
			seg = "其他"
		}
		b := touch(r.kind, seg)
		for _, x := range []*InterventionLearning{a, b} {
			x.N++
			switch r.outcome {
			case "improved":
				x.Improved++
			case "stable":
				x.Stable++
			case "worsened":
				x.Worsened++
			}
		}
		if r.outcome == "improved" {
			totalImp++
		}
		totalN++
	}

	// 4) 计算成功率 + Wilson 区间 + 样本不足标记。
	for _, a := range agg {
		finalizeLearning(a)
	}
	for _, a := range agg {
		if a.Segment == "全部" {
			ins.ByKind = append(ins.ByKind, *a)
		} else {
			ins.ByCell = append(ins.ByCell, *a)
		}
	}
	ins.TotalResolved = totalN
	if totalN > 0 {
		ins.BaseRate = roundF(float64(totalImp)/float64(totalN)*100, 1)
	}
	sortLearnings(ins.ByKind)
	sortLearnings(ins.ByCell)

	// 5) 诚实说明 + top 结论 + 洞察。
	ins.DataNote = dataNote(totalN)
	ins.BestActions = bestActions(ins.ByKind)
	ins.Insights = interventionInsights(ins)
	return ins, nil
}

// finalizeLearning 就地算出成功率、Wilson 区间与样本不足判定。
func finalizeLearning(a *InterventionLearning) {
	if a.N == 0 {
		return
	}
	p := float64(a.Improved) / float64(a.N)
	a.SuccessRate = roundF(p*100, 1)
	a.LowSample = a.N < interventionMinSample
	lo, hi := wilsonInterval(p, a.N, interventionWilsonZ)
	a.WilsonLow = roundF(lo*100, 1)
	a.WilsonHigh = roundF(hi*100, 1)
	a.Verdict = verdict(a)
}

// wilsonInterval 二项比例 Wilson score 区间（z≈1.96 → 95%）。
func wilsonInterval(p float64, n int, z float64) (float64, float64) {
	if n == 0 {
		return 0, 0
	}
	nn := float64(n)
	denom := 1 + z*z/nn
	center := (p + z*z/(2*nn)) / denom
	margin := z * math.Sqrt(p*(1-p)/nn+z*z/(4*nn*nn)) / denom
	lo, hi := center-margin, center+margin
	if lo < 0 {
		lo = 0
	}
	if hi > 1 {
		hi = 1
	}
	return lo, hi
}

func verdict(a *InterventionLearning) string {
	if a.LowSample {
		return fmt.Sprintf("样本偏少（%d 例），回暖率 %.0f%% 仅供参考，暂不做置信度校准", a.N, a.SuccessRate)
	}
	switch {
	case a.WilsonLow >= 50:
		return fmt.Sprintf("对%s用「%s」稳定有效：回暖率 %.0f%%（置信下界 %.0f%%）",
			a.Segment, a.KindLabel, a.SuccessRate, a.WilsonLow)
	case a.SuccessRate <= 25:
		return fmt.Sprintf("对%s用「%s」少见成效（回暖率 %.0f%%），可换个思路",
			a.Segment, a.KindLabel, a.SuccessRate)
	default:
		return fmt.Sprintf("对%s用「%s」回暖率 %.0f%%（%d～%d）",
			a.Segment, a.KindLabel, a.SuccessRate, int(a.WilsonLow), int(a.WilsonHigh))
	}
}

func sortLearnings(list []InterventionLearning) {
	// 排序：置信下界高者在前；样本不足的整体靠后；tie-break 稳定
	sort.Slice(list, func(i, j int) bool {
		if list[i].LowSample != list[j].LowSample {
			return !list[i].LowSample
		}
		if list[i].WilsonLow != list[j].WilsonLow {
			return list[i].WilsonLow > list[j].WilsonLow
		}
		if list[i].N != list[j].N {
			return list[i].N > list[j].N
		}
		if list[i].Kind != list[j].Kind {
			return list[i].Kind < list[j].Kind
		}
		return list[i].Segment < list[j].Segment
	})
}

func dataNote(total int) string {
	switch {
	case total == 0:
		return "还没有积累到可学习的样本：当你在周计划里联系某人、系统 14 天后回测互动变化，数据会自动攒进来。"
	case total < interventionMinSample:
		return fmt.Sprintf("目前累计 %d 条已回测样本，还不够下结论（建议攒够 %d 条再看置信度），以下仅作方向参考。",
			total, interventionMinSample)
	default:
		return fmt.Sprintf("已基于 %d 条「执行→14 天后回测」的真实样本学习，结论带 95%% 置信区间。", total)
	}
}

// bestActions 只从样本充足的"全部人群"分组里取高置信的有效动作。
func bestActions(byKind []InterventionLearning) []string {
	out := []string{}
	for _, a := range byKind {
		if a.LowSample || a.WilsonLow < 40 {
			continue
		}
		out = append(out, fmt.Sprintf("「%s」对整类人回暖率 %.0f%%（置信下界 %.0f%%），值得多做",
			a.KindLabel, a.SuccessRate, a.WilsonLow))
		if len(out) >= 3 {
			break
		}
	}
	return out
}

func interventionInsights(ins *InterventionInsight) []string {
	out := []string{}
	if ins.TotalResolved == 0 {
		return out
	}
	out = append(out, fmt.Sprintf("你被回测过的维护动作整体回暖率约 %.0f%%", ins.BaseRate))
	for _, a := range ins.BestActions {
		out = append(out, a)
	}
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}

func kindLabel(kind string) string {
	switch kind {
	case "cooling":
		return "主动破冰问候"
	case "silence":
		return "久未联系后重启话题"
	case "no_reply":
		return "回复对方未接的消息"
	case "birthday":
		return "重要日子送祝福"
	case "followup":
		return "兑现待跟进承诺"
	default:
		return kind
	}
}

// ---------- 缓存读写（镜像 life_state.go） ----------

func saveInterventionCache(db *sql.DB, now time.Time, ins *InterventionInsight) error {
	data, err := json.Marshal(ins)
	if err != nil {
		return fmt.Errorf("序列化干预学习失败: %w", err)
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err = db.Exec(
		`INSERT INTO intervention_cache (id, generated_at, learn_json) VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET generated_at=excluded.generated_at, learn_json=excluded.learn_json`,
		now.Format(time.RFC3339), string(data))
	return err
}

// GetCachedIntervention 读取缓存；无行返回 nil,nil。
func GetCachedIntervention(db *sql.DB) (*InterventionInsight, time.Time, error) {
	dbMu.Lock()
	var genAt, raw string
	err := db.QueryRow(`SELECT generated_at, learn_json FROM intervention_cache WHERE id = 1`).Scan(&genAt, &raw)
	dbMu.Unlock()
	if err == sql.ErrNoRows {
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	var ins InterventionInsight
	if err := json.Unmarshal([]byte(raw), &ins); err != nil {
		return nil, time.Time{}, fmt.Errorf("干预学习缓存解析失败: %w", err)
	}
	t, _ := time.Parse(time.RFC3339, genAt)
	return &ins, t, nil
}

// IsInterventionStale 缓存是否超过 7 天。
func IsInterventionStale(generatedAt time.Time, now time.Time) bool {
	if generatedAt.IsZero() {
		return true
	}
	return now.Sub(generatedAt) > 7*24*time.Hour
}
