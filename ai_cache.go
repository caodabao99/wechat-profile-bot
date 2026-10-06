package main

// AI 响应缓存（蓝图 §4.2 / §17）：以「语义键」复用 LLM 结果，命中即跳过一次模型调用。
//
// 键 = (contact_id, task, context_version, model, prompt_version)：
//   - context_version：认知输入指纹（profile/fact/state/goal/project/followup/new message 任一变化→变）；
//   - prompt_version：提示词模板正文指纹（DB 覆盖热编辑→变），与 context_version 职责正交；
//   - model：切换模型即失效，避免跨模型串用结果。
// 任一分量变化 → 主键变化 → 天然精确失效，无需显式清理。
//
// 失败哲学（与 LLM 双重降级一致）：缓存任何读写错误都当作「未命中 / 忽略」，绝不阻断业务、
// 绝不让本可完成的调用失败。缓存是尽力而为的加速层，不是正确性依赖。
//
// 锁纪律：沿用 getPromptOverride 的单层 dbMu 模式——自锁读取、持一层锁、查表存在再操作，
// 调用点必须处于「锁外」（与各 LLM 编排函数调 RenderPrompt 同一约束），绝不嵌套 dbMu。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
)

// shortHash 返回 s 的 sha256 前 16 字节 hex（32 字符）：稳定、短、可直接入缓存键。
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

// aiCacheKey 是 AI 响应缓存的语义主键（蓝图 §4.2）。
type aiCacheKey struct {
	ContactID      int64
	Task           string
	ContextVersion string
	Model          string
	PromptVersion  string
}

// valid 报告键是否完整（任一分量缺失即不启用缓存，避免误命中/脏写）。
func (k aiCacheKey) valid() bool {
	return k.ContactID > 0 && k.Task != "" && k.ContextVersion != "" && k.Model != "" && k.PromptVersion != ""
}

// modelOf 返回当前**活动**模型名（运行时解析；nil/空→"unknown"），用于缓存键：
// 切模型即改变键 → 跨模型结果天然不串用。
func (c *LLMClient) modelOf() string {
	if c == nil {
		return "unknown"
	}
	if m := strings.TrimSpace(c.resolveSpec().Model); m != "" {
		return m
	}
	return "unknown"
}

// aiCacheGet 查缓存命中并返回响应。任何错误（表缺失/无行/查询失败）都当作未命中，
// 不返回错误——保证本可完成的 LLM 调用不会因缓存层失败而中断。
func aiCacheGet(db *sql.DB, k aiCacheKey) (string, bool) {
	if !k.valid() {
		return "", false
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "ai_response_cache") {
		return "", false
	}
	var resp string
	err := db.QueryRow(`SELECT response FROM ai_response_cache
		WHERE contact_id=? AND task=? AND context_version=? AND model=? AND prompt_version=?`,
		k.ContactID, k.Task, k.ContextVersion, k.Model, k.PromptVersion).Scan(&resp)
	if err != nil {
		return "", false
	}
	return resp, true
}

// aiCachePut 写入/覆盖缓存。失败静默忽略（缓存是尽力而为的加速，写失败绝不影响主流程）。
func aiCachePut(db *sql.DB, k aiCacheKey, response string) {
	if !k.valid() || response == "" {
		return
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "ai_response_cache") {
		return
	}
	_, _ = db.Exec(`INSERT INTO ai_response_cache
		(contact_id, task, context_version, model, prompt_version, response, created_at)
		VALUES (?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(contact_id, task, context_version, model, prompt_version)
		DO UPDATE SET response=excluded.response, created_at=excluded.created_at`,
		k.ContactID, k.Task, k.ContextVersion, k.Model, k.PromptVersion, response)
}

// callLLMCached 带缓存的 LLM 调用（蓝图 §4.2 接管原语）：命中（相同 contact/task/上下文版本/
// 模型/最终 prompt）直接返回缓存，否则真调用并回写。
//
// 正确性锚点是「最终 prompt 的哈希」（存入 PromptVersion 位）：prompt = 模板(经 context 填充)，
// 故模板改动、context 输入改动、以及问题/草稿等即时输入变化都会改变 prompt → 改变键 → 不会误命中。
// context_version 作为可选的结构化分量（便于按认知快照归类）：传空则用哨兵 "n/a"，不削弱正确性。
//
// 降级哲学：context_version 恒有值故总启用缓存；缓存读写任何异常透明忽略；LLM 未配置或调用失败时
// 原样返回错误、绝不写缓存，保证本可完成的调用不被缓存层阻断（与 LLM 双重降级一致）。
func callLLMCached(ctx context.Context, db *sql.DB, llm *LLMClient, contactID int64, task ContextTask, contextVersion, prompt string) (string, error) {
	if llm == nil || !llm.configured() {
		return "", errors.New("LLM 未配置")
	}
	cv := contextVersion
	if cv == "" {
		cv = "n/a"
	}
	key := aiCacheKey{
		ContactID:      contactID,
		Task:           string(task),
		ContextVersion: cv,
		Model:          llm.modelOf(),
		PromptVersion:  shortHash(prompt),
	}
	if cached, ok := aiCacheGet(db, key); ok {
		return cached, nil
	}
	raw, err := llm.CallContext(ctx, prompt)
	if err != nil {
		return raw, err
	}
	aiCachePut(db, key, raw)
	return raw, nil
}
