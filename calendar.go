package main

// 日历订阅（.ics）与重要日子 AI 祝福语草稿。
//
// 设计原则：纯增量，不改动任何既有函数的行为。
//   - .ics 走独立的订阅密钥鉴权（日历客户端无法携带 Authorization 头），
//     密钥存放在 AssistantSettings.CalendarKey，未生成时订阅接口一律 403。
//   - 祝福语默认关闭（BlessingDraft=false），因为它会产生模型调用开销。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	calendarICSPath   = "/api/calendar.ics"
	calendarDaysAhead = 365 // 订阅只覆盖未来一年，足够日历客户端滚动续期
	calendarKeyBytes  = 16  // 16 字节随机 → 32 位十六进制

	blessingMaxCount  = 3
	blessingEmailMax  = 3 // 每日邮件里最多给几条重要日子生成祝福草稿（控制模型开销）
	blessingMaxRunes  = 80
	blessingTimeout   = 120 * time.Second
	blessingSampleMsg = 20
)

// ---------- 订阅密钥 ----------

// newCalendarKey 生成一个随机订阅密钥（读不到系统随机源时退化为时间哈希，保证可用）
func newCalendarKey() string {
	buf := make([]byte, calendarKeyBytes)
	if _, err := rand.Read(buf); err != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("wechat-profile-bot-%d", time.Now().UnixNano())))
		return hex.EncodeToString(sum[:])[:calendarKeyBytes*2]
	}
	return hex.EncodeToString(buf)
}

// calendarSubscribeURL 拼出可直接粘贴到日历 App 的订阅地址
func (s *apiServer) calendarSubscribeURL(r *http.Request, key string) string {
	if key == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = fwd
	}
	return fmt.Sprintf("%s://%s%s?key=%s", scheme, host, calendarICSPath, key)
}

// routeCalendar 处理 /api/assistant/calendar/*（已过认证）
func (s *apiServer) routeCalendar(w http.ResponseWriter, r *http.Request, sub []string) {
	if len(sub) == 0 {
		writeErr(w, http.StatusNotFound, "未知日历接口")
		return
	}
	switch sub[0] {
	case "key":
		switch r.Method {
		case http.MethodGet:
			s.hCalendarKeyGet(w, r)
		case http.MethodPost, http.MethodPut:
			s.hCalendarKeyRotate(w, r)
		case http.MethodDelete:
			s.hCalendarKeyClear(w, r)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		}
	case "events":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hCalendarEvents(w, r)
	default:
		writeErr(w, http.StatusNotFound, "未知日历接口")
	}
}

func (s *apiServer) hCalendarKeyGet(w http.ResponseWriter, r *http.Request) {
	st, err := loadAssistantSettings(s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取助手配置失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"key":     st.CalendarKey,
		"enabled": st.CalendarKey != "",
		"url":     s.calendarSubscribeURL(r, st.CalendarKey),
		"path":    calendarICSPath,
	})
}

func (s *apiServer) hCalendarKeyRotate(w http.ResponseWriter, r *http.Request) {
	st, err := loadAssistantSettings(s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取助手配置失败: "+err.Error())
		return
	}
	st.CalendarKey = newCalendarKey()
	if err := saveAssistantSettings(s.db, st); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存订阅密钥失败: "+err.Error())
		return
	}
	slog.Info("日历订阅密钥已更新")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true, "key": st.CalendarKey, "enabled": true,
		"url": s.calendarSubscribeURL(r, st.CalendarKey),
	})
}

func (s *apiServer) hCalendarKeyClear(w http.ResponseWriter, r *http.Request) {
	st, err := loadAssistantSettings(s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取助手配置失败: "+err.Error())
		return
	}
	st.CalendarKey = ""
	if err := saveAssistantSettings(s.db, st); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存订阅密钥失败: "+err.Error())
		return
	}
	slog.Info("日历订阅已关闭")
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "key": "", "enabled": false, "url": ""})
}

// ---------- .ics 输出 ----------

// icsEscape 按 RFC5545 转义文本值
func icsEscape(s string) string {
	s = strings.TrimSpace(s)
	r := strings.NewReplacer(`\`, `\\`, `;`, `\;`, `,`, `\,`, "\r\n", `\n`, "\n", `\n`, "\r", `\n`, "\t", " ")
	return r.Replace(s)
}

// icsFold 按 70 字节折行（RFC5545 上限 75 字节，留余量），续行以空格开头
func icsFold(line string) string {
	const limit = 70
	var b strings.Builder
	cur := 0
	for _, rn := range line {
		w := utf8.RuneLen(rn)
		if w < 0 {
			w = 1
		}
		if cur > 0 && cur+w > limit {
			b.WriteString("\r\n ")
			cur = 1
		}
		b.WriteRune(rn)
		cur += w
	}
	return b.String()
}

// icsUID 由联系人 + 原文生成稳定 UID，日历客户端重复订阅时不会产生重复事件
func icsUID(contactID int64, raw string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s", contactID, raw)))
	return fmt.Sprintf("%d-%s@wechat-profile-bot", contactID, hex.EncodeToString(sum[:])[:10])
}

// BuildCalendarICS 生成重要日子订阅日历（全年重复的全天事件）
func BuildCalendarICS(db *sql.DB, now time.Time) (string, error) {
	dates, _, err := collectUpcomingDates(db, now, calendarDaysAhead)
	if err != nil {
		return "", err
	}
	stamp := now.UTC().Format("20060102T150405Z")
	b := &strings.Builder{}
	b.WriteString("BEGIN:VCALENDAR\r\n")
	b.WriteString("VERSION:2.0\r\n")
	b.WriteString("PRODID:-//wechat-profile-bot//relationship-assistant//CN\r\n")
	b.WriteString("CALSCALE:GREGORIAN\r\n")
	b.WriteString("METHOD:PUBLISH\r\n")
	b.WriteString("X-WR-CALNAME:" + icsFold(icsEscape("微信关系提醒")) + "\r\n")
	b.WriteString("X-WR-TIMEZONE:Asia/Shanghai\r\n")

	seen := make(map[string]bool, len(dates))
	for _, d := range dates {
		start, err := time.ParseInLocation("2006-01-02", d.DateStr, time.Local)
		if err != nil || d.DateStr == "" {
			continue
		}
		uid := icsUID(d.ContactID, d.Raw)
		if seen[uid] {
			continue // 同一联系人同一原文只导出一条
		}
		seen[uid] = true

		summary := fmt.Sprintf("%s %s（%d月%d日）", d.Name, d.Kind, d.Month, d.Day)
		desc := "画像原文：" + d.Raw
		b.WriteString("BEGIN:VEVENT\r\n")
		b.WriteString(icsFold("UID:"+uid) + "\r\n")
		b.WriteString("DTSTAMP:" + stamp + "\r\n")
		b.WriteString("DTSTART;VALUE=DATE:" + start.Format("20060102") + "\r\n")
		b.WriteString("DTEND;VALUE=DATE:" + start.AddDate(0, 0, 1).Format("20060102") + "\r\n")
		b.WriteString("RRULE:FREQ=YEARLY\r\n")
		b.WriteString(icsFold("SUMMARY:"+icsEscape(summary)) + "\r\n")
		b.WriteString(icsFold("DESCRIPTION:"+icsEscape(desc)) + "\r\n")
		b.WriteString("TRANSP:TRANSPARENT\r\n")
		b.WriteString("END:VEVENT\r\n")
	}
	b.WriteString("END:VCALENDAR\r\n")
	return b.String(), nil
}

// routeCalendarICS 处理 /api/calendar.ics?key=xxx（不走 Bearer 认证，只走 IP 白名单 + 订阅密钥）
func (s *apiServer) routeCalendarICS(w http.ResponseWriter, r *http.Request) {
	st, err := loadAssistantSettings(s.db)
	if err != nil {
		// 这个接口不过 Bearer 认证，err.Error() 里可能带库文件路径等内部信息，
		// 只写日志、对外统一措辞
		slog.Error("日历订阅：读取助手配置失败", "err", err)
		writeErr(w, http.StatusInternalServerError, "读取助手配置失败")
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	// 定长比较：这个接口在公网可无限次尝试，用 == / EqualFold 会留下时序侧信道。
	// 密钥是 hex（本身小写），统一转小写后再比，保留"大小写不敏感"的老行为。
	if st.CalendarKey == "" || key == "" || subtle.ConstantTimeCompare(
		[]byte(strings.ToLower(key)), []byte(strings.ToLower(st.CalendarKey))) != 1 {
		s.guard.RecordDenied(s.realIP(r), r.URL.Path, "calendar-key")
		writeErr(w, http.StatusForbidden, "日历订阅未开启或密钥无效，请在网页端「关系助手 → 日历订阅」生成")
		return
	}
	body, err := BuildCalendarICS(s.db, time.Now())
	if err != nil {
		slog.Error("日历订阅：生成 ICS 失败", "err", err)
		writeErr(w, http.StatusInternalServerError, "生成日历失败")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/calendar; charset=utf-8")
	h.Set("Content-Disposition", `inline; filename="wechat-contacts.ics"`)
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(body))
}

// ---------- AI 祝福语草稿 ----------

// GenerateBlessings 依据画像与我的说话风格，为某个重要日子生成 blessingMaxCount 条祝福草稿。
// 只生成文本，不落库、不发送——用户自己复制粘贴。
func GenerateBlessings(db *sql.DB, llm *LLMClient, item AssistantDateItem) ([]string, error) {
	if llm == nil {
		return nil, fmt.Errorf("未配置模型接口，无法生成祝福语")
	}
	c, err := GetContactByID(db, item.ContactID)
	if err != nil {
		return nil, err
	}
	name := displayName(c)

	var hints []string
	if strings.TrimSpace(c.ProfileJSON) != "" {
		var p Profile
		if err := json.Unmarshal([]byte(c.ProfileJSON), &p); err == nil {
			add := func(label, v string) {
				if strings.TrimSpace(v) != "" {
					hints = append(hints, label+strings.TrimSpace(v))
				}
			}
			add("关系概括：", p.Summary)
			add("职业：", p.BasicInfo.Occupation)
			add("亲密程度：", p.Relationship.Closeness)
			add("互动模式：", p.Relationship.InteractionPattern)
			add("对方语气：", p.CommunicationStyle.Tone)
			if len(p.Personality) > 0 {
				add("性格特征：", strings.Join(p.Personality, "、"))
			}
			if len(p.Interests) > 0 {
				add("兴趣爱好：", strings.Join(p.Interests, "、"))
			}
			if len(p.CommunicationStyle.FrequentPhrases) > 0 {
				add("对方口头禅：", strings.Join(p.CommunicationStyle.FrequentPhrases, "、"))
			}
			if len(p.Relationship.RecentEvents) > 0 {
				add("近期共同事件：", strings.Join(p.Relationship.RecentEvents, "、"))
			}
			if len(p.EmotionalPatterns.Stressors) > 0 {
				add("需要避开的雷点：", strings.Join(p.EmotionalPatterns.Stressors, "、"))
			}
		}
	}

	// 摘几句「我」平时怎么说话，让草稿的口吻贴近用户本人
	var myLines []string
	if msgs, merr := GetRecentMessages(db, item.ContactID, blessingSampleMsg); merr == nil {
		for _, m := range msgs {
			if m.Sender != "me" {
				continue
			}
			myLines = append(myLines, preview(m.Content, 60))
			if len(myLines) >= 8 {
				break
			}
		}
	}
	styleHint := "（暂无历史消息可参考）"
	if len(myLines) > 0 {
		styleHint = strings.Join(myLines, " / ")
	}
	hintBlock := "（该联系人暂无画像信息）"
	if len(hints) > 0 {
		hintBlock = strings.Join(hints, "；")
	}

	when := "今天"
	if item.DaysUntil > 0 {
		when = fmt.Sprintf("%d 天后（%s）", item.DaysUntil, item.DateStr)
	}

	prompt := fmt.Sprintf(`你是微信关系助手。联系人「%s」的%s%s，就是%s。

已知画像信息：%s

用户（"我"）平时的说话风格参考：%s

请替用户起草 %d 条微信祝福语。要求：
1. 三条风格各不相同：第一条走心真诚、第二条轻松俏皮、第三条简短干脆；
2. 每条不超过 60 个字，口语化，像真人发的微信，不要书面套话、不要"值此…之际"；
3. 可以自然带入画像里的兴趣爱好或近期共同事件，但不要编造画像中没有的事实；
4. 避开已知雷点；不要使用表情符号以外的花哨符号，最多可用 1~2 个常见 emoji。

严格输出如下 JSON（不要输出任何其他内容）：
{"blessings":["第一条","第二条","第三条"]}`,
		name, item.Kind, fmt.Sprintf("%d月%d日", item.Month, item.Day), when,
		hintBlock, styleHint, blessingMaxCount)

	ctx, cancel := context.WithTimeout(context.Background(), blessingTimeout)
	defer cancel()
	raw, err := llm.CallContext(ctx, prompt)
	if err != nil {
		return nil, err
	}
	var out struct {
		Blessings []string `json:"blessings"`
	}
	if err := json.Unmarshal([]byte(ExtractJSON(raw)), &out); err != nil {
		return nil, fmt.Errorf("解析祝福语失败: %w", err)
	}

	list := make([]string, 0, blessingMaxCount)
	for _, b := range out.Blessings {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		list = append(list, truncateRunes(b, blessingMaxRunes))
		if len(list) >= blessingMaxCount {
			break
		}
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("模型没有返回可用的祝福语")
	}
	return list, nil
}

// hAssistantBlessing 网页端按需生成：POST /api/assistant/blessing
func (s *apiServer) hAssistantBlessing(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ContactID int64  `json:"contactId"`
		Kind      string `json:"kind"`
		Raw       string `json:"raw"`
		Month     int    `json:"month"`
		Day       int    `json:"day"`
		DateStr   string `json:"dateStr"`
		DaysUntil int    `json:"daysUntil"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil || req.ContactID <= 0 {
		writeErr(w, http.StatusBadRequest, "请提供有效的 contactId")
		return
	}
	if s.llm == nil {
		writeErr(w, http.StatusServiceUnavailable, "LLM 未配置，无法生成祝福语")
		return
	}
	item := AssistantDateItem{
		ContactID: req.ContactID, Kind: strings.TrimSpace(req.Kind),
		Raw: strings.TrimSpace(req.Raw), Month: req.Month, Day: req.Day,
		DateStr: strings.TrimSpace(req.DateStr), DaysUntil: req.DaysUntil,
	}
	if item.Kind == "" {
		item.Kind = "纪念日"
	}
	if item.Month < 1 || item.Month > 12 {
		item.Month = 1
	}
	if item.Day < 1 || item.Day > 31 {
		item.Day = 1
	}
	// 名字以库里的为准，请求体里的显示名不可信
	if c, err := GetContactByID(s.db, item.ContactID); err == nil {
		item.Name = displayName(c)
	} else {
		writeErr(w, http.StatusNotFound, "联系人不存在")
		return
	}

	list, err := GenerateBlessings(s.db, s.llm, item)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"list": list, "name": item.Name, "kind": item.Kind})
}

// attachBlessings 为每日提醒邮件里的重要日子补上祝福草稿。
// 只处理最近的 blessingEmailMax 条，单条失败不影响其他条目，也不影响邮件发送。
func attachBlessings(db *sql.DB, llm *LLMClient, dates []AssistantDateItem, max int) {
	if llm == nil || max <= 0 {
		return
	}
	for i := range dates {
		if i >= max {
			return
		}
		list, err := GenerateBlessings(db, llm, dates[i])
		if err != nil {
			slog.Info("祝福语草稿生成失败", "contact", dates[i].Name, "err", err)
			continue
		}
		dates[i].Blessings = list
	}
}

// buildBlessingEmailHTML 把祝福语草稿渲染成邮件里的一个 <li> 内部块
func buildBlessingEmailHTML(list []string) string {
	if len(list) == 0 {
		return ""
	}
	b := &strings.Builder{}
	b.WriteString(`<br><span style="color:#07c160;font-size:13px;">祝福草稿（可直接复制）：</span><ol style="color:#555;font-size:13px;margin:4px 0 0 18px;padding:0;line-height:1.8;">`)
	for _, s := range list {
		b.WriteString("<li>" + html.EscapeString(s) + "</li>")
	}
	b.WriteString(`</ol>`)
	return b.String()
}

// ---------- v4.7.0 关系维护日历：聚合日期事件 ----------

// CalendarEvent 维护日历上的一条事件（统一结构，供前端月历网格按 date 落格、按 kind 着色）。
type CalendarEvent struct {
	Date      string `json:"date"` // 实际落位日 YYYY-MM-DD
	Kind      string `json:"kind"` // birthday / anniversary / timeline / followup
	Title     string `json:"title"`
	ContactID int64  `json:"contactId"`
	Name      string `json:"name"`
	Meta      string `json:"meta,omitempty"`
}

// calDisplayName 与 ListFollowups/collectUpcomingDates 同一口径：有备注则「备注（名字）」。
func calDisplayName(name, remark string) string {
	if strings.TrimSpace(remark) != "" {
		return remark + "（" + name + "）"
	}
	return name
}

// dateOnly 取民用日（年/月/日，本地时区零时），比较与格式化只用日期部分。
func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// occurrenceInYear 返回 month/day 在该年的落位日（2/29 平年回退 2/28），无有效日期返回 ok=false。
func occurrenceInYear(year, month, day int) (time.Time, bool) {
	t := time.Date(year, time.Month(month), day, 12, 0, 0, 0, time.Local)
	if t.Month() == time.Month(month) && t.Day() == day {
		return dateOnly(t), true
	}
	if month == 2 && day == 29 {
		t2 := time.Date(year, 2, 28, 12, 0, 0, 0, time.Local)
		if t2.Month() == 2 && t2.Day() == 28 {
			return dateOnly(t2), true
		}
	}
	return time.Time{}, false
}

// buildCalendarEvents 聚合 [from,to]（含端点）内的事件：生日/纪念日（年度重复逐年展开）
// + 手动大事记（contact_events）+ 跟进截止（followup_items open 且 due_date 落窗口）。
// 三个数据源各自取一次 dbMu（绝不嵌套）；value-added 表缺失时该源静默跳过。
func buildCalendarEvents(db *sql.DB, from, to time.Time) ([]CalendarEvent, error) {
	fromD, toD := dateOnly(from), dateOnly(to)
	if toD.Before(fromD) {
		fromD, toD = toD, fromD
	}
	fs, ts := fromD.Format("2006-01-02"), toD.Format("2006-01-02")
	out := []CalendarEvent{}

	// 1) 生日 / 纪念日：一次取尽 contacts 进内存后释放锁，再逐条逐年展开。
	type impDate struct {
		id   int64
		name string
		raw  string
	}
	var items []impDate
	dbMu.Lock()
	rows, err := db.Query(`SELECT id, COALESCE(name,''), COALESCE(remark,''), COALESCE(profile_json,'') FROM contacts WHERE merged_into IS NULL`)
	if err == nil {
		for rows.Next() {
			var id int64
			var name, remark, pj string
			if rows.Scan(&id, &name, &remark, &pj) != nil || strings.TrimSpace(pj) == "" {
				continue
			}
			var p Profile
			if json.Unmarshal([]byte(pj), &p) != nil {
				continue
			}
			dn := calDisplayName(name, remark)
			for _, raw := range p.BasicInfo.ImportantDates {
				if strings.TrimSpace(raw) != "" {
					items = append(items, impDate{id, dn, raw})
				}
			}
		}
		err = rows.Err()
		rows.Close()
	}
	dbMu.Unlock()
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		m, d, isBirthday, ok := parseImportantDate(it.raw)
		if !ok {
			continue
		}
		kind, label := "anniversary", "纪念日"
		if isBirthday {
			kind, label = "birthday", "生日"
		}
		for y := fromD.Year(); y <= toD.Year(); y++ {
			occ, ok2 := occurrenceInYear(y, m, d)
			if !ok2 || occ.Before(fromD) || occ.After(toD) {
				continue
			}
			out = append(out, CalendarEvent{
				Date: occ.Format("2006-01-02"), Kind: kind,
				Title: it.name + " · " + label, ContactID: it.id, Name: it.name, Meta: it.raw,
			})
		}
	}

	// 2) 手动大事记（contact_events）；表缺失/出错则跳过该源。
	if evs, terr := calTimelineEvents(db, fs, ts); terr == nil {
		out = append(out, evs...)
	}
	// 3) 跟进截止（followup_items）；表缺失/出错则跳过该源。
	if evs, ferr := calFollowupEvents(db, fs, ts); ferr == nil {
		out = append(out, evs...)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		if out[i].ContactID != out[j].ContactID {
			return out[i].ContactID < out[j].ContactID
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Title < out[j].Title
	})
	return out, nil
}

// calTimelineEvents 取窗口内的事件。按 event_time 前缀日期（RFC3339 固定宽度）比较，
// 与展示同一口径、零时区歧义。
func calTimelineEvents(db *sql.DB, fs, ts string) ([]CalendarEvent, error) {
	out := []CalendarEvent{}
	dbMu.Lock()
	defer dbMu.Unlock()
	rows, err := db.Query(`
		SELECT e.contact_id, COALESCE(c.name,''), COALESCE(c.remark,''), e.title, e.event_time
		FROM contact_events e LEFT JOIN contacts c ON c.id = e.contact_id
		WHERE substr(e.event_time,1,10) BETWEEN ? AND ? AND COALESCE(c.merged_into,0) = 0`, fs, ts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name, remark, title, et string
		if rows.Scan(&id, &name, &remark, &title, &et) != nil || len(et) < 10 {
			continue
		}
		out = append(out, CalendarEvent{
			Date: et[:10], Kind: "timeline", Title: title,
			ContactID: id, Name: calDisplayName(name, remark),
		})
	}
	return out, rows.Err()
}

// calFollowupEvents 取窗口内 open 且已设 due_date 的跟进截止。
func calFollowupEvents(db *sql.DB, fs, ts string) ([]CalendarEvent, error) {
	out := []CalendarEvent{}
	dbMu.Lock()
	defer dbMu.Unlock()
	rows, err := db.Query(`
		SELECT f.contact_id, COALESCE(c.name,''), COALESCE(c.remark,''), f.content, f.due_date, f.kind
		FROM followup_items f LEFT JOIN contacts c ON c.id = f.contact_id
		WHERE f.status = 'open' AND f.due_date != '' AND f.due_date BETWEEN ? AND ? AND COALESCE(c.merged_into,0) = 0`, fs, ts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name, remark, content, due, kind string
		if rows.Scan(&id, &name, &remark, &content, &due, &kind) != nil {
			continue
		}
		out = append(out, CalendarEvent{
			Date: due, Kind: "followup", Title: content,
			ContactID: id, Name: calDisplayName(name, remark), Meta: followupKindLabel(kind),
		})
	}
	return out, rows.Err()
}

// hCalendarEvents GET /api/assistant/calendar/events?from=YYYY-MM-DD&to=YYYY-MM-DD（默认当前自然月）
func (s *apiServer) hCalendarEvents(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	from, to := first, first.AddDate(0, 1, 0).AddDate(0, 0, -1) // 当月首/末日
	q := r.URL.Query()
	if v := strings.TrimSpace(q.Get("from")); v != "" && isValidYMD(v) {
		if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
			from = t
		}
	}
	if v := strings.TrimSpace(q.Get("to")); v != "" && isValidYMD(v) {
		if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
			to = t
		}
	}
	// 窗口护栏：最多展 730 天，防越界参数把逐年展开循环拉大。
	if to.Sub(from) > 730*24*time.Hour {
		to = from.AddDate(0, 0, 730)
	}
	events, err := buildCalendarEvents(s.db, from, to)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取日历事件失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"from":   dateOnly(from).Format("2006-01-02"),
		"to":     dateOnly(to).Format("2006-01-02"),
		"total":  len(events),
		"events": events,
	})
}
