package main

// 年度关系报告（增值功能，纯只读，不调用 LLM）。
//
// 全部指标都由本地 SQL + Go 算出来，可以任意次重新生成，不产生模型费用。
// 结果既能以 JSON 返回给网页渲染，也能直接输出一个自包含的 HTML 长页用于分享/截图。

import (
	"database/sql"
	"fmt"
	"html"
	"sort"
	"strings"
	"time"
)

const (
	reportTopContacts = 10
	reportMaxKeywords = 24
	reportMaxEvents   = 40
	reportScanCap     = 400000
)

// MonthBar 某个月的消息量
type MonthBar struct {
	Month  int `json:"month"` // 1~12
	Mine   int `json:"mine"`
	Theirs int `json:"theirs"`
	Total  int `json:"total"`
}

// EmotionPoint 某个月的平均情绪分
type EmotionPoint struct {
	Month int     `json:"month"`
	Score float64 `json:"score"`
	Count int     `json:"count"`
}

// IntimacyChange 单个联系人上半年 vs 下半年的消息量变化
type IntimacyChange struct {
	ContactID  int64  `json:"contactId"`
	Name       string `json:"name"`
	FirstHalf  int    `json:"firstHalf"`
	SecondHalf int    `json:"secondHalf"`
	Delta      int    `json:"delta"`    // 下半年 - 上半年
	Trend      string `json:"trend"`    // up / down / flat
	FirstMsg   string `json:"firstMsg"` // 当年第一次聊天的日期
	LastMsg    string `json:"lastMsg"`
}

// ReportEvent 大事记里的一条
type ReportEvent struct {
	Kind   string `json:"kind"` // profile / merge / record / milestone
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Date   string `json:"date"`
}

// AnnualReport 年度报告
type AnnualReport struct {
	Year        int    `json:"year"`
	GeneratedAt string `json:"generatedAt"`

	TotalMessages  int    `json:"totalMessages"`
	MyMessages     int    `json:"myMessages"`
	TheirMessages  int    `json:"theirMessages"`
	ActiveContacts int    `json:"activeContacts"`
	ActiveDays     int    `json:"activeDays"`
	BusiestDay     string `json:"busiestDay"`
	BusiestDayCnt  int    `json:"busiestDayCount"`
	BusiestMonth   int    `json:"busiestMonth"`
	LongestSilence int    `json:"longestSilenceDays"` // 当年最长的沉默间隔
	SilenceBefore  string `json:"silenceBefore"`      // 沉默结束的那一天
	SilenceAfter   string `json:"silenceAfter"`
	NewContacts    int    `json:"newContacts"` // 当年新增的联系人

	Months    []MonthBar       `json:"months"`
	Top       []ContactMsgStat `json:"topContacts"`
	Emotions  []EmotionPoint   `json:"emotions"`
	Intimacy  []IntimacyChange `json:"intimacy"`
	Keywords  []PhraseStat     `json:"keywords"`
	Events    []ReportEvent    `json:"events"`
	EmotionOn bool             `json:"emotionAvailable"` // 是否启用过情绪分析
	Truncated bool             `json:"truncated"`
}

// BuildAnnualReport 生成指定年份的关系报告。
func BuildAnnualReport(db *sql.DB, year int) (*AnnualReport, error) {
	if year < 2000 || year > 2100 {
		year = time.Now().Year()
	}
	now := time.Now()
	from := time.Date(year, 1, 1, 0, 0, 0, 0, time.Local)
	to := time.Date(year+1, 1, 1, 0, 0, 0, 0, time.Local)
	fromStr, toStr := from.Format(time.RFC3339), to.Format(time.RFC3339)

	rep := &AnnualReport{
		Year:        year,
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
		Months:      make([]MonthBar, 12),
		Top:         []ContactMsgStat{},
		Emotions:    []EmotionPoint{},
		Intimacy:    []IntimacyChange{},
		Keywords:    []PhraseStat{},
		Events:      []ReportEvent{},
	}
	for i := range rep.Months {
		rep.Months[i] = MonthBar{Month: i + 1}
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
		LIMIT ?`, fromStr, toStr, reportScanCap+1)
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
	if nrs, nerr := db.Query(
		`SELECT id, COALESCE(remark, ''), name FROM contacts`); nerr == nil {
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

	// 当年新增联系人
	_ = db.QueryRow(
		`SELECT COUNT(*) FROM contacts
		 WHERE created_at IS NOT NULL AND created_at != ''
		   AND strftime('%s', created_at) >= strftime('%s', ?)
		   AND strftime('%s', created_at) <  strftime('%s', ?)`,
		fromStr, toStr).Scan(&rep.NewContacts)

	// 情绪曲线（关系助手未启用时表可能不存在，静默跳过）
	// assistant_emotions.created_at 走的是 DATETIME DEFAULT CURRENT_TIMESTAMP，
	// 存的是不带偏移的 UTC；报表其它统计全按本地时间，这里必须 localtime 换算，
	// 否则本地 0~8 点产生的情绪记录会归到前一天甚至整年被年份条件筛掉。
	emotionByMonth := map[int][]int{}
	if ers, eerr := db.Query(`
			SELECT strftime('%m', created_at, 'localtime'), score FROM assistant_emotions
			WHERE created_at IS NOT NULL AND created_at != ''
			  AND CAST(strftime('%Y', created_at, 'localtime') AS INTEGER) = ?`, year); eerr == nil {
		for ers.Next() {
			var mon string
			var score int
			if err := ers.Scan(&mon, &score); err != nil {
				break
			}
			m := atoiSafe(mon)
			if m >= 1 && m <= 12 {
				emotionByMonth[m] = append(emotionByMonth[m], score)
			}
		}
		ers.Close()
	}

	// 大事记素材
	var evts []ReportEvent
	if hrs, herr := db.Query(`
			SELECT COALESCE(h.change_summary, ''), COALESCE(h.created_at, ''), COALESCE(c.remark, ''), COALESCE(c.name, '')
			FROM profile_history h LEFT JOIN contacts c ON c.id = h.contact_id
			WHERE h.created_at IS NOT NULL AND h.created_at != ''
			  AND strftime('%s', h.created_at) >= strftime('%s', ?)
			  AND strftime('%s', h.created_at) <  strftime('%s', ?)
			ORDER BY strftime('%s', h.created_at) DESC LIMIT 60`, fromStr, toStr); herr == nil {
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
			evts = append(evts, ReportEvent{
				Kind: "profile", Title: summary, Detail: who,
				Date: formatReportDate(createdAt),
			})
		}
		hrs.Close()
	}
	if mrs, merr := db.Query(`
			SELECT COALESCE(source_name, ''), COALESCE(target_name, ''), COALESCE(created_at, ''), COALESCE(undone_at, '')
			FROM merge_log
			WHERE created_at IS NOT NULL AND created_at != ''
			  AND strftime('%s', created_at) >= strftime('%s', ?)
			  AND strftime('%s', created_at) <  strftime('%s', ?)
			ORDER BY strftime('%s', created_at) DESC LIMIT 30`, fromStr, toStr); merr == nil {
		for mrs.Next() {
			var src, tgt, createdAt, undone string
			if err := mrs.Scan(&src, &tgt, &createdAt, &undone); err != nil {
				break
			}
			detail := fmt.Sprintf("「%s」并入「%s」", src, tgt)
			if strings.TrimSpace(undone) != "" {
				detail += "（已撤销）"
			}
			evts = append(evts, ReportEvent{
				Kind: "merge", Title: "合并联系人", Detail: detail,
				Date: formatReportDate(createdAt),
			})
		}
		mrs.Close()
	}
	if crs, cerr := db.Query(`
			SELECT kind, title, COALESCE(detail, ''), event_time FROM contact_events
			WHERE event_time IS NOT NULL AND event_time != ''
			  AND strftime('%s', event_time) >= strftime('%s', ?)
			  AND strftime('%s', event_time) <  strftime('%s', ?)
			ORDER BY strftime('%s', event_time) DESC LIMIT 50`, fromStr, toStr); cerr == nil {
		for crs.Next() {
			var kind, title, detail, et string
			if err := crs.Scan(&kind, &title, &detail, &et); err != nil {
				break
			}
			evts = append(evts, ReportEvent{
				Kind: "record", Title: title, Detail: detail, Date: formatReportDate(et),
			})
		}
		crs.Close()
	}

	// 关键词素材（当年消息文本，量可能很大，只取一部分）
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

	if len(msgs) > reportScanCap {
		rep.Truncated = true
		msgs = msgs[:reportScanCap]
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}

	rep.TotalMessages = len(msgs)
	perContact := map[int64]*ContactMsgStat{}
	half := map[int64]*IntimacyChange{}
	dayCount := map[string]int{}
	midYear := from.AddDate(0, 6, 0).Unix()

	for _, m := range msgs {
		if m.sender == "me" {
			rep.MyMessages++
		} else {
			rep.TheirMessages++
		}
		t := time.Unix(m.ts, 0)
		mb := rep.Months[t.Month()-1]
		if m.sender == "me" {
			mb.Mine++
		} else {
			mb.Theirs++
		}
		mb.Total++
		rep.Months[t.Month()-1] = mb

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

		ic := half[m.contactID]
		if ic == nil {
			ic = &IntimacyChange{ContactID: m.contactID, Name: cs.Name}
			half[m.contactID] = ic
		}
		if m.ts < midYear {
			ic.FirstHalf++
		} else {
			ic.SecondHalf++
		}
		if ic.FirstMsg == "" {
			ic.FirstMsg = day
		}
		ic.LastMsg = day
	}

	rep.ActiveContacts = len(perContact)
	rep.ActiveDays = len(dayCount)
	for _, mb := range rep.Months {
		if mb.Total > 0 && (rep.BusiestMonth == 0 || mb.Total > rep.Months[rep.BusiestMonth-1].Total) {
			rep.BusiestMonth = mb.Month
		}
	}
	days := make([]string, 0, len(dayCount))
	for d := range dayCount {
		days = append(days, d)
	}
	sort.Strings(days)
	for _, d := range days {
		if dayCount[d] > rep.BusiestDayCnt {
			rep.BusiestDayCnt = dayCount[d]
			rep.BusiestDay = d
		}
	}
	// 最长沉默间隔（相邻有聊天记录的日子之间）
	for i := 1; i < len(days); i++ {
		p, err1 := time.ParseInLocation("2006-01-02", days[i-1], time.Local)
		c, err2 := time.ParseInLocation("2006-01-02", days[i], time.Local)
		if err1 != nil || err2 != nil {
			continue
		}
		gap := int(c.Sub(p).Hours() / 24)
		if gap > rep.LongestSilence {
			rep.LongestSilence = gap
			rep.SilenceAfter = days[i-1]
			rep.SilenceBefore = days[i]
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
	if len(all) > reportTopContacts {
		rep.Top = all[:reportTopContacts]
	} else {
		rep.Top = all
	}

	// 亲密度变化只取聊得最多的那批人，按变化幅度排序
	topIDs := map[int64]bool{}
	for _, cs := range rep.Top {
		topIDs[cs.ContactID] = true
	}
	for cid := range half {
		if !topIDs[cid] {
			continue
		}
		ic := *half[cid]
		ic.Delta = ic.SecondHalf - ic.FirstHalf
		switch {
		case ic.FirstHalf == 0 && ic.SecondHalf == 0:
			ic.Trend = "flat"
		case ic.Delta > 0 && float64(ic.Delta) > float64(maxInt(ic.FirstHalf, 1))*0.15:
			ic.Trend = "up"
		case ic.Delta < 0 && float64(-ic.Delta) > float64(maxInt(ic.SecondHalf, 1))*0.15:
			ic.Trend = "down"
		default:
			ic.Trend = "flat"
		}
		rep.Intimacy = append(rep.Intimacy, ic)
	}
	sort.Slice(rep.Intimacy, func(i, j int) bool {
		di, dj := rep.Intimacy[i].Delta, rep.Intimacy[j].Delta
		if di != dj {
			return di > dj
		}
		return rep.Intimacy[i].FirstHalf+rep.Intimacy[i].SecondHalf >
			rep.Intimacy[j].FirstHalf+rep.Intimacy[j].SecondHalf
	})

	for m := 1; m <= 12; m++ {
		scores := emotionByMonth[m]
		if len(scores) == 0 {
			continue
		}
		rep.EmotionOn = true
		var sum int
		for _, s := range scores {
			sum += s
		}
		rep.Emotions = append(rep.Emotions, EmotionPoint{
			Month: m, Score: float64(sum) / float64(len(scores)), Count: len(scores),
		})
	}

	rep.Keywords = extractPhrases(texts, reportMaxKeywords)

	sort.SliceStable(evts, func(i, j int) bool { return evts[i].Date > evts[j].Date })
	if len(evts) > reportMaxEvents {
		evts = evts[:reportMaxEvents]
	}
	rep.Events = evts
	// 空结果一律给 [] 而不是 nil，前端才不会读到 null.length
	if rep.Emotions == nil {
		rep.Emotions = []EmotionPoint{}
	}
	if rep.Intimacy == nil {
		rep.Intimacy = []IntimacyChange{}
	}
	if rep.Events == nil {
		rep.Events = []ReportEvent{}
	}
	return rep, nil
}

func atoiSafe(s string) int {
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// formatReportDate 把库里各种时间格式统一成 YYYY-MM-DD
func formatReportDate(s string) string {
	if t, ok := parseTimeLoose(strings.TrimSpace(s)); ok {
		return t.Format("2006-01-02")
	}
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// ---------- HTML 长页（可直接分享/截图） ----------

// RenderReportHTML 把年度报告渲染成一个自包含的 HTML 页面（全内联样式，无外链）
func RenderReportHTML(r *AnnualReport) string {
	e := html.EscapeString
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(fmt.Sprintf(`<title>%d 年度关系报告</title>`, r.Year))
	b.WriteString(`<style>`)
	b.WriteString(`body{margin:0;background:#f2f4f3;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;color:#1f2937}`)
	b.WriteString(`.wrap{max-width:720px;margin:0 auto;padding:24px 16px 48px}`)
	b.WriteString(`.hero{background:linear-gradient(135deg,#07c160,#0aa35a);color:#fff;border-radius:20px;padding:36px 28px;text-align:center;box-shadow:0 12px 30px rgba(7,193,96,.25)}`)
	b.WriteString(`.hero h1{margin:0;font-size:30px;letter-spacing:2px}`)
	b.WriteString(`.hero p{margin:10px 0 0;opacity:.9;font-size:14px}`)
	b.WriteString(`.grid{display:grid;grid-template-columns:repeat(3,1fr);gap:12px;margin:20px 0}`)
	b.WriteString(`.kpi{background:#fff;border-radius:14px;padding:16px 12px;text-align:center;box-shadow:0 2px 10px rgba(0,0,0,.05)}`)
	b.WriteString(`.kpi .n{font-size:26px;font-weight:700;color:#059669}`)
	b.WriteString(`.kpi .l{font-size:12px;color:#6b7280;margin-top:4px}`)
	b.WriteString(`.card{background:#fff;border-radius:16px;padding:20px;margin:16px 0;box-shadow:0 2px 10px rgba(0,0,0,.05)}`)
	b.WriteString(`.card h2{margin:0 0 14px;font-size:17px;border-left:4px solid #07c160;padding-left:10px}`)
	b.WriteString(`.bars{display:flex;align-items:flex-end;gap:6px;height:120px}`)
	b.WriteString(`.bar{flex:1;display:flex;flex-direction:column;justify-content:flex-end;align-items:center;gap:4px}`)
	b.WriteString(`.bar i{display:block;width:100%;background:#07c160;border-radius:4px 4px 0 0;min-height:2px}`)
	b.WriteString(`.bar span{font-size:10px;color:#9ca3af}`)
	b.WriteString(`table{width:100%;border-collapse:collapse;font-size:13px}`)
	b.WriteString(`th,td{text-align:left;padding:8px 6px;border-bottom:1px solid #f1f3f2}`)
	b.WriteString(`th{color:#6b7280;font-weight:500;font-size:12px}`)
	b.WriteString(`.tag{display:inline-block;background:#ecfdf5;color:#047857;border-radius:999px;padding:4px 10px;margin:3px;font-size:12px}`)
	b.WriteString(`.ev{display:flex;gap:12px;padding:8px 0;border-bottom:1px dashed #eef0ef}`)
	b.WriteString(`.ev .d{color:#9ca3af;font-size:12px;min-width:78px}`)
	b.WriteString(`.ev .t{font-size:13px}`)
	b.WriteString(`.up{color:#059669}.down{color:#dc2626}.flat{color:#6b7280}`)
	b.WriteString(`.foot{text-align:center;color:#9ca3af;font-size:12px;margin-top:24px}`)
	b.WriteString(`@media(max-width:640px){.grid{grid-template-columns:repeat(2,1fr)}}`)
	b.WriteString(`</style></head><body><div class="wrap">`)

	b.WriteString(fmt.Sprintf(`<div class="hero"><h1>%d 年度关系报告</h1><p>微信聊天画像助手 · 生成于 %s</p></div>`,
		r.Year, e(r.GeneratedAt)))

	kpis := []struct{ n, l string }{
		{fmt.Sprintf("%d", r.TotalMessages), "全年消息"},
		{fmt.Sprintf("%d", r.ActiveContacts), "聊过的人"},
		{fmt.Sprintf("%d", r.ActiveDays), "有记录的天数"},
		{fmt.Sprintf("%d", r.MyMessages), "我发出"},
		{fmt.Sprintf("%d", r.TheirMessages), "收到"},
		{fmt.Sprintf("%d", r.NewContacts), "新认识的人"},
	}
	b.WriteString(`<div class="grid">`)
	for _, k := range kpis {
		b.WriteString(fmt.Sprintf(`<div class="kpi"><div class="n">%s</div><div class="l">%s</div></div>`,
			e(k.n), e(k.l)))
	}
	b.WriteString(`</div>`)

	// 月度曲线
	maxMonth := 1
	for _, m := range r.Months {
		if m.Total > maxMonth {
			maxMonth = m.Total
		}
	}
	b.WriteString(`<div class="card"><h2>每月消息量</h2><div class="bars">`)
	for _, m := range r.Months {
		h := m.Total * 100 / maxMonth
		b.WriteString(fmt.Sprintf(`<div class="bar" title="%d月 共%d条"><i style="height:%d%%"></i><span>%d</span></div>`,
			m.Month, m.Total, h, m.Month))
	}
	b.WriteString(`</div></div>`)

	// 高光时刻
	b.WriteString(`<div class="card"><h2>这一年的几个瞬间</h2><table>`)
	if r.BusiestDay != "" {
		b.WriteString(fmt.Sprintf(`<tr><th>最热闹的一天</th><td>%s（%d 条）</td></tr>`,
			e(r.BusiestDay), r.BusiestDayCnt))
	}
	if r.BusiestMonth > 0 {
		b.WriteString(fmt.Sprintf(`<tr><th>最热闹的月份</th><td>%d 月</td></tr>`, r.BusiestMonth))
	}
	if r.LongestSilence > 0 {
		b.WriteString(fmt.Sprintf(`<tr><th>最长的一次沉默</th><td>%d 天（%s → %s）</td></tr>`,
			r.LongestSilence, e(r.SilenceAfter), e(r.SilenceBefore)))
	}
	if len(r.Top) > 0 {
		b.WriteString(fmt.Sprintf(`<tr><th>聊得最多的人</th><td>%s</td></tr>`, e(r.Top[0].Name)))
	}
	b.WriteString(`</table></div>`)

	// Top 联系人
	if len(r.Top) > 0 {
		b.WriteString(`<div class="card"><h2>聊得最多的人</h2><table><tr><th>#</th><th>联系人</th><th>消息</th><th>我发</th><th>收到</th></tr>`)
		for i, c := range r.Top {
			b.WriteString(fmt.Sprintf(`<tr><td>%d</td><td>%s</td><td>%d</td><td>%d</td><td>%d</td></tr>`,
				i+1, e(c.Name), c.Total, c.Mine, c.Theirs))
		}
		b.WriteString(`</table></div>`)
	}

	// 亲密度变化
	if len(r.Intimacy) > 0 {
		b.WriteString(`<div class="card"><h2>亲密度变化（上半年 → 下半年）</h2><table><tr><th>联系人</th><th>上半年</th><th>下半年</th><th>趋势</th></tr>`)
		for _, ic := range r.Intimacy {
			label, cls := "持平", "flat"
			switch ic.Trend {
			case "up":
				label, cls = fmt.Sprintf("↑ 升温 %+d", ic.Delta), "up"
			case "down":
				label, cls = fmt.Sprintf("↓ 转淡 %+d", ic.Delta), "down"
			}
			b.WriteString(fmt.Sprintf(`<tr><td>%s</td><td>%d</td><td>%d</td><td class="%s">%s</td></tr>`,
				e(ic.Name), ic.FirstHalf, ic.SecondHalf, cls, e(label)))
		}
		b.WriteString(`</table></div>`)
	}

	// 情绪曲线
	if len(r.Emotions) > 0 {
		b.WriteString(`<div class="card"><h2>情绪曲线</h2><div class="bars">`)
		for _, ep := range r.Emotions {
			h := int(ep.Score)
			if h < 4 {
				h = 4
			}
			b.WriteString(fmt.Sprintf(`<div class="bar" title="%d月 平均%.0f分（%d次）"><i style="height:%d%%;background:#34d399"></i><span>%d</span></div>`,
				ep.Month, ep.Score, ep.Count, h, ep.Month))
		}
		b.WriteString(`</div></div>`)
	}

	// 关键词
	if len(r.Keywords) > 0 {
		b.WriteString(`<div class="card"><h2>这一年聊了什么</h2><div>`)
		for _, k := range r.Keywords {
			b.WriteString(fmt.Sprintf(`<span class="tag">%s <b>%d</b></span>`, e(k.Phrase), k.Count))
		}
		b.WriteString(`</div></div>`)
	}

	// 大事记
	if len(r.Events) > 0 {
		b.WriteString(`<div class="card"><h2>大事记</h2>`)
		for _, ev := range r.Events {
			detail := ""
			if strings.TrimSpace(ev.Detail) != "" {
				detail = " · " + e(ev.Detail)
			}
			b.WriteString(fmt.Sprintf(`<div class="ev"><div class="d">%s</div><div class="t">%s%s</div></div>`,
				e(ev.Date), e(ev.Title), detail))
		}
		b.WriteString(`</div>`)
	}

	b.WriteString(fmt.Sprintf(`<div class="foot">wechat-profile-bot · %d 年度关系报告</div>`, r.Year))
	b.WriteString(`</div></body></html>`)
	return b.String()
}
