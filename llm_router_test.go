package main

// ═══════════════════════════════════════════════════════════════════════════
// Phase 2 验收：AI Model Router（蓝图 §5）+ Cost Governance（§6）。
//
// 钉死这些「架构级承诺」，而非只测 happy path：
//  1. 路由是**确定性查表**：无策略任务完全沿用活动档案（向后兼容 v6.x）；配了 primary 走 primary；
//     primary 失败退 fallback；fallback 也失败返回普通 error（绝不 500，由调用方降级）。
//  2. 隐私 local_only：只在有本地档案时改选本地；无本地档案→HasPrimary=false→调用返回 errNoLocalProfile，
//     绝不外发远端（这是隐私红线，不是可协商项）。
//  3. 多维预算：全局日/周/月 + 任务日 + 联系人日；只拦后台、不拦交互；命中缓存先于预算返回。
//  4. 归因不污染成本口径：cache_hit=1 记零 token；真实 calls/tokens 只算 cache_hit=0；ByContact 联名、
//     删者回落「(已删除#id)」不丢成本。
//  5. 窗口起点语义：本周一零点 / 本月一日零点（周月预算的基石，错了整条成本治理都失真）。
//  6. 视图单一来源：ModelPolicyViews.CacheEnabled 取 registry.Cacheable，前端不另建映射。
// ═══════════════════════════════════════════════════════════════════════════

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// routerDB 造一个带两个可用档案（远端 p-remote / 本地 p-local）的库，活动档案默认 p-remote。
func routerDB(t *testing.T) *sql.DB {
	t.Helper()
	db := regressionDB(t)
	settings := LLMSettings{
		ActiveProfileID: "p-remote",
		Profiles: []LLMProfile{
			{ID: "p-remote", Label: "远端主力", Provider: "deepseek", BaseURL: "https://api.remote.example/v1", APIKey: "k1", Model: "m-remote"},
			{ID: "p-local", Label: "本地隐私", Provider: "ollama", BaseURL: "http://127.0.0.1:11434/v1", APIKey: "k2", Model: "m-local"},
		},
	}
	if err := saveLLMSettings(db, settings); err != nil {
		t.Fatalf("存路由档案: %v", err)
	}
	return db
}

func TestResolveRouteDefaultsToActiveProfile(t *testing.T) {
	db := routerDB(t)
	c := NewLLMClient(&Config{}).WithDB(db)
	// 无策略任务：主档案 = 活动档案，无 fallback（与 v6.x 行为一致）。
	r := c.resolveRoute(TaskNarrative)
	if !r.HasPrimary || r.HasFallback {
		t.Fatalf("默认路由应有主无备，实得 %+v", r)
	}
	if r.Primary.ProfileID != "p-remote" {
		t.Fatalf("默认应走活动档案 p-remote，实得 %q", r.Primary.ProfileID)
	}
}

func TestResolveRoutePrimaryAndFallbackByID(t *testing.T) {
	db := routerDB(t)
	s, _ := loadLLMSettings(db)
	s.TaskPolicies = map[string]TaskModelPolicy{
		string(TaskNarrative): {PrimaryProfileID: "p-local", FallbackProfileID: "p-remote"},
	}
	if err := saveLLMSettings(db, s); err != nil {
		t.Fatal(err)
	}
	c := NewLLMClient(&Config{}).WithDB(db)
	r := c.resolveRoute(TaskNarrative)
	if r.Primary.ProfileID != "p-local" || !r.HasPrimary {
		t.Fatalf("primary 应绑到 p-local，实得 %+v", r.Primary)
	}
	if !r.HasFallback || r.Fallback.ProfileID != "p-remote" {
		t.Fatalf("fallback 应绑到 p-remote，实得 has=%+v id=%q", r.HasFallback, r.Fallback.ProfileID)
	}
}

func TestResolveRouteIgnoresFallbackEqualToPrimary(t *testing.T) {
	db := routerDB(t)
	s, _ := loadLLMSettings(db)
	// fallback 与 primary 相同档案 → 不该启用回退（否则同档再打一遍纯属浪费）。
	s.TaskPolicies = map[string]TaskModelPolicy{
		string(TaskNarrative): {PrimaryProfileID: "p-local", FallbackProfileID: "p-local"},
	}
	if err := saveLLMSettings(db, s); err != nil {
		t.Fatal(err)
	}
	r := NewLLMClient(&Config{}).WithDB(db).resolveRoute(TaskNarrative)
	if r.HasFallback {
		t.Fatal("fallback 与 primary 同档时不应启用回退")
	}
}

func TestResolveRouteLocalOnlySelectsLocalProfile(t *testing.T) {
	db := routerDB(t)
	s, _ := loadLLMSettings(db)
	// 活动档案是远端，但任务声明 local_only → 应自动改选本地档案。
	s.TaskPolicies = map[string]TaskModelPolicy{
		string(TaskEmotion): {PrivacyMode: privacyLocalOnly},
	}
	if err := saveLLMSettings(db, s); err != nil {
		t.Fatal(err)
	}
	r := NewLLMClient(&Config{}).WithDB(db).resolveRoute(TaskEmotion)
	if !r.HasPrimary || r.Primary.ProfileID != "p-local" {
		t.Fatalf("local_only 应改选本地档案，实得 has=%v id=%q", r.HasPrimary, r.Primary.ProfileID)
	}
}

func TestResolveRouteLocalOnlyWithoutLocalProfileFailsClosed(t *testing.T) {
	db := regressionDB(t)
	// 只有一个远端档案，任务却要求 local_only → 无本地可用：HasPrimary=false（失败方向保守）。
	if err := saveLLMSettings(db, LLMSettings{
		ActiveProfileID: "p-remote",
		Profiles:        []LLMProfile{{ID: "p-remote", Provider: "deepseek", BaseURL: "https://api.remote.example/v1", APIKey: "k", Model: "m"}},
		TaskPolicies:    map[string]TaskModelPolicy{string(TaskEmotion): {PrivacyMode: privacyLocalOnly}},
	}); err != nil {
		t.Fatal(err)
	}
	c := NewLLMClient(&Config{}).WithDB(db)
	if r := c.resolveRoute(TaskEmotion); r.HasPrimary {
		t.Fatal("local_only 无本地档案时不应有可用主档案")
	}
	// 端到端：CallContext 应返回 errNoLocalProfile，绝不触达远端（隐私红线）。
	var calls atomic.Int64
	server := stubCountingServer(t, &calls, `{"x":1}`)
	c.apiKey, c.baseURL, c.model = "k", server.URL, "stub"
	_, err := c.CallContext(withLLMTask(context.Background(), TaskEmotion), "P")
	if !errors.Is(err, errNoLocalProfile) {
		t.Fatalf("local_only 无本地档案应返回 errNoLocalProfile，实得 %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("隐私降级绝不能触达远端模型，实得 calls=%d", calls.Load())
	}
}

func TestCallContextFallsBackWhenPrimaryFails(t *testing.T) {
	db := routerDB(t)
	// primary=本地（指向不可达地址，必然失败），fallback=远端 stub（成功）。
	s, _ := loadLLMClientSettings(db)
	var fbCalls atomic.Int64
	server := stubCountingServer(t, &fbCalls, `{"ok":"from-fallback"}`)
	// 把本地档案改成不可达，远端档案指向成功的 stub。
	for i := range s.Profiles {
		if s.Profiles[i].ID == "p-local" {
			s.Profiles[i].BaseURL = "http://127.0.0.1:1/v1" // 端口 1 必拒连
		}
		if s.Profiles[i].ID == "p-remote" {
			s.Profiles[i].BaseURL = server.URL
			s.Profiles[i].APIKey = "k"
		}
	}
	s.TaskPolicies = map[string]TaskModelPolicy{
		string(TaskNarrative): {PrimaryProfileID: "p-local", FallbackProfileID: "p-remote"},
	}
	if err := saveLLMSettings(db, s); err != nil {
		t.Fatal(err)
	}
	c := NewLLMClient(&Config{}).WithDB(db)
	out, err := c.CallContext(withLLMTask(withLLMContact(context.Background(), 5), TaskNarrative), "P-fb")
	if err != nil {
		t.Fatalf("primary 失败应回退 fallback 成功，实得 %v", err)
	}
	if !strings.Contains(out, "from-fallback") || fbCalls.Load() != 1 {
		t.Fatalf("应由 fallback 拿到结果，实得 out=%q fbCalls=%d", out, fbCalls.Load())
	}
	// 用量日志应有一行 fallback=1（真实 API 消耗归因）。
	if n := countLogRows(t, db, `SELECT COUNT(*) FROM llm_call_log WHERE fallback=1 AND ok=1`); n < 1 {
		t.Fatalf("应记录一次成功的 fallback 调用，实得 %d", n)
	}
}

func TestNormalizeTaskPoliciesClampsAndCleans(t *testing.T) {
	db := regressionDB(t)
	hot := 9.9
	cold := -3.0
	s := LLMSettings{
		Profiles: []LLMProfile{{ID: "p1", Provider: "deepseek", BaseURL: "https://x.example/v1", APIKey: "k", Model: "m"}},
		TaskPolicies: map[string]TaskModelPolicy{
			"":                  {PrimaryProfileID: "p1"},                                    // 空 key 应被删
			"  narrative  ":     {PrimaryProfileID: " p1 "},                                  // key/值去空白
			string(TaskEmotion): {Temperature: &hot, BudgetLimitTokens: -100, MaxTokens: -5}, // 温度夹到 2、负归零
			string(TaskCoach):   {Temperature: &cold},                                        // 负温度夹到 0
		},
	}
	if err := saveLLMSettings(db, s); err != nil {
		t.Fatal(err)
	}
	got, _ := loadLLMSettings(db)
	if _, ok := got.TaskPolicies[""]; ok {
		t.Fatal("空 key 应被清除")
	}
	if _, ok := got.TaskPolicies["narrative"]; !ok {
		t.Fatalf("key 去空白后应保留 narrative，实得 keys=%v", policyKeys(got.TaskPolicies))
	}
	if p := got.TaskPolicies[string(TaskEmotion)]; p.Temperature == nil || *p.Temperature != 2 || p.BudgetLimitTokens != 0 || p.MaxTokens != 0 {
		t.Fatalf("local_only 温度应夹到 2、负预算/负 token 归零，实得 %+v", p)
	}
	if p := got.TaskPolicies[string(TaskCoach)]; p.Temperature == nil || *p.Temperature != 0 {
		t.Fatalf("负温度应夹到 0（0 是合法值），实得 %+v", p.Temperature)
	}
}

func TestLLMBudgetAllowsCallMultiDimension(t *testing.T) {
	db := regressionDB(t)
	c := NewLLMClient(&Config{}).WithDB(db)

	// 周预算用尽：全局日不限、周限 1000、真实已用 1200（cache_hit=0）。
	if err := saveLLMSettings(db, LLMSettings{WeeklyTokenBudget: 1000}); err != nil {
		t.Fatal(err)
	}
	c.logLLMCall(llmSpec{Model: "m"}, "", true, 200, llmUsage{Total: 1200}, 1)
	st := llmBudgetStatus(db)
	if !st.WeeklyOver || !st.AnyOver {
		t.Fatalf("周超预算应 WeeklyOver+AnyOver，实得 %+v", st)
	}
	if llmBudgetAllowsCall(context.Background(), db, TaskNarrative, 0) {
		t.Fatal("周预算用尽应拦后台")
	}
	if !llmBudgetAllowsCall(withInteractiveCall(context.Background()), db, TaskNarrative, 0) {
		t.Fatal("交互式永不被预算拦")
	}

	// 任务级日预算：全局都不限时，单任务超其 BudgetLimitTokens 也应被拦。
	taskDB := regressionDB(t)
	tc := NewLLMClient(&Config{}).WithDB(taskDB)
	if err := saveLLMSettings(taskDB, LLMSettings{
		TaskPolicies: map[string]TaskModelPolicy{string(TaskSummary): {BudgetLimitTokens: 500}},
	}); err != nil {
		t.Fatal(err)
	}
	tc.logLLMCall(llmSpec{Model: "m"}, string(TaskSummary), true, 200, llmUsage{Total: 600}, 1)
	if llmBudgetAllowsCall(context.Background(), taskDB, TaskSummary, 0) {
		t.Fatal("任务日预算用尽应拦该任务的后台调用")
	}
	if !llmBudgetAllowsCall(context.Background(), taskDB, TaskNarrative, 0) {
		t.Fatal("任务预算只作用于该任务，其它任务不受影响")
	}

	// 联系人级日预算：某联系人超 per-contact 上限应被拦，且不影响其它联系人。
	conDB := regressionDB(t)
	cc := NewLLMClient(&Config{}).WithDB(conDB)
	if err := saveLLMSettings(conDB, LLMSettings{PerContactDailyTokenBudget: 300}); err != nil {
		t.Fatal(err)
	}
	cc.logLLMCallFull(llmSpec{Model: "m"}, string(TaskSummary), 42, true, 200, llmUsage{Total: 400}, 1, false, false)
	if llmBudgetAllowsCall(context.Background(), conDB, TaskSummary, 42) {
		t.Fatal("联系人日预算用尽应拦该联系人的后台调用")
	}
	if !llmBudgetAllowsCall(context.Background(), conDB, TaskSummary, 43) {
		t.Fatal("联系人预算不应波及其它联系人")
	}
}

func TestCacheHitLoggedSeparatelyFromRealCalls(t *testing.T) {
	db := regressionDB(t)
	c := NewLLMClient(&Config{}).WithDB(db)
	spec := llmSpec{Model: "m", ProfileID: "p1", Provider: "deepseek"}
	// 一次真实调用 1000 token + 两次缓存命中（零消耗）。
	c.logLLMCallFull(spec, string(TaskSummary), 9, true, 200, llmUsage{Total: 1000}, 50, false, false)
	c.logCacheHit(spec, string(TaskSummary), 9)
	c.logCacheHit(spec, string(TaskSummary), 9)

	usage, err := ComputeLLMUsage(db)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Today.Calls != 1 {
		t.Fatalf("真实 calls 只应算 cache_hit=0（1 次），实得 %d", usage.Today.Calls)
	}
	if usage.Today.TotalTokens != 1000 {
		t.Fatalf("真实 tokens 不应把缓存命中的零消耗计入其它，实得 %d", usage.Today.TotalTokens)
	}
	if usage.Today.CacheHits != 2 {
		t.Fatalf("缓存命中应计 2，实得 %d", usage.Today.CacheHits)
	}
	// ByContact 归因：contact 9 有 1 真实调用 + 2 命中。
	var found bool
	for _, bc := range usage.ByContact {
		if bc.ContactID == 9 {
			found = true
			if bc.Calls != 1 || bc.CacheHits != 2 {
				t.Fatalf("contact 9 归因应 calls=1 cacheHits=2，实得 %+v", bc)
			}
		}
	}
	if !found {
		t.Fatalf("ByContact 应含 contact 9，实得 %+v", usage.ByContact)
	}
}

func TestByContactDeletedContactFallbackName(t *testing.T) {
	db := regressionDB(t)
	c := NewLLMClient(&Config{}).WithDB(db)
	// 归因到一个不存在的联系人（已删除语义）。
	c.logLLMCallFull(llmSpec{Model: "m"}, string(TaskSummary), 999, true, 200, llmUsage{Total: 100}, 10, false, false)
	usage, _ := ComputeLLMUsage(db)
	var name string
	for _, bc := range usage.ByContact {
		if bc.ContactID == 999 {
			name = bc.Name
		}
	}
	if !strings.Contains(name, "已删除") || !strings.Contains(name, "999") {
		t.Fatalf("已删除联系人应回落「(已删除#999)」不丢成本，实得 %q", name)
	}
}

func TestWeeklyMonthlyWindowStartSemantics(t *testing.T) {
	// 周一为周首：本机跑测试的当天，startOfWeekUnix 必须落在本周内且不晚于今日零点。
	today := startOfTodayUnix()
	week := startOfWeekUnix()
	month := startOfMonthUnix()
	if week > today {
		t.Fatalf("本周起点不得晚于今日零点：week=%d today=%d", week, today)
	}
	if month > today {
		t.Fatalf("本月起点不得晚于今日零点：month=%d today=%d", month, today)
	}
	if month > week && week > today {
		t.Fatal("窗口起点单调性异常")
	}
	// 校验 weekday：把起点还原为时间，必须是周一。
	wt := time.Unix(week, 0)
	if wt.Weekday() != time.Monday {
		t.Fatalf("周起点应是周一，实得 %v", wt.Weekday())
	}
	mt := time.Unix(month, 0)
	if mt.Day() != 1 {
		t.Fatalf("月起点应是 1 号，实得 %d 号", mt.Day())
	}
}

func TestModelPolicyViewsCacheFromRegistry(t *testing.T) {
	db := routerDB(t)
	s, _ := loadLLMSettings(db)
	views := ModelPolicyViews(s)
	if len(views) == 0 {
		t.Fatal("视图应覆盖全部注册任务")
	}
	byTask := map[ContextTask]ModelPolicyView{}
	for _, v := range views {
		byTask[v.Task] = v
		// CacheEnabled 必须等于 registry 的 Cacheable（单一来源，不另建）。
		if v.CacheEnabled != taskSpec(v.Task).Cacheable {
			t.Fatalf("任务 %s 的 CacheEnabled 应取 registry.Cacheable，实得 %v vs %v", v.Task, v.CacheEnabled, taskSpec(v.Task).Cacheable)
		}
	}
	// 已配置策略的任务应标 Configured，并回带 primary/fallback。
	if v, ok := byTask[TaskNarrative]; ok && v.Registered {
		if v.SuggestedTier == "" {
			t.Fatal("视图应带 §5.2 建议档位")
		}
	}
}

func TestModelPolicyViewsIncludesConfiguredUnregistered(t *testing.T) {
	s := LLMSettings{TaskPolicies: map[string]TaskModelPolicy{"legacy_task": {PrimaryProfileID: "p-x"}}}
	views := ModelPolicyViews(s)
	var sawLegacy bool
	for _, v := range views {
		if v.Task == ContextTask("legacy_task") {
			sawLegacy = true
			if v.Registered || !v.Configured || v.PrimaryProfile != "p-x" {
				t.Fatalf("未登记但已配置的任务应如实呈现，实得 %+v", v)
			}
		}
	}
	if !sawLegacy {
		t.Fatal("已配置但未登记的任务不应被隐藏")
	}
}

func TestRouterPUTClearsWhenEmptyAndPersists(t *testing.T) {
	db := routerDB(t)
	// 先写策略，再清空：TaskPolicies 归 nil（映射 TaskPolicies==nil 语义），保存往返稳定。
	s, _ := loadLLMSettings(db)
	s.TaskPolicies = map[string]TaskModelPolicy{string(TaskNarrative): {PrimaryProfileID: "p-local"}}
	if err := saveLLMSettings(db, s); err != nil {
		t.Fatal(err)
	}
	if r := NewLLMClient(&Config{}).WithDB(db).resolveRoute(TaskNarrative); r.Primary.ProfileID != "p-local" {
		t.Fatal("写入后 primary 应生效")
	}
	s2, _ := loadLLMSettings(db)
	s2.TaskPolicies = nil
	if err := saveLLMSettings(db, s2); err != nil {
		t.Fatal(err)
	}
	if r := NewLLMClient(&Config{}).WithDB(db).resolveRoute(TaskNarrative); r.Primary.ProfileID != "p-remote" {
		t.Fatalf("清空策略后应回落活动档案 p-remote，实得 %q", r.Primary.ProfileID)
	}
}

// loadLLMClientSettings 便于测试里就地改档案端点（绕开打码保存的密钥往返）。
func loadLLMClientSettings(db *sql.DB) (LLMSettings, error) { return loadLLMSettings(db) }

// stubCountingServer 返回一个总是成功的模型端点，并以 atomic 累计命中次数。
func stubCountingServer(t *testing.T, counter *atomic.Int64, content string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, content)
	}))
	t.Cleanup(server.Close)
	return server
}

// countLogRows 跑一个 COUNT 查询（测试内自省用量日志）。q 须自包含 FROM llm_call_log。
func countLogRows(t *testing.T, db *sql.DB, q string) int64 {
	t.Helper()
	if err := ensureLLMCallLogTable(db); err != nil {
		t.Fatal(err)
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	var n int64
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", q, err)
	}
	return n
}

// policyKeys 返回策略 map 的键切片（现有 keysOf 只收 map[string]any，此处专用于 TaskPolicies）。
func policyKeys(m map[string]TaskModelPolicy) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
