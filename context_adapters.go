package main

// ═══════════════════════════════════════════════════════════════════════════
// §4.1 Context Adapter — AI 功能的统一上下文入口（v7.0 Context Engine 唯一入口化）
//
// 蓝图要求所有 AI 任务必须经由本文件适配器获取上下文，不得自行查询
// contact / profile_json / facts / projects / goals / metrics / recent messages。
//
// 模式：Task → BuildContactContext → Adapter → Prompt → Cache → Model
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

// buildNarrativeContext 关系叙事上下文（TaskNarrative）。
func buildNarrativeContext(db *sql.DB, contactID int64, now time.Time) (*ContactContext, error) {
	return BuildContactContext(db, contactID, TaskNarrative, "", now)
}

// buildSummaryContext 往来摘要上下文（TaskSummary）。
func buildSummaryContext(db *sql.DB, contactID int64, now time.Time) (*ContactContext, error) {
	return BuildContactContext(db, contactID, TaskSummary, "", now)
}

// buildFollowupContext 待跟进抽取上下文（TaskFollowup）。
func buildFollowupContext(db *sql.DB, contactID int64, now time.Time) (*ContactContext, error) {
	return BuildContactContext(db, contactID, TaskFollowup, "", now)
}

// buildTopicContext 主题演化上下文（TaskTopic）。
func buildTopicContext(db *sql.DB, contactID int64, now time.Time) (*ContactContext, error) {
	return BuildContactContext(db, contactID, TaskTopic, "", now)
}

// buildEmotionContext 情绪分析上下文（TaskEmotion）。
func buildEmotionContext(db *sql.DB, contactID int64, now time.Time) (*ContactContext, error) {
	return BuildContactContext(db, contactID, TaskEmotion, "", now)
}

// buildOutreachContext 开场白草稿上下文（TaskOutreach）。
func buildOutreachContext(db *sql.DB, contactID int64, now time.Time) (*ContactContext, error) {
	return BuildContactContext(db, contactID, TaskOutreach, "", now)
}

// buildRehearsalContext 对话彩排上下文（TaskRehearsal）。
func buildRehearsalContext(db *sql.DB, contactID int64, now time.Time) (*ContactContext, error) {
	return BuildContactContext(db, contactID, TaskRehearsal, "", now)
}

// buildIntentContext 意图分析上下文（TaskIntent）。
func buildIntentContext(db *sql.DB, contactID int64, now time.Time) (*ContactContext, error) {
	return BuildContactContext(db, contactID, TaskIntent, "", now)
}

// buildReplayContext 重新认识 TA 上下文（TaskReplay）。
func buildReplayContext(db *sql.DB, contactID int64, now time.Time) (*ContactContext, error) {
	return BuildContactContext(db, contactID, TaskReplay, "", now)
}

// buildDecisionContext 行动决策上下文（TaskDecision）。
func buildDecisionContext(db *sql.DB, contactID int64, now time.Time) (*ContactContext, error) {
	return BuildContactContext(db, contactID, TaskDecision, "", now)
}

// buildBlessingContext 日历祝福语上下文（TaskBlessing）。
func buildBlessingContext(db *sql.DB, contactID int64, now time.Time) (*ContactContext, error) {
	return BuildContactContext(db, contactID, TaskBlessing, "", now)
}

// buildRewriteContext 草稿改写上下文（TaskRewrite）。
func buildRewriteContext(db *sql.DB, contactID int64, now time.Time) (*ContactContext, error) {
	return BuildContactContext(db, contactID, TaskRewrite, "", now)
}

// buildDraftReviewContext 草稿歧义检查上下文（TaskDraftReview）。
func buildDraftReviewContext(db *sql.DB, contactID int64, now time.Time) (*ContactContext, error) {
	return BuildContactContext(db, contactID, TaskDraftReview, "", now)
}

// buildTaskContext 按任务分派到对应适配器；统一入口（如 /context 端点）经此。
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
	case TaskNarrative:
		return buildNarrativeContext(db, contactID, now)
	case TaskSummary:
		return buildSummaryContext(db, contactID, now)
	case TaskFollowup:
		return buildFollowupContext(db, contactID, now)
	case TaskTopic:
		return buildTopicContext(db, contactID, now)
	case TaskEmotion:
		return buildEmotionContext(db, contactID, now)
	case TaskOutreach:
		return buildOutreachContext(db, contactID, now)
	case TaskRehearsal:
		return buildRehearsalContext(db, contactID, now)
	case TaskIntent:
		return buildIntentContext(db, contactID, now)
	case TaskReplay:
		return buildReplayContext(db, contactID, now)
	case TaskDecision:
		return buildDecisionContext(db, contactID, now)
	case TaskBlessing:
		return buildBlessingContext(db, contactID, now)
	case TaskRewrite:
		return buildRewriteContext(db, contactID, now)
	case TaskDraftReview:
		return buildDraftReviewContext(db, contactID, now)
	default:
		return BuildContactContext(db, contactID, task, query, now)
	}
}
