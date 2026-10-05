package main

// v5.3.0 #5：主动关系教练（零新增 LLM 调用，编排既有周计划 draft + 新算「时机建议」）。
//
// 在既有 weekly_plan（Top5 行动 + LLM 话术 draft）与 relationship 建议之上，补一个此前
//   缺失的维度——「时机建议」：按某联系人历史消息落点算出对方最活跃的「时段 × 周几」，
//   据此建议本周何时触达最自然。教练层只读装配、绝不重写既有逻辑、绝不新增模型调用。
//
// 设计铁律：
//   - 增量复用：话术直接取 GetCachedWeeklyPlan 的 Draft；无 draft 就留空 + note（与既有沉默降级一致）。
//   - 时机为纯确定性统计：从 messages(∪archive) 中「对方发言」的时间戳聚成小时/周几直方，
//     hourWeekdayHist/selectTopK 为纯函数、可脱库单测；小样本诚实降级不硬凑。
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

// hourWeekdayHist 纯函数：把「对方发言」(小时, 周几) 序列聚成小时(0-23)与周几(strftime '%w' 0=周日)
// 计数直方。纯确定性、可脱库单测。
func hourWeekdayHist(pairs [][2]int) (hourCounts, weekdayCounts []int) {
	hourCounts = make([]int, 24)
	weekdayCounts = make([]int, 7)
	for _, p := range pairs {
		h, w := p[0], p[1]
		if h >= 0 && h < 24 {
			hourCounts[h]++
		}
		if w >= 0 && w < 7 {
			weekdayCounts[w]++
		}
	}
	return hourCounts, weekdayCounts
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

// computeContactTiming 统计某联系人「对方发言」的活跃时段与星期。
func computeContactTiming(db *sql.DB, contactID int64, now time.Time) *ContactTiming {
	since := now.AddDate(0, 0, -coachTimingWindowDay).Format(time.RFC3339)

	var pairs [][2]int

	dbMu.Lock()
	// 时段/星期一律按本地时间取：用 msg_unix + 'unixepoch','localtime'（直接 strftime('%H', msg_time)
	//   会把带时区偏移的 RFC3339 折成 UTC 小时、与本地作息不符）。两个分支口径统一。
	var q string
	var args []interface{}
	if tableExistsLocked(db, "messages_archive") {
		q = `SELECT h, w FROM (
			SELECT CAST(strftime('%H', datetime(msg_unix,'unixepoch','localtime')) AS INTEGER) h, CAST(strftime('%w', datetime(msg_unix,'unixepoch','localtime')) AS INTEGER) w
			FROM messages WHERE contact_id=? AND sender='other' AND msg_unix>0 AND msg_unix >= CAST(strftime('%s', ?) AS INTEGER)
			UNION ALL
			SELECT CAST(strftime('%H', datetime(msg_unix,'unixepoch','localtime')) AS INTEGER) h, CAST(strftime('%w', datetime(msg_unix,'unixepoch','localtime')) AS INTEGER) w
			FROM messages_archive WHERE contact_id=? AND sender='other' AND msg_unix>0 AND msg_unix >= CAST(strftime('%s', ?) AS INTEGER)
		)`
		args = []interface{}{contactID, since, contactID, since}
	} else {
		q = `SELECT CAST(strftime('%H', datetime(msg_unix,'unixepoch','localtime')) AS INTEGER), CAST(strftime('%w', datetime(msg_unix,'unixepoch','localtime')) AS INTEGER)
			FROM messages WHERE contact_id=? AND sender='other' AND msg_unix>0 AND msg_unix >= CAST(strftime('%s', ?) AS INTEGER)`
		args = []interface{}{contactID, since}
	}
	if r, err := db.Query(q, args...); err == nil {
		for r.Next() {
			var h, w sql.NullInt64
			if r.Scan(&h, &w) == nil {
				pairs = append(pairs, [2]int{int(h.Int64), int(w.Int64)})
			}
		}
		r.Close()
	}
	dbMu.Unlock()

	t := &ContactTiming{BestHours: []int{}, BestWeekdays: []string{}, Sample: len(pairs)}
	if len(pairs) < coachMinSamples {
		t.Note = "对方发言样本不足，暂无可靠时段建议——按常规白天时段触达即可。"
		return t
	}
	hourCounts, wdCounts := hourWeekdayHist(pairs)
	for _, h := range selectTopK(hourCounts, coachTopHours) {
		t.BestHours = append(t.BestHours, h)
	}
	for _, w := range selectTopK(wdCounts, coachTopWeekdays) {
		t.BestWeekdays = append(t.BestWeekdays, coachWeekdayNames[w])
	}
	if len(t.BestHours) > 0 {
		t.Note = "对方通常在 " + formatHourRange(t.BestHours) + " 更活跃。"
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
