package main

// 多周期关系报告：日报 / 周报 / 月报 / 季度报 / 半年报 / 年报。
//
// 设计要点：
//   - 全部指标由本地 SQL + Go 计算，不调用 LLM，可任意次重新生成、不产生模型费用；
//   - 与年度报告同源（复用 ContactMsgStat / PhraseStat / extractPhrases / formatReportDate
//     等构件），但把「固定 12 个月 + 上下半年」泛化成「任意时间窗 + 自适应分桶」，
//     日按小时分桶、周按天、月按天、季/半年/年按月；
//   - 纯只读、零侵入：不新增表、不改任何既有数据路径；assistant 相关表缺失时静默跳过。

import (
	"database/sql"
	"fmt"
	"html"
	"sort"
	"strings"
	"time"
)

const (
	periodTopContacts = 10
	periodMaxKeywords = 24
	periodMaxEvents   = 40
	periodScanCap     = 400000
)

// validPeriods 支持的报告周期。
var validPeriods = map[string]bool{
	"day": true, "week": true, "month": true,
	"quarter": true, "half": true, "year": true,
}

// PeriodBucket 时间序列上的一个分桶（一天里的一个小时、一周里的一天、一年里的一个月…）。
type PeriodBucket struct {
	Label  string `json:"label"`
	Mine   int    `json:"mine"`
	Theirs int    `json:"theirs"`
	Total  int    `json:"total"`
}

// PeriodEmotion 分桶粒度上的平均情绪（仅关系助手启用过情绪分析时有数据）。
type PeriodEmotion struct {
	Label string  `json:"label"`
	Score float64 `json:"score"`
	Count int     `json:"count"`
}

// PeriodReport 一次周期报告的完整结果。
type PeriodReport struct {
	Period      string `json:"period"` // day|week|month|quarter|half|year
	Title       string `json:"title"`  // 人类可读区间标题，如「2026年3月」「2026 年第2季度」
	From        string `json:"from"`   // RFC3339，含
	To          string `json:"to"`     // RFC3339，不含
	GeneratedAt string `json:"generatedAt"`

	TotalMessages  int    `json:"totalMessages"`
	MyMessages     int    `json:"myMessages"`
	TheirMessages  int    `json:"theirMessages"`
	ActiveContacts int    `json:"activeContacts"`
	ActiveDays     int    `json:"activeDays"`
	NewContacts    int    `json:"newContacts"`
	BusiestLabel   string `json:"busiestBucketLabel"`
	BusiestCount   int    `json:"busiestBucketCount"`

	Buckets   []PeriodBucket   `json:"buckets"`
	Top       []ContactMsgStat `json:"topContacts"`
	Keywords  []PhraseStat     `json:"keywords"`
	Events    []ReportEvent    `json:"events"`
	Emotions  []PeriodEmotion  `json:"emotions"`
	EmotionOn bool             `json:"emotionAvailable"`
	Truncated bool             `json:"truncated"`
}

// periodWindow 解析出的时间窗与分桶元数据。
type periodWindow struct {
	From, To   time.Time
	Title      string
	NumBuckets int
	// bucketOf 把窗口内的时间点映射到 [0,NumBuckets) 的分桶下标。
	bucketOf func(time.Time) int
	// labels 每个分桶的展示名。
	labels []string
}

// resolvePeriodWindow 依据周期类型与锚点日期，算出自然区间、标题、分桶粒度与标签。
// anchor 的“日期部分”决定归属：日=当天、周=ISO 周（周一起）、月=当月、季=当季、半年=当半年、年=当年。
func resolvePeriodWindow(period string, anchor time.Time) (periodWindow, bool) {
	anchor = anchor.In(time.Local)
	y, m, d := anchor.Year(), int(anchor.Month()), anchor.Day()
	dayStart := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.Local)
	weekday := int(anchor.Weekday()) // 0=周日 … 6=周六

	switch period {
	case "day":
		from := dayStart
		to := from.AddDate(0, 0, 1)
		labels := make([]string, 24)
		for i := range labels {
			labels[i] = fmt.Sprintf("%02d时", i)
		}
		return periodWindow{
			From: from, To: to, Title: from.Format("2006年1月2日"),
			NumBuckets: 24, bucketOf: func(t time.Time) int { return t.Hour() }, labels: labels,
		}, true

	case "week":
		// 周一起算：周一=1 … 周日=0→按 7 计
		offset := weekday
		if offset == 0 {
			offset = 7
		}
		from := dayStart.AddDate(0, 0, -(offset - 1))
		to := from.AddDate(0, 0, 7)
		wdCN := []string{"周一", "周二", "周三", "周四", "周五", "周六", "周日"}
		labels := make([]string, 7)
		for i := range labels {
			labels[i] = wdCN[i]
		}
		return periodWindow{
			From: from, To: to, Title: fmt.Sprintf("%s ~ %s", from.Format("2006-01-02"), to.AddDate(0, 0, -1).Format("01-02")),
			NumBuckets: 7,
			bucketOf: func(t time.Time) int {
				idx := int(t.Sub(from).Hours() / 24)
				if idx < 0 {
					idx = 0
				}
				if idx > 6 {
					idx = 6
				}
				return idx
			}, labels: labels,
		}, true

	case "month":
		from := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, time.Local)
		to := from.AddDate(0, 1, 0)
		days := int(to.Sub(from).Hours() / 24)
		labels := make([]string, days)
		for i := range labels {
			labels[i] = fmt.Sprintf("%d日", i+1)
		}
		return periodWindow{
			From: from, To: to, Title: from.Format("2006年1月"),
			NumBuckets: days,
			bucketOf: func(t time.Time) int {
				idx := t.Day() - 1
				if idx < 0 {
					idx = 0
				}
				if idx >= days {
					idx = days - 1
				}
				return idx
			}, labels: labels,
		}, true

	case "quarter":
		q := (m - 1) / 3 // 0..3
		startMonth := q*3 + 1
		from := time.Date(y, time.Month(startMonth), 1, 0, 0, 0, 0, time.Local)
		to := from.AddDate(0, 3, 0)
		return periodWindow{
			From: from, To: to, Title: fmt.Sprintf("%d 年第%d季度", y, q+1),
			NumBuckets: 3,
			bucketOf: func(t time.Time) int {
				idx := int(t.Month()) - startMonth
				if idx < 0 {
					idx = 0
				}
				if idx > 2 {
					idx = 2
				}
				return idx
			},
			labels: []string{fmt.Sprintf("%d月", startMonth), fmt.Sprintf("%d月", startMonth+1), fmt.Sprintf("%d月", startMonth+2)},
		}, true

	case "half":
		startMonth := 1
		halfTitle := "上半年"
		if m >= 7 {
			startMonth = 7
			halfTitle = "下半年"
		}
		from := time.Date(y, time.Month(startMonth), 1, 0, 0, 0, 0, time.Local)
		to := from.AddDate(0, 6, 0)
		labels := make([]string, 6)
		for i := range labels {
			labels[i] = fmt.Sprintf("%d月", startMonth+i)
		}
		return periodWindow{
			From: from, To: to, Title: fmt.Sprintf("%d 年%s", y, halfTitle),
			NumBuckets: 6,
			bucketOf: func(t time.Time) int {
				idx := int(t.Month()) - startMonth
				if idx < 0 {
					idx = 0
				}
				if idx > 5 {
					idx = 5
				}
				return idx
			}, labels: labels,
		}, true

	case "year":
		from := time.Date(y, 1, 1, 0, 0, 0, 0, time.Local)
		to := from.AddDate(1, 0, 0)
		labels := make([]string, 12)
		for i := range labels {
			labels[i] = fmt.Sprintf("%d月", i+1)
		}
		return periodWindow{
			From: from, To: to, Title: fmt.Sprintf("%d 年", y),
			NumBuckets: 12,
			bucketOf:   func(t time.Time) int { return int(t.Month()) - 1 }, labels: labels,
		}, true
	}
	return periodWindow{}, false
}

// BuildPeriodReport 生成指定周期、锚点日期所属区间的关系报告。
// period 非法时返回 error；anchor 为零值时按当前时间。
func BuildPeriodReport(db *sql.DB, period string, anchor time.Time) (*PeriodReport, error) {
	if !validPeriods[period] {
		return nil, fmt.Errorf("不支持的报告周期: %s", period)
	}
	if anchor.IsZero() {
		anchor = time.Now()
	}
	w, ok := resolvePeriodWindow(period, anchor)
	if !ok {
		return nil, fmt.Errorf("无法解析报告区间: %s", period)
	}
	fromStr, toStr := w.From.Format(time.RFC3339), w.To.Format(time.RFC3339)

	rep := &PeriodReport{
		Period:      period,
		Title:       w.Title,
		From:        fromStr,
		To:          toStr,
		GeneratedAt: time.Now().Format("2006-01-02 15:04:05"),
		Buckets:     make([]PeriodBucket, w.NumBuckets),
		Top:         []ContactMsgStat{},
		Keywords:    []PhraseStat{},
		Events:      []ReportEvent{},
		Emotions:    []PeriodEmotion{},
	}
	for i := range rep.Buckets {
		rep.Buckets[i] = PeriodBucket{Label: w.labels[i]}
	}

	type rawMsg struct {
		contactID int64
		sender    string
		ts        int64
	}
	var msgs []rawMsg

	dbMu.Lock()
	rows, err := db.Query(`
		SELECT contact_id, sender, strftime('%s', msg_time)
		FROM messages
		WHERE msg_time IS NOT NULL AND msg_time != ''
		  AND strftime('%s', msg_time) >= strftime('%s', ?)
		  AND strftime('%s', msg_time) <  strftime('%s', ?)
		ORDER BY strftime('%s', msg_time) DESC, id DESC
		LIMIT ?`, fromStr, toStr, periodScanCap+1)
	if err != nil {
		dbMu.Unlock()
		return nil, err
	}
	for rows.Next() {
		var m rawMsg
		if err := rows.Scan(&m.contactID, &m.sender, &m.ts); err != nil {
			rows.Close()
			dbMu.Unlock()
			return nil, err
		}
		msgs = append(msgs, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		dbMu.Unlock()
		return nil, err
	}

	names := map[int64]string{}
	if nrs, nerr := db.Query(`SELECT id, COALESCE(remark, ''), name FROM contacts`); nerr == nil {
		for nrs.Next() {
			var id int64
			var remark, name string
			if err := nrs.Scan(&id, &remark, &name); err != nil {
				break
			}
			if strings.TrimSpace(remark) != "" {
				names[id] = remark + "（" + name + "）"
			} else {
				names[id] = name
			}
		}
		nrs.Close()
	}

	_ = db.QueryRow(`SELECT COUNT(*) FROM contacts
		 WHERE created_at IS NOT NULL AND created_at != ''
		   AND strftime('%s', created_at) >= strftime('%s', ?)
		   AND strftime('%s', created_at) <  strftime('%s', ?)`,
		fromStr, toStr).Scan(&rep.NewContacts)

	// 情绪（表可能不存在——关系助手未启用——静默跳过）。created_at 为 UTC，须 localtime 换算。
	emotionByBucket := map[int][]int{}
	if ers, eerr := db.Query(`
			SELECT created_at, score FROM assistant_emotions
			WHERE created_at IS NOT NULL AND created_at != ''
			  AND strftime('%s', created_at, 'localtime') >= strftime('%s', ?)
			  AND strftime('%s', created_at, 'localtime') <  strftime('%s', ?)`,
		fromStr, toStr); eerr == nil {
		for ers.Next() {
			var created string
			var score int
			if err := ers.Scan(&created, &score); err != nil {
				break
			}
			if t, ok := parseTimeLoose(created); ok {
				if ti := w.bucketOf(t.In(time.Local)); ti >= 0 && ti < w.NumBuckets {
					emotionByBucket[ti] = append(emotionByBucket[ti], score)
				}
			}
		}
		ers.Close()
	}

	var evts []ReportEvent
	collectEvents(db, fromStr, toStr, &evts)

	var texts []string
	if trs, terr := db.Query(`
			SELECT content FROM messages
			WHERE msg_time IS NOT NULL AND msg_time != ''
			  AND strftime('%s', msg_time) >= strftime('%s', ?)
			  AND strftime('%s', msg_time) <  strftime('%s', ?)
			  AND length(content) BETWEEN 4 AND 200
			ORDER BY strftime('%s', msg_time) DESC
			LIMIT 30000`, fromStr, toStr); terr == nil {
		for trs.Next() {
			var c string
			if err := trs.Scan(&c); err != nil {
				break
			}
			texts = append(texts, c)
		}
		trs.Close()
	}
	dbMu.Unlock()

	if len(msgs) > periodScanCap {
		rep.Truncated = true
		msgs = msgs[:periodScanCap]
	}

	rep.TotalMessages = len(msgs)
	perContact := map[int64]*ContactMsgStat{}
	dayCount := map[string]int{}

	for _, m := range msgs {
		if m.sender == "me" {
			rep.MyMessages++
		} else {
			rep.TheirMessages++
		}
		t := time.Unix(m.ts, 0).In(time.Local)
		if ti := w.bucketOf(t); ti >= 0 && ti < w.NumBuckets {
			b := rep.Buckets[ti]
			if m.sender == "me" {
				b.Mine++
			} else {
				b.Theirs++
			}
			b.Total++
			rep.Buckets[ti] = b
		}

		day := t.Format("2006-01-02")
		dayCount[day]++

		cs := perContact[m.contactID]
		if cs == nil {
			cs = &ContactMsgStat{ContactID: m.contactID, Name: names[m.contactID]}
			if cs.Name == "" {
				cs.Name = "未知联系人"
			}
			perContact[m.contactID] = cs
		}
		cs.Total++
		if m.sender == "me" {
			cs.Mine++
		} else {
			cs.Theirs++
		}
		cs.LastMsgTime = day
	}

	rep.ActiveContacts = len(perContact)
	rep.ActiveDays = len(dayCount)
	for _, b := range rep.Buckets {
		if b.Total > rep.BusiestCount {
			rep.BusiestCount = b.Total
			rep.BusiestLabel = b.Label
		}
	}

	all := make([]ContactMsgStat, 0, len(perContact))
	for _, cs := range perContact {
		all = append(all, *cs)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Total != all[j].Total {
			return all[i].Total > all[j].Total
		}
		return all[i].ContactID < all[j].ContactID
	})
	if len(all) > periodTopContacts {
		rep.Top = all[:periodTopContacts]
	} else {
		rep.Top = all
	}

	for i := 0; i < w.NumBuckets; i++ {
		scores := emotionByBucket[i]
		if len(scores) == 0 {
			continue
		}
		rep.EmotionOn = true
		var sum int
		for _, s := range scores {
			sum += s
		}
		rep.Emotions = append(rep.Emotions, PeriodEmotion{
			Label: w.labels[i], Score: float64(sum) / float64(len(scores)), Count: len(scores),
		})
	}

	rep.Keywords = extractPhrases(texts, periodMaxKeywords)

	sort.SliceStable(evts, func(i, j int) bool { return evts[i].Date > evts[j].Date })
	if len(evts) > periodMaxEvents {
		evts = evts[:periodMaxEvents]
	}
	rep.Events = evts
	if rep.Emotions == nil {
		rep.Emotions = []PeriodEmotion{}
	}
	return rep, nil
}

// collectEvents 汇总区间内的大事记：画像更新、合并、手动记录的事件。
// 三段查询各自容错（表缺失/出错只跳过该来源），不阻断报告生成。
func collectEvents(db *sql.DB, fromStr, toStr string, out *[]ReportEvent) {
	if hrs, err := db.Query(`
			SELECT COALESCE(h.change_summary, ''), COALESCE(h.created_at, ''), COALESCE(c.remark, ''), COALESCE(c.name, '')
			FROM profile_history h LEFT JOIN contacts c ON c.id = h.contact_id
			WHERE h.created_at IS NOT NULL AND h.created_at != ''
			  AND strftime('%s', h.created_at) >= strftime('%s', ?)
			  AND strftime('%s', h.created_at) <  strftime('%s', ?)
			ORDER BY strftime('%s', h.created_at) DESC LIMIT 60`, fromStr, toStr); err == nil {
		for hrs.Next() {
			var summary, createdAt, remark, name string
			if err := hrs.Scan(&summary, &createdAt, &remark, &name); err != nil {
				break
			}
			who := name
			if strings.TrimSpace(remark) != "" {
				who = remark
			}
			if strings.TrimSpace(summary) == "" {
				summary = "画像更新"
			}
			*out = append(*out, ReportEvent{
				Kind: "profile", Title: summary, Detail: who, Date: formatReportDate(createdAt),
			})
		}
		hrs.Close()
	}
	if mrs, err := db.Query(`
			SELECT COALESCE(source_name, ''), COALESCE(target_name, ''), COALESCE(created_at, ''), COALESCE(undone_at, '')
			FROM merge_log
			WHERE created_at IS NOT NULL AND created_at != ''
			  AND strftime('%s', created_at) >= strftime('%s', ?)
			  AND strftime('%s', created_at) <  strftime('%s', ?)
			ORDER BY strftime('%s', created_at) DESC LIMIT 30`, fromStr, toStr); err == nil {
		for mrs.Next() {
			var src, tgt, createdAt, undone string
			if err := mrs.Scan(&src, &tgt, &createdAt, &undone); err != nil {
				break
			}
			detail := fmt.Sprintf("「%s」并入「%s」", src, tgt)
			if strings.TrimSpace(undone) != "" {
				detail += "（已撤销）"
			}
			*out = append(*out, ReportEvent{
				Kind: "merge", Title: "合并联系人", Detail: detail, Date: formatReportDate(createdAt),
			})
		}
		mrs.Close()
	}
	if crs, err := db.Query(`
			SELECT kind, title, COALESCE(detail, ''), event_time FROM contact_events
			WHERE event_time IS NOT NULL AND event_time != ''
			  AND strftime('%s', event_time) >= strftime('%s', ?)
			  AND strftime('%s', event_time) <  strftime('%s', ?)
			ORDER BY strftime('%s', event_time) DESC LIMIT 50`, fromStr, toStr); err == nil {
		for crs.Next() {
			var kind, title, detail, et string
			if err := crs.Scan(&kind, &title, &detail, &et); err != nil {
				break
			}
			*out = append(*out, ReportEvent{Kind: "record", Title: title, Detail: detail, Date: formatReportDate(et)})
		}
		crs.Close()
	}
}

// periodTitleCN 周期中文名，用于标题与分享页。
func periodTitleCN(period string) string {
	switch period {
	case "day":
		return "日报"
	case "week":
		return "周报"
	case "month":
		return "月报"
	case "quarter":
		return "季度报"
	case "half":
		return "半年报"
	case "year":
		return "年报"
	}
	return "报告"
}

// RenderPeriodHTML 把周期报告渲染成一个自包含的 HTML 长页（全内联样式、无外链），
// 供网页端“另存/分享”或截图，风格与年度报告一致。
func RenderPeriodHTML(r *PeriodReport) string {
	e := html.EscapeString
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(fmt.Sprintf(`<title>%s · %s</title>`, e(r.Title), e(periodTitleCN(r.Period))))
	b.WriteString(`<style>`)
	b.WriteString(`body{margin:0;background:#f2f4f3;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;color:#1f2937}`)
	b.WriteString(`.wrap{max-width:720px;margin:0 auto;padding:24px 16px 48px}`)
	b.WriteString(`.hero{background:linear-gradient(135deg,#07c160,#0aa35a);color:#fff;border-radius:20px;padding:32px 28px;text-align:center;box-shadow:0 12px 30px rgba(7,193,96,.25)}`)
	b.WriteString(`.hero h1{margin:0;font-size:26px;letter-spacing:1px}`)
	b.WriteString(`.hero p{margin:8px 0 0;opacity:.9;font-size:13px}`)
	b.WriteString(`.grid{display:grid;grid-template-columns:repeat(3,1fr);gap:12px;margin:20px 0}`)
	b.WriteString(`.kpi{background:#fff;border-radius:14px;padding:16px 12px;text-align:center;box-shadow:0 2px 10px rgba(0,0,0,.05)}`)
	b.WriteString(`.kpi .n{font-size:24px;font-weight:700;color:#059669}`)
	b.WriteString(`.kpi .l{font-size:12px;color:#6b7280;margin-top:4px}`)
	b.WriteString(`.card{background:#fff;border-radius:16px;padding:20px;margin:16px 0;box-shadow:0 2px 10px rgba(0,0,0,.05)}`)
	b.WriteString(`.card h2{margin:0 0 14px;font-size:17px;border-left:4px solid #07c160;padding-left:10px}`)
	b.WriteString(`.bars{display:flex;align-items:flex-end;gap:4px;height:130px;overflow-x:auto}`)
	b.WriteString(`.bar{flex:1;min-width:14px;display:flex;flex-direction:column;justify-content:flex-end;align-items:center;gap:4px}`)
	b.WriteString(`.bar i{display:block;width:100%;background:#07c160;border-radius:4px 4px 0 0;min-height:2px}`)
	b.WriteString(`.bar span{font-size:9px;color:#9ca3af;white-space:nowrap}`)
	b.WriteString(`table{width:100%;border-collapse:collapse;font-size:13px}`)
	b.WriteString(`th,td{text-align:left;padding:8px 6px;border-bottom:1px solid #f1f3f2}`)
	b.WriteString(`th{color:#6b7280;font-weight:500;font-size:12px}`)
	b.WriteString(`.tag{display:inline-block;background:#ecfdf5;color:#047857;border-radius:999px;padding:4px 10px;margin:3px;font-size:12px}`)
	b.WriteString(`.ev{display:flex;gap:12px;padding:8px 0;border-bottom:1px dashed #eef0ef}`)
	b.WriteString(`.ev .d{color:#9ca3af;font-size:12px;min-width:78px}`)
	b.WriteString(`.foot{text-align:center;color:#9ca3af;font-size:12px;margin-top:24px}`)
	b.WriteString(`@media(max-width:640px){.grid{grid-template-columns:repeat(2,1fr)}}`)
	b.WriteString(`</style></head><body><div class="wrap">`)

	b.WriteString(fmt.Sprintf(`<div class="hero"><h1>%s · %s</h1><p>微信聊天画像助手 · %s ~ %s · 生成于 %s</p></div>`,
		e(r.Title), e(periodTitleCN(r.Period)), e(r.From[:10]), e(r.To[:10]), e(r.GeneratedAt)))

	kpis := []struct{ n, l string }{
		{fmt.Sprintf("%d", r.TotalMessages), "消息总数"},
		{fmt.Sprintf("%d", r.ActiveContacts), "聊过的人"},
		{fmt.Sprintf("%d", r.ActiveDays), "有记录的天数"},
		{fmt.Sprintf("%d", r.MyMessages), "我发出"},
		{fmt.Sprintf("%d", r.TheirMessages), "收到"},
		{fmt.Sprintf("%d", r.NewContacts), "新认识的人"},
	}
	b.WriteString(`<div class="grid">`)
	for _, k := range kpis {
		b.WriteString(fmt.Sprintf(`<div class="kpi"><div class="n">%s</div><div class="l">%s</div></div>`, e(k.n), e(k.l)))
	}
	b.WriteString(`</div>`)

	maxB := 1
	for _, bk := range r.Buckets {
		if bk.Total > maxB {
			maxB = bk.Total
		}
	}
	b.WriteString(`<div class="card"><h2>时间分布</h2><div class="bars">`)
	for _, bk := range r.Buckets {
		h := bk.Total * 100 / maxB
		b.WriteString(fmt.Sprintf(`<div class="bar" title="%s 共%d条"><i style="height:%d%%"></i><span>%s</span></div>`,
			e(bk.Label), bk.Total, h, e(shortBucketLabel(bk.Label))))
	}
	b.WriteString(`</div></div>`)

	if len(r.Top) > 0 {
		b.WriteString(`<div class="card"><h2>聊得最多的人</h2><table><tr><th>#</th><th>联系人</th><th>消息</th><th>我发</th><th>收到</th></tr>`)
		for i, c := range r.Top {
			b.WriteString(fmt.Sprintf(`<tr><td>%d</td><td>%s</td><td>%d</td><td>%d</td><td>%d</td></tr>`,
				i+1, e(c.Name), c.Total, c.Mine, c.Theirs))
		}
		b.WriteString(`</table></div>`)
	}

	if len(r.Keywords) > 0 {
		b.WriteString(`<div class="card"><h2>这段时间聊了什么</h2><div>`)
		for _, k := range r.Keywords {
			b.WriteString(fmt.Sprintf(`<span class="tag">%s <b>%d</b></span>`, e(k.Phrase), k.Count))
		}
		b.WriteString(`</div></div>`)
	}

	if len(r.Events) > 0 {
		b.WriteString(`<div class="card"><h2>大事记</h2>`)
		for _, ev := range r.Events {
			detail := ""
			if strings.TrimSpace(ev.Detail) != "" {
				detail = " · " + e(ev.Detail)
			}
			b.WriteString(fmt.Sprintf(`<div class="ev"><div class="d">%s</div><div>%s%s</div></div>`,
				e(ev.Date), e(ev.Title), detail))
		}
		b.WriteString(`</div>`)
	}

	b.WriteString(fmt.Sprintf(`<div class="foot">wechat-profile-bot · %s</div>`, e(periodTitleCN(r.Period))))
	b.WriteString(`</div></body></html>`)
	return b.String()
}

// shortBucketLabel 从分桶标签里取一个适合竖排显示的核心字（"15时"→"15"、"3月"→"3"、"周一"→"一"）。
func shortBucketLabel(label string) string {
	r := []rune(label)
	var digits strings.Builder
	for _, c := range r {
		if c >= '0' && c <= '9' {
			digits.WriteRune(c)
		}
	}
	if digits.Len() > 0 {
		return digits.String()
	}
	if len(r) >= 2 && r[0] == '周' {
		return string(r[1])
	}
	return label
}
