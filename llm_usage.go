package main

// 模型调用量统计（v6.2）。每次真实 LLM 调用（含失败）在 llm_call_log 记一行；
// 提供 today/7d/30d 汇总 + 按模型分解的聚合查询，供网页端「模型与代理」页与状态页展示。
//
// 纪律：
//   - llm_call_log 为追加式分析日志（无 contact_id、不可重建）→ registry Type=audit、Backup=false、
//     不进 derivedTables/contactCleanupTables/restorePreserveWhenAbsent；不进 user_version 迁移（懒建）。
//   - 记录/读取各自自锁 dbMu（单层），调用点（CallContext）处于锁外，绝不嵌套。
//   - 缓存命中不会记（CallContext 只在 cache miss 触发），故统计口径 = 真实 API 消耗，符合「花了多少」的语义。

import (
	"database/sql"
	"time"
)

// llmUsage 一次调用返回的 token 用量（缺失时为零值）。
type llmUsage struct {
	Prompt     int
	Completion int
	Total      int
}

// ensureLLMCallLogTable 懒建用量日志表（幂等 DDL，自持 dbMu）。
func ensureLLMCallLogTable(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS llm_call_log (
		id                INTEGER PRIMARY KEY AUTOINCREMENT,
		ts                INTEGER NOT NULL,          -- unix 秒（时区无关，便于窗口统计）
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
		used_proxy        INTEGER NOT NULL DEFAULT 0
	)`)
	if err == nil {
		_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_llm_call_log_ts ON llm_call_log(ts)`)
	}
	return err
}

// logLLMCall 记一次调用用量。db 为 nil（未接库的纯 config 客户端/测试）时静默跳过。
// 任何写失败都不影响主流程（统计是尽力而为的观测层）。
func (c *LLMClient) logLLMCall(spec llmSpec, ok bool, status int, u llmUsage, latencyMS int64) {
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
	okInt := 0
	if ok {
		okInt = 1
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	_, _ = c.db.Exec(
		`INSERT INTO llm_call_log (ts, profile_id, profile_label, provider, model, region, ok, http_status, prompt_tokens, completion_tokens, total_tokens, latency_ms, used_proxy)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().Unix(), spec.ProfileID, spec.Label, spec.Provider, spec.Model, spec.Region, okInt, status, u.Prompt, u.Completion, u.Total, latencyMS, usedProxy)
}

// LLMWindowStats 一个时间窗内的调用汇总。
type LLMWindowStats struct {
	Calls            int   `json:"calls"`
	Success          int   `json:"success"`
	Errors           int   `json:"errors"`
	TotalTokens      int64 `json:"totalTokens"`
	PromptTokens     int64 `json:"promptTokens"`
	CompletionTokens int64 `json:"completionTokens"`
	AvgLatencyMS     int64 `json:"avgLatencyMs"`
	ProxyCalls       int   `json:"proxyCalls"`
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

// LLMUsage 全量用量视图（三窗口 + 按模型分解）。
type LLMUsage struct {
	Today   LLMWindowStats  `json:"today"`
	Week    LLMWindowStats  `json:"week"`
	Month   LLMWindowStats  `json:"month"`
	ByModel []LLMModelUsage `json:"byModel"`
}

// windowStats 统计 [sinceUnix, ∞) 窗口内的聚合。
func windowStats(db *sql.DB, sinceUnix int64) (LLMWindowStats, error) {
	var w LLMWindowStats
	var sumLatency int64
	err := db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(ok),0), COALESCE(SUM(1-ok),0),
		        COALESCE(SUM(total_tokens),0), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
		        COALESCE(SUM(latency_ms),0), COALESCE(SUM(used_proxy),0)
		 FROM llm_call_log WHERE ts >= ?`, sinceUnix).
		Scan(&w.Calls, &w.Success, &w.Errors, &w.TotalTokens, &w.PromptTokens, &w.CompletionTokens, &sumLatency, &w.ProxyCalls)
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
		 FROM llm_call_log WHERE ts >= ?
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
	return out, nil
}

// startOfTodayUnix 返回本地时区今日零点的 unix 秒。
func startOfTodayUnix() int64 {
	t := time.Now()
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location()).Unix()
}
