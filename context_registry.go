package main

// context_registry.go —— Context Task Registry（蓝图 §5.3）。
//
// 设计约束（严格遵循蓝图）：
//   - 不建数据库表，纯代码注册表。
//   - 单一事实来源：各 AI 任务的预算（原 budgetFor 的 switch）与所需上下文块
//     集中在此声明，budgetFor 委托本表，杜绝「预算散落在多处」。
//   - 本文件仅新增能力、不改变既有 BuildContactContext 的取块行为（向后兼容）；
//     shouldFetchBlock 供后续逐模块迁移时按需收敛查询（§5.2）使用。
//
// 术语：required = 该任务语义上必须存在的块；optional = 有则更好、可缺省降级。

// ContextBlock 标识 ContactContext 的一个组成块（用于按任务声明所需数据）。
type ContextBlock string

const (
	BlockIdentity          ContextBlock = "identity"
	BlockFacts             ContextBlock = "facts"
	BlockEvidence          ContextBlock = "evidence"
	BlockRelationshipState ContextBlock = "relationship_state"
	BlockTimeline          ContextBlock = "timeline"
	BlockGoals             ContextBlock = "goals"
	BlockProjects          ContextBlock = "projects"
	BlockFollowups         ContextBlock = "followups"
	BlockTopics            ContextBlock = "topics"
	BlockMetrics           ContextBlock = "metrics"
	BlockRecentMessages    ContextBlock = "recent_messages"
	BlockRelevantMessages  ContextBlock = "relevant_messages"
	BlockPreviousActions   ContextBlock = "previous_actions"
	BlockPreviousOutcomes  ContextBlock = "previous_outcomes"
	BlockActionLog         ContextBlock = "action_log"
)

// contextTaskSpec 单个 AI 任务的注册条目。
type contextTaskSpec struct {
	Task        ContextTask
	Label       string // 人类可读（Context Debug / 前端展示用）
	Required    []ContextBlock
	Optional    []ContextBlock
	Budget      ContextBudget // 与旧 budgetFor 完全一致的数值，迁移到此处做单一来源
	Cacheable   bool          // 是否走 ai_response_cache（false=每次都需新鲜结果，如强时效任务）
	RequiresLLM bool          // 是否需要 LLM（false=纯确定性任务）
}

// 补充任务常量：蓝图要求纳入 registry 的内存复核 / 主题演化 / 关系实验。
const (
	TaskMemoryReview ContextTask = "memory_review"
	TaskTopic        ContextTask = "topic"
	TaskExperiment   ContextTask = "experiment"
)

// allBlocks 便捷全量块集合（画像等重任务需要几乎全部上下文）。
var allBlocks = []ContextBlock{
	BlockIdentity, BlockFacts, BlockEvidence, BlockRelationshipState, BlockTimeline,
	BlockGoals, BlockProjects, BlockFollowups, BlockTopics, BlockMetrics,
	BlockRecentMessages, BlockPreviousActions, BlockPreviousOutcomes, BlockActionLog,
}

// contextTaskRegistry 是「任务 → 规格」的进程内注册表。key 必须与 ContextTask 值一致。
var contextTaskRegistry = map[ContextTask]contextTaskSpec{
	TaskProfile: {
		Task: TaskProfile, Label: "画像生成",
		Required:  []ContextBlock{BlockIdentity, BlockFacts, BlockRecentMessages},
		Optional:  allBlocks,
		Budget:    ContextBudget{MaxMessages: 20, MaxRelevant: 10, MaxEvidence: 12, MaxEvents: 16, MaxTopics: 8, MaxWeeks: 8, MaxTokens: 1400},
		Cacheable: true, RequiresLLM: true,
	},
	TaskAsk: {
		Task: TaskAsk, Label: "问答",
		Required:  []ContextBlock{BlockIdentity, BlockFacts, BlockEvidence, BlockRelevantMessages},
		Optional:  []ContextBlock{BlockRecentMessages, BlockRelationshipState, BlockTopics, BlockMetrics},
		Budget:    ContextBudget{MaxMessages: 8, MaxRelevant: 24, MaxEvidence: 12, MaxEvents: 10, MaxTopics: 6, MaxWeeks: 4, MaxTokens: 1400},
		Cacheable: true, RequiresLLM: true,
	},
	TaskCoach: {
		Task: TaskCoach, Label: "教练建议",
		Required:  []ContextBlock{BlockIdentity, BlockFacts, BlockRelationshipState},
		Optional:  []ContextBlock{BlockRecentMessages, BlockTimeline, BlockGoals, BlockProjects, BlockFollowups, BlockTopics, BlockMetrics, BlockPreviousActions, BlockActionLog},
		Budget:    ContextBudget{MaxMessages: 12, MaxRelevant: 8, MaxEvidence: 8, MaxEvents: 10, MaxTopics: 6, MaxWeeks: 4, MaxTokens: 1000},
		Cacheable: true, RequiresLLM: true,
	},
	TaskNarrative: {
		Task: TaskNarrative, Label: "关系叙事",
		Required:  []ContextBlock{BlockIdentity, BlockTimeline, BlockTopics, BlockRecentMessages},
		Optional:  allBlocks,
		Budget:    ContextBudget{MaxMessages: 24, MaxRelevant: 12, MaxEvidence: 14, MaxEvents: 40, MaxTopics: 12, MaxWeeks: 12, MaxTokens: 2000},
		Cacheable: true, RequiresLLM: true,
	},
	TaskSimulation: {
		Task: TaskSimulation, Label: "对话预演",
		Required:  []ContextBlock{BlockIdentity, BlockFacts, BlockRecentMessages},
		Optional:  []ContextBlock{BlockRelationshipState, BlockTopics, BlockEvidence},
		Budget:    ContextBudget{MaxMessages: 48, MaxRelevant: 8, MaxEvidence: 6, MaxEvents: 8, MaxTopics: 6, MaxWeeks: 4, MaxTokens: 1600},
		Cacheable: true, RequiresLLM: true,
	},
	TaskDecision: {
		Task: TaskDecision, Label: "行动决策",
		Required:  []ContextBlock{BlockIdentity, BlockRelationshipState, BlockMetrics, BlockFacts, BlockRecentMessages},
		Optional:  []ContextBlock{BlockTimeline, BlockGoals, BlockProjects, BlockFollowups, BlockPreviousOutcomes, BlockActionLog, BlockEvidence},
		Budget:    ContextBudget{MaxMessages: 6, MaxRelevant: 6, MaxEvidence: 8, MaxEvents: 6, MaxTopics: 4, MaxWeeks: 2, MaxTokens: 900},
		Cacheable: true, RequiresLLM: true,
	},
	TaskBriefing: {
		Task: TaskBriefing, Label: "主动简报",
		Required:  []ContextBlock{BlockIdentity, BlockRelationshipState, BlockMetrics},
		Optional:  []ContextBlock{BlockRecentMessages, BlockTimeline, BlockGoals, BlockProjects, BlockFollowups, BlockTopics},
		Budget:    ContextBudget{MaxMessages: 12, MaxRelevant: 8, MaxEvidence: 8, MaxEvents: 10, MaxTopics: 6, MaxWeeks: 4, MaxTokens: 1000},
		Cacheable: true, RequiresLLM: true,
	},
	TaskReplay: {
		Task: TaskReplay, Label: "重新认识 TA",
		Required:  []ContextBlock{BlockIdentity, BlockTimeline, BlockFacts, BlockEvidence},
		Optional:  allBlocks,
		Budget:    ContextBudget{MaxMessages: 24, MaxRelevant: 12, MaxEvidence: 14, MaxEvents: 40, MaxTopics: 12, MaxWeeks: 12, MaxTokens: 2000},
		Cacheable: true, RequiresLLM: true,
	},
	TaskMemoryReview: {
		Task: TaskMemoryReview, Label: "记忆复核",
		Required:  []ContextBlock{BlockIdentity, BlockFacts, BlockEvidence},
		Optional:  []ContextBlock{BlockRelationshipState, BlockTimeline},
		Budget:    ContextBudget{MaxMessages: 6, MaxRelevant: 6, MaxEvidence: 16, MaxEvents: 8, MaxTopics: 0, MaxWeeks: 2, MaxTokens: 800},
		Cacheable: false, RequiresLLM: false,
	},
	TaskTopic: {
		Task: TaskTopic, Label: "主题演化",
		Required:  []ContextBlock{BlockIdentity, BlockTopics, BlockRecentMessages},
		Optional:  []ContextBlock{BlockTimeline, BlockMetrics},
		Budget:    ContextBudget{MaxMessages: 16, MaxRelevant: 0, MaxEvidence: 4, MaxEvents: 8, MaxTopics: 12, MaxWeeks: 12, MaxTokens: 1200},
		Cacheable: true, RequiresLLM: true,
	},
	TaskExperiment: {
		Task: TaskExperiment, Label: "关系实验",
		Required:  []ContextBlock{BlockIdentity, BlockRelationshipState, BlockMetrics, BlockActionLog},
		Optional:  []ContextBlock{BlockPreviousOutcomes, BlockTimeline, BlockGoals},
		Budget:    ContextBudget{MaxMessages: 12, MaxRelevant: 8, MaxEvidence: 8, MaxEvents: 12, MaxTopics: 6, MaxWeeks: 4, MaxTokens: 1000},
		Cacheable: true, RequiresLLM: false,
	},
}

// defaultTaskSpec 未注册任务的安全兜底（等价旧 budgetFor default 分支）。
var defaultTaskSpec = contextTaskSpec{
	Task: "", Label: "默认",
	Required:  []ContextBlock{BlockIdentity, BlockFacts},
	Optional:  allBlocks,
	Budget:    ContextBudget{MaxMessages: 20, MaxRelevant: 10, MaxEvidence: 12, MaxEvents: 16, MaxTopics: 8, MaxWeeks: 8, MaxTokens: 1400},
	Cacheable: true, RequiresLLM: true,
}

// taskSpec 返回任务规格；未知任务回落 defaultTaskSpec（不 panic）。
func taskSpec(task ContextTask) contextTaskSpec {
	if spec, ok := contextTaskRegistry[task]; ok {
		return spec
	}
	return defaultTaskSpec
}

// isTaskRegistered 报告任务是否在注册表内（供迁移检查：新 AI 任务必须登记）。
func isTaskRegistered(task ContextTask) bool {
	_, ok := contextTaskRegistry[task]
	return ok
}

// shouldFetchBlock 判定某任务是否需要指定上下文块（required ∪ optional 命中即需要）。
// 供 BuildContactContext 迁移期按需收敛查询使用；当前不作为强制开关以保持向后兼容。
func shouldFetchBlock(task ContextTask, b ContextBlock) bool {
	spec := taskSpec(task)
	for _, x := range spec.Required {
		if x == b {
			return true
		}
	}
	for _, x := range spec.Optional {
		if x == b {
			return true
		}
	}
	return false
}

// RegisteredTasks 返回注册表内全部任务（供 Context Debug / 测试遍历）。
func RegisteredTasks() []ContextTask {
	out := make([]ContextTask, 0, len(contextTaskRegistry))
	for t := range contextTaskRegistry {
		out = append(out, t)
	}
	return out
}
