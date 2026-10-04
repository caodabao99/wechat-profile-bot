package main

// 关系助手（网页端增值功能）：重要日子提醒、冷却联系人、亲密度评分、
// 情绪预警（LLM）、每日提醒邮件。
//
// 设计原则——对现有功能零侵入：
//   - 全部逻辑在本文件 + assistant_api.go + mailer.go，现有文件只加路由分发和启动调用
//   - 配置存 SQLite 新表（不改 config.json 格式），数据只读现有表、只写新表
//   - 默认关闭；不启用时定时任务每分钟醒一次读一行配置即返回，无任何副作用
//   - 提醒走邮件，不碰 iLink clawbot 推送通道

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- 配置（存 assistant_settings 表，单行 JSON） ----------

type AssistantSettings struct {
	Enabled             bool          `json:"enabled"`
	SMTP                AssistantSMTP `json:"smtp"`
	DailyCheckTime      string        `json:"dailyCheckTime"`      // "08:00"
	BirthdayAdvanceDays int           `json:"birthdayAdvanceDays"` // 重要日子提前几天提醒
	CoolingDays         int           `json:"coolingDays"`         // 超过 N 天无互动算冷却
	EmotionAlert        bool          `json:"emotionAlert"`
	EmotionDailyMax     int           `json:"emotionDailyMax"` // 每日最多分析几个联系人（控制 LLM 花费）
	RemindBirthday      bool          `json:"remindBirthday"`
	RemindCooling       bool          `json:"remindCooling"`

	// ---- 关系趋势阈值（决定“降温/升温/沉寂”判定的灵敏度；读不到时保持旧的硬编码行为） ----
	SilenceDays     int `json:"silenceDays"`     // 距上次互动 >= 该天数判定「沉寂」（旧硬编码 30）
	CoolingMinPrior int `json:"coolingMinPrior"` // 降温判定要求前期至少这么多互动（旧硬编码 5）
	WarmingMinPrior int `json:"warmingMinPrior"` // 升温判定要求前期至少这么多互动（旧硬编码 3）

	// ---- 增值功能（默认关闭，老配置读不到这些字段时保持零值/默认值，行为不变） ----
	RemindFollowup     bool   `json:"remindFollowup"`     // 每日邮件是否附「待跟进」板块
	FollowupEnabled    bool   `json:"followupEnabled"`    // 是否用 LLM 自动抽取待跟进（有模型开销）
	FollowupDailyMax   int    `json:"followupDailyMax"`   // 每日最多扫描几个联系人
	FollowupWindowDays int    `json:"followupWindowDays"` // 只看最近 N 天的消息
	BlessingDraft      bool   `json:"blessingDraft"`      // 重要日子提醒里是否附 AI 祝福语草稿
	CalendarKey        string `json:"calendarKey"`        // .ics 订阅链接的独立密钥（ICS 客户端带不了 Authorization 头）
	WeeklyPlanEnabled  bool   `json:"weeklyPlanEnabled"`  // 是否启用每周维护计划（默认开，opt-out）
}

func defaultAssistantSettings() AssistantSettings {
	// 开箱默认即“主动帮你维护关系”的高灵敏档位（原「客户模式」）：
	// 提醒全开、沉寂 14 天即判降温、生日提前 5 天、情绪预警与待跟进自动抽取默认开（会调用模型、产生费用）。
	// 但总开关 Enabled 仍默认关闭：需用户配好 SMTP 后主动启用，避免未配置邮箱就后台空跑/发信失败。
	return AssistantSettings{
		Enabled: false, SMTP: AssistantSMTP{Port: 465, SSL: true},
		DailyCheckTime: "08:00", BirthdayAdvanceDays: 5, CoolingDays: 7,
		EmotionAlert: true, EmotionDailyMax: 10,
		RemindBirthday: true, RemindCooling: true,
		// 关系趋势阈值：高灵敏（沉寂 14 天、降温前期≥4、升温前期≥3）
		SilenceDays: 14, CoolingMinPrior: 4, WarmingMinPrior: 3,
		// 待跟进与自动抽取默认开（要调模型花钱，与“主动维护关系”定位一致）；祝福语草稿仍默认关
		RemindFollowup: true, FollowupEnabled: true, FollowupDailyMax: 8,
		FollowupWindowDays: 30, BlessingDraft: false,
		WeeklyPlanEnabled: true,
	}
}

// normalize 兜底非法值，防止用户把数字改空后逻辑除零/永不触发
func (s *AssistantSettings) normalize() {
	if s.BirthdayAdvanceDays <= 0 || s.BirthdayAdvanceDays > 30 {
		s.BirthdayAdvanceDays = 5
	}
	if s.CoolingDays <= 0 || s.CoolingDays > 365 {
		s.CoolingDays = 7
	}
	if s.SilenceDays <= 0 || s.SilenceDays > 365 {
		s.SilenceDays = 14
	}
	if s.CoolingMinPrior <= 0 || s.CoolingMinPrior > 1000 {
		s.CoolingMinPrior = 4
	}
	if s.WarmingMinPrior <= 0 || s.WarmingMinPrior > 1000 {
		s.WarmingMinPrior = 3
	}
	if s.EmotionDailyMax <= 0 || s.EmotionDailyMax > 50 {
		s.EmotionDailyMax = 10
	}
	if !validHHMM(s.DailyCheckTime) {
		s.DailyCheckTime = "08:00"
	}
	if s.SMTP.Port <= 0 {
		s.SMTP.Port = 465
	}
	if s.FollowupDailyMax <= 0 || s.FollowupDailyMax > 30 {
		s.FollowupDailyMax = 8
	}
	if s.FollowupWindowDays <= 0 || s.FollowupWindowDays > 180 {
		s.FollowupWindowDays = 30
	}
	if len(s.CalendarKey) > 64 {
		s.CalendarKey = s.CalendarKey[:64]
	}
}

func validHHMM(t string) bool {
	parts := strings.Split(strings.TrimSpace(t), ":")
	if len(parts) != 2 {
		return false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	return err1 == nil && err2 == nil && h >= 0 && h <= 23 && m >= 0 && m <= 59
}

// ensureAssistantTables 建关系助手专用表（幂等，DDL 全部 IF NOT EXISTS，不触碰现有表）
func ensureAssistantTables(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS assistant_settings (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			settings_json TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		// 每日/每周任务运行记录，(kind, run_date) 唯一 = 天然防重跑
		`CREATE TABLE IF NOT EXISTS assistant_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL,
			run_date TEXT NOT NULL,
			detail TEXT DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(kind, run_date)
		)`,
		// 情绪分析结果（每联系人多条历史，看板取最新）
		`CREATE TABLE IF NOT EXISTS assistant_emotions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			contact_id INTEGER NOT NULL,
			emotion TEXT DEFAULT '',
			score INTEGER DEFAULT 50,
			summary TEXT DEFAULT '',
			advice TEXT DEFAULT '',
			alert INTEGER DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		// 提醒去重：同一事项在窗口期内只提醒一次（生日每年一次、冷却每 7 天一次）
		`CREATE TABLE IF NOT EXISTS assistant_notified (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL,
			contact_id INTEGER NOT NULL,
			item TEXT DEFAULT '',
			notified_date TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_notified_lookup ON assistant_notified(kind, contact_id, item)`,
		`CREATE TABLE IF NOT EXISTS email_send_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL,
			subject TEXT DEFAULT '',
			recipient TEXT DEFAULT '',
			status TEXT NOT NULL,
			error TEXT DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func loadAssistantSettings(db *sql.DB) (AssistantSettings, error) {
	s := defaultAssistantSettings()
	dbMu.Lock()
	row := db.QueryRow(`SELECT settings_json FROM assistant_settings WHERE id = 1`)
	var raw string
	err := row.Scan(&raw)
	dbMu.Unlock()
	if err == sql.ErrNoRows {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return defaultAssistantSettings(), fmt.Errorf("关系助手配置解析失败: %w", err)
	}
	s.normalize()
	return s, nil
}

// sqlExec 抽象 *sql.DB 与 *sql.Tx 共同的 Exec 方法，便于把写操作抽成
// 可在事务内复用的核心（应用预设时两张设置表需在同一事务里原子写入）。
type sqlExec interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

// writeAssistantSettings 只做 UPSERT 本身，不加锁、不开事务；
// 调用方负责持有 dbMu 或提供事务，以支持多表原子写。
func writeAssistantSettings(ex sqlExec, s AssistantSettings) error {
	s.normalize()
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = ex.Exec(
		`INSERT INTO assistant_settings (id, settings_json, updated_at) VALUES (1, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(id) DO UPDATE SET settings_json = excluded.settings_json, updated_at = CURRENT_TIMESTAMP`,
		string(data))
	return err
}

func saveAssistantSettings(db *sql.DB, s AssistantSettings) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	return writeAssistantSettings(db, s)
}

// ---------- 重要日子解析 ----------

// AssistantDateItem 看板上的一条「即将到来的重要日子」
type AssistantDateItem struct {
	ContactID int64  `json:"contactId"`
	Name      string `json:"name"`
	Kind      string `json:"kind"` // 生日 / 纪念日
	Raw       string `json:"raw"`  // 画像里的原文
	Month     int    `json:"month"`
	Day       int    `json:"day"`
	DaysUntil int    `json:"daysUntil"`
	DateStr   string `json:"dateStr"` // 下一次发生的日期 YYYY-MM-DD

	// Blessings 为 AI 生成的祝福草稿，默认关闭（BlessingDraft）时恒为空，行为与旧版一致
	Blessings []string `json:"blessings,omitempty"`
}

var (
	reDateCN   = regexp.MustCompile(`(\d{1,2})\s*月\s*(\d{1,2})\s*[日号]?`)
	reDateFull = regexp.MustCompile(`(\d{4})[-/.](\d{1,2})[-/.](\d{1,2})`)
	reDateMD   = regexp.MustCompile(`(?:^|[^\d.])(\d{1,2})[-/.](\d{1,2})(?:[^\d.]|$)`)
	reBirthday = regexp.MustCompile(`生日|birthday|Birthday`)
)

// parseImportantDate 从画像 ImportantDates 的自由文本里提取月/日。
// 支持「5月20日」「2020-10-01」「10/1」「05-20」「5.20」等常见写法；
// 解析不出来返回 ok=false——宁可漏报不误报，原文仍会展示在看板「未识别」列表里。
func parseImportantDate(raw string) (month, day int, isBirthday bool, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, 0, false, false
	}
	isBirthday = reBirthday.MatchString(raw)
	valid := func(m, d int) bool { return m >= 1 && m <= 12 && d >= 1 && d <= 31 }

	if m := reDateFull.FindStringSubmatch(raw); m != nil {
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		if valid(mo, d) {
			return mo, d, isBirthday, true
		}
	}
	if m := reDateCN.FindStringSubmatch(raw); m != nil {
		mo, _ := strconv.Atoi(m[1])
		d, _ := strconv.Atoi(m[2])
		if valid(mo, d) {
			return mo, d, isBirthday, true
		}
	}
	if m := reDateMD.FindStringSubmatch(raw); m != nil {
		mo, _ := strconv.Atoi(m[1])
		d, _ := strconv.Atoi(m[2])
		if valid(mo, d) {
			return mo, d, isBirthday, true
		}
	}
	return 0, 0, isBirthday, false
}

// daysUntilNext 计算 month/day 相对 now 的下一次发生日期与相隔天数（今天=0）。
//
// 相隔天数按「民用日历日」计算：把两端都归一到各自 UTC 正午再取整天差，
// 绝不用 now.Location() 下 time.Sub().Hours()/24——那种算法在有夏令时的时区
// 会因切换日只有 23/25 小时而把天数算错一天（Asia/Shanghai 无 DST 才恰好没暴露）。
func daysUntilNext(now time.Time, month, day int) (int, time.Time) {
	candidate := func(year, m, d int) (time.Time, bool) {
		t := time.Date(year, time.Month(m), d, 0, 0, 0, 0, now.Location())
		// time.Date 会把 2/30 这类非法日期滚到 3 月，滚了就说明今年没有这一天
		return t, t.Month() == time.Month(m) && t.Day() == d
	}
	civilDays := func(a, b time.Time) int {
		an := time.Date(a.Year(), a.Month(), a.Day(), 12, 0, 0, 0, time.UTC)
		bn := time.Date(b.Year(), b.Month(), b.Day(), 12, 0, 0, 0, time.UTC)
		return int(bn.Sub(an).Hours() / 24)
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	d, ok := candidate(now.Year(), month, day)
	if !ok && month == 2 && day == 29 {
		// 闰年生日（2/29）在平年不存在：回退到 2/28，而不是把这个人连续 3 年静默丢弃
		d, ok = candidate(now.Year(), 2, 28)
	}
	if ok && !d.Before(today) {
		return civilDays(today, d), d
	}

	d2, ok2 := candidate(now.Year()+1, month, day)
	if !ok2 && month == 2 && day == 29 {
		d2, ok2 = candidate(now.Year()+1, 2, 28)
	}
	if ok2 {
		return civilDays(today, d2), d2
	}
	return -1, time.Time{} // 2月30日之类根本不存在的日期
}

// collectUpcomingDates 扫描所有未合并联系人的画像 ImportantDates，
// 返回 withinDays 天内（含今天）的重要日子，按距今天数排序
func collectUpcomingDates(db *sql.DB, now time.Time, withinDays int) ([]AssistantDateItem, []AssistantDateItem, error) {
	dbMu.Lock()
	rows, err := db.Query(
		`SELECT id, name, COALESCE(remark, ''), profile_json FROM contacts WHERE merged_into IS NULL`)
	var contacts []Contact
	if err == nil {
		for rows.Next() {
			var c Contact
			var pj sql.NullString
			if err := rows.Scan(&c.ID, &c.Name, &c.Remark, &pj); err != nil {
				continue
			}
			if pj.Valid {
				c.ProfileJSON = pj.String
			}
			contacts = append(contacts, c)
		}
		err = rows.Err()
		rows.Close()
	}
	dbMu.Unlock()
	if err != nil {
		return nil, nil, err
	}

	// 空切片而非 nil：JSON 里输出 [] 而不是 null，网页端直接读 .length 才不会崩
	upcoming, unparsed := []AssistantDateItem{}, []AssistantDateItem{}
	for _, c := range contacts {
		if strings.TrimSpace(c.ProfileJSON) == "" {
			continue
		}
		var p Profile
		if err := json.Unmarshal([]byte(c.ProfileJSON), &p); err != nil {
			continue
		}
		for _, raw := range p.BasicInfo.ImportantDates {
			if strings.TrimSpace(raw) == "" {
				continue
			}
			month, day, isBirthday, ok := parseImportantDate(raw)
			if !ok {
				unparsed = append(unparsed, AssistantDateItem{
					ContactID: c.ID, Name: displayName(&c), Kind: "未识别", Raw: raw})
				continue
			}
			days, next := daysUntilNext(now, month, day)
			if days < 0 || days > withinDays {
				continue
			}
			kind := "纪念日"
			if isBirthday {
				kind = "生日"
			}
			upcoming = append(upcoming, AssistantDateItem{
				ContactID: c.ID, Name: displayName(&c), Kind: kind, Raw: raw,
				Month: month, Day: day, DaysUntil: days, DateStr: next.Format("2006-01-02"),
			})
		}
	}
	sort.Slice(upcoming, func(i, j int) bool { return upcoming[i].DaysUntil < upcoming[j].DaysUntil })
	return upcoming, unparsed, nil
}

// ---------- 冷却联系人 ----------

type AssistantCoolingItem struct {
	ContactID   int64  `json:"contactId"`
	Name        string `json:"name"`
	LastTime    string `json:"lastTime"`
	Days        int    `json:"days"`
	LastContent string `json:"lastContent"`
}

// collectCoolingContacts 找出超过 coolingDays 天无任何互动的联系人（按冷却时长倒序）。
// msg_time 是 RFC3339 字符串，排序/换算必须走 strftime('%s')，与 GetContactStats 同一套路。
func collectCoolingContacts(db *sql.DB, now time.Time, coolingDays int) ([]AssistantCoolingItem, error) {
	threshold := now.AddDate(0, 0, -coolingDays)
	out := []AssistantCoolingItem{}
	// 锁内迭代完再解锁：rows 未关闭时会独占唯一连接，
	// 若提前放锁，别的 goroutine 拿到 dbMu 后仍会卡在连接池上，把整库拖住。
	dbMu.Lock()
	rows, err := db.Query(`
		SELECT c.id, c.name, COALESCE(c.remark, ''),
			COALESCE((SELECT m.msg_time FROM messages m
			          WHERE m.contact_id = c.id AND m.msg_time IS NOT NULL AND m.msg_time != ''
			          ORDER BY strftime('%s', m.msg_time) DESC, m.id DESC LIMIT 1), ''),
			COALESCE((SELECT m.content FROM messages m
			          WHERE m.contact_id = c.id AND m.msg_time IS NOT NULL AND m.msg_time != ''
			          ORDER BY strftime('%s', m.msg_time) DESC, m.id DESC LIMIT 1), '')
		FROM contacts c WHERE c.merged_into IS NULL`)
	if err != nil {
		dbMu.Unlock()
		return nil, err
	}
	for rows.Next() {
		var id int64
		var name, remark, lastTime, lastContent string
		if err := rows.Scan(&id, &name, &remark, &lastTime, &lastContent); err != nil {
			continue
		}
		if lastTime == "" {
			continue // 从没聊过的不参与冷却提醒
		}
		t, err := time.Parse(time.RFC3339, lastTime)
		if err != nil {
			continue
		}
		if t.After(threshold) {
			continue
		}
		label := name
		if strings.TrimSpace(remark) != "" {
			label = remark + "（" + name + "）"
		}
		out = append(out, AssistantCoolingItem{
			ContactID: id, Name: label, LastTime: lastTime,
			Days:        int(now.Sub(t).Hours() / 24),
			LastContent: preview(lastContent, 60),
		})
	}
	err = rows.Err()
	rows.Close()
	dbMu.Unlock()
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Days > out[j].Days })
	return out, nil
}

// ---------- 亲密度评分（纯本地统计，不调 LLM） ----------

type AssistantIntimacyItem struct {
	ContactID int64  `json:"contactId"`
	Name      string `json:"name"`
	Score     int    `json:"score"`
	Mine      int    `json:"mine"`
	Other     int    `json:"other"`
	Days      int    `json:"days"` // 近 30 天有互动的天数
}

// computeIntimacy 近 windowDays 天的亲密度（0-100），三个维度加权：
//   - 活跃度 40 分：有互动的天数（15 天封顶）
//   - 均衡度 30 分：双方消息量比值（单方刷屏不算亲密）
//   - 对方投入 30 分：对方消息条数（30 条封顶）15 分 + 对方平均消息长度（50 字封顶）15 分
//
// 只读 messages 表，一次聚合查询算完所有联系人。
func computeIntimacy(db *sql.DB, now time.Time, windowDays int) ([]AssistantIntimacyItem, error) {
	since := now.AddDate(0, 0, -windowDays)
	out := []AssistantIntimacyItem{}
	dbMu.Lock()
	rows, err := db.Query(`
		SELECT m.contact_id, c.name, COALESCE(c.remark, ''),
			SUM(CASE WHEN m.sender='me' THEN 1 ELSE 0 END),
			SUM(CASE WHEN m.sender='other' THEN 1 ELSE 0 END),
			COUNT(DISTINCT substr(m.msg_time, 1, 10)),
			SUM(CASE WHEN m.sender='other' THEN LENGTH(m.content) ELSE 0 END)
		FROM messages m JOIN contacts c ON c.id = m.contact_id
		WHERE c.merged_into IS NULL
		  AND m.msg_time IS NOT NULL AND m.msg_time != ''
		  AND strftime('%s', m.msg_time) >= strftime('%s', ?)
		GROUP BY m.contact_id`, since.Format(time.RFC3339))
	if err != nil {
		dbMu.Unlock()
		return nil, err
	}
	// 锁内迭代完：见 collectCoolingContacts 的说明
	for rows.Next() {
		var it AssistantIntimacyItem
		var name, remark string
		var otherLenSum int64
		if err := rows.Scan(&it.ContactID, &name, &remark, &it.Mine, &it.Other, &it.Days, &otherLenSum); err != nil {
			continue
		}
		if it.Mine+it.Other == 0 {
			continue
		}
		label := name
		if strings.TrimSpace(remark) != "" {
			label = remark + "（" + name + "）"
		}
		it.Name = label

		score := 0.0
		// 活跃度
		d := float64(it.Days)
		if d > 15 {
			d = 15
		}
		score += d / 15 * 40
		// 均衡度
		mn, mx := float64(min64(int64(it.Mine), int64(it.Other))), float64(max64(int64(it.Mine), int64(it.Other)))
		if mx > 0 {
			score += mn / mx * 30
		}
		// 对方投入
		o := float64(it.Other)
		if o > 30 {
			o = 30
		}
		score += o / 30 * 15
		avgLen := 0.0
		if it.Other > 0 {
			avgLen = float64(otherLenSum) / float64(it.Other)
		}
		if avgLen > 50 {
			avgLen = 50
		}
		score += avgLen / 50 * 15

		it.Score = int(score + 0.5)
		out = append(out, it)
	}
	err = rows.Err()
	rows.Close()
	dbMu.Unlock()
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ---------- 情绪分析（LLM） ----------

// EmotionResult 一次情绪分析的结论
type EmotionResult struct {
	Emotion string `json:"emotion"` // 积极/中性/低落/愤怒/焦虑
	Score   int    `json:"score"`   // 0-100 积极程度
	Summary string `json:"summary"`
	Advice  string `json:"advice"`
	Alert   bool   `json:"alert"`
}

// analyzeContactEmotion 取最近一周双方消息 + 画像情绪特征，让 LLM 判断对方近期情绪。
// 结果写入 assistant_emotions；对方消息少于 3 条时跳过（样本不足不瞎猜）。
func analyzeContactEmotion(db *sql.DB, llm *LLMClient, contactID int64, now time.Time) (*EmotionResult, error) {
	c, err := GetContactByID(db, contactID)
	if err != nil {
		return nil, err
	}
	msgs, err := GetRecentMessages(db, contactID, 60)
	if err != nil {
		return nil, err
	}
	weekAgo := now.AddDate(0, 0, -7)
	lines := make([]string, 0, len(msgs))
	otherCount := 0
	for _, m := range msgs {
		if m.Timestamp.Before(weekAgo) {
			continue
		}
		who := "我"
		if m.Sender == "other" {
			who = "对方"
			otherCount++
		}
		lines = append(lines, fmt.Sprintf("%s[%s]: %s", who,
			m.Timestamp.Format("01-02 15:04"), preview(m.Content, 200)))
	}
	if otherCount < 3 {
		return nil, fmt.Errorf("对方最近一周消息不足 3 条，暂不分析")
	}

	// 画像里的情绪特征给 LLM 做参照（没有画像就省略这段）
	emotionHint := ""
	if strings.TrimSpace(c.ProfileJSON) != "" {
		var p Profile
		if err := json.Unmarshal([]byte(c.ProfileJSON), &p); err == nil {
			var hints []string
			if len(p.EmotionalPatterns.Stressors) > 0 {
				hints = append(hints, "压力源/雷点："+strings.Join(p.EmotionalPatterns.Stressors, "、"))
			}
			if strings.TrimSpace(p.EmotionalPatterns.WhenUpset) != "" {
				hints = append(hints, "不高兴时的表现："+p.EmotionalPatterns.WhenUpset)
			}
			if len(hints) > 0 {
				emotionHint = "\n该联系人的已知情绪特征：" + strings.Join(hints, "；") + "。\n"
			}
		}
	}

	prompt := fmt.Sprintf(`你是微信关系分析助手。下面是用户（"我"）与联系人「%s」（"对方"）最近一周的聊天记录：

%s
%s
请分析"对方"最近的情绪状态。要求：
1. 只依据聊天记录，不要臆测记录之外的事实；
2. 对方消息大多是事务性内容（约时间、收发文件等）时，emotion 用"中性"、alert 用 false；
3. 只有出现明显负面情绪（持续低落、烦躁、冷淡、抱怨）且值得用户主动关心时 alert 才为 true。

严格输出如下 JSON（不要输出任何其他内容）：
{"emotion":"积极|中性|低落|愤怒|焦虑 五选一","score":0到100的整数（情绪积极程度）,"summary":"50字以内概括对方近期状态","advice":"50字以内给用户的具体建议","alert":true或false}`,
		displayName(c), strings.Join(lines, "\n"), emotionHint)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	raw, err := llm.CallContext(ctx, prompt)
	if err != nil {
		return nil, err
	}
	var out EmotionResult
	if err := json.Unmarshal([]byte(ExtractJSON(raw)), &out); err != nil {
		return nil, fmt.Errorf("解析情绪分析结果失败: %w", err)
	}
	if out.Score < 0 {
		out.Score = 0
	}
	if out.Score > 100 {
		out.Score = 100
	}
	if strings.TrimSpace(out.Emotion) == "" {
		out.Emotion = "中性"
	}

	saveEmotionResult(db, contactID, &out)
	return &out, nil
}

func saveEmotionResult(db *sql.DB, contactID int64, r *EmotionResult) {
	alert := 0
	if r.Alert {
		alert = 1
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	if _, err := db.Exec(
		`INSERT INTO assistant_emotions (contact_id, emotion, score, summary, advice, alert)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		contactID, r.Emotion, r.Score, r.Summary, r.Advice, alert); err != nil {
		slog.Warn("情绪分析结果入库失败", "contact", contactID, "err", err)
	}
}

// recentEmotions 每个联系人取最近一条分析结果（近 withinDays 天内）
func recentEmotions(db *sql.DB, now time.Time, withinDays int) ([]map[string]interface{}, error) {
	// created_at 由 CURRENT_TIMESTAMP 写入，是 UTC；比较基准必须同样转 UTC，否则差 8 小时
	since := now.AddDate(0, 0, -withinDays).UTC().Format("2006-01-02 15:04:05")
	out := []map[string]interface{}{}
	dbMu.Lock()
	rows, err := db.Query(`
		SELECT e.contact_id, c.name, COALESCE(c.remark, ''), e.emotion, e.score, e.summary, e.advice, e.alert, e.created_at
		FROM assistant_emotions e JOIN contacts c ON c.id = e.contact_id
		WHERE c.merged_into IS NULL AND e.created_at >= ?
		  AND e.id = (SELECT MAX(e2.id) FROM assistant_emotions e2 WHERE e2.contact_id = e.contact_id)
		ORDER BY e.alert DESC, e.score ASC`, since)
	if err != nil {
		dbMu.Unlock()
		return nil, err
	}
	for rows.Next() {
		var id int64
		var name, remark, emotion, summary, advice, createdAt string
		var score, alert int
		if err := rows.Scan(&id, &name, &remark, &emotion, &score, &summary, &advice, &alert, &createdAt); err != nil {
			continue
		}
		label := name
		if strings.TrimSpace(remark) != "" {
			label = remark + "（" + name + "）"
		}
		out = append(out, map[string]interface{}{
			"contactId": id, "name": label, "emotion": emotion, "score": score,
			"summary": summary, "advice": advice, "alert": alert == 1, "createdAt": createdAt,
		})
	}
	err = rows.Err()
	rows.Close()
	dbMu.Unlock()
	return out, err
}

// ---------- 提醒去重 ----------

// shouldNotify 判断 (kind, contactID, item) 在 sinceDays 天内是否已经提醒过
func shouldNotify(db *sql.DB, kind string, contactID int64, item string, sinceDays int, today string) bool {
	dbMu.Lock()
	defer dbMu.Unlock()
	var cnt int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM assistant_notified
		WHERE kind = ? AND contact_id = ? AND item = ? AND notified_date > date(?, ?)`,
		kind, contactID, item, today, fmt.Sprintf("-%d days", sinceDays)).Scan(&cnt)
	return err != nil || cnt == 0
}

func markNotified(db *sql.DB, kind string, contactID int64, item, today string) {
	dbMu.Lock()
	defer dbMu.Unlock()
	if _, err := db.Exec(
		`INSERT INTO assistant_notified (kind, contact_id, item, notified_date) VALUES (?, ?, ?, ?)`,
		kind, contactID, item, today); err != nil {
		slog.Warn("提醒去重记录写入失败", "kind", kind, "contact", contactID, "err", err)
	}
}

// ---------- 邮件内容 ----------

func emailHeader(title string) string {
	return `<!DOCTYPE html><html><body style="font-family:'Microsoft YaHei',sans-serif;color:#333;max-width:640px;margin:0 auto;padding:16px;">` +
		`<h2 style="color:#07c160;border-bottom:2px solid #07c160;padding-bottom:8px;">` + html.EscapeString(title) + `</h2>`
}

const emailFooter = `<p style="color:#999;font-size:12px;margin-top:24px;">本邮件由 微信人物画像助手 · 关系助手 自动发送。可在网页端「关系助手」页调整或关闭。</p></body></html>`

func buildDailyEmailHTML(now time.Time, dates []AssistantDateItem, cooling []AssistantCoolingItem, alerts []map[string]interface{}) string {
	b := &strings.Builder{}
	b.WriteString(emailHeader("每日关系提醒 · " + now.Format("2006年01月02日")))

	if len(dates) > 0 {
		b.WriteString(`<h3>📅 重要日子</h3><ul style="line-height:1.9;">`)
		for _, d := range dates {
			when := "今天"
			if d.DaysUntil == 1 {
				when = "明天"
			} else if d.DaysUntil > 1 {
				when = fmt.Sprintf("%d 天后（%s）", d.DaysUntil, d.DateStr)
			}
			b.WriteString(fmt.Sprintf(`<li><b>%s</b> — %s：%s，就是%s<br><span style="color:#888;font-size:13px;">画像原文：%s</span>%s</li>`,
				html.EscapeString(d.Name), html.EscapeString(d.Kind),
				fmt.Sprintf("%d月%d日", d.Month, d.Day), when, html.EscapeString(d.Raw),
				buildBlessingEmailHTML(d.Blessings)))
		}
		b.WriteString(`</ul>`)
	}
	if len(alerts) > 0 {
		b.WriteString(`<h3>💬 情绪关注</h3><ul style="line-height:1.9;">`)
		for _, a := range alerts {
			b.WriteString(fmt.Sprintf(`<li><b>%s</b>（%s，积极度 %v/100）：%s<br><span style="color:#07c160;font-size:13px;">建议：%s</span></li>`,
				html.EscapeString(fmt.Sprint(a["name"])), html.EscapeString(fmt.Sprint(a["emotion"])),
				a["score"], html.EscapeString(fmt.Sprint(a["summary"])),
				html.EscapeString(fmt.Sprint(a["advice"]))))
		}
		b.WriteString(`</ul>`)
	}
	if len(cooling) > 0 {
		b.WriteString(`<h3>🕐 好久没联系</h3><ul style="line-height:1.9;">`)
		for _, c := range cooling {
			last := ""
			if c.LastContent != "" {
				last = fmt.Sprintf(`<br><span style="color:#888;font-size:13px;">最后一句：%s</span>`, html.EscapeString(c.LastContent))
			}
			b.WriteString(fmt.Sprintf(`<li><b>%s</b> — 已 %d 天没有互动（最后联系 %s）%s</li>`,
				html.EscapeString(c.Name), c.Days, html.EscapeString(c.LastTime[:10]), last))
		}
		b.WriteString(`</ul>`)
	}
	b.WriteString(emailFooter)
	return b.String()
}

// ---------- 每日检查 ----------

// logEmail 记录发送结果
func logEmail(db *sql.DB, kind, subject, recipient, status, errMsg string) {
	dbMu.Lock()
	defer dbMu.Unlock()
	if _, err := db.Exec(
		`INSERT INTO email_send_log (kind, subject, recipient, status, error) VALUES (?, ?, ?, ?, ?)`,
		kind, subject, recipient, status, errMsg); err != nil {
		slog.Warn("邮件日志写入失败", "err", err)
	}
}

// tryClaimRun 用 (kind, run_date) 唯一约束抢占运行权，false 表示今天已经跑过
func tryClaimRun(db *sql.DB, kind, runDate string) bool {
	dbMu.Lock()
	defer dbMu.Unlock()
	res, err := db.Exec(
		`INSERT OR IGNORE INTO assistant_runs (kind, run_date) VALUES (?, ?)`, kind, runDate)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

func updateRunDetail(db *sql.DB, kind, runDate, detail string) {
	dbMu.Lock()
	defer dbMu.Unlock()
	db.Exec(`UPDATE assistant_runs SET detail = ? WHERE kind = ? AND run_date = ?`, detail, kind, runDate)
}

// runDailyCheck 每日检查：重要日子 + 冷却 + 情绪分析，聚合成一封邮件。
// force=true 时跳过「今天已跑过」去重（网页端手动触发）。
func runDailyCheck(db *sql.DB, llm *LLMClient, now time.Time, force bool) (string, error) {
	s, err := loadAssistantSettings(db)
	if err != nil {
		return "", err
	}
	if !s.Enabled {
		return "", fmt.Errorf("关系助手未启用，请先在网页端「关系助手」页开启并配置 SMTP")
	}
	today := now.Format("2006-01-02")
	runKind := "daily"
	if force {
		runKind = "manual"
	}
	if !force && !tryClaimRun(db, runKind, today) {
		return "今天已经运行过", nil
	}
	if force {
		dbMu.Lock()
		db.Exec(`INSERT OR REPLACE INTO assistant_runs (kind, run_date) VALUES (?, ?)`, runKind, today)
		dbMu.Unlock()
	}

	summaryParts := []string{}

	// 1. 重要日子（提醒窗口 = 提前天数；同一事项窗口内只提醒一次）
	var dates []AssistantDateItem
	if s.RemindBirthday {
		all, _, err := collectUpcomingDates(db, now, s.BirthdayAdvanceDays)
		if err != nil {
			slog.Warn("关系助手：重要日子扫描失败", "err", err)
		}
		for _, d := range all {
			if shouldNotify(db, "birthday", d.ContactID, d.DateStr, s.BirthdayAdvanceDays+1, today) {
				dates = append(dates, d)
			}
		}
	}

	// 2. 冷却联系人（同一联系人 7 天内只提醒一次）
	var cooling []AssistantCoolingItem
	if s.RemindCooling {
		all, err := collectCoolingContacts(db, now, s.CoolingDays)
		if err != nil {
			slog.Warn("关系助手：冷却扫描失败", "err", err)
		}
		for _, c := range all {
			if len(cooling) >= 10 { // 一封邮件最多列 10 个，完整名单看网页看板
				break
			}
			if shouldNotify(db, "cooling", c.ContactID, "", 7, today) {
				cooling = append(cooling, c)
			}
		}
	}

	// 3. 情绪分析：近 3 天有对方消息的联系人，每日上限 s.EmotionDailyMax 个
	var alerts []map[string]interface{}
	if s.EmotionAlert && llm != nil {
		targets := emotionAnalysisTargets(db, now, 3, s.EmotionDailyMax)
		for _, id := range targets {
			if _, err := analyzeContactEmotion(db, llm, id, now); err != nil {
				slog.Info("关系助手：情绪分析跳过", "contact", id, "err", err)
			}
		}
		all, err := recentEmotions(db, now, 1)
		if err == nil {
			for _, e := range all {
				if e["alert"] == true {
					alerts = append(alerts, e)
				}
			}
		}
	}

	// 4. 待跟进（对方问了我没回 / 我答应的事 / 钱款往来）
	//    自动抽取默认关闭（FollowupEnabled），关闭时只读已有的手动记录，无模型开销
	var followups []FollowupItem
	if s.RemindFollowup {
		if s.FollowupEnabled && llm != nil {
			RefreshFollowups(db, llm, now, s.FollowupDailyMax, s.FollowupWindowDays)
		}
		items, ferr := ListFollowups(db, "open", followupEmailMax)
		switch {
		case ferr == nil:
			followups = items
		case strings.Contains(ferr.Error(), "no such table"):
			// 表尚未建立（老库还没跑过 ensureFollowupTables），静默跳过
		default:
			slog.Warn("关系助手：待跟进查询失败", "err", ferr)
		}
	}

	// 5. 重要日子祝福草稿：默认关闭，只给最紧急的几条生成，控制模型开销
	if s.BlessingDraft && llm != nil && len(dates) > 0 {
		attachBlessings(db, llm, dates, blessingEmailMax)
	}

	if len(dates) == 0 && len(cooling) == 0 && len(alerts) == 0 && len(followups) == 0 {
		detail := "无提醒事项，未发送邮件"
		updateRunDetail(db, runKind, today, detail)
		return detail, nil
	}

	subject := fmt.Sprintf("【关系提醒】%s：%d 个重要日子，%d 位久未联系，%d 条情绪关注",
		now.Format("1月2日"), len(dates), len(cooling), len(alerts))
	if len(followups) > 0 {
		subject += fmt.Sprintf("，%d 项待跟进", len(followups))
	}
	// 待跟进板块插到页脚之前，不改动 buildDailyEmailHTML 的既有输出
	body := insertEmailSection(buildDailyEmailHTML(now, dates, cooling, alerts), buildFollowupEmailSection(followups))
	err = sendAssistantMail(s.SMTP, subject, body)
	status := "ok"
	errMsg := ""
	if err != nil {
		status, errMsg = "fail", err.Error()
		slog.Error("关系助手：每日提醒邮件发送失败", "err", err)
	} else {
		// 发送成功才记去重，失败下次还能补
		for _, d := range dates {
			markNotified(db, "birthday", d.ContactID, d.DateStr, today)
		}
		for _, c := range cooling {
			markNotified(db, "cooling", c.ContactID, "", today)
		}
	}
	logEmail(db, "daily", subject, strings.Join(recipients(s.SMTP.To), ","), status, errMsg)
	detail := fmt.Sprintf("重要日子 %d，冷却 %d，情绪关注 %d，邮件%s",
		len(dates), len(cooling), len(alerts), status)
	if len(followups) > 0 {
		detail += fmt.Sprintf("，待跟进 %d", len(followups))
	}
	updateRunDetail(db, runKind, today, detail)
	summaryParts = append(summaryParts, detail)

	// 隐式反馈回测：对 acted 超过 14 天的建议自动评估效果
	checkPendingOutcomes(db, now)

	return strings.Join(summaryParts, "；"), err
}

// emotionAnalysisTargets 近 withinDays 天有对方消息的联系人 ID，按最近活跃排序，最多 limit 个
func emotionAnalysisTargets(db *sql.DB, now time.Time, withinDays, limit int) []int64 {
	since := now.AddDate(0, 0, -withinDays).Format(time.RFC3339)
	dbMu.Lock()
	defer dbMu.Unlock()
	rows, err := db.Query(`
		SELECT m.contact_id FROM messages m JOIN contacts c ON c.id = m.contact_id
		WHERE c.merged_into IS NULL AND m.sender = 'other'
		  AND m.msg_time IS NOT NULL AND m.msg_time != ''
		  AND strftime('%s', m.msg_time) >= strftime('%s', ?)
		GROUP BY m.contact_id
		ORDER BY MAX(strftime('%s', m.msg_time)) DESC
		LIMIT ?`, since, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// ---------- 定时调度 ----------

// startAssistantScheduler 启动后台定时任务。每 30 秒醒一次，检查是否到点该跑
// 每日检查。每日检查在独立 goroutine 中执行：一次耗时很长
// 的每日检查（内含多联系人 LLM 情绪分析，可达数分钟）不会阻塞 ticker。
// 触发判定为「到点即触发 + 按天/进程内抢占」，服务在计划时间之后重启也能当天补跑一次。
func startAssistantScheduler(db *sql.DB, llm *LLMClient) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		// 进程内记录当天是否已就该任务尝试过，避免触发时间之后每 30 秒空跑一次；
		// 真正的去重仍以 DB 抢占（tryClaimRun）为准，所以重启后仍能补跑。
		var lastDaily, lastWeekly string
		for range ticker.C {
			// 每轮单独 recover：即便某一轮 checkAssistantSchedule 意外 panic（如配置读取路径），
			// 也不能让本 goroutine 永久退出、导致助手调度从此不再触发。
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("关系助手调度周期异常，已恢复（下一周期继续）", "panic", r)
					}
				}()
				lastDaily, lastWeekly = checkAssistantSchedule(db, llm, time.Now(), lastDaily, lastWeekly)
			}()
		}
	}()
	slog.Info("关系助手定时任务已启动（默认关闭，网页端「关系助手」页可开启）")
}

// atOrAfter 判断 now 是否已到今天的 hhmm 时刻（含）。hhmm 非法时返回 false。
func atOrAfter(now time.Time, hhmm string) bool {
	parts := strings.Split(strings.TrimSpace(hhmm), ":")
	if len(parts) != 2 {
		return false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return false
	}
	target := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	return !now.Before(target)
}

// safeAssistantTask 在独立 goroutine 里跑一个助手任务，panic 全部 recover，
// 绝不影响主服务与其它定时任务。
func safeAssistantTask(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("关系助手定时任务异常", "task", name, "panic", r)
		}
	}()
	fn()
}

// checkAssistantSchedule 每个 ticker 周期调用一次，返回更新后的 lastDaily、lastWeekly。
func checkAssistantSchedule(db *sql.DB, llm *LLMClient, now time.Time, lastDaily, lastWeekly string) (string, string) {
	s, err := loadAssistantSettings(db)
	if err != nil {
		slog.Warn("关系助手配置读取失败", "err", err)
		return lastDaily, lastWeekly
	}
	if !s.Enabled {
		return lastDaily, lastWeekly
	}
	today := now.Format("2006-01-02")

	if atOrAfter(now, s.DailyCheckTime) && lastDaily != today {
		lastDaily = today
		go safeAssistantTask("每日检查", func() {
			if summary, err := runDailyCheck(db, llm, now, false); err != nil {
				slog.Error("关系助手每日检查失败", "err", err)
			} else {
				slog.Info("关系助手每日检查完成", "result", summary)
			}
		})
	}

	// 每周一到点触发周计划生成 + 图谱重建
	if s.WeeklyPlanEnabled && int(now.Weekday()) == 1 && atOrAfter(now, s.DailyCheckTime) {
		weekKey := now.Format("2006-W01")
		if lastWeekly != weekKey {
			lastWeekly = weekKey
			go safeAssistantTask("每周计划生成", func() {
				if err := GenerateWeeklyPlan(db, llm, now); err != nil {
					slog.Error("每周维护计划生成失败", "err", err)
				} else {
					slog.Info("每周维护计划生成完成")
				}
			})
		}
	}

	return lastDaily, lastWeekly
}
