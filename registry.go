package main

// 数据层登记表（Data Layer Registry）—— 全库表元数据的单一事实来源。
//
// 背景：本项目的表分四类语义，但过去散落在 backup.go（backupTables / derivedTables /
// restoreSkipTables）、cleanup.go（删联系人要级联哪些表）、以及各模块的 ensureXxx 里，
// 靠注释与口口相传维持一致。模块越多越容易漂移，故集中成这一份可读、可查、可导出诊断的清单。
//
// 消费方：
//   - cleanup.go：contactCleanupTables 逐个用 GetTableMeta(name) != nil 作「此表已纳管」门槛，
//     避免对未登记表误发 DELETE。
//   - datareport.go：GET /api/system/table-registry 导出本清单，供运维核对表归属与备份策略。
//
// 四个正交轴（尽量如实反映当前运行行为，而非理想设计）：
//   Type   语义类别：core 用户真实数据 / derived 由 core 派生可重建 / cache 时效缓存 /
//          audit 操作日志 / config 全局配置。
//   Backup 是否随整库备份恢复（= 不属于 backup.go 的 restoreSkipTables）。
//   Rebuild 是否可全量重建（派生/缓存在缺数据时自愈重算）。
//   HasContactID 是否含单一 contact_id 列（决定删联系人时能否用 WHERE contact_id=? 级联清理）。
//          注意：contact_connections 用 contact_a/contact_b 双列，故此处为 false，
//          它由 contact.go 的专用 DELETE 处理，绝不能进 contactCleanupTables。

// TableMeta 单张表的登记信息。JSON 键与前端/运维脚本约定一致（camelCase）。
type TableMeta struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Owner        string `json:"owner"`
	Backup       bool   `json:"backup"`
	Rebuild      bool   `json:"rebuild"`
	HasContactID bool   `json:"hasContactId"`
	Note         string `json:"note,omitempty"`
}

// tableRegistry 是唯一的表清单数组（在 init 中填充）。新增表时在此登记，并与所属模块的
// ensureXxx 对齐；若新增了含 contact_id 的派生/用户表，还要评估是否纳入 cleanup.go 的
// contactCleanupTables。var 留空、init 赋值可让下方复合字面量缩进保持整洁。
var tableRegistry = []TableMeta{}

func init() {
	tableRegistry = []TableMeta{
		// —— 核心业务数据（随整库备份恢复，不可从别处重建）——
		{Name: "contacts", Type: "core", Owner: "contact", Backup: true, Rebuild: false, HasContactID: false, Note: "联系人主表，id 即 contact_id 本体"},
		{Name: "contact_aliases", Type: "core", Owner: "contact", Backup: true, Rebuild: false, HasContactID: true},
		{Name: "messages", Type: "core", Owner: "message", Backup: true, Rebuild: false, HasContactID: true, Note: "活跃消息表，一切派生的源"},
		{Name: "messages_archive", Type: "core", Owner: "archive", Backup: true, Rebuild: false, HasContactID: true, Note: "归档消息，统计须并入"},
		{Name: "profile_history", Type: "core", Owner: "profile", Backup: true, Rebuild: false, HasContactID: true, Note: "画像历史快照"},
		{Name: "merge_log", Type: "audit", Owner: "merge", Backup: true, Rebuild: false, HasContactID: false, Note: "合并记录（source_id/target_id），删联系人时专用 DELETE"},

		// —— 用户自建、含 contact_id 的业务表（随备份恢复，删联系人时级联清理）——
		{Name: "contact_tag_links", Type: "core", Owner: "tag", Backup: true, Rebuild: false, HasContactID: true, Note: "联系人↔标签关联"},
		{Name: "contact_tags", Type: "config", Owner: "tag", Backup: true, Rebuild: false, HasContactID: false, Note: "标签字典（全局，不按联系人关联）"},
		{Name: "followup_items", Type: "core", Owner: "followup", Backup: true, Rebuild: false, HasContactID: true},
		{Name: "relationship_goals", Type: "core", Owner: "goals", Backup: true, Rebuild: false, HasContactID: true, Note: "关系目标，用户创建非派生"},
		{Name: "relationship_projects", Type: "core", Owner: "projects", Backup: true, Rebuild: false, HasContactID: true, Note: "关系项目（Phase 5），目标之上的高层经营单元，用户创建非派生"},
		{Name: "contact_events", Type: "audit", Owner: "timeline", Backup: true, Rebuild: false, HasContactID: true, Note: "关系时间线事件"},

		// —— 游戏化 / 质量 / 情绪等派生状态表（含 contact_id，删联系人时清理；可重建但仍入备份）——
		{Name: "weekly_challenges", Type: "derived", Owner: "gamification", Backup: true, Rebuild: true, HasContactID: true, Note: "每周挑战进度"},
		{Name: "contact_achievements", Type: "derived", Owner: "achievements", Backup: true, Rebuild: true, HasContactID: true, Note: "关系成就"},
		{Name: "contact_quality_history", Type: "derived", Owner: "quality", Backup: true, Rebuild: true, HasContactID: true, Note: "对话质量周历史"},
		{Name: "assistant_emotions", Type: "derived", Owner: "assistant", Backup: true, Rebuild: true, HasContactID: true, Note: "对话情绪打分"},
		{Name: "gamification_state", Type: "derived", Owner: "gamification", Backup: true, Rebuild: true, HasContactID: false, Note: "全局 XP/等级状态"},

		// —— 派生/缓存表：不入备份（restoreSkipTables），恢复末尾清空，缺则自愈重建 ——
		{Name: "profile_facts", Type: "derived", Owner: "facts", Backup: false, Rebuild: true, HasContactID: true, Note: "可信画像事实，deriveFacts 产；含 FACT 生命周期（status/source_type/valid_from/valid_until/superseded_by/confidence_type/evidence_strength）"},
		{Name: "profile_fact_evidence", Type: "derived", Owner: "facts", Backup: false, Rebuild: true, HasContactID: true, Note: "事实证据，FK 依赖 profile_facts；含证据评估（match_type/support_strength/quote/is_direct_support）"},
		{Name: "relationship_daily_metrics", Type: "derived", Owner: "metrics", Backup: false, Rebuild: true, HasContactID: true, Note: "日粒度互动聚合，RebuildDailyMetrics 产"},
		{Name: "relationship_state", Type: "derived", Owner: "statemachine", Backup: false, Rebuild: true, HasContactID: true, Note: "关系状态机当前快照（base_state×dynamic_state），复用 ComputeHealth，RefreshRelationshipStates 产"},
		{Name: "relationship_state_history", Type: "derived", Owner: "statemachine", Backup: false, Rebuild: true, HasContactID: true, Note: "关系状态变迁事件（仅跨阈值才写），供回放/趋势回看"},
		{Name: "relationship_action_suggestions", Type: "derived", Owner: "relationship", Backup: false, Rebuild: true, HasContactID: true},
		{Name: "suggestion_outcomes", Type: "derived", Owner: "relationship", Backup: false, Rebuild: true, HasContactID: true, Note: "建议回测结果，FK 依赖 suggestions"},
		{Name: "contact_connections", Type: "derived", Owner: "network", Backup: false, Rebuild: true, HasContactID: false, Note: "关系连线（contact_a/contact_b 双列），不入 cleanup，由 contact.go 专用 DELETE"},
		{Name: "contact_topic_history", Type: "derived", Owner: "topics", Backup: true, Rebuild: true, HasContactID: true, Note: "联系人主题演化历史"},
		{Name: "topic_evolution_state", Type: "derived", Owner: "topics", Backup: true, Rebuild: true, HasContactID: false, Note: "主题演化全局游标状态"},
		{Name: "weekly_plan_cache", Type: "cache", Owner: "weekly_plan", Backup: false, Rebuild: true, HasContactID: false, Note: "每周维护计划缓存"},
		{Name: "life_state_cache", Type: "cache", Owner: "life_state", Backup: false, Rebuild: true, HasContactID: false, Note: "人生总览快照缓存"},
		{Name: "life_projection_cache", Type: "cache", Owner: "life_state", Backup: false, Rebuild: true, HasContactID: false, Note: "90 天推演快照缓存"},
		{Name: "network_insight_cache", Type: "cache", Owner: "network", Backup: false, Rebuild: true, HasContactID: false, Note: "社交网络洞察缓存"},
		{Name: "self_portrait_cache", Type: "cache", Owner: "portrait", Backup: false, Rebuild: true, HasContactID: false, Note: "自我关系画像缓存"},
		{Name: "intervention_cache", Type: "cache", Owner: "intervention", Backup: false, Rebuild: true, HasContactID: false, Note: "干预学习统计缓存"},
		{Name: "briefing_cache", Type: "cache", Owner: "briefing", Backup: false, Rebuild: true, HasContactID: false, Note: "主动简报缓存"},
		{Name: "today_snooze", Type: "cache", Owner: "today_aggregate", Backup: false, Rebuild: true, HasContactID: true, Note: "v6.3 §P11 TODAY 屏蔽表，过期行自动清理"},
		{Name: "ingest_stats", Type: "cache", Owner: "ingest_stats", Backup: false, Rebuild: true, HasContactID: true, Note: "v6.3 §P12 Smart Paste 去重统计，90天自动清理"},
		{Name: "ai_response_cache", Type: "cache", Owner: "context", Backup: false, Rebuild: true, HasContactID: true, Note: "AI 响应缓存，语义键=(contact_id,task,context_version,model,prompt_version)，蓝图 §4.2；按 contact_id 级联清理"},
		{Name: "relationship_action_log", Type: "audit", Owner: "actions", Backup: true, Rebuild: false, HasContactID: true, Note: "行动账本（蓝图 §5 P1）：跨来源(decision/coach/goal/project/calendar/manual)+八态生命周期+outcome/provenance 分离；记真实用户行为不可重建，故参与备份、按 contact_id 级联清理"},
		{Name: "relationship_experiment", Type: "audit", Owner: "experiment", Backup: true, Rebuild: false, HasContactID: true, Note: "个人关系实验（蓝图 §9 P5）：用户定义的观察性实验(目标/策略/周期)+前后测量快照与相关性结论；定义不可重建，故参与备份、按 contact_id 级联清理"},
		{Name: "insight_trend_history", Type: "derived", Owner: "trend", Backup: false, Rebuild: true, HasContactID: false, Note: "洞察趋势周历史"},

		// —— 全局配置（不按联系人关联，恢复时缺表则保留主库现状）——
		{Name: "assistant_settings", Type: "config", Owner: "assistant", Backup: true, Rebuild: false, HasContactID: false},
		{Name: "portfolio_settings", Type: "config", Owner: "portfolio", Backup: true, Rebuild: false, HasContactID: false, Note: "关系组合时间预算与类别权重/逐人覆盖（蓝图 §12 P8），用户自建不可重建，恢复缺表则保留主库"},
		{Name: "archive_settings", Type: "config", Owner: "archive", Backup: true, Rebuild: false, HasContactID: false},
		{Name: "mode_presets", Type: "config", Owner: "assistant", Backup: true, Rebuild: false, HasContactID: false, Note: "运行模式预设，用户自建"},
		{Name: "prompt_templates", Type: "config", Owner: "prompts", Backup: true, Rebuild: false, HasContactID: false, Note: "提示词模板，用户可编辑"},
		{Name: "llm_settings", Type: "config", Owner: "llm", Backup: true, Rebuild: false, HasContactID: false, Note: "模型与代理运行时配置（多档案/活动模型/推理开关/代理，v6.2），含密钥故参与备份、恢复缺表则保留主库"},

		// —— 审计日志（不参与业务恢复）——
		{Name: "backup_log", Type: "audit", Owner: "backup", Backup: false, Rebuild: false, HasContactID: false, Note: "备份/恢复审计日志，恢复时须保留自身"},
		{Name: "assistant_runs", Type: "audit", Owner: "assistant", Backup: true, Rebuild: false, HasContactID: false, Note: "助手调度运行记录"},
		{Name: "assistant_notified", Type: "audit", Owner: "assistant", Backup: true, Rebuild: false, HasContactID: false, Note: "推送去重账本"},
		{Name: "email_send_log", Type: "audit", Owner: "legacy", Backup: true, Rebuild: false, HasContactID: false, Note: "邮件发送日志（邮件推送功能已移除，遗留表）"},
		{Name: "llm_call_log", Type: "audit", Owner: "llm", Backup: true, Rebuild: false, HasContactID: false, Note: "模型调用量日志（v6.2）：每次真实 LLM 调用追加一行，供用量统计/状态页展示；无 contact_id、不可重建"},
	}
}

// TableRegistry 返回全库表元数据（值拷贝切片，供只读展示与运维诊断）。
func TableRegistry() []TableMeta {
	out := make([]TableMeta, len(tableRegistry))
	copy(out, tableRegistry)
	return out
}

// GetTableMeta 按名查表元数据；未登记返回 nil。cleanup.go 以「!= nil」判定此表是否已纳管。
func GetTableMeta(name string) *TableMeta {
	for i := range tableRegistry {
		if tableRegistry[i].Name == name {
			m := tableRegistry[i]
			return &m
		}
	}
	return nil
}

// IsContactScoped 报告某表是否可用 WHERE contact_id=? 做删联系人级联清理
// （已登记、含单一 contact_id 列、且非双列/全局表）。供 cleanup 与后续一致性校验复用。
func IsContactScoped(name string) bool {
	m := GetTableMeta(name)
	return m != nil && m.HasContactID
}
