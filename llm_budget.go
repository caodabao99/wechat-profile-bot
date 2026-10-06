package main

// v6.3 P2c 日预算护栏（§5.4 用量归因的下一步：从「看得见」到「拦得住」）。
//
// 三条刻意的、可验证的设计纪律：
//  1. 只拦「后台批量派生」，绝不拦用户当场发起的调用。预算的用途是止住自动任务半夜
//     把额度烧光，而不是让用户点开的按钮莫名报错。拦不拦由 ctx 上有没有「交互式」标记
//     决定：HTTP 入口与微信命令入口统一打标记，调度器/批处理用裸 ctx。
//     未标记一律视为后台（**失败方向保守**：漏标记只会多拦一层自动任务，不会误伤人）。
//  2. 预算不改变缓存语义：命中缓存零成本，因此先查缓存再判预算——已经算好的结果照样能取。
//  3. 不 503：超预算返回普通 error，调用方沿用既有的确定性降级路径（草稿留空、本轮跳过、
//     下次调度重试），绝不把「额度用尽」伪装成「服务不可用」。

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// llmInteractiveKey 是「本次调用由用户当场发起、正在等结果」的 ctx 标记键。
type llmInteractiveKey struct{}

// withInteractiveCall 给 ctx 打上交互式标记。
func withInteractiveCall(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, llmInteractiveKey{}, true)
}

// isInteractiveCall 读取标记；无 ctx、未标记都返回 false（= 按后台批量处理，可被预算拦）。
func isInteractiveCall(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	b, _ := ctx.Value(llmInteractiveKey{}).(bool)
	return b
}

// errDailyBudgetExceeded 后台批量调用因日预算用尽而被跳过。
var errDailyBudgetExceeded = errors.New("今日模型用量已达预算上限，后台任务本轮跳过（次日自动恢复）")

// LLMBudgetStatus 预算快照（v7.0 升级为日/周/月全局 + 告警位）：既是成本面板的展示数据，
// 也是判定的唯一实现（不留第二套口径）。Limit/UsedToday/Over 保留 v6.3 语义（=全局日）。
type LLMBudgetStatus struct {
	Limit     int64 `json:"limit"`     // 日上限 token；0=不限
	UsedToday int64 `json:"usedToday"` // 今日已用（只算真实调用，缓存命中不计）
	Remaining int64 `json:"remaining"` // 日剩余额度；不限时等于 0（配合 unlimited 读）
	Unlimited bool  `json:"unlimited"` // 未设任何预算（日/周/月均不限）
	Over      bool  `json:"over"`      // 日已用尽

	WeeklyLimit int64 `json:"weeklyLimit"` // 周上限；0=不限
	UsedWeek    int64 `json:"usedWeek"`
	WeeklyOver  bool  `json:"weeklyOver"`

	MonthlyLimit int64 `json:"monthlyLimit"` // 月上限；0=不限
	UsedMonth    int64 `json:"usedMonth"`
	MonthlyOver  bool  `json:"monthlyOver"`

	AnyOver bool `json:"anyOver"` // 任一已启用维度达 100%
	Warning bool `json:"warning"` // 任一已启用维度 >= 80%（§6.3）
}

// dailyTokenBudgetLimit 读库里的日上限；读失败按「不限制」处理（宁可多花，不可莫名停摆）。
func dailyTokenBudgetLimit(db *sql.DB) int64 {
	s, err := loadLLMSettings(db)
	if err != nil {
		return 0
	}
	return s.DailyTokenBudget
}

// todayTokensUsed 汇总今日真实调用的 total_tokens。
// 锁纪律：ensure* 自持 dbMu，取数前已释锁，此处单层加锁、不嵌套。
func todayTokensUsed(db *sql.DB) int64 {
	return tokensConsumedSince(db, startOfTodayUnix())
}

// tokensConsumedSince 汇总 [since,∞) 内真实调用（cache_hit=0）的 total_tokens。单层自锁、不嵌套。
func tokensConsumedSince(db *sql.DB, sinceUnix int64) int64 {
	if err := ensureLLMCallLogTable(db); err != nil {
		return 0
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	var n int64
	if err := db.QueryRow(
		`SELECT COALESCE(SUM(total_tokens), 0) FROM llm_call_log WHERE cache_hit=0 AND ts >= ?`,
		sinceUnix).Scan(&n); err != nil {
		return 0
	}
	return n
}

// tokensConsumedForTaskToday 今日该任务真实消耗（任务级预算，蓝图 §6.1 task 维度）。0 task 不查。
func tokensConsumedForTaskToday(db *sql.DB, task ContextTask) int64 {
	if task == "" {
		return 0
	}
	if err := ensureLLMCallLogTable(db); err != nil {
		return 0
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	var n int64
	if err := db.QueryRow(
		`SELECT COALESCE(SUM(total_tokens), 0) FROM llm_call_log WHERE cache_hit=0 AND ts >= ? AND task = ?`,
		startOfTodayUnix(), string(task)).Scan(&n); err != nil {
		return 0
	}
	return n
}

// tokensConsumedForContactToday 今日该联系人真实消耗（联系人级预算，蓝图 §6.1 contact 维度）。
func tokensConsumedForContactToday(db *sql.DB, contactID int64) int64 {
	if contactID <= 0 {
		return 0
	}
	if err := ensureLLMCallLogTable(db); err != nil {
		return 0
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	var n int64
	if err := db.QueryRow(
		`SELECT COALESCE(SUM(total_tokens), 0) FROM llm_call_log WHERE cache_hit=0 AND ts >= ? AND contact_id = ?`,
		startOfTodayUnix(), contactID).Scan(&n); err != nil {
		return 0
	}
	return n
}

// llmBudgetStatus 组装预算快照（日/周/月全局）。db 为 nil 或未设任何预算 → 视为不限制。
func llmBudgetStatus(db *sql.DB) LLMBudgetStatus {
	st := LLMBudgetStatus{Unlimited: true}
	if db == nil {
		return st
	}
	s, _ := loadLLMSettings(db)
	st.Limit = s.DailyTokenBudget
	st.WeeklyLimit = s.WeeklyTokenBudget
	st.MonthlyLimit = s.MonthlyTokenBudget
	st.UsedToday = todayTokensUsed(db)
	st.UsedWeek = tokensConsumedSince(db, startOfWeekUnix())
	st.UsedMonth = tokensConsumedSince(db, startOfMonthUnix())

	if st.Limit > 0 {
		st.Unlimited = false
		if st.UsedToday >= st.Limit {
			st.Over = true
		} else {
			st.Remaining = st.Limit - st.UsedToday
		}
		st.Warning = st.Warning || pctAtLeast(st.UsedToday, st.Limit, 80)
	}
	if st.WeeklyLimit > 0 {
		st.Unlimited = false
		st.WeeklyOver = st.UsedWeek >= st.WeeklyLimit
		st.Warning = st.Warning || pctAtLeast(st.UsedWeek, st.WeeklyLimit, 80)
	}
	if st.MonthlyLimit > 0 {
		st.Unlimited = false
		st.MonthlyOver = st.UsedMonth >= st.MonthlyLimit
		st.Warning = st.Warning || pctAtLeast(st.UsedMonth, st.MonthlyLimit, 80)
	}
	st.AnyOver = st.Over || st.WeeklyOver || st.MonthlyOver
	return st
}

// pctAtLeast 报告 used 是否已达 limit 的 pct%（整数乘除避免溢出：used*100 >= limit*pct）。
func pctAtLeast(used, limit int64, pct int64) bool {
	if limit <= 0 {
		return false
	}
	return used*100 >= limit*pct
}

// llmBudgetAllows 判定一次「即将真实消耗模型额度」的后台调用能否发起（仅全局日维度，保留 v6.3 语义）。
// 只在缓存未命中之后调用：命中不花钱，不该被预算挡住。
func llmBudgetAllows(ctx context.Context, db *sql.DB) bool {
	if isInteractiveCall(ctx) {
		return true // 用户在等结果：预算不介入
	}
	return !llmBudgetStatus(db).Over
}

// llmBudgetAllowsCall 是多维预算（日/周/月全局 + 任务日 + 联系人日）的统一判定入口。
// 交互调用一律放行；任一已启用维度达 100% → 拦后台非必要 LLM（蓝图 §6.3）。缓存命中在此之前已返回。
func llmBudgetAllowsCall(ctx context.Context, db *sql.DB, task ContextTask, contactID int64) bool {
	if isInteractiveCall(ctx) {
		return true
	}
	if db == nil {
		return true
	}
	if llmBudgetStatus(db).AnyOver {
		return false
	}
	s, _ := loadLLMSettings(db)
	if task != "" {
		if lim := s.TaskPolicies[string(task)].BudgetLimitTokens; lim > 0 && tokensConsumedForTaskToday(db, task) >= lim {
			return false
		}
	}
	if lim := s.PerContactDailyTokenBudget; lim > 0 && contactID > 0 && tokensConsumedForContactToday(db, contactID) >= lim {
		return false
	}
	return true
}

// newInteractiveCtx 给「无请求 ctx 可继承、但确实是用户当场发起」的入口造一个带标记的超时 ctx
// （如微信命令异步执行）。漏用只会让这类调用变得可被预算拦，不会反过来误伤后台任务。
func newInteractiveCtx(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(withInteractiveCall(context.Background()), timeout)
}
