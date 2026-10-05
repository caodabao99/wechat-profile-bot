package main

// trend.go —— v4.5.0 A：高阶洞察趋势快照与周环比。
//
// 设计原则：系统主动做、人只看结果 / 零操作减负 / 纯确定性可解释 / 不调大模型。
//
// v4.4.0 四层洞察都是"单行瞬时快照、每周覆盖、历史丢弃"。本文件把每周的聚合标量
// 追加进 insight_trend_history（一周恒一行），让洞察长出"时间维度"：
//   - appendTrendSnapshot：每周一随四件套刷新追加/覆盖当周一行，只留近 52 周；
//   - GetTrendSeries：供前端画 sparkline；
//   - latestPrevSnapshot：取上一周快照作周环比基准；
//   - computeWeeklyChange：纯函数，比对本周 vs 上周产出人话 delta，进简报"本周关键变化"块。
//
// 单连接池铁律：读缓存/读历史都用 getCached*、QueryRow、或"取尽进内存再 Close"，
//   写库一律先顺序读完再单独取一次 dbMu 写，绝不嵌套取锁、绝不边迭代边写。

import (
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// trendMaxWeeks 历史表只保留最近约一年（52 周），超出即 prune。
const trendMaxWeeks = 52

// trendDefaultSeries 前端默认展示近 N 周走势。
const trendDefaultSeries = 12

// trendColumns 趋势表列顺序（写入/读取共用，避免漂移）。
var trendColumns = []string{
	"week_start", "generated_at",
	"total_wealth", "high_risk_count", "concentration", "contact_count",
	"initiation_rate", "one_way_count", "imbalance_avg",
	"node_count", "edge_count", "cluster_count", "fragility_score", "bridge_count", "intro_count",
	"outcome_sample", "best_action_count",
}

// TrendSnapshot 一周的聚合标量快照（映射 insight_trend_history 一行）。
type TrendSnapshot struct {
	WeekStart       string  `json:"weekStart"`
	GeneratedAt     string  `json:"generatedAt"`
	TotalWealth     float64 `json:"totalWealth"`
	HighRiskCount   int     `json:"highRiskCount"`
	Concentration   float64 `json:"concentration"`
	ContactCount    int     `json:"contactCount"`
	InitiationRate  float64 `json:"initiationRate"`
	OneWayCount     int     `json:"oneWayCount"`
	ImbalanceAvg    float64 `json:"imbalanceAvg"`
	NodeCount       int     `json:"nodeCount"`
	EdgeCount       int     `json:"edgeCount"`
	ClusterCount    int     `json:"clusterCount"`
	FragilityScore  float64 `json:"fragilityScore"`
	BridgeCount     int     `json:"bridgeCount"`
	IntroCount      int     `json:"introCount"`
	OutcomeSample   int     `json:"outcomeSample"`
	BestActionCount int     `json:"bestActionCount"`
}

// BriefingChange 一条周环比变化（简报展示）。Dir：up/down/flat。
type BriefingChange struct {
	Text  string `json:"text"`
	Dir   string `json:"dir"`
	Layer string `json:"layer"`
}

// weekStartOf 返回 t 所在周的周一（ISO 日期，YYYY-MM-DD）。
// 用日历 AddDate 而非时长加减，规避夏令时/时长漂移（见 daysUntilNext 教训）。
func weekStartOf(t time.Time) string {
	offset := (int(t.Weekday()) - int(time.Monday) + 7) % 7
	mon := t.AddDate(0, 0, -offset)
	return fmt.Sprintf("%04d-%02d-%02d", mon.Year(), int(mon.Month()), mon.Day())
}

// snapFromCaches 由四层快照构造本周标量。任一为 nil 时对应列留零值（各取各层，互不影响）。
func snapFromCaches(weekStart string, now time.Time,
	state *LifeState, self *SelfPortrait, net *NetworkInsight, learn *InterventionInsight) TrendSnapshot {
	s := TrendSnapshot{WeekStart: weekStart, GeneratedAt: now.Format(time.RFC3339)}
	if state != nil {
		s.TotalWealth = roundF(state.Portfolio.TotalWealth, 1)
		s.HighRiskCount = state.Portfolio.HighRiskCount
		s.Concentration = roundF(state.Portfolio.Concentration, 3)
		s.ContactCount = state.Portfolio.ContactCount
	}
	if self != nil {
		s.InitiationRate = roundF(self.InitiationRate, 1)
		s.OneWayCount = self.OneWayCount
		s.ImbalanceAvg = roundF(self.ImbalanceAvg, 1)
	}
	if net != nil {
		s.NodeCount = net.NodeCount
		s.EdgeCount = net.EdgeCount
		s.ClusterCount = net.ClusterCount
		s.FragilityScore = net.FragilityScore
		s.BridgeCount = len(net.Bridges)
		s.IntroCount = len(net.Introductions)
	}
	if learn != nil {
		s.OutcomeSample = learn.TotalResolved
		s.BestActionCount = len(learn.BestActions)
	}
	return s
}

// appendTrendSnapshot 把本周四层标量追加进历史表（同周幂等覆盖），并 prune 只留近 52 周。
// 供 ComputeAdvancedInsights 末尾调用——此时四层缓存刚写好。
func appendTrendSnapshot(db *sql.DB, now time.Time) error {
	// 1) 先顺序读回四层缓存（各自取锁、自带释放），聚成快照——绝不持锁嵌套。
	ws := weekStartOf(now)
	s := snapFromCaches(ws, now,
		getCachedNoRecomputeState(db),
		getCachedNoRecomputeSelf(db),
		getCachedNoRecomputeNetwork(db),
		getCachedNoRecomputeIntervention(db),
	)

	// 2) 再单独取一次锁写库 + prune。
	dbMu.Lock()
	defer dbMu.Unlock()
	if _, err := db.Exec(
		`INSERT INTO insight_trend_history (`+join(trendColumns, ",")+`)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(week_start) DO UPDATE SET
		   generated_at=excluded.generated_at,
		   total_wealth=excluded.total_wealth, high_risk_count=excluded.high_risk_count,
		   concentration=excluded.concentration, contact_count=excluded.contact_count,
		   initiation_rate=excluded.initiation_rate, one_way_count=excluded.one_way_count,
		   imbalance_avg=excluded.imbalance_avg,
		   node_count=excluded.node_count, edge_count=excluded.edge_count,
		   cluster_count=excluded.cluster_count, fragility_score=excluded.fragility_score,
		   bridge_count=excluded.bridge_count, intro_count=excluded.intro_count,
		   outcome_sample=excluded.outcome_sample, best_action_count=excluded.best_action_count`,
		ws, s.GeneratedAt,
		s.TotalWealth, s.HighRiskCount, s.Concentration, s.ContactCount,
		s.InitiationRate, s.OneWayCount, s.ImbalanceAvg,
		s.NodeCount, s.EdgeCount, s.ClusterCount, s.FragilityScore, s.BridgeCount, s.IntroCount,
		s.OutcomeSample, s.BestActionCount,
	); err != nil {
		return fmt.Errorf("写入洞察趋势快照失败: %w", err)
	}
	// 只留近一年（week_start 为 YYYY-MM-DD，字典序即时间序）。
	cut := now.AddDate(-1, 0, 0)
	cutStr := fmt.Sprintf("%04d-%02d-%02d", cut.Year(), int(cut.Month()), cut.Day())
	if _, err := db.Exec(`DELETE FROM insight_trend_history WHERE week_start < ?`, cutStr); err != nil {
		return fmt.Errorf("清理过期洞察趋势失败: %w", err)
	}
	return nil
}

// scanTrendRow 把一行趋势记录扫描进 TrendSnapshot（列顺序须与 trendColumns 一致）。
func scanTrendRow(scan func(dest ...interface{}) error) (TrendSnapshot, error) {
	var s TrendSnapshot
	err := scan(&s.WeekStart, &s.GeneratedAt,
		&s.TotalWealth, &s.HighRiskCount, &s.Concentration, &s.ContactCount,
		&s.InitiationRate, &s.OneWayCount, &s.ImbalanceAvg,
		&s.NodeCount, &s.EdgeCount, &s.ClusterCount, &s.FragilityScore, &s.BridgeCount, &s.IntroCount,
		&s.OutcomeSample, &s.BestActionCount)
	return s, err
}

const trendSelectCols = `week_start, generated_at, total_wealth, high_risk_count, concentration, contact_count,
	initiation_rate, one_way_count, imbalance_avg,
	node_count, edge_count, cluster_count, fragility_score, bridge_count, intro_count,
	outcome_sample, best_action_count`

// GetTrendSeries 取近 weeks 周快照，返回按 week_start 升序（老→新）；无历史返回空切片。
func GetTrendSeries(db *sql.DB, weeks int) ([]TrendSnapshot, error) {
	if weeks <= 0 {
		weeks = trendDefaultSeries
	}
	dbMu.Lock()
	rows, err := db.Query(`SELECT `+trendSelectCols+` FROM insight_trend_history ORDER BY week_start DESC LIMIT ?`, weeks)
	if err != nil {
		dbMu.Unlock()
		return nil, err
	}
	var desc []TrendSnapshot
	for rows.Next() {
		if s, err := scanTrendRow(rows.Scan); err == nil {
			desc = append(desc, s)
		}
	}
	rows.Close()
	dbMu.Unlock()
	// DESC 取最近 N 周后翻回升序，供画线。
	sort.Slice(desc, func(i, j int) bool { return desc[i].WeekStart < desc[j].WeekStart })
	if desc == nil {
		desc = []TrendSnapshot{}
	}
	return desc, nil
}

// latestPrevSnapshot 取严格早于 thisWeek 的最近一行作环比基准；无则返回 nil（首周无对比）。
func latestPrevSnapshot(db *sql.DB, thisWeek string) (*TrendSnapshot, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	row := db.QueryRow(`SELECT `+trendSelectCols+` FROM insight_trend_history
		WHERE week_start < ? ORDER BY week_start DESC LIMIT 1`, thisWeek)
	s, err := scanTrendRow(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// computeWeeklyChange 纯函数：比对本周 cur 与上周 prev，产出人话 delta。
// prev 为 nil（首周/无历史）时返回空切片，简报优雅跳过该块。逐项固定顺序，确定性。
func computeWeeklyChange(cur, prev *TrendSnapshot) []BriefingChange {
	out := []BriefingChange{}
	if cur == nil || prev == nil {
		return out
	}
	add := func(layer, text string, dir string) {
		out = append(out, BriefingChange{Text: text, Dir: dir, Layer: layer})
	}

	// 人际总财富（升=暖，降=冷）。
	if d := cur.TotalWealth - prev.TotalWealth; d > 0.5 || d < -0.5 {
		add("life", fmt.Sprintf("人际总财富 %.0f → %.0f", prev.TotalWealth, cur.TotalWealth), dirOf(d))
	}
	// 高风险关系数（增多=更危险）。
	if cur.HighRiskCount != prev.HighRiskCount {
		d := float64(cur.HighRiskCount - prev.HighRiskCount)
		add("life", fmt.Sprintf("高风险关系 %d 段 → %d 段", prev.HighRiskCount, cur.HighRiskCount), dirOf(d))
	}
	// 圈子数（减少=网络更合并，增多=更分散）。
	if cur.ClusterCount != prev.ClusterCount && cur.NodeCount > 0 && prev.NodeCount > 0 {
		d := float64(cur.ClusterCount - prev.ClusterCount)
		tail := "网络更分散"
		if d < 0 {
			tail = "圈子出现合并"
		}
		add("network", fmt.Sprintf("圈子 %d 个 → %d 个，%s", prev.ClusterCount, cur.ClusterCount, tail), dirOf(d))
	}
	// 网络脆弱度（0-1，升高=更依赖少数桥）。
	if d := cur.FragilityScore - prev.FragilityScore; d > 0.03 || d < -0.03 {
		add("network", fmt.Sprintf("网络脆弱度 %.0f%% → %.0f%%", prev.FragilityScore*100, cur.FragilityScore*100), dirOf(d))
	}
	// 你先开口率（升高=更主动，回落=更被动）。
	if d := cur.InitiationRate - prev.InitiationRate; d > 2 || d < -2 {
		tail := ""
		if d > 0 {
			tail = "（你更主动了）"
		} else {
			tail = "（你更被动了）"
		}
		add("self", fmt.Sprintf("你先开口率 %.0f%% → %.0f%% %s", prev.InitiationRate, cur.InitiationRate, tail), dirOf(d))
	}
	// 单向关系数（增多=更多一厢情愿）。
	if cur.OneWayCount != prev.OneWayCount {
		d := float64(cur.OneWayCount - prev.OneWayCount)
		add("self", fmt.Sprintf("几乎单向维系的关系 %d 段 → %d 段", prev.OneWayCount, cur.OneWayCount), dirOf(d))
	}

	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

// dirOf 数值增减映射方向串（升 up / 降 down / 平 flat）。
func dirOf(delta float64) string {
	switch {
	case delta > 0:
		return "up"
	case delta < 0:
		return "down"
	default:
		return "flat"
	}
}
