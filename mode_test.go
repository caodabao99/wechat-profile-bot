package main

// 特性③「运行模式一键切换预设」回归：真实 SQLite。
//
//	证明：内置预设存在且可应用；应用后既有设置表落到预期值；另存为/删除往返一致；
//	内置名不可被覆盖/删除；趋势阈值随预设变化（读设置带默认，向后兼容）。

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func modeTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := regressionDB(t)
	if err := ensureAssistantTables(db); err != nil {
		t.Fatal(err)
	}
	if err := ensureArchiveTables(db); err != nil {
		t.Fatal(err)
	}
	if err := ensureModePresetTables(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestModePresetBuiltinListAndApply(t *testing.T) {
	db := modeTestDB(t)

	list, err := ListModePresets(db)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, p := range list {
		if !p.Builtin {
			t.Fatalf("初始不应有自定义预设: %s", p.Name)
		}
		names[p.Name] = true
	}
	for _, want := range []string{"家人", "好友", "同事", "客户", "静音专注"} {
		if !names[want] {
			t.Fatalf("缺少内置预设 %s", want)
		}
	}

	// 应用「静音专注」→ 助手关闭、周报关、归档开且保留 730 天
	if _, err := ApplyModePreset(db, "静音专注"); err != nil {
		t.Fatal(err)
	}
	a, err := loadAssistantSettings(db)
	if err != nil {
		t.Fatal(err)
	}
	if a.Enabled || a.EmotionAlert {
		t.Fatalf("静音专注应关闭主动提醒: %+v", a)
	}
	ar, err := loadArchiveSettings(db)
	if err != nil {
		t.Fatal(err)
	}
	if !ar.Enabled || ar.RetentionDays != 730 {
		t.Fatalf("静音专注归档应为开/730 天: %+v", ar)
	}

	// 应用「客户」→ 趋势阈值收紧、归档 90 天
	if _, err := ApplyModePreset(db, "客户"); err != nil {
		t.Fatal(err)
	}
	a2, _ := loadAssistantSettings(db)
	if a2.SilenceDays != 14 || a2.CoolingMinPrior != 4 {
		t.Fatalf("客户预设阈值不符: %+v", a2)
	}
	ar2, _ := loadArchiveSettings(db)
	if ar2.RetentionDays != 90 {
		t.Fatalf("客户预设归档保留应 90 天: %+v", ar2)
	}
}

func TestModePresetSaveAndDeleteRoundTrip(t *testing.T) {
	db := modeTestDB(t)

	// 先把当前设置调成可辨识的值，再另存为自定义预设
	cur, _ := loadAssistantSettings(db)
	cur.CoolingDays = 11
	cur.DailyCheckTime = "09:30"
	if err := saveAssistantSettings(db, cur); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveModePreset(db, "我的模式"); err != nil {
		t.Fatal(err)
	}

	list, _ := ListModePresets(db)
	var found *ModePreset
	for i := range list {
		if list[i].Name == "我的模式" {
			found = &list[i]
		}
	}
	if found == nil || found.Builtin {
		t.Fatal("自定义预设应出现在列表且非内置")
	}
	if found.Payload.Assistant.CoolingDays != 11 || found.Payload.Assistant.DailyCheckTime != "09:30" {
		t.Fatalf("预设往返数据不一致: %+v", found.Payload.Assistant)
	}

	// 删除自定义预设
	if err := DeleteModePreset(db, "我的模式"); err != nil {
		t.Fatal(err)
	}
	list2, _ := ListModePresets(db)
	for _, p := range list2 {
		if p.Name == "我的模式" {
			t.Fatal("删除后不应再出现")
		}
	}
}

func TestModePresetGuardBuiltinNames(t *testing.T) {
	db := modeTestDB(t)

	if _, err := SaveModePreset(db, "家人"); err == nil {
		t.Fatal("不允许覆盖内置预设名")
	}
	if err := DeleteModePreset(db, "家人"); err == nil {
		t.Fatal("内置预设不可删除")
	}
	if _, err := SaveModePreset(db, "   "); err == nil {
		t.Fatal("空名应报错")
	}
	if _, err := ApplyModePreset(db, "不存在的模式"); err == nil {
		t.Fatal("应用不存在的预设应报错")
	}
}

func TestClassifyTrendWithThresholds(t *testing.T) {
	def := defaultAssistantSettings()
	// DaysSinceLast=20：默认(沉寂门槛30)→ 不误判为 dormant
	if state, _ := classifyTrendWith(&RelationshipTrend{Recent30: 4, Prior30: 4, DaysSinceLast: 20}, def); state != "stable" {
		t.Fatalf("默认阈值下 20 天应 stable, got %s", state)
	}
	// 收紧沉寂门槛到 14 → 同样数据变 dormant
	tight := defaultAssistantSettings()
	tight.SilenceDays = 14
	if state, _ := classifyTrendWith(&RelationshipTrend{Recent30: 4, Prior30: 4, DaysSinceLast: 20}, tight); state != "dormant" {
		t.Fatalf("收紧后 20 天应 dormant, got %s", state)
	}
	// 降温前期门槛：Prior=4，默认 CoolingMinPrior=5 → 不算降温；降到 4 → 算降温
	if state, _ := classifyTrendWith(&RelationshipTrend{Recent30: 1, Prior30: 4, DaysSinceLast: 3}, def); state == "cooling" {
		t.Fatal("默认 CoolingMinPrior=5 时 Prior=4 不应判降温")
	}
	loose := defaultAssistantSettings()
	loose.CoolingMinPrior = 4
	if state, _ := classifyTrendWith(&RelationshipTrend{Recent30: 1, Prior30: 4, DaysSinceLast: 3}, loose); state != "cooling" {
		t.Fatalf("放宽后 Prior=4 应判降温, got %s", state)
	}
}

func TestTrendFollowsAppliedPresetEndToEnd(t *testing.T) {
	db := modeTestDB(t)
	id := regressionContact(t, db, "趋势阈值人")
	// 21 天前一条消息：DaysSinceLast 落在 (14,30) 之间
	_, err := SaveMessages(db, id, []Message{{
		Sender: "other", Content: "很久以前的一句话",
		Timestamp: time.Now().AddDate(0, 0, -21),
	}})
	if err != nil {
		t.Fatal(err)
	}

	// 默认（未应用预设，沉寂门槛 30）→ stable
	tr, err := GetRelationshipTrend(db, id)
	if err != nil {
		t.Fatal(err)
	}
	if tr.State != "stable" {
		t.Fatalf("默认阈值 21 天应 stable, got %s", tr.State)
	}

	// 应用「客户」（沉寂门槛 14）→ 同一数据变 dormant
	if _, err := ApplyModePreset(db, "客户"); err != nil {
		t.Fatal(err)
	}
	tr2, err := GetRelationshipTrend(db, id)
	if err != nil {
		t.Fatal(err)
	}
	if tr2.State != "dormant" {
		t.Fatalf("应用客户预设后 21 天应 dormant, got %s", tr2.State)
	}
}

func TestModePresetAPIEndToEnd(t *testing.T) {
	db := modeTestDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}

	// GET 列表
	resp := callAPI(s, http.MethodGet, "/api/mode-presets", "")
	if resp.Code != http.StatusOK {
		t.Fatalf("GET presets: %d %s", resp.Code, resp.Body.String())
	}
	var listResp struct {
		Presets []ModePreset `json:"presets"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Presets) < 5 {
		t.Fatalf("应至少 5 个内置预设, got %d", len(listResp.Presets))
	}

	// POST 应用
	resp = callAPI(s, http.MethodPost, "/api/mode-presets/apply", `{"name":"好友"}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", resp.Code, resp.Body.String())
	}

	// POST 另存为
	resp = callAPI(s, http.MethodPost, "/api/mode-presets", `{"name":"API自定义"}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("save: %d %s", resp.Code, resp.Body.String())
	}

	// 覆盖内置名 → 400
	resp = callAPI(s, http.MethodPost, "/api/mode-presets", `{"name":"家人"}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("覆盖内置应 400, got %d", resp.Code)
	}

	// DELETE 自定义 → 200
	resp = callAPI(s, http.MethodDelete, "/api/mode-presets/API自定义", "")
	if resp.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", resp.Code, resp.Body.String())
	}

	// DELETE 内置 → 400
	resp = callAPI(s, http.MethodDelete, "/api/mode-presets/家人", "")
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("删内置应 400, got %d", resp.Code)
	}

	// 应用不存在的预设 → 400
	resp = callAPI(s, http.MethodPost, "/api/mode-presets/apply", `{"name":"查无此模式"}`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("应用不存在应 400, got %d", resp.Code)
	}
}

// TestApplyModePresetAtomicRollback 验证：当归档设置表不可写时，应用预设必须整体失败并
// 回滚，绝不能把助手设置“半更新”——否则接口报失败但用户的提醒/阈值已被静默切换。
func TestApplyModePresetAtomicRollback(t *testing.T) {
	db := modeTestDB(t)

	// 先把助手设置置为一个可识别、且与「客户」预设不同的初始态
	before := defaultAssistantSettings()
	before.Enabled = true
	before.SilenceDays = 60
	before.CoolingMinPrior = 9
	if err := saveAssistantSettings(db, before); err != nil {
		t.Fatal(err)
	}

	// 模拟归档设置写入失败：直接删掉 archive_settings 表（预设第二步会 INSERT 它 → 报错）
	if _, err := db.Exec(`DROP TABLE archive_settings`); err != nil {
		t.Fatal(err)
	}

	// 「客户」预设会把阈值改成 14/4；应用应失败
	if _, err := ApplyModePreset(db, "客户"); err == nil {
		t.Fatal("归档表缺失时应用预设应返回错误")
	}

	// 关键断言：第一步的助手写入必须被事务回滚，仍是初始值
	after, err := loadAssistantSettings(db)
	if err != nil {
		t.Fatal(err)
	}
	if after.SilenceDays != 60 || after.CoolingMinPrior != 9 {
		t.Fatalf("应用失败应整体回滚，助手设置却被半更新: %+v", after)
	}
}

// TestApplyModePresetPreservesCreds 验证 Critical-1 修复：内置预设以空 SMTP 为底，
// 应用任何预设都不能抹掉用户已配好的 SMTP 密码与日历密钥，同时行为阈值照常切换。
func TestApplyModePresetPreservesCreds(t *testing.T) {
	db := modeTestDB(t)
	cur := defaultAssistantSettings()
	cur.SMTP = AssistantSMTP{Host: "smtp.x", Port: 465, SSL: true, User: "u",
		Pass: "real-secret-pass", From: "a@x", To: []string{"b@x"}}
	cur.CalendarKey = "real-calendar-key"
	if err := saveAssistantSettings(db, cur); err != nil {
		t.Fatal(err)
	}

	// 「客户」内置预设的 payload 以空 SMTP 为底（Pass=""/CalendarKey=""）
	if _, err := ApplyModePreset(db, "客户"); err != nil {
		t.Fatal(err)
	}

	got, err := loadAssistantSettings(db)
	if err != nil {
		t.Fatal(err)
	}
	if got.SMTP.Pass != "real-secret-pass" || got.CalendarKey != "real-calendar-key" {
		t.Fatalf("应用预设不应抹掉凭据: pass=%q calKey=%q", got.SMTP.Pass, got.CalendarKey)
	}
	// 凭据保留不能影响行为阈值：「客户」预设应为 14/4/3
	if got.SilenceDays != 14 || got.CoolingMinPrior != 4 || got.WarmingMinPrior != 3 {
		t.Fatalf("预设行为阈值未生效: %+v", got)
	}
}

// TestMaskPresetCredsStripsSecrets 验证预设凭据打码助手：密码/密钥替为掩码，
// 非凭据字段不受波及，空凭据保持空（不能把 "" 填成掩码）。
func TestMaskPresetCredsStripsSecrets(t *testing.T) {
	var p ModePresetPayload
	p.Assistant.SMTP.Pass = "topsecret"
	p.Assistant.SMTP.User = "user@x" // 非凭据，不应被动
	p.Assistant.CalendarKey = "calsecret"
	m := maskPresetCreds(p)
	if m.Assistant.SMTP.Pass != smtpPassMask || m.Assistant.CalendarKey != calendarKeyMask {
		t.Fatalf("未打码: pass=%q key=%q", m.Assistant.SMTP.Pass, m.Assistant.CalendarKey)
	}
	if m.Assistant.SMTP.User != "user@x" {
		t.Errorf("非凭据字段被误改: user=%q", m.Assistant.SMTP.User)
	}
	e := maskPresetCreds(ModePresetPayload{})
	if e.Assistant.SMTP.Pass != "" || e.Assistant.CalendarKey != "" {
		t.Errorf("空凭据应保持空: pass=%q key=%q", e.Assistant.SMTP.Pass, e.Assistant.CalendarKey)
	}
}

// TestModePresetApplyResponseMasksCreds 验证 High-5 修复：ApplyModePreset 会在 payload
// 里注入真实凭据（为了写库保留），但 /api/mode-presets/apply 响应必须打码，绝不能再把
// 明文密码/密钥回传前端。
func TestModePresetApplyResponseMasksCreds(t *testing.T) {
	db := modeTestDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}

	cur := defaultAssistantSettings()
	cur.SMTP.Pass = "leak-me-please"
	cur.CalendarKey = "leak-cal-key"
	if err := saveAssistantSettings(db, cur); err != nil {
		t.Fatal(err)
	}

	resp := callAPI(s, http.MethodPost, "/api/mode-presets/apply", `{"name":"客户"}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "leak-me-please") || strings.Contains(resp.Body.String(), "leak-cal-key") {
		t.Fatalf("apply 响应泄漏明文凭据: %s", resp.Body.String())
	}
	// 同理 GET 列表也不得出现明文
	resp = callAPI(s, http.MethodGet, "/api/mode-presets", "")
	if resp.Code != http.StatusOK {
		t.Fatalf("list: %d %s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "leak-me-please") {
		t.Fatalf("list 响应泄漏明文凭据: %s", resp.Body.String())
	}
}
