package main

// 人生模拟器 · Phase A：人生状态模型。
//
// 设计原则：系统主动做，人只看结果 / 零操作减负。
//   - ComputeLifeState 纯 SQL 聚合现有数据（messages / contacts / tags / 亲密度），
//     算出「关系资产账本 + 时间精力回流 + 你的轨迹」，缓存到 life_state_cache
//   - 不新增采数、不调大模型、不要求用户配置；看板打开即看结果
//   - 复用 weekly_plan.go 的派生缓存模式：id=1 单行 UPSERT、访问自愈、不入备份
//
// 单连接池铁律：所有 gather 查询一次性读进内存并 Close，聚合纯在 Go 层做，
// 最后再单独取锁写缓存——绝不「边迭代 rows 边 Exec」。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// LifeAsset 单个联系人的一行「关系资产」记录。
type LifeAsset struct {
	ContactID  int64   `json:"contactId"`
	Name       string  `json:"name"`
	Balance    int     `json:"balance"`    // 关系深度分 0-100（近 180 天亲密度）
	Cashflow   float64 `json:"cashflow"`   // 近 30 天日均互动 / 历史基线日均（>1 增值）
	DecayDays  int     `json:"decayDays"`  // 距上次联系天数
	WeeklyRate float64 `json:"weeklyRate"` // 近 90 天互动量的每周变化斜率（条/周）
	RiskScore  float64 `json:"riskScore"`  // 0-1 综合风险
	RiskLevel  string  `json:"riskLevel"`  // low / mid / high
	AssetClass string  `json:"assetClass"` // close / friend / acquaintance / weak / transactional
	Category   string  `json:"category"`   // 家人 / 同事 / 朋友 / 其他（由标签推断）
	Freq30     int     `json:"freq30"`     // 近 30 天消息总数
}

// LifePortfolio 全局组合聚合。
type LifePortfolio struct {
	TotalWealth    float64            `json:"totalWealth"`
	QuarterDelta   float64            `json:"quarterDelta"` // ∑(cashflow-1)*权重，正=增值
	HighRiskCount  int                `json:"highRiskCount"`
	ContactCount   int                `json:"contactCount"`
	Concentration  float64            `json:"concentration"` // HHI 0-1，越高越依赖少数人
	ClassCounts    map[string]int     `json:"classCounts"`
	CategoryShares map[string]float64 `json:"categoryShares"` // 按余额的类别占比
}

// TimeReflux 时间精力回流。
type TimeReflux struct {
	Hourly         []int              `json:"hourly"`         // 24 桶
	Weekday        []int              `json:"weekday"`        // 7 桶（0=周日）
	CategoryShares map[string]float64 `json:"categoryShares"` // 近 30 天按消息量的类别占比
	Insights       []string           `json:"insights"`       // 自动生成的偏差洞察句
}

// TrajectoryPoint 月度轨迹点。
type TrajectoryPoint struct {
	Month  string `json:"month"` // YYYY-MM
	Msgs   int    `json:"msgs"`
	Active int    `json:"active"` // 当月有往来联系人数
}

// LifeState 一次完整的人生状态快照。
type LifeState struct {
	GeneratedAt string            `json:"generatedAt"`
	From        string            `json:"from"` // 快照覆盖窗口起点（近 90 天）
	Portfolio   LifePortfolio     `json:"portfolio"`
	Assets      []LifeAsset       `json:"assets"`
	HighRisk    []LifeAsset       `json:"highRisk"` // Assets 中 riskLevel=high，按风险降序
	Time        TimeReflux        `json:"time"`
	Trajectory  []TrajectoryPoint `json:"trajectory"`
}

// assetClassWeight 类别对「人际总财富」的权重。
func assetClassWeight(class string) float64 {
	switch class {
	case "close":
		return 1.5
	case "friend":
		return 1.0
	case "transactional":
		return 0.5
	case "acquaintance":
		return 0.6
	default: // weak
		return 0.4
	}
}

// ---------- 计算 ----------

// ComputeLifeState 重算人生状态并写入缓存。
func ComputeLifeState(db *sql.DB, now time.Time) error {
	raw, err := gatherLifeRaw(db, now)
	if err != nil {
		return err
	}
	state := buildLifeState(now, raw)
	return saveLifeStateCache(db, now, state)
}

// lifeRaw 是所有 SQL 读出来的原始聚合，供纯计算层使用。
type lifeRaw struct {
	balance     map[int64]int     // 180 天亲密度
	freq30      map[int64]int     // 30 天消息总数
	mine30      map[int64]int     // 30 天我发
	other30     map[int64]int     // 30 天对方发
	name        map[int64]string  // 展示名（备注优先）
	lastTS      map[int64]int64   // 最后一条消息 unix
	lifeCount   map[int64]int     // 生命周期消息总数
	lifeDays    map[int64]int     // 生命周期跨度天数
	slope       map[int64]float64 // 90 天每周斜率
	category    map[int64]string  // 类别（标签推断）
	hourly      [24]int           // 近 30 天小时分布
	weekday     [7]int            // 近 30 天周几分布
	monthMsgs   map[string]int    // 近 12 月每月消息数
	monthActive map[string]int    // 近 12 月每月活跃联系人数
	catRecent   map[string]int    // 近 30 天各类别消息量
	catLife     map[string]int    // 生命周期各类别消息量
	active      map[int64]bool    // 未合并联系人集合（资产账本 universe）
}

func gatherLifeRaw(db *sql.DB, now time.Time) (*lifeRaw, error) {
	r := &lifeRaw{
		balance: map[int64]int{}, freq30: map[int64]int{}, mine30: map[int64]int{},
		other30: map[int64]int{}, name: map[int64]string{}, lastTS: map[int64]int64{},
		lifeCount: map[int64]int{}, lifeDays: map[int64]int{}, slope: map[int64]float64{},
		category: map[int64]string{}, monthMsgs: map[string]int{}, monthActive: map[string]int{},
		catRecent: map[string]int{}, catLife: map[string]int{},
	}

	// 1) 亲密度：180 天作关系深度，30 天作当前活动。两者内部各自取锁并读尽，返回切片。
	deep, err := computeIntimacy(db, now, 180)
	if err != nil {
		return nil, fmt.Errorf("深度亲密度: %w", err)
	}
	for _, it := range deep {
		r.balance[it.ContactID] = it.Score
		r.name[it.ContactID] = it.Name
	}
	recent, err := computeIntimacy(db, now, 30)
	if err != nil {
		return nil, fmt.Errorf("近期亲密度: %w", err)
	}
	for _, it := range recent {
		r.freq30[it.ContactID] = it.Mine + it.Other
		r.mine30[it.ContactID] = it.Mine
		r.other30[it.ContactID] = it.Other
		if _, ok := r.name[it.ContactID]; !ok {
			r.name[it.ContactID] = it.Name
		}
	}

	dbMu.Lock()
	defer dbMu.Unlock()

	// 2) 生命周期聚合：总数、首末时间跨度、最后联系时间。
	// 走统一历史层（P9）：归档的老消息同样是这段关系生命周期的一部分，漏算会把
	// 「认识很久的人」算成「刚认识」，而且不报错。
	lifeQ, lifeArgs := historySelectLocked(db,
		"contact_id, "+historyTimeExpr+" AS su",
		"contact_id, COUNT(*), MIN(su), MAX(su)",
		HistoryFilter{WithTimeOnly: true}, "GROUP BY contact_id")
	if rows, err := db.Query(lifeQ, lifeArgs...); err == nil {
		for rows.Next() {
			var cid int64
			var cnt int
			var mn, mx sql.NullInt64
			if rows.Scan(&cid, &cnt, &mn, &mx) == nil && mn.Valid && mx.Valid {
				r.lifeCount[cid] = cnt
				r.lastTS[cid] = mx.Int64
				days := int((mx.Int64 - mn.Int64) / 86400)
				if days < 1 {
					days = 1
				}
				r.lifeDays[cid] = days
			}
		}
		rows.Close()
	}

	// 3) 未合并联系人集合（资产账本 universe）+ 备注名补全。
	active := map[int64]bool{}
	if rows, err := db.Query(`SELECT id, name, COALESCE(remark,'') FROM contacts WHERE merged_into IS NULL`); err == nil {
		for rows.Next() {
			var cid int64
			var name, remark string
			if rows.Scan(&cid, &name, &remark) == nil {
				active[cid] = true
				if _, ok := r.name[cid]; !ok {
					label := name
					if strings.TrimSpace(remark) != "" {
						label = remark + "（" + name + "）"
					}
					r.name[cid] = label
				}
			}
		}
		rows.Close()
	}

	// 4) 类别：由标签名推断（无标签→其他），纯字符串匹配，永不失败。
	tagByContact := map[int64][]string{}
	if rows, err := db.Query(`
		SELECT l.contact_id, t.name FROM contact_tag_links l
		JOIN contact_tags t ON t.id = l.tag_id`); err == nil {
		for rows.Next() {
			var cid int64
			var name string
			if rows.Scan(&cid, &name) == nil {
				tagByContact[cid] = append(tagByContact[cid], name)
			}
		}
		rows.Close()
	}
	for cid := range active {
		r.category[cid] = categorizeByTags(tagByContact[cid])
	}

	// 5) 90 天每周斜率：按联系人为每个互动日计数，再做线性回归。
	since90 := now.AddDate(0, 0, -90).Format("2006-01-02")
	type dayCount struct {
		cid int64
		day string
		n   int
	}
	var daily []dayCount
	// 同样必须并归档表：90 天斜率是「关系冷热」的核心信号，少算一段会误判走向。
	// 下界用当日零点（与原先的日期字串比较同口径，不是当前时刻）。
	slopeQ, slopeArgs := historySelectLocked(db,
		"contact_id, substr(msg_time,1,10) AS d",
		"contact_id, d, COUNT(*)",
		HistoryFilter{SinceUnix: mustParseDate(since90).Unix(), WithTimeOnly: true}, "GROUP BY contact_id, d")
	if rows, err := db.Query(slopeQ, slopeArgs...); err == nil {
		for rows.Next() {
			var dc dayCount
			if rows.Scan(&dc.cid, &dc.day, &dc.n) == nil {
				daily = append(daily, dc)
			}
		}
		rows.Close()
	}
	// 归拢成每联系人定长 90 序列
	seq := map[int64][]float64{}
	base := mustParseDate(since90)
	for _, dc := range daily {
		d := mustParseDate(dc.day)
		idx := int(d.Sub(base).Hours() / 24)
		if idx < 0 || idx >= 90 {
			continue
		}
		if _, ok := seq[dc.cid]; !ok {
			seq[dc.cid] = make([]float64, 90)
		}
		seq[dc.cid][idx] = float64(dc.n)
	}
	for cid, s := range seq {
		r.slope[cid] = weeklySlope(s)
	}

	// 6) 近 30 天小时/周几分布（并上归档表，否则作息样本会被归档抹掉一部分）。
	// 必须带 'localtime'：msg_time 是带偏移的 RFC3339，SQLite 的 strftime 会先转 UTC，
	// 不加修饰符会让早上 8 点的消息被记到 0 点、整体偏 8 小时（仓库内其它日历聚合均已用 localtime）。
	hourQ, hourArgs := historySelectLocked(db,
		"CAST(strftime('%H', msg_time, 'localtime') AS INT) AS hh, CAST(strftime('%w', msg_time, 'localtime') AS INT) AS ww",
		"hh, ww, COUNT(*)",
		HistoryFilter{SinceUnix: now.AddDate(0, 0, -30).Unix(), WithTimeOnly: true}, "GROUP BY hh, ww")
	if rows, err := db.Query(hourQ, hourArgs...); err == nil {
		for rows.Next() {
			var h, w, n int
			if rows.Scan(&h, &w, &n) == nil {
				if h >= 0 && h < 24 {
					r.hourly[h] += n
				}
				if w >= 0 && w < 7 {
					r.weekday[w] += n
				}
			}
		}
		rows.Close()
	}

	// 7) 近 12 月轨迹。
	since12 := now.AddDate(0, 0, -365).Format("2006-01")
	if rows, err := db.Query(`
		SELECT substr(msg_time,1,7) m, COUNT(*), COUNT(DISTINCT contact_id)
		FROM messages
		WHERE msg_time IS NOT NULL AND msg_time != '' AND substr(msg_time,1,7) >= ?
		GROUP BY m ORDER BY m`, since12); err == nil {
		for rows.Next() {
			var m string
			var cnt, act int
			if rows.Scan(&m, &cnt, &act) == nil {
				r.monthMsgs[m] = cnt
				r.monthActive[m] = act
			}
		}
		rows.Close()
	}

	// 8) 类别消息量：近期(30d) 与 生命周期，供时间回流偏差洞察。
	for cid, n := range r.freq30 {
		if active[cid] {
			r.catRecent[r.category[cid]] += n
		}
	}
	for cid, n := range r.lifeCount {
		if active[cid] {
			r.catLife[r.category[cid]] += n
		}
	}
	r.active = active
	return r, nil
}

// buildLifeState 把原始聚合算成结构化快照（纯函数，不碰 DB）。
func buildLifeState(now time.Time, raw *lifeRaw) *LifeState {
	st := &LifeState{
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
		From:        now.AddDate(0, 0, -90).Format("2006-01-02"),
	}
	st.Time.Hourly = raw.hourly[:]
	st.Time.Weekday = raw.weekday[:]

	sumBalance := 0.0
	weighted := 0.0
	delta := 0.0
	classCounts := map[string]int{}
	catBalance := map[string]float64{}

	for cid := range raw.active {
		life := raw.lifeCount[cid]
		if life == 0 {
			continue // 从没聊过天的联系人不进账本
		}
		balance := raw.balance[cid]
		freq := raw.freq30[cid]
		mine, other := raw.mine30[cid], raw.other30[cid]

		// 现金流：近 30 天日均 / 历史日均
		recentRate := float64(freq) / 30.0
		baseRate := float64(life) / float64(max(raw.lifeDays[cid], 1))
		cashflow := 0.0
		if baseRate > 0 {
			cashflow = recentRate / baseRate
		}
		if cashflow > 10 {
			cashflow = 10
		}

		// 距上次联系天数
		decay := 0
		if last, ok := raw.lastTS[cid]; ok {
			decay = int(now.Unix()-last) / 86400
			if decay < 0 {
				decay = 0
			}
		}

		// 失衡度：0 均衡 .. 1 完全单向
		imbalance := 0.0
		if mine+other > 0 {
			imbalance = absF(float64(mine-other)) / float64(mine+other)
		}

		wr := raw.slope[cid]
		// 风险：陈旧 + 负趋势 + 单向
		stale := minF(float64(decay)/90.0, 1)
		negTrend := 0.0
		if wr < 0 {
			negTrend = minF(-wr/5.0, 1)
		}
		risk := 0.35*stale + 0.35*negTrend + 0.30*imbalance
		level := "low"
		if risk >= 0.6 {
			level = "high"
		} else if risk >= 0.35 {
			level = "mid"
		}

		class := classifyAsset(balance, freq, imbalance)
		cat := raw.category[cid]
		if cat == "" {
			cat = "其他"
		}

		a := LifeAsset{
			ContactID: cid, Name: raw.name[cid], Balance: balance,
			Cashflow: round2(cashflow), DecayDays: decay, WeeklyRate: round2(wr),
			RiskScore: round2(risk), RiskLevel: level, AssetClass: class, Category: cat, Freq30: freq,
		}
		st.Assets = append(st.Assets, a)

		w := assetClassWeight(class)
		sumBalance += float64(balance)
		weighted += float64(balance) * w
		delta += (cashflow - 1) * w
		classCounts[class]++
		catBalance[cat] += float64(balance)
		if level == "high" {
			st.Portfolio.HighRiskCount++
		}
	}

	// 集中度 HHI
	if sumBalance > 0 {
		hhi := 0.0
		for cid := range raw.active {
			if raw.lifeCount[cid] == 0 {
				continue
			}
			s := float64(raw.balance[cid]) / sumBalance
			hhi += s * s
		}
		st.Portfolio.Concentration = round2(hhi)
	}
	st.Portfolio.TotalWealth = round1(weighted)
	st.Portfolio.QuarterDelta = round1(delta)
	st.Portfolio.ContactCount = len(st.Assets)
	st.Portfolio.ClassCounts = classCounts
	st.Portfolio.CategoryShares = sharesOf(catBalance)

	// 高风险清单（按风险降序）
	st.HighRisk = make([]LifeAsset, 0)
	for _, a := range st.Assets {
		if a.RiskLevel == "high" {
			st.HighRisk = append(st.HighRisk, a)
		}
	}
	sort.Slice(st.HighRisk, func(i, j int) bool { return st.HighRisk[i].RiskScore > st.HighRisk[j].RiskScore })
	sort.Slice(st.Assets, func(i, j int) bool { return st.Assets[i].Balance > st.Assets[j].Balance })

	// 时间回流：类别占比 + 偏差洞察
	recentTotal := sumMap(raw.catRecent)
	lifeTotal := sumMap(raw.catLife)
	st.Time.CategoryShares = map[string]float64{}
	for cat, n := range raw.catRecent {
		if recentTotal > 0 {
			st.Time.CategoryShares[cat] = round2(float64(n) / float64(recentTotal) * 100)
		}
	}
	st.Time.Insights = categoryDriftInsights(raw.catRecent, raw.catLife, recentTotal, lifeTotal)

	// 轨迹
	months := make([]string, 0, len(raw.monthMsgs))
	for m := range raw.monthMsgs {
		months = append(months, m)
	}
	sort.Strings(months)
	for _, m := range months {
		st.Trajectory = append(st.Trajectory, TrajectoryPoint{Month: m, Msgs: raw.monthMsgs[m], Active: raw.monthActive[m]})
	}

	return st
}

// ---------- 缓存读写（镜像 weekly_plan.go） ----------

func saveLifeStateCache(db *sql.DB, now time.Time, st *LifeState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("序列化人生状态失败: %w", err)
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err = db.Exec(
		`INSERT INTO life_state_cache (id, generated_at, state_json) VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET generated_at=excluded.generated_at, state_json=excluded.state_json`,
		now.Format(time.RFC3339), string(data))
	return err
}

// GetCachedLifeState 读取缓存；无行返回零值快照。
func GetCachedLifeState(db *sql.DB) (*LifeState, time.Time, error) {
	dbMu.Lock()
	var genAt, raw string
	err := db.QueryRow(`SELECT generated_at, state_json FROM life_state_cache WHERE id = 1`).Scan(&genAt, &raw)
	dbMu.Unlock()

	if err == sql.ErrNoRows {
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	var st LifeState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return nil, time.Time{}, fmt.Errorf("人生状态缓存解析失败: %w", err)
	}
	t, _ := time.Parse(time.RFC3339, genAt)
	return &st, t, nil
}

// IsLifeStateStale 缓存是否超过 7 天。
func IsLifeStateStale(generatedAt time.Time, now time.Time) bool {
	if generatedAt.IsZero() {
		return true
	}
	return now.Sub(generatedAt) > 7*24*time.Hour
}

// ---------- 辅助 ----------

// categorizeByTags 依据标签名把联系人归入 家人/同事/朋友/其他。
func categorizeByTags(tags []string) string {
	joined := strings.Join(tags, " ")
	switch {
	case containsAny(joined, "家人", "家庭", "亲属", "亲戚", "爸", "妈", "父", "母", "儿", "女", "哥", "姐", "弟", "妹", "老婆", "老公", "伴侣", "妻", "夫", "家"):
		return "家人"
	case containsAny(joined, "同事", "工作", "公司", "客户", "领导", "老板", "上司", "业务", "职业"):
		return "同事"
	case containsAny(joined, "朋友", "同学", "兄弟", "姐妹", "好友", "哥们", "闺蜜", "室友"):
		return "朋友"
	default:
		return "其他"
	}
}

// classifyAsset 依据 (深度分, 近期频率, 失衡度) 判定资产类别。
func classifyAsset(balance, freq30 int, imbalance float64) string {
	// 频繁但浅且单向 → 事务型
	if balance < 50 && imbalance > 0.75 && freq30 >= 10 {
		return "transactional"
	}
	switch {
	case balance >= 70 && freq30 >= 20:
		return "close"
	case balance >= 50:
		return "friend"
	case balance >= 25:
		return "acquaintance"
	default:
		return "weak"
	}
}

// weeklySlope 对定长 90 每日序列做最小二乘，返回每周斜率（条/周）。
func weeklySlope(daily []float64) float64 {
	n := len(daily)
	if n == 0 {
		return 0
	}
	var sx, sy, sxy, sxx float64
	for i, y := range daily {
		x := float64(i)
		sx += x
		sy += y
		sxy += x * y
		sxx += x * x
	}
	denom := float64(n)*sxx - sx*sx
	if denom == 0 {
		return 0
	}
	perDay := (float64(n)*sxy - sx*sy) / denom
	return perDay * 7
}

// categoryDriftInsights 对比近期与历史各类别占比，产出偏差洞察句。
func categoryDriftInsights(recent, life map[string]int, recentTotal, lifeTotal int) []string {
	out := []string{}
	if recentTotal <= 0 || lifeTotal <= 0 {
		return out
	}
	cats := map[string]bool{}
	for c := range recent {
		cats[c] = true
	}
	for c := range life {
		cats[c] = true
	}
	type drift struct {
		cat    string
		diff   float64
		rShare float64
		lShare float64
	}
	var list []drift
	for c := range cats {
		if c == "其他" || c == "" {
			continue
		}
		r := float64(recent[c]) / float64(recentTotal) * 100
		l := float64(life[c]) / float64(lifeTotal) * 100
		list = append(list, drift{cat: c, diff: r - l, rShare: r, lShare: l})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].diff < list[j].diff }) // 降得最狠的在前
	for _, d := range list {
		if d.diff <= -8 {
			out = append(out, fmt.Sprintf("%s占比从 %.0f%% 降到 %.0f%%，值得留意", d.cat, d.lShare, d.rShare))
		} else if d.diff >= 8 {
			out = append(out, fmt.Sprintf("%s占比从 %.0f%% 升到 %.0f%%，投入在增加", d.cat, d.lShare, d.rShare))
		}
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func sharesOf(m map[string]float64) map[string]float64 {
	total := 0.0
	for _, v := range m {
		total += v
	}
	out := map[string]float64{}
	if total == 0 {
		return out
	}
	for k, v := range m {
		out[k] = round2(v / total * 100)
	}
	return out
}

func sumMap(m map[string]int) int {
	t := 0
	for _, v := range m {
		t += v
	}
	return t
}

func absF(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}

func mustParseDate(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}
	}
	return t
}
