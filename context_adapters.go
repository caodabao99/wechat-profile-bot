package main

// ═══════════════════════════════════════════════════════════════════════════
// §4.1 Context Adapter — legacy AI 功能的统一上下文入口（渐进「影子接管」第一步）
//
// 蓝图要求为 profile/ask/coach/simulation 增加 buildXxxContext，内部一律走
// BuildContactContext。本文件落地这四个适配器：它们产出**规范化、带 context_version
// 的认知快照**，作为这些功能唯一的上下文来源，但**不**在此处替换各模块自身的 prompt
// 文案生成——保持旧输出逐字不变、不一次性重写所有 prompt（蓝图 §4.1 红线）。
//
// 后续各 Phase 再逐模块把 prompt 迁移到消费这里的上下文对象 + callLLMCached 精确复用。
// ═══════════════════════════════════════════════════════════════════════════

import (
	"database/sql"
	"time"
)

// buildProfileContext 画像生成上下文（TaskProfile）。
func buildProfileContext(db *sql.DB, contactID int64, now time.Time) (*ContactContext, error) {
	return BuildContactContext(db, contactID, TaskProfile, "", now)
}

// buildAskContext 智能问答上下文（TaskAsk）；query 触发分层构造的「相关消息」臂。
func buildAskContext(db *sql.DB, contactID int64, query string, now time.Time) (*ContactContext, error) {
	return BuildContactContext(db, contactID, TaskAsk, query, now)
}

// buildCoachContext 关系教练上下文（TaskCoach）。
func buildCoachContext(db *sql.DB, contactID int64, now time.Time) (*ContactContext, error) {
	return BuildContactContext(db, contactID, TaskCoach, "", now)
}

// buildSimulationContext 回复推演上下文（TaskSimulation）；以 draft 作相关消息查询侧重。
func buildSimulationContext(db *sql.DB, contactID int64, draft string, now time.Time) (*ContactContext, error) {
	return BuildContactContext(db, contactID, TaskSimulation, draft, now)
}

// buildTaskContext 按任务分派到对应 legacy 适配器；其余任务回退通用 BuildContactContext。
// 统一入口（如 /context 端点）经此，确保四大任务都经由上述适配器收敛。
func buildTaskContext(db *sql.DB, contactID int64, task ContextTask, query string, now time.Time) (*ContactContext, error) {
	switch task {
	case TaskProfile:
		return buildProfileContext(db, contactID, now)
	case TaskAsk:
		return buildAskContext(db, contactID, query, now)
	case TaskCoach:
		return buildCoachContext(db, contactID, now)
	case TaskSimulation:
		return buildSimulationContext(db, contactID, query, now)
	default:
		return BuildContactContext(db, contactID, task, query, now)
	}
}
