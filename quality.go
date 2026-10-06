package main

// 对话质量评分（增值功能，纯确定性、离线可算、零 LLM）。
//
// 给单个联系人量化四个维度，各 0-100，再加权出综合分：
//   - 回复及时性 responsiveness：近窗口内「对方→我」的回复间隔中位数越短越高分。
//   - 对话深度     depth        ：窗口内消息量 / 活跃天数 / 对方平均长度。
//   - 话题多样性   diversity    ：画像里可辨识的离散话题（兴趣 / 性格 / 重要事实 / 口头禅）去重数。
//   - 情绪正向度   positivity    ：assistant_emotions 近窗口均分（0-100，50 中性）。
//
// 设计铁律：
//   - 全程本地 SQL + Go 计算，不调 LLM，可随便刷新、可离线单测、结果可复现（跑两次逐字段相等）。
//   - 单连接池：一次计算分层取 dbMu，绝不嵌套锁。取数分两趟——messages+contacts 一趟、
//     assistant_emotions 一趟（该表属增值表，缺失/无数据则该维度不计入综合，静默跳过）。
//   - 小样本诚实标注：回复样本不足、无画像、无情绪记录时给保守值并在 Detail 说明，不虚高。

import (
	"database/sql"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	qualityDefaultDays     = 90   // 缺省评分窗口
	qualityMaxDays         = 3650 // 窗口上限
	qualityMinReplySamples = 3    // 回复及时性可采信的最小样本数
	qualityMaxScan         = 20000
	qualityReplyCap        = 6 * 3600 // 超过 6 小时不计为「回复」（同社交大盘口径）
)

// 四维权重（positivity 无数据时按比例归一到其余三维）
const (
	wResponsive = 0.30
	wDepth      = 0.30
	wDiversity  = 0.20
	wPositivity = 0.20
)

// QualityDim 单个维度得分
type QualityDim struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Value  int    `json:"value"`
	Detail string `json:"detail"`
}

// ContactQuality 某联系人的对话质量评分
type ContactQuality struct {
	ContactID   int64                 `json:"contactId"`
	Name        string                `json:"name"`
	Score       int                   `json:"score"`
	Dims        []QualityDim          `json:"dims"`
	WindowDays  int                   `json:"windowDays"`
	GeneratedAt string                `json:"generatedAt"`
	History     []QualityHistoryPoint `json:"history,omitempty"` // v5.2.1 综合分历史趋势（近 N 周，仅 HTTP 读取路径回填）
}

// clampPct 把 [0,1] 比例夹到 [0,100] 整数
func clampPct(ratio float64) int {
	v := ratio * 100
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return int(v + 0.5)
}

// replyGapToScore 回复间隔中位数（秒）→ 0-100 分。分段线性，直观且确定性。
func replyGapToScore(sec float64) int {
	m := sec / 60.0 // 分钟
	switch {
	case m <= 1:
		return 100
	case m <= 5:
		return 90
	case m <= 15:
		return 80
	case m <= 30:
		return 70
	case m <= 60:
		return 60
	case m <= 180:
		return 45
	default:
		return 30
	}
}

// formatGap 把秒数成人话（用于 Detail）
func formatGap(sec float64) string {
	switch {
	case sec < 60:
		return strconv.Itoa(int(sec+0.5)) + " 秒"
	case sec < 3600:
		return strconv.Itoa(int(sec/60+0.5)) + " 分钟"
	default:
		h := sec / 3600
		if h < 24 {
			return strconv.Itoa(int(h+0.5)) + " 小时"
		}
		return strconv.Itoa(int(h/24+0.5)) + " 天"
	}
}

// ComputeContactQuality 计算某联系人近 windowDays 天的对话质量评分。
// 联系人不存在时由上层 HTTP 先校验；本函数对空数据返回中性低分而非报错。
func ComputeContactQuality(db *sql.DB, contactID int64, windowDays int) (*ContactQuality, error) {
	if windowDays <= 0 {
		windowDays = qualityDefaultDays
	}
	if windowDays > qualityMaxDays {
		windowDays = qualityMaxDays
	}
	now := time.Now()
	sinceStr := now.AddDate(0, 0, -windowDays).Format(time.RFC3339)

	q := &ContactQuality{
		ContactID:   contactID,
		WindowDays:  windowDays,
		Dims:        []QualityDim{},
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
	}

	// —— 第 1 趟锁：联系人 + 窗口内消息时间线，取尽即释放 ——
	var name, remark string
	var profileJSON sql.NullString
	type tlMsg struct {
		sender string
		ts     int64
		clen   int64
	}
	var timeline []tlMsg

	dbMu.Lock()
	if err := db.QueryRow(
		`SELECT COALESCE(name,''), COALESCE(remark,''), COALESCE(profile_json,'')
		 FROM contacts WHERE id=?`, contactID).Scan(&name, &remark, &profileJSON); err != nil {
		dbMu.Unlock()
		return nil, err
	}
	// 时间线走统一历史层（P9）：归档后窗口语料分散在两表，只查 messages 会静默少一段。
	tlQ, tlArgs := historySelectLocked(db,
		"sender, id, "+historyTimeExpr+" AS ts, COALESCE(LENGTH(content), 0) AS clen",
		"sender, ts, clen",
		HistoryFilter{ContactID: contactID, SinceUnix: now.AddDate(0, 0, -windowDays).Unix(), WithTimeOnly: true},
		"ORDER BY ts ASC, id ASC LIMIT ?")
	tlArgs = append(tlArgs, qualityMaxScan+1)
	rows, err := db.Query(tlQ, tlArgs...)
	if err != nil {
		dbMu.Unlock()
		return nil, err
	}
	for rows.Next() {
		var m tlMsg
		if err := rows.Scan(&m.sender, &m.ts, &m.clen); err != nil {
			rows.Close()
			dbMu.Unlock()
			return nil, err
		}
		timeline = append(timeline, m)
	}
	err = rows.Err()
	rows.Close()
	dbMu.Unlock()
	if err != nil {
		return nil, err
	}
	if len(timeline) > qualityMaxScan {
		timeline = timeline[:qualityMaxScan]
	}

	// 展示名：备注优先
	q.Name = name
	if strings.TrimSpace(remark) != "" {
		q.Name = remark + "（" + name + "）"
	}

	// —— 汇总窗口内基础量（顺序遍历时间线，确定性） ——
	var mineCount, otherCount int64
	var otherLenSum int64
	daySeen := map[string]bool{}
	var myReplyGaps []int64
	for i := range timeline {
		m := timeline[i]
		if m.sender == "me" {
			mineCount++
		} else {
			otherCount++
			otherLenSum += m.clen
		}
		if m.ts > 0 {
			daySeen[time.Unix(m.ts, 0).Format("2006-01-02")] = true
		}
		if i > 0 {
			prev := timeline[i-1]
			gap := m.ts - prev.ts
			// 换了发言人、且在回复窗口内：一次「对方→我」的真实回复
			if prev.sender != m.sender && m.sender == "me" && gap > 0 && gap <= int64(qualityReplyCap) {
				myReplyGaps = append(myReplyGaps, gap)
			}
		}
	}
	activeDays := len(daySeen)

	// —— 维度 1：回复及时性 ——
	{
		dim := QualityDim{Key: "responsiveness", Label: "回复及时性"}
		samples := len(myReplyGaps)
		medianSec := medianInt(myReplyGaps)
		v := replyGapToScore(medianSec)
		if samples < qualityMinReplySamples {
			// 样本不足：向中性值 55 收敛，避免个别极端间隔主导评分
			v = (v + 55*2) / 3
			dim.Value = clampPct(float64(v) / 100)
			dim.Detail = "回复样本仅 " + strconv.Itoa(samples) + " 次，结果仅供参考"
		} else {
			dim.Value = v
			dim.Detail = "近 " + strconv.Itoa(samples) + " 次回复，中位间隔约 " + formatGap(medianSec)
		}
		q.Dims = append(q.Dims, dim)
	}

	// —— 维度 2：对话深度 ——
	{
		dim := QualityDim{Key: "depth", Label: "对话深度"}
		daysPart := math.Min(float64(activeDays), 15) / 15 * 40
		volPart := math.Min(float64(mineCount+otherCount), 200) / 200 * 30
		otherAvg := 0.0
		if otherCount > 0 {
			otherAvg = float64(otherLenSum) / float64(otherCount)
		}
		lenPart := math.Min(otherAvg, 50) / 50 * 30
		score := daysPart + volPart + lenPart
		dim.Value = clampPct(score / 100)
		dim.Detail = "窗口内 " + strconv.Itoa(int(mineCount+otherCount)) + " 条 / 活跃 " + strconv.Itoa(activeDays) +
			" 天 / 对方均长 " + strconv.Itoa(int(otherAvg+0.5)) + " 字"
		q.Dims = append(q.Dims, dim)
	}

	// —— 维度 3：话题多样性（画像派生） ——
	{
		dim := QualityDim{Key: "diversity", Label: "话题多样性"}
		concepts := distinctProfileConcepts(profileJSON.String)
		dim.Value = clampPct(math.Min(float64(concepts), 12) / 12)
		if concepts == 0 {
			dim.Detail = "暂无画像，无法评估话题多样性"
		} else {
			dim.Detail = "可辨识话题 " + strconv.Itoa(concepts) + " 个（兴趣/性格/事实/口头禅去重）"
		}
		q.Dims = append(q.Dims, dim)
	}

	// —— 第 2 趟锁：情绪（增值表，缺失或无数据则跳过，不计入综合） ——
	var emoAvg float64
	var emoSamples int
	emoOK := false
	dbMu.Lock()
	if tableExistsLocked(db, "assistant_emotions") {
		var sum sql.NullInt64
		var cnt int64
		qerr := db.QueryRow(
			`SELECT COALESCE(SUM(score),0), COUNT(*) FROM assistant_emotions
			 WHERE contact_id = ?
			   AND created_at IS NOT NULL AND created_at != ''
			   AND strftime('%s', created_at, 'localtime') >= strftime('%s', ?)`,
			contactID, sinceStr).Scan(&sum, &cnt)
		if qerr == nil && cnt > 0 {
			emoSamples = int(cnt)
			emoAvg = float64(sum.Int64) / float64(cnt)
			emoOK = true
		}
	}
	dbMu.Unlock()

	{
		dim := QualityDim{Key: "positivity", Label: "情绪正向度"}
		if emoOK {
			v := emoAvg
			if v < 0 {
				v = 0
			}
			if v > 100 {
				v = 100
			}
			dim.Value = int(v + 0.5)
			dim.Detail = "近 " + strconv.Itoa(emoSamples) + " 次情绪分析均分 " + strconv.Itoa(dim.Value) + "（50 为中性）"
		} else {
			dim.Value = -1
			dim.Detail = "暂无情绪分析记录，未计入综合分"
		}
		q.Dims = append(q.Dims, dim)
	}

	q.Score = compositeQualityScore(q.Dims)
	return q, nil
}

// compositeQualityScore 按固定权重加权求综合分；情绪维度无数据（value<0）时剔除并把权重按比例归一到其余维度。
func compositeQualityScore(dims []QualityDim) int {
	wOf := func(key string) float64 {
		switch key {
		case "responsiveness":
			return wResponsive
		case "depth":
			return wDepth
		case "diversity":
			return wDiversity
		case "positivity":
			return wPositivity
		}
		return 0
	}
	var weighted, totalW float64
	for _, d := range dims {
		if d.Value < 0 {
			continue // 无数据的维度不计入
		}
		w := wOf(d.Key)
		weighted += float64(d.Value) * w
		totalW += w
	}
	if totalW <= 0 {
		return 0
	}
	v := weighted / totalW
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return int(v + 0.5)
}

// distinctProfileConcepts 从画像 JSON 派生去重后的「话题概念」数量：兴趣 + 性格 + 重要事实 + 口头禅。
// 解析失败/空画像返回 0（不报错）。去重保证与字段顺序无关，评分确定。
func distinctProfileConcepts(profileJSON string) int {
	profileJSON = strings.TrimSpace(profileJSON)
	if profileJSON == "" || profileJSON == "{}" {
		return 0
	}
	var p Profile
	if err := json.Unmarshal([]byte(profileJSON), &p); err != nil {
		return 0
	}
	set := map[string]bool{}
	addAll := func(items []string) {
		for _, it := range items {
			if it = strings.Join(strings.Fields(it), " "); it != "" {
				set[it] = true
			}
		}
	}
	addAll(p.Interests)
	addAll(p.Personality)
	addAll(p.ImportantFacts)
	addAll(p.CommunicationStyle.FrequentPhrases)
	return len(set)
}

// sortQualityDims 供测试与展示：确保维度顺序稳定（按 key 升序）。
// 计算函数内已按固定顺序 append，这里只是对外部切片做防御性排序。
func sortQualityDims(dims []QualityDim) {
	sort.SliceStable(dims, func(i, j int) bool { return dims[i].Key < dims[j].Key })
}
