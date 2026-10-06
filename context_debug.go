package main

// context_debug.go —— Context Debug 可观测层（蓝图 §5.4）。
//
// 目的：把「某任务在这个联系人身上到底取了什么上下文、渲染后值多少 token、缓存里有没有、
// 由哪个模型服务」变成可查询的事实，而不是散落在各模块的隐式行为。它消费的是既有
// Context Task Registry（§5.3）+ ContactContext + ai_response_cache，不新建引擎、不建表。
//
// 纪律（严格遵循蓝图禁止清单）：
//   - 隐私优先（§4.3）：本文件产出的全部是「计数 / 指纹 / 布尔 / 估算」，**绝不含消息正文**；
//     正文与 rendered 仍只由 config.contextDebug 闸门决定是否返回。
//   - 不假装精确：项目内无分词器，故 token 一律标注为**估算**（estimate），不冒充真实用量。
//   - 不假装命中：缓存主键含「最终 prompt 哈希」（prompt_version），而本端点不构建 prompt，
//     因此只能报告同 context_version 的**快照存在性**，绝不宣称「必然命中」。

import (
	"database/sql"
	"strings"
	"unicode"
)

// isCJK 报告该码位是否属于中日韩文字（估算时按「一字约一 token」计）。
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}

// estimateTokens 粗估文本 token 数：CJK 逐字计 1，其余非空白字符每 4 个计 1（向上取整）。
// 这是保守近似，用于「是否接近预算」的判断，不代表模型侧真实计费 token。
func estimateTokens(s string) int {
	var cjk, other int
	for _, r := range s {
		switch {
		case isCJK(r):
			cjk++
		case unicode.IsSpace(r):
			// 空白不计入
		default:
			other++
		}
	}
	return cjk + (other+3)/4
}

// oneIf 把布尔存在性折算成计数 1/0（供块计数表统一为「条数」语义）。
func oneIf(ok bool) int {
	if ok {
		return 1
	}
	return 0
}

// blockCounts 把 ContactContext 折算成「块 → 实取条数」的单一映射。
// contextSummary 与块计划共用此映射，避免两处各写一份而漂移。
func blockCounts(cc *ContactContext) map[ContextBlock]int {
	if cc == nil {
		return map[ContextBlock]int{}
	}
	topicWeeks := 0
	if cc.RecentTopics != nil {
		topicWeeks = len(cc.RecentTopics.Weeks)
	}
	return map[ContextBlock]int{
		BlockIdentity:          oneIf(strings.TrimSpace(cc.Identity.Name) != ""),
		BlockFacts:             len(cc.TrustedFacts),
		BlockEvidence:          len(cc.RelevantEvidence),
		BlockRelationshipState: oneIf(cc.CurrentRelationshipState != nil),
		BlockTimeline:          len(cc.RecentEvents),
		BlockGoals:             len(cc.ActiveGoals),
		BlockProjects:          len(cc.Projects),
		BlockFollowups:         len(cc.OpenFollowups),
		BlockTopics:            topicWeeks,
		BlockMetrics:           oneIf(cc.Metrics != nil),
		BlockRecentMessages:    len(cc.RecentMessages),
		BlockRelevantMessages:  len(cc.RelevantMessages),
		BlockPreviousActions:   len(cc.PreviousActions),
		BlockPreviousOutcomes:  len(cc.PreviousOutcomes),
		BlockActionLog:         len(cc.ActionLog),
	}
}

// contextBlockPlan 是注册表「计划」与实取「结果」的逐项对照（§5.4 核心可观测单元）。
type contextBlockPlan struct {
	Block   ContextBlock `json:"block"`
	Need    string       `json:"need"`    // required / optional / -（该任务未声明此块）
	Planned bool         `json:"planned"` // 注册表声明该任务需要此块
	Count   int          `json:"count"`   // 实取条数（0 = 空块）
	Filled  bool         `json:"filled"`  // count > 0
}

// buildBlockPlan 对照 taskSpec（计划）与 blockCounts（实取），并单独列出「声明必需却为空」的块
// ——这是排障时最有信号量的一项（例如 required=recent_messages 但一条没取到）。
func buildBlockPlan(cc *ContactContext) (plan []contextBlockPlan, missingRequired []ContextBlock) {
	spec := taskSpec(cc.Task)
	counts := blockCounts(cc)
	need := map[ContextBlock]string{}
	for _, b := range spec.Optional {
		need[b] = "optional"
	}
	for _, b := range spec.Required {
		need[b] = "required" // required 覆盖 optional（同一块两者同列时以 required 为准）
	}
	// 稳定顺序：先按 required 声明顺序，再 optional，最后其余已知块。
	seen := map[ContextBlock]bool{}
	appendBlock := func(b ContextBlock) {
		if seen[b] {
			return
		}
		seen[b] = true
		n, declared := need[b]
		if !declared {
			n = "-"
		}
		c := counts[b]
		plan = append(plan, contextBlockPlan{Block: b, Need: n, Planned: declared, Count: c, Filled: c > 0})
		if declared && n == "required" && c == 0 {
			missingRequired = append(missingRequired, b)
		}
	}
	for _, b := range spec.Required {
		appendBlock(b)
	}
	for _, b := range spec.Optional {
		appendBlock(b)
	}
	for b := range counts {
		appendBlock(b)
	}
	return plan, missingRequired
}

// aiCacheProbe 只读探查该「联系人 × 任务 × 上下文版本」的缓存快照状态。
// 绝不 SELECT response（正文不入可观测层）；任何错误（缺表/查询失败）都当作「无快照」，
// 与缓存层既有的尽力而为哲学一致。
func aiCacheProbe(db *sql.DB, contactID int64, task ContextTask, contextVersion, model string) map[string]any {
	out := map[string]any{
		"table_present":            false,
		"rows_for_task":            0,
		"distinct_versions":        0,
		"rows_for_version":         0,
		"latest_created_at":        "",
		"model_match_rows":         0,
		"hit_can_be_asserted":      false, // 本端点不构建 prompt → 不知 prompt_version → 不能断言命中
		"hit_assertion_limitation": "缓存主键含最终 prompt 哈希；可观测端点不构建 prompt，故仅报告同 context_version 的快照存在性，不宣称必然命中",
	}
	if db == nil || contactID <= 0 || task == "" {
		return out
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "ai_response_cache") {
		return out
	}
	out["table_present"] = true
	var rows, distinct int
	var latest sql.NullString
	if err := db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT context_version), MAX(created_at)
		FROM ai_response_cache WHERE contact_id=? AND task=?`, contactID, string(task)).
		Scan(&rows, &distinct, &latest); err == nil {
		out["rows_for_task"] = rows
		out["distinct_versions"] = distinct
		if latest.Valid {
			out["latest_created_at"] = latest.String
		}
	}
	if contextVersion != "" {
		var vr int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ai_response_cache
			WHERE contact_id=? AND task=? AND context_version=?`, contactID, string(task), contextVersion).
			Scan(&vr); err == nil {
			out["rows_for_version"] = vr
		}
		if strings.TrimSpace(model) != "" && model != "unknown" {
			var mr int
			if err := db.QueryRow(`SELECT COUNT(*) FROM ai_response_cache
				WHERE contact_id=? AND task=? AND context_version=? AND model=?`,
				contactID, string(task), contextVersion, model).Scan(&mr); err == nil {
				out["model_match_rows"] = mr
			}
		}
	}
	return out
}

// contextDebug 汇总 §5.4 的可观测字段（无私人正文），供默认（未开 debug）响应返回。
// rendered 为该上下文渲染后的提示词文本，仅用于本地 token 估算，**不进入本函数的返回值**。
// 由调用方渲染一次并复用，避免同一请求内重复构建文本。
func contextDebug(db *sql.DB, cc *ContactContext, llm *LLMClient, rendered string) map[string]any {
	if cc == nil {
		return map[string]any{}
	}
	spec := taskSpec(cc.Task)
	estTokens := estimateTokens(rendered)
	plan, missingRequired := buildBlockPlan(cc)
	model := llm.modelOf()
	return map[string]any{
		"task":             string(cc.Task),
		"label":            spec.Label,
		"registered":       isTaskRegistered(cc.Task), // false=未登记、回落默认规格（迁移期告警信号）
		"cacheable":        spec.Cacheable,
		"requires_llm":     spec.RequiresLLM,
		"llm_configured":   llm.configured(),
		"model":            model,
		"context_version":  cc.ContextVersion,
		"block_plan":       plan,
		"missing_required": missingRequired, // 声明必需却为空的块
		"token_estimate": map[string]any{
			"rendered_est":  estTokens, // 估算，非模型侧真实用量
			"budget_max":    cc.Budget.MaxTokens,
			"pct_of_budget": pctOf(estTokens, cc.Budget.MaxTokens),
			"truncated":     cc.Truncated,
			"estimate_only": true,
		},
		"cache": aiCacheProbe(db, cc.ContactID, cc.Task, cc.ContextVersion, model),
	}
}

// pctOf 计算 est 占 budget 的百分比（整数；budget<=0 时返回 0，避免除零）。
func pctOf(est, budget int) int {
	if budget <= 0 {
		return 0
	}
	return est * 100 / budget
}
