package main

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// ---------- 纯函数 ----------

func TestParseImportantDate(t *testing.T) {
	cases := []struct {
		raw        string
		month, day int
		birthday   bool
		ok         bool
	}{
		{"5月1日", 5, 1, false, true},
		{"生日：5月1日", 5, 1, true, true},
		{"birthday 12/25", 12, 25, true, true},
		{"1995-05-01", 5, 1, false, true},
		{"2020.10.01 在一起", 10, 1, false, true},
		{"05-20", 5, 20, false, true},
		{"5.20", 5, 20, false, true},
		{"农历八月十五", 0, 0, false, false},
		{"", 0, 0, false, false},
		{"13月40日", 0, 0, false, false},
	}
	for _, c := range cases {
		m, d, b, ok := parseImportantDate(c.raw)
		if ok != c.ok || (ok && (m != c.month || d != c.day || b != c.birthday)) {
			t.Errorf("parseImportantDate(%q) = (%d,%d,%v,%v), want (%d,%d,%v,%v)",
				c.raw, m, d, b, ok, c.month, c.day, c.birthday, c.ok)
		}
	}
}

func TestDaysUntilNext(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.Local)

	// 今天 = 0
	if d, _ := daysUntilNext(now, 3, 10); d != 0 {
		t.Errorf("今天应为 0 天，得到 %d", d)
	}
	// 未来
	if d, next := daysUntilNext(now, 3, 12); d != 2 || next.Format("2006-01-02") != "2026-03-12" {
		t.Errorf("3月12日应为 2 天后，得到 %d / %v", d, next)
	}
	// 跨年：明年 1 月 1 日
	if d, next := daysUntilNext(now, 1, 1); d <= 0 || next.Year() != 2027 {
		t.Errorf("1月1日应落到明年，得到 %d / %v", d, next)
	}
	// 不存在的日期
	if d, _ := daysUntilNext(now, 2, 30); d != -1 {
		t.Errorf("2月30日应为 -1，得到 %d", d)
	}
}

func TestValidHHMM(t *testing.T) {
	for _, s := range []string{"08:00", "0:0", "23:59"} {
		if !validHHMM(s) {
			t.Errorf("validHHMM(%q) 应为 true", s)
		}
	}
	for _, s := range []string{"", "8", "24:00", "12:60", "ab:cd"} {
		if validHHMM(s) {
			t.Errorf("validHHMM(%q) 应为 false", s)
		}
	}
}

func TestAssistantSettingsNormalize(t *testing.T) {
	s := AssistantSettings{
		BirthdayAdvanceDays: 0, CoolingDays: -5, EmotionDailyMax: 999,
		WeeklyDay: 9, DailyCheckTime: "xx", WeeklyTime: "", SMTP: AssistantSMTP{Port: 0},
	}
	s.normalize()
	def := defaultAssistantSettings()
	if s.BirthdayAdvanceDays != def.BirthdayAdvanceDays || s.CoolingDays != def.CoolingDays ||
		s.EmotionDailyMax != def.EmotionDailyMax || s.WeeklyDay != def.WeeklyDay ||
		s.DailyCheckTime != def.DailyCheckTime || s.WeeklyTime != def.WeeklyTime ||
		s.SMTP.Port != def.SMTP.Port {
		t.Errorf("normalize 后非法值应回默认，得到 %+v", s)
	}
}

// ---------- 需要 DB 的 ----------

func assistantTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := regressionDB(t)
	if err := ensureAssistantTables(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedContact(t *testing.T, db *sql.DB, name, remark, profileJSON string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO contacts (name, remark, profile_json) VALUES (?, ?, ?)`,
		name, remark, profileJSON)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func seedMessage(t *testing.T, db *sql.DB, contactID int64, sender, content, hash string, ts time.Time) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO messages (contact_id, sender, content, msg_hash, msg_time) VALUES (?, ?, ?, ?, ?)`,
		contactID, sender, content, hash, ts.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
}

func TestAssistantSettingsRoundTrip(t *testing.T) {
	db := assistantTestDB(t)
	// 无记录时返回默认
	s, err := loadAssistantSettings(db)
	if err != nil || s.Enabled || s.DailyCheckTime != "08:00" {
		t.Fatalf("默认配置加载异常: %+v err=%v", s, err)
	}
	s.Enabled = true
	s.SMTP = AssistantSMTP{Host: "smtp.test", Port: 465, SSL: true, User: "u", Pass: "p",
		From: "a@test", To: []string{"b@test"}}
	s.CoolingDays = 14
	if err := saveAssistantSettings(db, s); err != nil {
		t.Fatal(err)
	}
	got, err := loadAssistantSettings(db)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.CoolingDays != 14 || got.SMTP.Host != "smtp.test" || len(got.SMTP.To) != 1 {
		t.Errorf("配置回读不一致: %+v", got)
	}
	// 覆盖保存（UPSERT 单行）
	got.CoolingDays = 30
	if err := saveAssistantSettings(db, got); err != nil {
		t.Fatal(err)
	}
	again, _ := loadAssistantSettings(db)
	if again.CoolingDays != 30 {
		t.Errorf("覆盖保存失败: %+v", again)
	}
}

func TestTryClaimRun(t *testing.T) {
	db := assistantTestDB(t)
	if !tryClaimRun(db, "daily", "2026-03-10") {
		t.Error("首次抢占应成功")
	}
	if tryClaimRun(db, "daily", "2026-03-10") {
		t.Error("同日重复抢占应失败")
	}
	if !tryClaimRun(db, "daily", "2026-03-11") {
		t.Error("不同日期应可抢占")
	}
	if !tryClaimRun(db, "weekly", "2026-03-10") {
		t.Error("不同 kind 应可抢占")
	}
}

func TestShouldNotifyMarkNotified(t *testing.T) {
	db := assistantTestDB(t)
	if !shouldNotify(db, "cooling", 1, "", 7, "2026-03-10") {
		t.Error("首次应提醒")
	}
	markNotified(db, "cooling", 1, "", "2026-03-10")
	if shouldNotify(db, "cooling", 1, "", 7, "2026-03-10") {
		t.Error("窗口期内不应重复提醒")
	}
	// 8 天后（超出 7 天窗口）应再次提醒
	if !shouldNotify(db, "cooling", 1, "", 7, "2026-03-18") {
		t.Error("超出窗口应再次提醒")
	}
	// 不同联系人互不影响
	if !shouldNotify(db, "cooling", 2, "", 7, "2026-03-10") {
		t.Error("不同联系人应提醒")
	}
}

func TestCollectUpcomingDates(t *testing.T) {
	db := assistantTestDB(t)
	now := time.Date(2026, 3, 10, 8, 0, 0, 0, time.Local)
	seedContact(t, db, "wxid_a", "老王",
		`{"basic_info":{"important_dates":["生日：3月12日","2026-12-25 圣诞节","农历八月十五"]}}`)
	seedContact(t, db, "wxid_b", "", `{}`)

	upcoming, unparsed, err := collectUpcomingDates(db, now, 14)
	if err != nil {
		t.Fatal(err)
	}
	if len(upcoming) != 1 {
		t.Fatalf("14 天内应只有 1 条，得到 %+v", upcoming)
	}
	d := upcoming[0]
	if d.Kind != "生日" || d.Month != 3 || d.Day != 12 || d.DaysUntil != 2 {
		t.Errorf("生日条目不符: %+v", d)
	}
	if d.Name != "老王（wxid_a）" {
		t.Errorf("显示名不符: %s", d.Name)
	}
	if len(unparsed) != 1 || unparsed[0].Raw != "农历八月十五" {
		t.Errorf("未识别列表不符: %+v", unparsed)
	}
}

func TestCollectCoolingContacts(t *testing.T) {
	db := assistantTestDB(t)
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.Local)
	cold := seedContact(t, db, "wxid_cold", "冷却", "")
	warm := seedContact(t, db, "wxid_warm", "", "")
	never := seedContact(t, db, "wxid_never", "", "")
	seedMessage(t, db, cold, "other", "在吗", "h1", now.AddDate(0, 0, -10))
	seedMessage(t, db, cold, "me", "在的", "h2", now.AddDate(0, 0, -11))
	seedMessage(t, db, warm, "other", "刚聊过", "h3", now.AddDate(0, 0, -1))

	items, err := collectCoolingContacts(db, now, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ContactID != cold {
		t.Fatalf("应只有 wxid_cold 冷却，得到 %+v", items)
	}
	if items[0].Days != 10 {
		t.Errorf("冷却天数应为 10，得到 %d", items[0].Days)
	}
	if items[0].LastContent != "在吗" { // 最后一条按时间倒序
		t.Errorf("最后一句不符: %s", items[0].LastContent)
	}
	_ = never // 从没聊过的不参与
}

func TestComputeIntimacy(t *testing.T) {
	db := assistantTestDB(t)
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.Local)
	active := seedContact(t, db, "wxid_active", "活跃", "")
	stale := seedContact(t, db, "wxid_stale", "", "")
	for i := 0; i < 3; i++ {
		day := now.AddDate(0, 0, -i)
		seedMessage(t, db, active, "me", "我说的话", "a-me-"+string(rune('0'+i)), day)
		seedMessage(t, db, active, "other", "对方回复的内容比较长的一些话", "a-ot-"+string(rune('0'+i)), day)
	}
	// 窗口外（40 天前）的消息不计入
	seedMessage(t, db, stale, "other", "很久以前", "s1", now.AddDate(0, 0, -40))

	items, err := computeIntimacy(db, now, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ContactID != active {
		t.Fatalf("应只有活跃联系人，得到 %+v", items)
	}
	it := items[0]
	if it.Mine != 3 || it.Other != 3 || it.Days != 3 {
		t.Errorf("统计不符: %+v", it)
	}
	if it.Score <= 0 || it.Score > 100 {
		t.Errorf("分数应在 (0,100]，得到 %d", it.Score)
	}
	if it.Name != "活跃（wxid_active）" {
		t.Errorf("显示名不符: %s", it.Name)
	}
}

func TestRunDailyCheckDisabled(t *testing.T) {
	db := assistantTestDB(t)
	_, err := runDailyCheck(db, nil, time.Now(), false)
	if err == nil || !strings.Contains(err.Error(), "未启用") {
		t.Errorf("未启用应返回错误，得到 %v", err)
	}
	_, err = runWeeklyReport(db, nil, time.Now(), false)
	if err == nil || !strings.Contains(err.Error(), "未启用") {
		t.Errorf("周报未启用应返回错误，得到 %v", err)
	}
}

func TestRunDailyCheckNothingToSend(t *testing.T) {
	db := assistantTestDB(t)
	s := defaultAssistantSettings()
	s.Enabled = true // SMTP 不配置
	if err := saveAssistantSettings(db, s); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 10, 8, 0, 0, 0, time.Local)
	detail, err := runDailyCheck(db, nil, now, false)
	if err != nil {
		t.Fatalf("无提醒事项不应报错: %v", err)
	}
	if !strings.Contains(detail, "无提醒事项") {
		t.Errorf("应返回无提醒事项，得到 %q", detail)
	}
	// 非 force 时同日第二次运行应被去重
	detail2, err := runDailyCheck(db, nil, now, false)
	if err != nil || !strings.Contains(detail2, "已经运行过") {
		t.Errorf("同日应去重，得到 %q err=%v", detail2, err)
	}
	// 手动运行（force）不受去重限制
	detail3, err := runDailyCheck(db, nil, now, true)
	if err != nil || !strings.Contains(detail3, "无提醒事项") {
		t.Errorf("force 应照常运行，得到 %q err=%v", detail3, err)
	}
}

// ---------- 邮件内容冒烟 ----------

func TestBuildEmailHTML(t *testing.T) {
	now := time.Date(2026, 3, 10, 8, 0, 0, 0, time.Local)
	dates := []AssistantDateItem{{Name: "老王", Kind: "生日", Month: 3, Day: 12, DaysUntil: 2, DateStr: "2026-03-12"}}
	cooling := []AssistantCoolingItem{{Name: "小李", Days: 10, LastTime: "2026-02-28", LastContent: "在吗"}}
	alerts := []map[string]interface{}{{
		"name": "小张", "emotion": "低落", "score": 30, "summary": "最近情绪不佳", "advice": "多关心", "alert": true,
	}}
	daily := buildDailyEmailHTML(now, dates, cooling, alerts)
	for _, want := range []string{"每日关系提醒", "老王", "小李", "小张", "多关心"} {
		if !strings.Contains(daily, want) {
			t.Errorf("每日邮件缺少 %q", want)
		}
	}
	if !strings.Contains(daily, "</html>") {
		t.Error("每日邮件 HTML 未闭合")
	}

	top := []AssistantIntimacyItem{{Name: "老王", Score: 88, Mine: 30, Other: 28, Days: 12}}
	weekly := buildWeeklyEmailHTML(now, top, top, cooling, dates, alerts, 58)
	for _, want := range []string{"每周关系报告", "老王", "88"} {
		if !strings.Contains(weekly, want) {
			t.Errorf("每周邮件缺少 %q", want)
		}
	}
	if !strings.Contains(weekly, "</html>") {
		t.Error("每周邮件 HTML 未闭合")
	}
}

// ---------- SMTP / API 辅助 ----------

func TestSMTPReadyAndRecipients(t *testing.T) {
	s := AssistantSMTP{}
	if s.Ready() {
		t.Error("空配置不应 Ready")
	}
	s = AssistantSMTP{Host: "smtp.test", Port: 465, From: "a@test", To: []string{" ", "b@test", ""}}
	if !s.Ready() {
		t.Error("完整配置应 Ready")
	}
	to := recipients(s.To)
	if len(to) != 1 || to[0] != "b@test" {
		t.Errorf("收件人清洗不符: %+v", to)
	}
	s.From = ""
	if s.Ready() {
		t.Error("缺 From 不应 Ready")
	}
}

func TestMaskSMTPPass(t *testing.T) {
	st := AssistantSettings{SMTP: AssistantSMTP{Pass: "secret"}}
	masked := maskSMTPPass(st)
	if masked.SMTP.Pass != smtpPassMask {
		t.Errorf("密码应打码为 %s，得到 %q", smtpPassMask, masked.SMTP.Pass)
	}
	if st.SMTP.Pass != "secret" {
		t.Error("不应修改原值")
	}
	// 空密码不打码
	empty := maskSMTPPass(AssistantSettings{})
	if empty.SMTP.Pass != "" {
		t.Errorf("空密码应保持空，得到 %q", empty.SMTP.Pass)
	}
}
