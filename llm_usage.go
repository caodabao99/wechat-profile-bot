package main

// 模型调用量统计（v6.2）。每次真实 LLM 调用（含失败）在 llm_call_log 记一行；
// 提供 today/7d/30d 汇总 + 按模型分解的聚合查询，供网页端「模型与代理」页与状态页展示。
//
// 纪律：
//   - llm_call_log 为追加式分析日志（v7.0 起携 contact_id / cache_hit / fallback 供多维归因）
//     → registry Type=audit、Backup=false、不可重建；不进 derivedTables/contactCleanupTables/
//     restorePreserveWhenAbsent；不进 user_version 迁移（懒建 + 守门 ALTER）。
//   - 记录/读取各自自锁 dbMu（单层），调用点（CallContext）处于锁外，绝不嵌套。
//   - 缓存命中零真实 API 消耗，但仍记一行 cache_hit=1、tokens=0，供命中率与「缓存省下多少」观测。

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// llmUsage 一次调用返回的 token 用量（缺失时为零值）。
type llmUsage struct {
	Prompt     int
	Completion int
	Total      int
}

// ensureLLMCallLogTable 懒建用量日志表（幂等 DDL，自持 dbMu）。
// 老库已存在本表而无 task 列（v6.3 后才加）→ 用 pragma_table_info 守门做一次 ALTER，
// 与仓内既有用量列迁移同一手法；失败只影响归因、绝不阻断主流程。
func ensureLLMCallLogTable(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS llm_call_log (
		id                INTEGER PRIMARY KEY AUTOINCREMENT,
		ts                INTEGER NOT NULL,          -- unix 秒（时区无关，便于窗口统计）
		task              TEXT    NOT NULL DEFAULT '', -- 所属 AI 任务（§6 归因；空=未接管路径的裸调）
		profile_id        TEXT    NOT NULL DEFAULT '',
		profile_label     TEXT    NOT NULL DEFAULT '',
		provider          TEXT    NOT NULL DEFAULT '',
		model             TEXT    NOT NULL DEFAULT '',
		region            TEXT    NOT NULL DEFAULT '',
		ok                INTEGER NOT NULL DEFAULT 0, -- 1 成功 0 失败
		http_status       INTEGER NOT NULL DEFAULT 0,
		prompt_tokens     INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens      INTEGER NOT NULL DEFAULT 0,
		latency_ms        INTEGER NOT NULL DEFAULT 0,
		used_proxy        INTEGER NOT NULL DEFAULT 0,
		contact_id        INTEGER NOT NULL DEFAULT 0, -- v7.0 §6 按联系人归因（0=无联系人）
		cache_hit         INTEGER NOT NULL DEFAULT 0, -- 1=缓存命中（零真实 API 消耗）
		fallback          INTEGER NOT NULL DEFAULT 0  -- 1=primary 失败后走 fallback 档案
	)`)
	if err != nil {
		return err
	}
	if _, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_llm_call_log_ts ON llm_call_log(ts)`); err != nil {
		return err
	}
	// 已存在旧版表时逐个补列（pragma_table_info 守门，同一手法）。
	for _, col := range []struct{ name, ddl string }{
		{"task", `ALTER TABLE llm_call_log ADD COLUMN task TEXT NOT NULL DEFAULT ''`},
		{"contact_id", `ALTER TABLE llm_call_log ADD COLUMN contact_id INTEGER NOT NULL DEFAULT 0`},
		{"cache_hit", `ALTER TABLE llm_call_log ADD COLUMN cache_hit INTEGER NOT NULL DEFAULT 0`},
		{"fallback", `ALTER TABLE llm_call_log ADD COLUMN fallback INTEGER NOT NULL DEFAULT 0`},
	} {
		var colCount int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('llm_call_log') WHERE name=?`, col.name).Scan(&colCount); err != nil {
			return err
		}
		if colCount == 0 {
			if _, err := db.Exec(col.ddl); err != nil {
				return err
			}
		}
	}
	return nil
}

// logLLMCall 记一次真实调用用量（兼容旧签名：contact_id=0、非缓存命中、非 fallback）。
// db 为 nil（未接库的纯 config 客户端/测试）时静默跳过；任何写失败都不影响主流程。
func (c *LLMClient) logLLMCall(spec llmSpec, task string, ok bool, status int, u llmUsage, latencyMS int64) {
	c.logLLMCallFull(spec, task, 0, ok, status, u, latencyMS, false, false)
}

// logLLMCallFull 写一行完整归因：task/model/contact/ok/status/tokens/latency + cache_hit/fallback。
// task 为空串时据实记为未归因；contactID<=0 记 0；均不伪造。
func (c *LLMClient) logLLMCallFull(spec llmSpec, task string, contactID int64, ok bool, status int, u llmUsage, latencyMS int64, cacheHit, fallback bool) {
	if c.db == nil {
		return
	}
	if err := ensureLLMCallLogTable(c.db); err != nil {
		return
	}
	usedProxy := 0
	if spec.UseProxy && spec.ProxyURL != "" {
		usedProxy = 1
	}
	okInt, cacheInt, fbInt := 0, 0, 0
	if ok {
		okInt = 1
	}
	if cacheHit {
		cacheInt = 1
	}
	if fallback {
		fbInt = 1
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	_, _ = c.db.Exec(
		`INSERT INTO llm_call_log (ts, task, profile_id, profile_label, provider, model, region, ok, http_status, prompt_tokens, completion_tokens, total_tokens, latency_ms, used_proxy, contact_id, cache_hit, fallback)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().Unix(), task, spec.ProfileID, spec.Label, spec.Provider, spec.Model, spec.Region, okInt, status, u.Prompt, u.Completion, u.Total, latencyMS, usedProxy, contactID, cacheInt, fbInt)
}

// logCacheHit 记一次缓存命中：零真实 API 消耗，cache_hit=1，仅供命中率观测。
func (c *LLMClient) logCacheHit(spec llmSpec, task string, contactID int64) {
	c.logLLMCallFull(spec, task, contactID, true, 200, llmUsage{}, 0, true, false)
}

// LLMWindowStats 一个时间窗内的调用汇总。Calls/Success/Errors/Tokens 只统计真实 API 调用
// （cache_hit=0），保持「花了多少」口径不变；CacheHits/FallbackCalls 为 v7.0 新增观测维度。
type LLMWindowStats struct {
	Calls            int   `json:"calls"`
	Success          int   `json:"success"`
	Errors           int   `json:"errors"`
	TotalTokens      int64 `json:"totalTokens"`
	PromptTokens     int64 `json:"promptTokens"`
	CompletionTokens int64 `json:"completionTokens"`
	AvgLatencyMS     int64 `json:"avgLatencyMs"`
	ProxyCalls       int   `json:"proxyCalls"`
	CacheHits        int   `json:"cacheHits"`     // 窗内缓存命中次数（零真实消耗）
	FallbackCalls    int   `json:"fallbackCalls"` // 窗内 primary 失败后走 fallback 的成功次数
}

// LLMModelUsage 按模型聚合的一行。
type LLMModelUsage struct {
	Model        string `json:"model"`
	Provider     string `json:"provider"`
	Label        string `json:"label"`
	Calls        int    `json:"calls"`
	Success      int    `json:"success"`
	TotalTokens  int64  `json:"totalTokens"`
	AvgLatencyMS int64  `json:"avgLatencyMs"`
	LastUsedAt   int64  `json:"lastUsedAt"`
}

// LLMUsage 全量用量视图（三窗口 + 按模型分解 + 按任务分解 + 按联系人分解）。
type LLMUsage struct {
	Today     LLMWindowStats    `json:"today"`
	Week      LLMWindowStats    `json:"week"`
	Month     LLMWindowStats    `json:"month"`
	ByModel   []LLMModelUsage   `json:"byModel"`
	ByTask    []LLMTaskUsage    `json:"byTask"`    // §6 任务级归因（未接管路径归入「未归因」，不隐去成本）
	ByContact []LLMContactUsage `json:"byContact"` // §6 联系人级成本归因
}

// LLMTaskUsage 按 AI 任务聚合的一行（供任务级预算/路由决策与成本面板）。
type LLMTaskUsage struct {
	Task         string `json:"task"`
	Label        string `json:"label"`      // 人类可读名（单一来源仍为 registry，不在前端另建映射）
	Registered   bool   `json:"registered"` // 是否已在 Context Task Registry 登记（否则为裸调/遗留名）
	Calls        int    `json:"calls"`
	Success      int    `json:"success"`
	TotalTokens  int64  `json:"totalTokens"`
	AvgLatencyMS int64  `json:"avgLatencyMs"`
	LastUsedAt   int64  `json:"lastUsedAt"`
}

// LLMContactUsage 按联系人聚合的一行（§6 成本按联系人归因）。
type LLMContactUsage struct {
	ContactID   int64  `json:"contactId"`
	Name        string `json:"name"` // 库中名字；已删除联系人回落「(已删除#id)」
	Calls       int    `json:"calls"`
	TotalTokens int64  `json:"totalTokens"`
	CacheHits   int    `json:"cacheHits"`
	LastUsedAt  int64  `json:"lastUsedAt"`
}

// taskLabelFor 给聚合行配人类可读标签。标签唯一来源是 Context Task Registry；
// 未登记任务与未归因调用**如实标注**，不回落成「默认」以免看起来像个真任务。
func taskLabelFor(task string) string {
	if task == unattributedTaskKey {
		return "未接管调用（无任务归因）"
	}
	ct := ContextTask(task)
	if !isTaskRegistered(ct) {
		return task + "（未登记）"
	}
	return taskSpec(ct).Label
}

// windowStats 统计 [sinceUnix, ∞) 窗口内的聚合。真实 API 指标（calls/tokens/latency）只算
// cache_hit=0 的行；cache_hit/fallback 单独计数，不污染成本口径。
func windowStats(db *sql.DB, sinceUnix int64) (LLMWindowStats, error) {
	var w LLMWindowStats
	var sumLatency int64
	err := db.QueryRow(
		`SELECT
		    COALESCE(SUM(CASE WHEN cache_hit=0 THEN 1 ELSE 0 END),0),
		    COALESCE(SUM(CASE WHEN cache_hit=0 THEN ok ELSE 0 END),0),
		    COALESCE(SUM(CASE WHEN cache_hit=0 THEN 1-ok ELSE 0 END),0),
		    COALESCE(SUM(CASE WHEN cache_hit=0 THEN total_tokens ELSE 0 END),0),
		    COALESCE(SUM(CASE WHEN cache_hit=0 THEN prompt_tokens ELSE 0 END),0),
		    COALESCE(SUM(CASE WHEN cache_hit=0 THEN completion_tokens ELSE 0 END),0),
		    COALESCE(SUM(CASE WHEN cache_hit=0 THEN latency_ms ELSE 0 END),0),
		    COALESCE(SUM(CASE WHEN cache_hit=0 THEN used_proxy ELSE 0 END),0),
		    COALESCE(SUM(cache_hit),0),
		    COALESCE(SUM(CASE WHEN cache_hit=0 AND fallback=1 AND ok=1 THEN 1 ELSE 0 END),0)
		 FROM llm_call_log WHERE ts >= ?`, sinceUnix).
		Scan(&w.Calls, &w.Success, &w.Errors, &w.TotalTokens, &w.PromptTokens, &w.CompletionTokens, &sumLatency, &w.ProxyCalls, &w.CacheHits, &w.FallbackCalls)
	if err != nil {
		return w, err
	}
	if w.Calls > 0 {
		w.AvgLatencyMS = sumLatency / int64(w.Calls)
	}
	return w, nil
}

// ComputeLLMUsage 汇总近 30 天的模型调用量。表缺失或无数据返回空视图（不报错、不 500）。
func ComputeLLMUsage(db *sql.DB) (LLMUsage, error) {
	var out LLMUsage
	if err := ensureLLMCallLogTable(db); err != nil {
		return out, err
	}
	dbMu.Lock()
	defer dbMu.Unlock()

	now := time.Now().Unix()
	day := int64(86400)
	out.Today, _ = windowStats(db, startOfTodayUnix())
	out.Week, _ = windowStats(db, now-7*day)
	out.Month, _ = windowStats(db, now-30*day)

	rows, err := db.Query(
		`SELECT model, provider, MAX(profile_label), COUNT(*), COALESCE(SUM(ok),0),
		        COALESCE(SUM(total_tokens),0), COALESCE(SUM(latency_ms),0), MAX(ts)
		 FROM llm_call_log WHERE ts >= ? AND cache_hit=0
		 GROUP BY model, provider ORDER BY COUNT(*) DESC, MAX(ts) DESC LIMIT 50`, now-30*day)
	if err != nil {
		return out, nil // 聚合失败仍返回已算好的窗口
	}
	defer rows.Close()
	for rows.Next() {
		var m LLMModelUsage
		var sumLatency int64
		if err := rows.Scan(&m.Model, &m.Provider, &m.Label, &m.Calls, &m.Success, &m.TotalTokens, &sumLatency, &m.LastUsedAt); err != nil {
			continue
		}
		if m.Calls > 0 {
			m.AvgLatencyMS = sumLatency / int64(m.Calls)
		}
		out.ByModel = append(out.ByModel, m)
	}

	// 按任务分解（§6 归因）。空 task 统一映射为「未归因」而非丢掉，保证任务合计与窗口总量可对账。
	taskRows, err := db.Query(
		`SELECT COALESCE(NULLIF(task,''), ?), COUNT(*), COALESCE(SUM(ok),0),
		        COALESCE(SUM(total_tokens),0), COALESCE(SUM(latency_ms),0), MAX(ts)
		 FROM llm_call_log WHERE ts >= ? AND cache_hit=0
		 GROUP BY COALESCE(NULLIF(task,''), ?)
		 ORDER BY COUNT(*) DESC, MAX(ts) DESC LIMIT 64`, unattributedTaskKey, now-30*day, unattributedTaskKey)
	if err == nil {
		defer taskRows.Close()
		for taskRows.Next() {
			var t LLMTaskUsage
			var sumLatency int64
			if err := taskRows.Scan(&t.Task, &t.Calls, &t.Success, &t.TotalTokens, &sumLatency, &t.LastUsedAt); err != nil {
				continue
			}
			if t.Calls > 0 {
				t.AvgLatencyMS = sumLatency / int64(t.Calls)
			}
			t.Registered = isTaskRegistered(ContextTask(t.Task))
			t.Label = taskLabelFor(t.Task)
			out.ByTask = append(out.ByTask, t)
		}
	}

	// 按联系人分解（§6 成本归因）。只统计有联系人归因（contact_id>0）的行；名字从 contacts 联取，
	// 已删除联系人回落「(已删除#id)」，不丢成本。Calls 沿用全文件口径 = 真实 API 调用（cache_hit=0），
	// 缓存命中另记 CacheHits，保证成本可对账。
	contactRows, err := db.Query(
		`SELECT l.contact_id, COALESCE(c.name,''),
		        COALESCE(SUM(CASE WHEN l.cache_hit=0 THEN 1 ELSE 0 END),0),
		        COALESCE(SUM(CASE WHEN l.cache_hit=0 THEN l.total_tokens ELSE 0 END),0),
		        COALESCE(SUM(l.cache_hit),0), MAX(l.ts)
		 FROM llm_call_log l LEFT JOIN contacts c ON c.id = l.contact_id
		 WHERE l.ts >= ? AND l.contact_id > 0
		 GROUP BY l.contact_id
		 ORDER BY COUNT(*) DESC, MAX(l.ts) DESC LIMIT 50`, now-30*day)
	if err == nil {
		defer contactRows.Close()
		for contactRows.Next() {
			var cu LLMContactUsage
			if err := contactRows.Scan(&cu.ContactID, &cu.Name, &cu.Calls, &cu.TotalTokens, &cu.CacheHits, &cu.LastUsedAt); err != nil {
				continue
			}
			if strings.TrimSpace(cu.Name) == "" {
				cu.Name = fmt.Sprintf("(已删除#%d)", cu.ContactID)
			}
			out.ByContact = append(out.ByContact, cu)
		}
	}
	return out, nil
}

// startOfTodayUnix 返回本地时区今日零点的 unix 秒。
func startOfTodayUnix() int64 {
	t := time.Now()
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location()).Unix()
}

// startOfWeekUnix 返回本地时区本周一零点的 unix 秒（周预算窗口起点，蓝图 §6.1）。
// Go 的 Weekday 以周日=0 起始，故 (Weekday+6)%7 把周日归到上一周（周一为一周之首）。
func startOfWeekUnix() int64 {
	t := time.Now()
	y, m, d := t.Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, t.Location())
	days := (int(midnight.Weekday()) + 6) % 7 // 周一=0, 周日=6
	return midnight.AddDate(0, 0, -days).Unix()
}

// startOfMonthUnix 返回本地时区本月一日零点的 unix 秒（月预算窗口起点，蓝图 §6.1）。
func startOfMonthUnix() int64 {
	t := time.Now()
	y, mo, _ := t.Date()
	return time.Date(y, mo, 1, 0, 0, 0, 0, t.Location()).Unix()
}
