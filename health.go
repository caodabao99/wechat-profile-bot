package main

// v5.3.0 #7：关系健康度综合仪表盘（零 LLM、纯确定性、可离线单测）。
//
// 给每个联系人融合出一个 0-100 的「关系健康分」——注意与 quality.go 的「对话质量」是
//   不同口径：quality 看单场会话四维（回复及时性/深度/多样性/正向度），health 看整段
//   关系的「活力」：亲密度基座 + 趋势(升温/降温/沉寂) + 情绪 + 沉默衰减 + 单向惩罚。
//
// 设计铁律：
//   - 增量复用：亲密度直接取 computeIntimacy(90)；趋势口径复用 classifyTrendWith
//     （与 relationship.go 的 GetRelationshipTrend 完全一致）；情绪取 assistant_emotions 近 30 天均分。
//     绝不重写这些既有模块。
//   - 单连接池分层锁：computeIntimacy / loadAssistantSettings 各自取锁、先在其外调用；
//     本函数的批量取数（自愈重建 + 趋势聚合 + 情绪聚合 + 联系人全集）全在「一趟锁」内顺序
//     读完再释放，绝不嵌套、绝不边迭代边写。
//   - 计算即读、不落库、不新增表（批量 SQL 足够快、结果天然新鲜）。
//   - 联系人规模护栏 healthMaxContacts，超出截断并标注 truncated。

import (
	"database/sql"
	"math"
	"sort"
	"time"
)

const (
	healthMaxContacts = 300 // 全局健康仪表盘一次最多覆盖的联系人数
	healthWindowDays  = 90  // 亲密度/趋势窗口（与 tagsuggest、computeIntimacy 口径一致）
	healthEmotionDays = 30  // 情绪均分窗口

	// v5.4.0 #1 断点预警：前瞻「正在降温」判定的沉寂阈值（连续无互动达此天数即视为趋于断联）。
	healthDormantHorizon = 30
)

// HealthSignal 一条对健康分的贡献说明（label + 实际作用的加减分）。
type HealthSignal struct {
	Label string `json:"label"`
	Delta int    `json:"delta"`
}

// HealthItem 单个联系人的健康分。
type HealthItem struct {
	ContactID  int64          `json:"contactId"`
	Name       string         `json:"name"`
	Health     int            `json:"health"`
	Band       string         `json:"band"`
	Intimacy   int            `json:"intimacy"`
	TrendState string         `json:"trendState"`
	Signals    []HealthSignal `json:"signals"`
	Alert      string         `json:"alert"`   // v5.4.0 #1：none/watching/urgent（前瞻断点风险）
	EtaDays    int            `json:"etaDays"` // v5.4.0 #1：预计距进入沉寂的剩余天数（越小越紧急）
}

// HealthBand 分档直方的一档。
type HealthBand struct {
	Band  string `json:"band"`
	Count int    `json:"count"`
}

// HealthSummary 全局聚合。
type HealthSummary struct {
	Avg        int          `json:"avg"`
	Total      int          `json:"total"`
	Truncated  bool         `json:"truncated"`
	AlertCount int          `json:"alertCount"` // v5.4.0 #1：watching+urgent 的关系数
	Bands      []HealthBand `json:"bands"`      // 固定顺序：优秀/良好/一般/需关注/危险
}

// HealthDashboard 仪表盘响应体。
type HealthDashboard struct {
	WindowDays  int           `json:"windowDays"`
	GeneratedAt string        `json:"generatedAt"`
	Summary     HealthSummary `json:"summary"`
	Items       []HealthItem  `json:"items"`  // 按 health 升序（最需关注的在前）
	Alerts      []HealthItem  `json:"alerts"` // v5.4.0 #1：正在降温的关系子集（alert!=none），按紧急度排序
}

// healthBandOrder 分档固定顺序（供直方与前端配色）。
var healthBandOrder = []string{"优秀", "良好", "一般", "需关注", "危险"}

// bandOfHealth 健康分 → 分档名。
func bandOfHealth(h int) string {
	switch {
	case h >= 80:
		return "优秀"
	case h >= 60:
		return "良好"
	case h >= 40:
		return "一般"
	case h >= 20:
		return "需关注"
	default:
		return "危险"
	}
}

// fuseHealth 纯函数：把各因子融合成 0-100 健康分，并返回逐项贡献说明（可脱库单测、确定性）。
//   - 亲密度基座：intimacy(0-100)
//   - 趋势：warming +8 / cooling -12 / dormant -20 / 其余 0
//   - 情绪：emoOK 时按 (emoAvg-50)/50*10 映射 [-10,+10]
//   - 沉默：-min(20, daysSinceLast/7*2)
//   - 单向：meRatio>0.85 追加 -5
func fuseHealth(intimacy int, trendState string, emoAvg float64, emoOK bool, daysSinceLast int, meRatio float64) (int, []HealthSignal) {
	sig := []HealthSignal{{"亲密度", intimacy}}
	score := float64(intimacy)

	td := 0
	switch trendState {
	case "warming":
		td = 8
	case "cooling":
		td = -12
	case "dormant":
		td = -20
	}
	if td != 0 {
		score += float64(td)
		sig = append(sig, HealthSignal{"趋势", td})
	}

	if emoOK {
		ef := int(math.Round((emoAvg - 50) / 50 * 10))
		if ef != 0 {
			score += float64(ef)
			sig = append(sig, HealthSignal{"情绪", ef})
		}
	}

	if daysSinceLast > 0 {
		sd := daysSinceLast / 7 * 2
		if sd > 20 {
			sd = 20
		}
		if sd > 0 {
			score -= float64(sd)
			sig = append(sig, HealthSignal{"沉默", -sd})
		}
	}

	if meRatio > 0.85 {
		score -= 5
		sig = append(sig, HealthSignal{"单向", -5})
	}

	h := int(score + 0.5)
	if h < 0 {
		h = 0
	}
	if h > 100 {
		h = 100
	}
	return h, sig
}

// projectCooling 纯函数：由近30天/前30天互动量与已沉默天数，前瞻预测「正在降温」的程度与
// 距进入沉寂的剩余天数（确定性、可脱库单测）。
//   - df = recent30/prior30（prior30>0）；前期无近期有视为 1（升温）；两头皆无为 0（已沉寂）。
//   - eta  = healthDormantHorizon - daysSinceLast（夹到 ≥0），即按沉默增长折算的「距断点」天数。
//   - df>=0.9（持平/升温）→ none；两头皆无→urgent；df<0.5 或 eta<=15→urgent；其余冷却→watching。
func projectCooling(recent30, prior30, daysSinceLast int) (string, int) {
	df := 1.0
	switch {
	case prior30 > 0:
		df = float64(recent30) / float64(prior30)
	case recent30 > 0:
		df = 1.0
	default:
		df = 0.0
	}
	eta := healthDormantHorizon - daysSinceLast
	if eta < 0 {
		eta = 0
	}
	if df >= 0.9 {
		return "none", eta
	}
	if recent30 == 0 && prior30 == 0 {
		return "urgent", 0
	}
	if df < 0.5 || eta <= 15 {
		return "urgent", eta
	}
	return "watching", eta
}

// alertRank 预警分级排序权重（urgent 靠前）。
func alertRank(level string) int {
	switch level {
	case "urgent":
		return 0
	case "watching":
		return 1
	default:
		return 2
	}
}

// ComputeHealth 计算全局关系健康度仪表盘。
func ComputeHealth(db *sql.DB, now time.Time, windowDays int) (*HealthDashboard, error) {
	if windowDays <= 0 {
		windowDays = healthWindowDays
	}
	thr, _ := loadAssistantSettings(db) // 趋势阈值：必须在取锁前读（内部自锁）

	// 亲密度批量：computeIntimacy 内部已按分层锁取 dbMu，先在其外算好做成查找表。
	intimacyMap := map[int64]int{}
	if list, err := computeIntimacy(db, now, windowDays); err == nil {
		for _, it := range list {
			intimacyMap[it.ContactID] = it.Score
		}
	}

	// —— 一趟锁：自愈重建 + 趋势聚合 + 情绪聚合 + 联系人全集，顺序读完再释放 ——
	type trendAgg struct {
		recent30, prior30, recentMe int
		lastDay                     string
	}
	trendMap := map[int64]trendAgg{}
	emoMap := map[int64]float64{}
	type contactRow struct {
		id     int64
		name   string
		remark string
	}
	var universe []contactRow
	truncated := false

	today := now.Format("2006-01-02")
	recentFrom := now.AddDate(0, 0, -29).Format("2006-01-02")
	priorFrom := now.AddDate(0, 0, -59).Format("2006-01-02")
	priorTo := now.AddDate(0, 0, -30).Format("2006-01-02")
	emoSince := now.AddDate(0, 0, -healthEmotionDays).Format("2006-01-02 15:04:05")

	dbMu.Lock()
	// 自愈：指标表为空但有消息（活跃或归档）时全量重建一次（与 GetRelationshipTrend 同语义，但批量）。
	var mc int
	if db.QueryRow(`SELECT COUNT(*) FROM relationship_daily_metrics`).Scan(&mc) == nil && mc == 0 {
		if hasMessagesForRebuildLocked(db, 0) {
			_, _ = rebuildDailyMetricsLocked(db, 0)
		}
	}

	// 趋势聚合（一次 GROUP BY 读完，取尽即 Close）。
	if rows, err := db.Query(
		`SELECT contact_id,
		        SUM(CASE WHEN day>=? AND day<=? THEN me_count+other_count ELSE 0 END),
		        SUM(CASE WHEN day>=? AND day<=? THEN me_count ELSE 0 END),
		        SUM(CASE WHEN day>=? AND day<=? THEN me_count+other_count ELSE 0 END),
		        MAX(day)
		 FROM relationship_daily_metrics GROUP BY contact_id`,
		recentFrom, today, recentFrom, today, priorFrom, priorTo); err == nil {
		for rows.Next() {
			var cid int64
			var ta trendAgg
			var last sql.NullString
			if rows.Scan(&cid, &ta.recent30, &ta.recentMe, &ta.prior30, &last) == nil {
				ta.lastDay = last.String
				trendMap[cid] = ta
			}
		}
		rows.Close()
	}

	// 情绪聚合（增值表，缺失静默跳过）。
	if tableExistsLocked(db, "assistant_emotions") {
		if rows, err := db.Query(
			`SELECT contact_id, COALESCE(SUM(score),0), COUNT(*) FROM assistant_emotions
			 WHERE created_at IS NOT NULL AND created_at != ''
			   AND strftime('%s', created_at, 'localtime') >= strftime('%s', ?)
			 GROUP BY contact_id`, emoSince); err == nil {
			for rows.Next() {
				var cid int64
				var sum, cnt int64
				if rows.Scan(&cid, &sum, &cnt) == nil && cnt > 0 {
					emoMap[cid] = float64(sum) / float64(cnt)
				}
			}
			rows.Close()
		}
	}

	// 联系人全集（未合并），规模护栏截断。
	if rows, err := db.Query(
		`SELECT id, COALESCE(name,''), COALESCE(remark,'') FROM contacts
		 WHERE merged_into IS NULL ORDER BY id ASC LIMIT ?`, healthMaxContacts+1); err == nil {
		for rows.Next() {
			var c contactRow
			if rows.Scan(&c.id, &c.name, &c.remark) == nil {
				universe = append(universe, c)
			}
		}
		rows.Close()
	}
	dbMu.Unlock()

	if len(universe) > healthMaxContacts {
		universe = universe[:healthMaxContacts]
		truncated = true
	}

	// —— 逐个装配健康分（纯内存、确定性）——
	items := make([]HealthItem, 0, len(universe))
	bandCounts := map[string]int{}
	sum := 0
	for _, c := range universe {
		intim := intimacyMap[c.id]

		ta := trendMap[c.id]
		t := &RelationshipTrend{ContactID: c.id, Recent30: ta.recent30, Prior30: ta.prior30}
		if ta.recent30 > 0 {
			t.MeRatioRecent = float64(ta.recentMe) / float64(ta.recent30)
		}
		if ta.lastDay != "" {
			if lt, err := time.ParseInLocation("2006-01-02", ta.lastDay, time.Local); err == nil {
				t.DaysSinceLast = int(now.Sub(lt).Hours() / 24)
			}
		}
		state, _ := classifyTrendWith(t, thr)

		emoAvg, emoOK := emoMap[c.id]

		h, sig := fuseHealth(intim, state, emoAvg, emoOK, t.DaysSinceLast, t.MeRatioRecent)
		band := bandOfHealth(h)
		bandCounts[band]++
		sum += h
		alert, eta := projectCooling(ta.recent30, ta.prior30, t.DaysSinceLast)

		label := displayName(&Contact{ID: c.id, Name: c.name, Remark: c.remark})
		items = append(items, HealthItem{
			ContactID: c.id, Name: label, Health: h, Band: band,
			Intimacy: intim, TrendState: state, Signals: sig,
			Alert: alert, EtaDays: eta,
		})
	}

	// 升序（最需关注在前），同分按 contactId 升序保确定性。
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Health != items[j].Health {
			return items[i].Health < items[j].Health
		}
		return items[i].ContactID < items[j].ContactID
	})

	bands := make([]HealthBand, 0, len(healthBandOrder))
	for _, b := range healthBandOrder {
		bands = append(bands, HealthBand{Band: b, Count: bandCounts[b]})
	}

	avg := 0
	if len(items) > 0 {
		avg = int(float64(sum)/float64(len(items)) + 0.5)
	}

	// 断点预警子集：取 alert!=none 的关系，按紧急度（urgent 前）→ etaDays 升序 → contactId 升序。
	// 与 items 共享同一份条目值拷贝，互不干扰（不改 items 自身升序语义）。
	alerts := make([]HealthItem, 0)
	for _, it := range items {
		if it.Alert != "none" {
			alerts = append(alerts, it)
		}
	}
	sort.SliceStable(alerts, func(i, j int) bool {
		if ri, rj := alertRank(alerts[i].Alert), alertRank(alerts[j].Alert); ri != rj {
			return ri < rj
		}
		if alerts[i].EtaDays != alerts[j].EtaDays {
			return alerts[i].EtaDays < alerts[j].EtaDays
		}
		return alerts[i].ContactID < alerts[j].ContactID
	})

	return &HealthDashboard{
		WindowDays:  windowDays,
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
		Summary:     HealthSummary{Avg: avg, Total: len(items), Truncated: truncated, AlertCount: len(alerts), Bands: bands},
		Items:       items,
		Alerts:      alerts,
	}, nil
}
