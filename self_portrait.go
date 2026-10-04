package main

// 高阶洞察 · Phase B：自我关系画像（镜像层）。
//
// 设计原则：零操作、纯确定性、不调模型。工具从"看别人"升级成"照见你自己"——
//   跨全体联系人反照出你的社交模式：谁在主动先开口、精力往哪些类别迁移、
//   有多少关系几乎只有你在一厢情愿、以及你的整体社交风格。
//
// 数据复用 life_state.go 的 gatherLifeRaw（一次读尽进内存），本层只做纯计算，
//   零新增采数。缓存 self_portrait_cache，沿用单行派生缓存模式。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// StyleTag 一条社交风格标签（规则判定，附触发依据）。
type StyleTag struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Reason string `json:"reason"`
}

// SelfPortrait 一次完整的自我关系画像快照。
type SelfPortrait struct {
	GeneratedAt        string             `json:"generatedAt"`
	InitiationRate     float64            `json:"initiationRate"`     // 全局：近 30 天你先开口的比例 0-100
	CategoryInitiation map[string]float64 `json:"categoryInitiation"` // 各类别你先开口比例 0-100
	EnergyShares       map[string]float64 `json:"energyShares"`       // 近 30 天精力(消息量)按类别占比 0-100
	Migration          []string           `json:"migration"`          // 精力迁移洞察（近期 vs 生命周期）
	OneWayCount        int                `json:"oneWayCount"`        // 几乎只有你在主动的关系段数
	ImbalanceAvg       float64            `json:"imbalanceAvg"`       // 平均投入失衡度 0-100
	ActiveContacts     int                `json:"activeContacts"`     // 近 30 天有往来的联系人数
	StyleTags          []StyleTag         `json:"styleTags"`
	Insights           []string           `json:"insights"`
}

const (
	selfProactiveThreshold = 65 // 你先开口率 >= 此值 → 主动型
	selfPassiveThreshold   = 40 // <= 此值 → 被动型
	selfOneWayImbalance    = 80 // 失衡度 >= 此值(且你发得多) → 记为"单向"
	selfDeepHHI            = 0.25
)

// ComputeSelfPortrait 重算自我关系画像并写入缓存。
func ComputeSelfPortrait(db *sql.DB, now time.Time) error {
	raw, err := gatherLifeRaw(db, now)
	if err != nil {
		return err
	}
	sp := buildSelfPortrait(now, raw)
	return saveSelfPortraitCache(db, now, sp)
}

// buildSelfPortrait 纯计算：从 lifeRaw 反照用户社交模式。不碰 DB，供单测直接调用。
func buildSelfPortrait(now time.Time, raw *lifeRaw) *SelfPortrait {
	sp := &SelfPortrait{
		GeneratedAt:        now.Format("2006-01-02 15:04:05"),
		CategoryInitiation: map[string]float64{},
		EnergyShares:       map[string]float64{},
		Migration:          []string{},
		StyleTags:          []StyleTag{},
		Insights:           []string{},
	}

	totalMine, totalOther := 0, 0
	catMine, catOther := map[string]int{}, map[string]int{}
	catRecent := map[string]int{}
	imbalanceSum, imbalanceN := 0.0, 0
	activeSet := map[int64]bool{}

	for cid := range raw.active {
		if raw.lifeCount[cid] == 0 {
			continue // 从没聊过天，不计入
		}
		mine, other := raw.mine30[cid], raw.other30[cid]
		totalMine += mine
		totalOther += other
		cat := raw.category[cid]
		if cat == "" {
			cat = "其他"
		}
		catMine[cat] += mine
		catOther[cat] += other
		if mine+other > 0 {
			catRecent[cat] += mine + other
			activeSet[cid] = true
		}
		// 失衡度：0 均衡 .. 1 完全单向
		imb := 0.0
		if mine+other > 0 {
			imb = absF(float64(mine-other)) / float64(mine+other)
		}
		imbalanceSum += imb
		imbalanceN++
		// 单向：你发得明显多、且失衡严重
		if mine >= 5 && imb*100 >= selfOneWayImbalance && mine > other {
			sp.OneWayCount++
		}
	}

	sp.ActiveContacts = len(activeSet)
	if imbalanceN > 0 {
		sp.ImbalanceAvg = roundF(imbalanceSum/float64(imbalanceN)*100, 1)
	}
	if totalMine+totalOther > 0 {
		sp.InitiationRate = roundF(float64(totalMine)/float64(totalMine+totalOther)*100, 1)
	}
	for cat := range catMine {
		if t := catMine[cat] + catOther[cat]; t > 0 {
			sp.CategoryInitiation[cat] = roundF(float64(catMine[cat])/float64(t)*100, 1)
		}
	}

	// 近期精力按类别占比
	recentTotal := 0
	for _, n := range catRecent {
		recentTotal += n
	}
	for cat, n := range catRecent {
		if recentTotal > 0 {
			sp.EnergyShares[cat] = roundF(float64(n)/float64(recentTotal)*100, 1)
		}
	}

	// 精力迁移：近期占比 vs 生命周期占比（复用 life_state 的漂移口径）
	sp.Migration = migrationLines(catRecent, raw.catLife, recentTotal, sumMap(raw.catLife))

	// 社交风格标签
	sp.StyleTags = styleTags(sp, raw, recentTotal)

	// 洞察句
	sp.Insights = selfInsights(sp)

	return sp
}

// migrationLines 对比近期与生命周期各类别占比，产出"精力往哪迁移"的句子。
func migrationLines(recent, life map[string]int, recentTotal, lifeTotal int) []string {
	out := []string{}
	if recentTotal <= 0 || lifeTotal <= 0 {
		return out
	}
	type d struct {
		cat  string
		diff float64
		r, l float64
	}
	var list []d
	cats := map[string]bool{}
	for c := range recent {
		cats[c] = true
	}
	for c := range life {
		cats[c] = true
	}
	for c := range cats {
		if c == "其他" || c == "" {
			continue
		}
		r := float64(recent[c]) / float64(recentTotal) * 100
		l := float64(life[c]) / float64(lifeTotal) * 100
		list = append(list, d{cat: c, diff: r - l, r: r, l: l})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].diff != list[j].diff {
			return list[i].diff > list[j].diff // 涨得最猛的在前
		}
		return list[i].cat < list[j].cat
	})
	for _, x := range list {
		if x.diff >= 8 {
			out = append(out, fmt.Sprintf("你把更多精力投向了 %s（从 %.0f%% 升到 %.0f%%）", x.cat, x.l, x.r))
		} else if x.diff <= -8 {
			out = append(out, fmt.Sprintf("%s 在你生活里的占比下滑了（从 %.0f%% 到 %.0f%%）", x.cat, x.l, x.r))
		}
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

// styleTags 用确定性规则给用户的社交风格贴标签，每条附依据。
func styleTags(sp *SelfPortrait, raw *lifeRaw, recentTotal int) []StyleTag {
	out := []StyleTag{}

	// 1) 主动 / 被动 / 均衡
	switch {
	case sp.InitiationRate >= selfProactiveThreshold:
		out = append(out, StyleTag{
			Key: "proactive", Label: "主动型",
			Reason: fmt.Sprintf("近 30 天有 %.0f%% 的消息是你先发的", sp.InitiationRate),
		})
	case sp.InitiationRate > 0 && sp.InitiationRate <= selfPassiveThreshold:
		out = append(out, StyleTag{
			Key: "passive", Label: "被动型",
			Reason: fmt.Sprintf("近 30 天只有 %.0f%% 是你先开口，多数互动由对方发起", sp.InitiationRate),
		})
	case sp.InitiationRate > 0:
		out = append(out, StyleTag{
			Key: "balanced", Label: "你来我往型",
			Reason: fmt.Sprintf("你先开口占 %.0f%%，攻守比较均衡", sp.InitiationRate),
		})
	}

	// 2) 深耕 / 广撒网：对你近期发出消息量做集中度 HHI
	if recentTotal > 0 {
		hhi := 0.0
		for cid := range raw.active {
			m := raw.mine30[cid]
			if m <= 0 {
				continue
			}
			s := float64(m) / float64(recentTotal)
			hhi += s * s
		}
		if hhi >= selfDeepHHI {
			out = append(out, StyleTag{
				Key: "deep", Label: "深耕型",
				Reason: fmt.Sprintf("你把绝大部分话都留给了少数几个人（精力集中度 %.0f%%）", roundF(hhi*100, 0)),
			})
		} else if sp.ActiveContacts >= 8 {
			out = append(out, StyleTag{
				Key: "broad", Label: "广撒网型",
				Reason: fmt.Sprintf("近 30 天你和 %d 个人都有往来，覆盖面很广", sp.ActiveContacts),
			})
		}
	}

	// 3) 偏向哪类人：近期精力占比最高的类别
	bestCat, best := "", -1.0
	for c, v := range sp.EnergyShares {
		if c == "其他" {
			continue
		}
		if v > best {
			bestCat, best = c, v
		}
	}
	if bestCat != "" && best >= 40 {
		out = append(out, StyleTag{
			Key: "lean", Label: "偏" + bestCat,
			Reason: fmt.Sprintf("近 30 天你有 %.0f%% 的精力花在了%s身上", best, bestCat),
		})
	}

	return out
}

// selfInsights 组装自我画像的自动洞察句。
func selfInsights(sp *SelfPortrait) []string {
	out := []string{}
	if sp.ActiveContacts == 0 {
		return out
	}
	if sp.OneWayCount > 0 {
		out = append(out, fmt.Sprintf("有 %d 段关系几乎是你在单方面维系——留意这些是不是该放手或该深谈的信号", sp.OneWayCount))
	}
	if sp.InitiationRate >= selfProactiveThreshold && sp.ImbalanceAvg >= 50 {
		out = append(out, "你总是主动的那一个，偶尔也可以等等看谁会向你走来")
	}
	if len(sp.Migration) > 0 {
		out = append(out, sp.Migration[0])
	}
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}

// ---------- 缓存读写（镜像 life_state.go） ----------

func saveSelfPortraitCache(db *sql.DB, now time.Time, sp *SelfPortrait) error {
	data, err := json.Marshal(sp)
	if err != nil {
		return fmt.Errorf("序列化自我画像失败: %w", err)
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err = db.Exec(
		`INSERT INTO self_portrait_cache (id, generated_at, portrait_json) VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET generated_at=excluded.generated_at, portrait_json=excluded.portrait_json`,
		now.Format(time.RFC3339), string(data))
	return err
}

// GetCachedSelfPortrait 读取缓存；无行返回 nil,nil。
func GetCachedSelfPortrait(db *sql.DB) (*SelfPortrait, time.Time, error) {
	dbMu.Lock()
	var genAt, raw string
	err := db.QueryRow(`SELECT generated_at, portrait_json FROM self_portrait_cache WHERE id = 1`).Scan(&genAt, &raw)
	dbMu.Unlock()
	if err == sql.ErrNoRows {
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	var sp SelfPortrait
	if err := json.Unmarshal([]byte(raw), &sp); err != nil {
		return nil, time.Time{}, fmt.Errorf("自我画像缓存解析失败: %w", err)
	}
	t, _ := time.Parse(time.RFC3339, genAt)
	return &sp, t, nil
}

// IsSelfPortraitStale 缓存是否超过 7 天。
func IsSelfPortraitStale(generatedAt time.Time, now time.Time) bool {
	if generatedAt.IsZero() {
		return true
	}
	return now.Sub(generatedAt) > 7*24*time.Hour
}
