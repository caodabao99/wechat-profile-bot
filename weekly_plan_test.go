package main

// 每周维护计划 + 隐式反馈闭环回归。
// 覆盖：调度触发、评分排行、缓存读写、幂等标记、14 天回测分档、反馈统计、API 全链路。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// regressionAssistantDB 带助手专表（settings/runs 表由启动时 ensure 建，不在版本迁移里）。
func regressionAssistantDB(t *testing.T) *sql.DB {
	t.Helper()
	db := regressionDB(t)
	if err := ensureAssistantTables(db); err != nil {
		t.Fatal(err)
	}
	return db
}

// seedDailyMetric 直插一条互动度量，避免依赖 messages → metrics 的聚合链路。
func seedDailyMetric(t *testing.T, db *sql.DB, cid int64, day string, me, other int) {
	t.Helper()
	// 同一天重复种时累加，方便多次模拟互动增长
	_, err := db.Exec(
		`INSERT INTO relationship_daily_metrics (contact_id, day, me_count, other_count, first_unix, last_unix)
		 VALUES (?, ?, ?, ?, 0, 0)
		 ON CONFLICT(contact_id, day) DO UPDATE SET me_count = me_count + excluded.me_count,
		   other_count = other_count + excluded.other_count`, cid, day, me, other)
	if err != nil {
		t.Fatal(err)
	}
}

// insertOpenSuggestion 插一条 open 建议，返回其 id。
func insertOpenSuggestion(t *testing.T, db *sql.DB, cid int64, kind string, window, reason string, prio int) int64 {
	t.Helper()
	var id int64
	err := db.QueryRow(
		`INSERT INTO relationship_action_suggestions (contact_id, kind, reason, draft, priority, status, window_key, created_at, updated_at)
		 VALUES (?, ?, ?, '', ?, 'open', ?, datetime('now'), datetime('now')) RETURNING id`,
		cid, kind, reason, prio, window).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAssistantSettingsWeeklyPlanEnabledDefaultsTrue(t *testing.T) {
	if !defaultAssistantSettings().WeeklyPlanEnabled {
		t.Fatal("WeeklyPlanEnabled 默认应为 true（opt-out 思路）")
	}
	db := regressionAssistantDB(t)
	if _, err := db.Exec(`INSERT INTO assistant_settings (id, settings_json) VALUES (1, '{}')
		ON CONFLICT(id) DO UPDATE SET settings_json=excluded.settings_json`); err != nil {
		t.Fatal(err)
	}
	st, err := loadAssistantSettings(db)
	if err != nil {
		t.Fatal(err)
	}
	if !st.WeeklyPlanEnabled {
		t.Fatal("老存量配置 JSON 里没这个字段时，加载后应保持默认 true")
	}
}

func TestCheckAssistantScheduleWeeklyMonday(t *testing.T) {
	db := regressionAssistantDB(t)
	st := defaultAssistantSettings()
	st.Enabled = true
	if err := saveAssistantSettings(db, st); err != nil {
		t.Fatal(err)
	}
	// 有意传 nil：生成会降级为「无建议则缓存空列表」，不真的调模型
	mon := time.Date(2026, 10, 5, 8, 30, 0, 0, time.Local) // 周一，过了每日提醒时间点
	_, lw := checkAssistantSchedule(db, nil, mon, "2026-W40", "")
	if lw == "" {
		t.Fatal("周一同应触发周计划生成")
	}
	// 同周再跑（哪怕重启把 lastDaily 丢了）不得重复触发
	_, lw2 := checkAssistantSchedule(db, nil, mon.Add(2*time.Hour), "", lw)
	if lw2 != lw {
		t.Fatalf("同周不应重复生成: %q -> %q", lw, lw2)
	}
	// 周二不触发
	if _, lw3 := checkAssistantSchedule(db, nil, mon.AddDate(0, 0, 1), "", ""); lw3 != "" {
		t.Fatalf("周二不该生成周计划, got %q", lw3)
	}
	// 关掉开关不触发
	st.WeeklyPlanEnabled = false
	if err := saveAssistantSettings(db, st); err != nil {
		t.Fatal(err)
	}
	if _, lw4 := checkAssistantSchedule(db, nil, mon.AddDate(0, 0, 7), "", ""); lw4 != "" {
		t.Fatalf("关闭开关后不该生成周计划, got %q", lw4)
	}
	// 调度器触发的是后台 goroutine，等它们收尾再关库，避免干扰后续测试
	time.Sleep(500 * time.Millisecond)
}

func TestGenerateWeeklyPlanRankingAndCap(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()
	today := now.Format("2006-01-02")

	// c1 上周最后一次互动（超过默认 cooling 14 天）→ 应产生 silence 建议；
	// c2 今天有互动 → 不应产生建议。
	c1 := regressionContact(t, db, "冷却的人")
	c2 := regressionContact(t, db, "热乎的人")
	seedDailyMetric(t, db, c1, now.AddDate(0, 0, -20).Format("2006-01-02"), 1, 1)
	seedDailyMetric(t, db, c2, today, 2, 2)

	// 手动补建议（含不同优先级与亲密度加成场景），验证排行逻辑不依赖规则引擎的阈值细节
	if _, err := GenerateActionSuggestions(db, nil, 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		insertOpenSuggestion(t, db, c1, "cooling", fmt.Sprintf("test-window-%d", i), fmt.Sprintf("理由 %d", i), i+1)
	}
	// c2 近 30 天互动更密 → 亲密度加成会抬高其分数
	for i := 0; i < 10; i++ {
		db.Exec(`INSERT INTO messages (contact_id, sender, content, msg_hash, msg_time, captured_at)
			 VALUES (?, 'other', ?, ?, ?, ?)`,
			c2, fmt.Sprintf("hi-%d", i), fmt.Sprintf("h-%d", i),
			now.AddDate(0, 0, -i).Format(time.RFC3339), now.Format(time.RFC3339))
	}
	insertOpenSuggestion(t, db, c2, "cooling", "intimacy-window", "亲密加成样本", 2)

	if err := GenerateWeeklyPlan(db, nil, now); err != nil {
		t.Fatal(err)
	}
	items, genAt, err := GetCachedWeeklyPlan(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 5 {
		t.Fatalf("周计划应硬上限 5 条, got %d", len(items))
	}
	if genAt.IsZero() {
		t.Fatal("生成时间不应为空")
	}
	for i := 1; i < len(items); i++ {
		if items[i-1].Score < items[i].Score {
			t.Fatalf("应按分数降序: %v", items)
		}
	}
	// c2 的建议带满额亲密度加成（+5.0）， prio=2 → score=7.0；c1 prio 最高才 6.0 → 必须排第一
	if items[0].ContactID != c2 {
		t.Fatalf("亲密度加成未生效, top1=%+v", items[0])
	}
	if got := items[0].Score; got < 6.9 || got > 7.1 {
		t.Fatalf("c2 期望分数≈7.0 (prio 2 + 加成 5), got %v", got)
	}
}

func TestWeeklyPlanCacheRoundTripAndStale(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()

	// 无缓存 → 空列表零值时间，视为过期
	items, genAt, err := GetCachedWeeklyPlan(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 || !genAt.IsZero() {
		t.Fatalf("无缓存应返回空: %+v %v", items, genAt)
	}
	if !IsWeeklyPlanStale(genAt, now) {
		t.Fatal("空生成时间应视为过期")
	}

	want := []WeeklyPlanItem{
		{ContactID: 7, ContactName: "老王", Kind: "cooling", Reason: "14 天没互动", Draft: "最近忙啥呢", Score: 6.5},
	}
	if err := saveWeeklyPlanCache(db, now, want); err != nil {
		t.Fatal(err)
	}
	got, genAt, err := GetCachedWeeklyPlan(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ContactName != "老王" || got[0].Draft != "最近忙啥呢" {
		t.Fatalf("缓存读回不符: %+v", got)
	}
	if IsWeeklyPlanStale(genAt, now) {
		t.Fatal("刚生成的缓存不该判过期")
	}
	if !IsWeeklyPlanStale(genAt, now.AddDate(0, 0, 8)) {
		t.Fatal("超过 7 天应判过期")
	}

	// UPSERT 语义：再存一次是刷新而非追加
	if err := saveWeeklyPlanCache(db, now.Add(time.Minute), []WeeklyPlanItem{}); err != nil {
		t.Fatal(err)
	}
	got, _, err = GetCachedWeeklyPlan(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("刷新后应为空列表, got %+v", got)
	}
}

func TestAutoMarkSuggestionActedIdempotent(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()
	cid := regressionContact(t, db, "被联系的人")
	seedDailyMetric(t, db, cid, now.Format("2006-01-02"), 3, 2)
	sid := insertOpenSuggestion(t, db, cid, "silence", "w1", "很久没联系", 6)
	other := insertOpenSuggestion(t, db, cid, "cooling", "w2", "降温", 3)
	db.Exec(`UPDATE relationship_action_suggestions SET status='dismissed' WHERE id=?`, other)

	autoMarkSuggestionActed(db, cid, now)
	autoMarkSuggestionActed(db, cid, now) // 幂等：不得重复插 outcome

	var status string
	if err := db.QueryRow(`SELECT status FROM relationship_action_suggestions WHERE id=?`, sid).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "done" {
		t.Fatalf("open 建议应被自动标 done, got %s", status)
	}
	var n, before int
	if err := db.QueryRow(`SELECT COUNT(*), MAX(trend_before) FROM suggestion_outcomes WHERE suggestion_id=?`, sid).Scan(&n, &before); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("重复调用不该产生第二条 outcome, got %d", n)
	}
	if before != 5 {
		t.Fatalf("trend_before 应为标记时近 30 天互动数 5, got %d", before)
	}
	// dismissed 的建议不能被动
	var st2 string
	db.QueryRow(`SELECT status FROM relationship_action_suggestions WHERE id=?`, other).Scan(&st2)
	if st2 != "dismissed" {
		t.Fatalf("dismissed 建议不应被改动, got %s", st2)
	}

	// 回测：acted 超 14 天后趋势涨了 → improved
	// 注意：RFC3339 字符串在 SQLite 里按字典序比较，跨时区偏移会失真，
	// 测试统一用同一 time.Now() 派生的本地偏移串，-15d1h 保证稳赢 cutoff。
	ago := now.AddDate(0, 0, -15).Add(-time.Hour).Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE suggestion_outcomes SET acted_at=? WHERE suggestion_id=?`, ago, sid); err != nil {
		t.Fatal(err)
	}
	seedDailyMetric(t, db, cid, now.Format("2006-01-02"), 6, 6) // 近 30 天总量 5 → 17
	checkPendingOutcomes(db, now)

	var outcome string
	var after int
	if err := db.QueryRow(`SELECT outcome, trend_after FROM suggestion_outcomes WHERE suggestion_id=?`, sid).Scan(&outcome, &after); err != nil {
		t.Fatal(err)
	}
	if outcome == "pending" {
		t.Fatal("超期 pending 应被回测")
	}
	if after <= 5 {
		t.Fatalf("trend_after 应反映最新互动, got %d", after)
	}

	// 未满 14 天的不动
	if _, err := db.Exec(`UPDATE suggestion_outcomes SET outcome='pending', acted_at=?, trend_after=0, checked_at='' WHERE suggestion_id=?`,
		now.Format(time.RFC3339), sid); err != nil {
		t.Fatal(err)
	}
	checkPendingOutcomes(db, now)
	db.QueryRow(`SELECT outcome FROM suggestion_outcomes WHERE suggestion_id=?`, sid).Scan(&outcome)
	if outcome != "pending" {
		t.Fatalf("刚标记的建议不该立刻回测, got %s", outcome)
	}
}

func TestCheckPendingOutcomesClassification(t *testing.T) {
	db := regressionDB(t)
	now := time.Now().In(time.Local)
	// 同上用 -15d1h 保证超期判定不受秒级/时区字符串比较影响
	ago := now.AddDate(0, 0, -15).Add(-time.Hour).Format(time.RFC3339)
	today := now.Format("2006-01-02")

	cases := []struct {
		name      string
		before    int
		me, other int
		want      string
	}{
		{"回暖", 10, 10, 5, "improved"}, // 15/10 = 1.5 > 1.3
		{"持平", 10, 4, 4, "stable"},    // 8/10 = 0.8
		{"走弱", 10, 2, 1, "worsened"},  // 3/10 = 0.3 < 0.7
		{"零基线有互动算回暖", 0, 1, 1, "improved"},
		{"零基线无互动算持平", 0, 0, 0, "stable"},
	}
	for _, tc := range cases {
		cid := regressionContact(t, db, "回测-"+tc.name)
		if tc.me+tc.other > 0 {
			seedDailyMetric(t, db, cid, today, tc.me, tc.other)
		}
		sid := insertOpenSuggestion(t, db, cid, "cooling", tc.name, "x", 5)
		db.Exec(`INSERT INTO suggestion_outcomes (suggestion_id, contact_id, acted_at, trend_before) VALUES (?, ?, ?, ?)`,
			sid, cid, ago, tc.before)
	}

	checkPendingOutcomes(db, now)

	for _, tc := range cases {
		var got string
		if err := db.QueryRow(
			`SELECT o.outcome FROM suggestion_outcomes o
			 JOIN relationship_action_suggestions s ON s.id = o.suggestion_id
			 WHERE s.window_key = ?`, tc.name).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("case %s: 期望 %s, got %s", tc.name, tc.want, got)
		}
	}
}

func TestWeeklyPlanAPIEndToEnd(t *testing.T) {
	db := regressionAssistantDB(t)
	st := defaultAssistantSettings()
	st.Enabled = true
	if err := saveAssistantSettings(db, st); err != nil {
		t.Fatal(err)
	}
	if err := saveWeeklyPlanCache(db, time.Now(), []WeeklyPlanItem{
		{ContactID: 1, ContactName: "老王", Kind: "silence", Reason: "很久没联系", Score: 6},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}

	w := callAPI(s, http.MethodGet, "/api/assistant/weekly-plan", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET weekly-plan: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Items       []WeeklyPlanItem `json:"items"`
		GeneratedAt string           `json:"generatedAt"`
		Stats       WeeklyPlanStats  `json:"stats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 || resp.Items[0].ContactName != "老王" {
		t.Fatalf("items 不符: %+v", resp.Items)
	}
	if resp.GeneratedAt == "" {
		t.Fatal("generatedAt 缺失")
	}

	// POST 手动重生：异步任务，接口本身必须立刻 200
	w = callAPI(s, http.MethodPost, "/api/assistant/weekly-plan", "")
	if w.Code != http.StatusOK {
		t.Fatalf("POST weekly-plan: %d %s", w.Code, w.Body.String())
	}
	// 未知子路径 404
	if c := callAPI(s, http.MethodGet, "/api/assistant/weekly-plan/nope", ""); c.Code != http.StatusNotFound {
		t.Fatalf("未知子路径应 404, got %d", c.Code)
	}
	// 给后台任务留出收尾时间：cleanup 关库后 goroutine 还在用 db 会干扰其他测试
	time.Sleep(500 * time.Millisecond)
}

func TestOutcomeStatsCounts(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()
	cid := regressionContact(t, db, "统计样本")
	sid := insertOpenSuggestion(t, db, cid, "cooling", "s", "x", 5)
	day := now.Format("2006-01-02")
	db.Exec(`INSERT INTO suggestion_outcomes (suggestion_id, contact_id, acted_at, trend_before, outcome, trend_after, checked_at)
		VALUES (?, ?, ?, 5, 'improved', 9, ?)`, sid, cid, now.Format(time.RFC3339), now.Format(time.RFC3339))
	db.Exec(`INSERT INTO relationship_daily_metrics (contact_id, day, me_count, other_count, first_unix, last_unix)
		VALUES (?, ?, 4, 5, 0, 0)`, cid, day)

	stats := getOutcomeStats(db, now)
	if stats.Total < 1 {
		t.Fatalf("total 应≥1, got %+v", stats)
	}
	if stats.Acted != 1 || stats.Improved != 1 {
		t.Fatalf("统计不符: %+v", stats)
	}
	if stats.Stable != 0 || stats.Worsened != 0 {
		t.Fatalf("不该有别的分档: %+v", stats)
	}
}

// stubChatLLM 返固定开场白的桩模型（OpenAI 兼容 /chat/completions）。
func stubChatLLM(t *testing.T, content string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"messages"`) {
			http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{{"message": map[string]interface{}{"content": content}}},
		})
	}))
}

func llmForURL(url string) *LLMClient {
	cfg := &Config{}
	cfg.LLM.BaseURL = url
	cfg.LLM.ApiKey = "k"
	cfg.LLM.Model = "m"
	return NewLLMClient(cfg)
}

func TestGenerateOutreachDraft(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "老王")
	if err := SaveProfile(db, cid, `{"summary":"爱攀岩的设计师"}`, "爱攀岩", "seed"); err != nil {
		t.Fatal(err)
	}

	// 未配置模型 → 空串不空转
	if got := generateOutreachDraft(db, &LLMClient{}, cid, "cooling"); got != "" {
		t.Fatalf("未配置模型应返空, got %q", got)
	}

	srv := stubChatLLM(t, `{"draft":"好久没聊了，最近忙啥呢？"}`)
	defer srv.Close()
	got := generateOutreachDraft(db, llmForURL(srv.URL), cid, "silence")
	if !strings.Contains(got, "好久没聊") {
		t.Fatalf("应返回模型生成的开场白, got %q", got)
	}

	// 模型挂了 → 降级为空，不 panic 不报错到用户
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer dead.Close()
	if got := generateOutreachDraft(db, llmForURL(dead.URL), cid, "cooling"); got != "" {
		t.Fatalf("模型失败应降级为空, got %q", got)
	}
}

func TestGenerateWeeklyPlanFillsDraftsWithLLM(t *testing.T) {
	db := regressionAssistantDB(t)
	now := time.Now()
	cid := regressionContact(t, db, "待唤醒的人")
	if err := SaveProfile(db, cid, `{"summary":"老同学"}`, "老同学", "seed"); err != nil {
		t.Fatal(err)
	}
	seedDailyMetric(t, db, cid, now.AddDate(0, 0, -30).Format("2006-01-02"), 1, 1)
	insertOpenSuggestion(t, db, cid, "silence", "w-draft", "30 天没联系", 7)

	srv := stubChatLLM(t, `{"draft":"周末看到个好东西想起你"}`)
	defer srv.Close()

	if err := GenerateWeeklyPlan(db, llmForURL(srv.URL), now); err != nil {
		t.Fatal(err)
	}
	items, _, err := GetCachedWeeklyPlan(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		t.Fatal("应有周计划项")
	}
	// 规则产出的建议 draft 为空 → 应被 LLM 补全；注意补全只在建议自带 draft 为空时发生
	for _, it := range items {
		if it.ContactID == cid && it.Draft == "" {
			t.Fatalf("draft 为空时应被 LLM 补全: %+v", it)
		}
	}
	time.Sleep(300 * time.Millisecond) // 等可能残留的后台任务收尾
}

func TestWeeklyPlanMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mig.db")
	db, err := InitDB(path)
	if err != nil {
		t.Fatal(err)
	}
	var ver int
	db.QueryRow(`PRAGMA user_version`).Scan(&ver)
	if ver != 17 {
		t.Fatalf("迁移后 user_version 应为 17, got %d", ver)
	}
	for _, tbl := range []string{"weekly_plan_cache", "suggestion_outcomes", "contact_connections", "life_state_cache", "life_projection_cache", "network_insight_cache", "self_portrait_cache", "intervention_cache", "briefing_cache"} {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?)`, tbl).Scan(&n); err != nil || n == 0 {
			t.Fatalf("v14~v17 表 %s 应存在: err=%v cols=%d", tbl, err, n)
		}
	}
	// 写入数据后关库重开：migrate 对已是 v15 的库应全部跳过，数据不丢
	if err := saveWeeklyPlanCache(db, time.Now(), []WeeklyPlanItem{{ContactID: 1, ContactName: "持久化"}}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db2, err := InitDB(path)
	if err != nil {
		t.Fatalf("重开已迁移库报错（迁移不幂等）: %v", err)
	}
	defer db2.Close()
	db2.QueryRow(`PRAGMA user_version`).Scan(&ver)
	if ver != 17 {
		t.Fatalf("重开后版本应变, got %d", ver)
	}
	items, _, err := GetCachedWeeklyPlan(db2)
	if err != nil || len(items) != 1 || items[0].ContactName != "持久化" {
		t.Fatalf("重开后周计划缓存应完好: items=%+v err=%v", items, err)
	}
	// 备份头的版本号必须跟迁移终点一致，否则恢复链会误判兼容性
	if backupCurrentDBVer != 17 {
		t.Fatalf("backupCurrentDBVer 应同步到 17, got %d", backupCurrentDBVer)
	}
}

func TestIngestMeMessageAutoMarksSuggestion(t *testing.T) {
	db := regressionAssistantDB(t)
	cfg := &Config{MyName: "我"}
	cfg.LLM.BaseURL = "http://127.0.0.1:1" // 画像后台任务会优雅失败
	cfg.LLM.ApiKey = "k"
	cfg.LLM.Model = "m"

	cid := regressionContact(t, db, "阿珍")
	sid := insertOpenSuggestion(t, db, cid, "silence", "ingest-w", "很久没联系", 8)

	text := "阿珍 2025/6/10 10:23:45\n最近怎么样\n我 2025/6/10 10:25:00\n刚忙完，周末约饭！"
	out, err := ingestAndStore(db, cfg, llmForURL(cfg.LLM.BaseURL), text)
	if err != nil {
		t.Fatal(err)
	}
	if out.NewCount == 0 || out.Contact == nil || out.Contact.ID != cid {
		t.Fatalf("摄入结果不符: %+v", out)
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM relationship_action_suggestions WHERE id=?`, sid).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "done" {
		t.Fatalf("含 sender=me 的摄入应自动标建议 done, got %s", status)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM suggestion_outcomes WHERE suggestion_id=?`, sid).Scan(&n)
	if n != 1 {
		t.Fatalf("应留下回测记录, got %d", n)
	}
	time.Sleep(300 * time.Millisecond) // 等画像后台任务失败收尾
}
