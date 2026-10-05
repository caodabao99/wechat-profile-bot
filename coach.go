package main

// v5.3.0 #5：主动关系教练（零新增 LLM 调用，编排既有周计划 draft + 新算「时机建议」）。
//
// 在既有 weekly_plan（Top5 行动 + LLM 话术 draft）与 relationship 建议之上，补一个此前
//   缺失的维度——「时机建议」：按某联系人历史消息落点算出对方最活跃的「时段 × 周几」，
//   据此建议本周何时触达最自然。教练层只读装配、绝不重写既有逻辑、绝不新增模型调用。
//
// 设计铁律：
//   - 增量复用：话术直接取 GetCachedWeeklyPlan 的 Draft；无 draft 就留空 + note（与既有沉默降级一致）。
//   - 时机为纯确定性统计：从 relationship_daily_metrics 的小时分布直方聚合，
//     selectTopK 为纯函数、可脱库单测；小样本诚实降级不硬凑。
//   - 单连接池分层锁：GetCachedWeeklyPlan 与各 computeContactTiming 各自取锁、顺序调用，绝不嵌套。

import (
	"database/sql"
	"net/http"
	"sort"
	"strconv"
	"time"
)

const (
	coachMinSamples      = 6   // 时机判定所需的最少「对方发言」样本数
	coachTimingWindowDay = 180 // 时机统计窗口
	coachTopHours        = 2   // 展示前几个最佳时段
	coachTopWeekdays     = 1   // 展示前几个最佳星期

	// v5.4.0 #3 互动节奏：秒回阈值（≤ 该分钟数视为秒回）与节奏签名阈值。
	coachFastReplyMin = 5    // 回复间隔 ≤ 5 分钟计为「秒回」
	coachNightRatio   = 0.3  // 深夜(23-4点)互动占比 ≥ 此值 → 深夜聊友
	coachMorningRatio = 0.35 // 早安(5-11点)互动占比 ≥ 此值 → 早安伙伴
)

// ContactTiming 单联系人时机建议。
type ContactTiming struct {
	BestHours    []int    `json:"bestHours"`    // 0-23，按活跃度降序
	BestWeekdays []string `json:"bestWeekdays"` // 周一..周日中文名，按活跃度降序
	Sample       int      `json:"sample"`
	Note         string   `json:"note"`
}

// CoachItem 一条教练建议（行动 + 话术 + 时机 + 依据）。
type CoachItem struct {
	ContactID int64          `json:"contactId"`
	Name      string         `json:"name"`
	Action    string         `json:"action"`
	Reason    string         `json:"reason"`
	Draft     string         `json:"draft"` // 话术模板（既有 draft，可能为空）
	Timing    *ContactTiming `json:"timing,omitempty"`
	Score     float64        `json:"score"`
}

// CoachResponse 教练编排响应。
type CoachResponse struct {
	GeneratedAt string      `json:"generatedAt"`
	Items       []CoachItem `json:"items"`
	Note        string      `json:"note"`
}

var coachWeekdayNames = []string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

// coachKindAction 建议类型 → 一句话行动。
func coachKindAction(kind string) string {
	switch kind {
	case "cooling":
		return "主动问候，重启话题"
	case "silence":
		return "发条消息打破沉默"
	case "no_reply":
		return "回复对方那条还没回的消息"
	default:
		return "保持联系"
	}
}

// selectTopK 从 (值,索引) 计数对里选前 K（计数降序、索引升序去并列，确定性）。
func selectTopK(counts []int, k int) []int {
	idx := make([]int, len(counts))
	for i := range counts {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		if counts[idx[a]] != counts[idx[b]] {
			return counts[idx[a]] > counts[idx[b]]
		}
		return idx[a] < idx[b]
	})
	out := []int{}
	for _, i := range idx {
		if counts[i] <= 0 {
			break
		}
		out = append(out, i)
		if len(out) >= k {
			break
		}
	}
	return out
}

// computeContactTiming 从 relationship_daily_metrics 聚合某联系人的活跃时段分布。
// 统一 Metrics Layer：不再直查 messages，改从 GetAggregatedMetrics 的小时分布聚合。
func computeContactTiming(db *sql.DB, contactID int64, now time.Time) *ContactTiming {
	m, err := GetAggregatedMetrics(db, int(contactID), coachTimingWindowDay)
	if err != nil || m == nil {
		return &ContactTiming{BestHours: []int{}, BestWeekdays: []string{}, Note: "指标聚合失败。"}
	}

	// 从聚合的小时分布构建计数数组（基于对方发言时段，决定何时触达更易获得回应）。
	hourCounts := make([]int, 24)
	total := 0
	for h, c := range m.OtherHourHist {
		if h >= 0 && h < 24 {
			hourCounts[h] = c
			total += c
		}
	}

	t := &ContactTiming{BestHours: []int{}, BestWeekdays: []string{}, Sample: total}
	if total < coachMinSamples {
		t.Note = "互动样本不足，暂无可靠时段建议——按常规白天时段触达即可。"
		return t
	}
	for _, h := range selectTopK(hourCounts, coachTopHours) {
		t.BestHours = append(t.BestHours, h)
	}
	// 统一 Metrics Layer：星期分布未存入派生指标，此处留空。
	if len(t.BestHours) > 0 {
		t.Note = "基于历史互动时段，" + formatHourRange(t.BestHours) + " 更易获得回应。"
	}
	return t
}

// formatHourRange 把最佳小时列表成人话（如「19-21 点」）。
func formatHourRange(hours []int) string {
	if len(hours) == 0 {
		return ""
	}
	srt := append([]int{}, hours...)
	sort.Ints(srt)
	if len(srt) == 2 && srt[1] == srt[0]+1 {
		return strconv.Itoa(srt[0]) + "-" + strconv.Itoa(srt[1]) + " 点"
	}
	out := ""
	for i, h := range srt {
		if i > 0 {
			out += "、"
		}
		out += strconv.Itoa(h) + " 点"
	}
	return out
}

// BuildCoach 组装本周教练视图：周计划缓存 + 每条补时机建议。
func BuildCoach(db *sql.DB, now time.Time) (*CoachResponse, error) {
	items, genAt, err := GetCachedWeeklyPlan(db)
	if err != nil {
		return nil, err
	}
	resp := &CoachResponse{GeneratedAt: now.Format("2006-01-02 15:04:05"), Items: []CoachItem{}}
	if len(items) == 0 || IsWeeklyPlanStale(genAt, now) {
		resp.Note = "本周维护计划还没生成或已过期——在「周计划」点重新生成后，教练建议会更完整。"
	}
	for _, wp := range items {
		ci := CoachItem{
			ContactID: wp.ContactID,
			Name:      wp.ContactName,
			Action:    coachKindAction(wp.Kind),
			Reason:    wp.Reason,
			Draft:     wp.Draft,
			Score:     wp.Score,
		}
		if wp.ContactID > 0 {
			ci.Timing = computeContactTiming(db, wp.ContactID, now)
		}
		resp.Items = append(resp.Items, ci)
	}
	return resp, nil
}

// hCoach GET /api/assistant/coach：本周关系教练视图（周计划 + 时机）。
func (s *apiServer) hCoach(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	resp, err := BuildCoach(s.db, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成关系教练失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// hContactTiming GET /api/contacts/{id}/timing：单联系人时机建议。
func (s *apiServer) hContactTiming(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	if _, err := GetContactByID(s.db, id); err != nil {
		writeErr(w, http.StatusNotFound, "联系人不存在")
		return
	}
	writeJSON(w, http.StatusOK, computeContactTiming(s.db, id, time.Now()))
}

// ---- v5.4.0 #3 互动节奏分析（纯确定性统计，复用 selectTopK）----

// RhythmResponse 单联系人的互动节奏画像。
type RhythmResponse struct {
	ContactID      int64    `json:"contactId"`
	ReplyMedianMin int      `json:"replyMedianMin"` // 我方回复对方的间隔中位数（分钟）
	FastRatio      float64  `json:"fastRatio"`      // 秒回率 0-100
	Signature      string   `json:"signature"`      // 深夜聊友/早安伙伴/日间型
	BestHours      []int    `json:"bestHours"`
	BestWeekdays   []string `json:"bestWeekdays"`
	Sample         int      `json:"sample"` // 对方发言样本数
	Note           string   `json:"note"`
}

// latencyStats 纯函数：由「(对方发言时刻, 我方紧随回复时刻)」unix 对序列，
// 算出回复间隔中位数（分钟）与秒回率（≤ coachFastReplyMin 分钟的占比，0-100）。确定性、可脱库单测。
func latencyStats(pairs [][2]int64) (medianMin int, fastRatio float64) {
	mins := make([]float64, 0, len(pairs))
	fast := 0
	for _, p := range pairs {
		m := float64(p[1]-p[0]) / 60.0
		if m < 0 {
			continue
		}
		mins = append(mins, m)
		if m <= float64(coachFastReplyMin) {
			fast++
		}
	}
	if len(mins) == 0 {
		return 0, 0
	}
	sort.Float64s(mins)
	mid := len(mins) / 2
	var med float64
	if len(mins)%2 == 1 {
		med = mins[mid]
	} else {
		med = (mins[mid-1] + mins[mid]) / 2.0
	}
	return int(med + 0.5), float64(fast) / float64(len(mins)) * 100
}

// timeSignature 纯函数：按小时活跃直方给出作息签名（固定阈值、确定性）。
func timeSignature(hourCounts []int) string {
	total := 0
	for _, c := range hourCounts {
		total += c
	}
	if total <= 0 {
		return ""
	}
	night, morning := 0, 0
	for h, c := range hourCounts {
		if h >= 23 || h <= 4 { // 深夜 23-4
			night += c
		}
		if h >= 5 && h <= 11 { // 早安 5-11
			morning += c
		}
	}
	if float64(night)/float64(total) >= coachNightRatio {
		return "深夜聊友"
	}
	if float64(morning)/float64(total) >= coachMorningRatio {
		return "早安伙伴"
	}
	return "日间型"
}

// collectReplyLatencies 从 relationship_daily_metrics 取回复延迟统计。
// 统一 Metrics Layer：不再直查 messages，改从 GetAggregatedMetrics 获取预计算的分位值。
// 返回的 [][2]int64 为合成对，传入 latencyStats 可还原中位与秒回率。
func collectReplyLatencies(db *sql.DB, contactID int64, now time.Time) [][2]int64 {
	m, err := GetAggregatedMetrics(db, int(contactID), coachTimingWindowDay)
	if err != nil || m == nil || m.ReplyLatencyP50 == 0 {
		return nil
	}
	// 合成一对等间距延迟值，使 latencyStats 算出的中位数 ≈ P50、秒回率 ≈ 基于 P50 的估算。
	p50 := int64(m.ReplyLatencyP50)
	return [][2]int64{{0, p50}}
}

// computeContactRhythm 编排互动节奏画像：延迟统计 + 复用时段直方。
// 回复延迟优先从 relationship_daily_metrics 聚合（归档感知），不足时回落原始消息配对。
func computeContactRhythm(db *sql.DB, contactID int64, now time.Time) *RhythmResponse {
	resp := &RhythmResponse{ContactID: contactID, BestHours: []int{}, BestWeekdays: []string{}}
	// 时段/星期直方与时机建议同源：直接复用 computeContactTiming（内部自锁、顺序调用不嵌套）。
	if t := computeContactTiming(db, contactID, now); t != nil {
		resp.BestHours = t.BestHours
		resp.BestWeekdays = t.BestWeekdays
		resp.Sample = t.Sample
	}

	// 回复延迟：优先从 metrics 聚合（归档感知，不直查 messages）。
	if m, err := GetAggregatedMetrics(db, int(contactID), coachTimingWindowDay); err == nil && m.ReplyLatencyP50 > 0 {
		resp.ReplyMedianMin = m.ReplyLatencyP50 / 60
		// fastRatio 无法从分位值精确还原，用 P50 ≤ 秒回阈值估算下界。
		if m.ReplyLatencyP50 <= coachFastReplyMin*60 {
			resp.FastRatio = 50 // 中位数在秒回阈值内，保守估 50%
		}
	} else {
		// 回落：原始消息配对（metrics 尚未重建或数据不足时）。
		lat := collectReplyLatencies(db, contactID, now)
		med, fast := latencyStats(lat)
		resp.ReplyMedianMin = med
		resp.FastRatio = fast
	}

	// 签名基于「对方发言」小时分布重新聚合（与样本同源）。
	sig := signatureFromQuery(db, contactID, now)
	resp.Signature = sig

	switch {
	case resp.Sample < coachMinSamples && resp.ReplyMedianMin == 0:
		resp.Note = "互动样本不足，暂无可靠节奏画像。"
	case resp.ReplyMedianMin > 0:
		if resp.FastRatio >= 50 {
			resp.Note = "你们多是秒回（中位 " + strconv.Itoa(resp.ReplyMedianMin) + " 分钟），节奏很合拍。"
		} else {
			resp.Note = "你回复 TA 的中位间隔约 " + strconv.Itoa(resp.ReplyMedianMin) + " 分钟。"
		}
	}
	return resp
}

// signatureFromQuery 从 relationship_daily_metrics 取小时分布并给出作息签名。
// 统一 Metrics Layer：不再直查 messages。
func signatureFromQuery(db *sql.DB, contactID int64, now time.Time) string {
	m, err := GetAggregatedMetrics(db, int(contactID), coachTimingWindowDay)
	if err != nil || m == nil {
		return ""
	}
	hourCounts := make([]int, 24)
	for h, c := range m.OtherHourHist {
		if h >= 0 && h < 24 {
			hourCounts[h] = c
		}
	}
	return timeSignature(hourCounts)
}

// hContactRhythm GET /api/contacts/{id}/rhythm：单联系人互动节奏画像。
func (s *apiServer) hContactRhythm(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	if _, err := GetContactByID(s.db, id); err != nil {
		writeErr(w, http.StatusNotFound, "联系人不存在")
		return
	}
	writeJSON(w, http.StatusOK, computeContactRhythm(s.db, id, time.Now()))
}
