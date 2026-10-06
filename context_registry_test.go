package main

import "testing"

// TestContextRegistryBudgetNoDrift 锁定 registry 预算与迁移前 budgetFor 的数值逐一一致，
// 防止「集中到注册表」时无意改动任一任务的上下文预算（蓝图 §5.3 要求单一来源但不改语义）。
func TestContextRegistryBudgetNoDrift(t *testing.T) {
	want := map[ContextTask]ContextBudget{
		TaskAsk:        {MaxMessages: 8, MaxRelevant: 24, MaxEvidence: 12, MaxEvents: 10, MaxTopics: 6, MaxWeeks: 4, MaxTokens: 1400},
		TaskNarrative:  {MaxMessages: 24, MaxRelevant: 12, MaxEvidence: 14, MaxEvents: 40, MaxTopics: 12, MaxWeeks: 12, MaxTokens: 2000},
		TaskReplay:     {MaxMessages: 24, MaxRelevant: 12, MaxEvidence: 14, MaxEvents: 40, MaxTopics: 12, MaxWeeks: 12, MaxTokens: 2000},
		TaskSimulation: {MaxMessages: 48, MaxRelevant: 8, MaxEvidence: 6, MaxEvents: 8, MaxTopics: 6, MaxWeeks: 4, MaxTokens: 1600},
		TaskDecision:   {MaxMessages: 6, MaxRelevant: 6, MaxEvidence: 8, MaxEvents: 6, MaxTopics: 4, MaxWeeks: 2, MaxTokens: 900},
		TaskCoach:      {MaxMessages: 12, MaxRelevant: 8, MaxEvidence: 8, MaxEvents: 10, MaxTopics: 6, MaxWeeks: 4, MaxTokens: 1000},
		TaskBriefing:   {MaxMessages: 12, MaxRelevant: 8, MaxEvidence: 8, MaxEvents: 10, MaxTopics: 6, MaxWeeks: 4, MaxTokens: 1000},
		TaskProfile:    {MaxMessages: 20, MaxRelevant: 10, MaxEvidence: 12, MaxEvents: 16, MaxTopics: 8, MaxWeeks: 8, MaxTokens: 1400},
	}
	for task, wb := range want {
		if got := budgetFor(task); got != wb {
			t.Errorf("budgetFor(%s) 漂移: got %+v want %+v", task, got, wb)
		}
	}
}

// TestContextRegistryIntegrity 校验注册表自洽：key 与 spec.Task 一致、必含新任务、未知回落默认。
func TestContextRegistryIntegrity(t *testing.T) {
	for task, spec := range contextTaskRegistry {
		if spec.Task != task {
			t.Errorf("registry key %q 与 spec.Task %q 不一致", task, spec.Task)
		}
		if spec.Label == "" {
			t.Errorf("任务 %q 缺少可读 Label", task)
		}
	}
	// 蓝图 §5.1/§5.3 点名的任务必须登记。
	for _, task := range []ContextTask{TaskProfile, TaskAsk, TaskCoach, TaskSimulation, TaskNarrative, TaskBriefing, TaskTopic, TaskDecision, TaskMemoryReview, TaskExperiment} {
		if !isTaskRegistered(task) {
			t.Errorf("任务 %q 未登记到 Context Task Registry", task)
		}
	}
	// 未知任务回落默认（等价旧 default 分支），且不 panic。
	unknown := ContextTask("__no_such_task__")
	if taskSpec(unknown).Budget.MaxMessages != 20 || isTaskRegistered(unknown) {
		t.Errorf("未知任务未正确回落默认预算/未注册判定异常")
	}
}

// TestShouldFetchBlock 校验块需求判定：required 命中、纯 optional 命中、不在集合内为 false。
func TestShouldFetchBlock(t *testing.T) {
	if !shouldFetchBlock(TaskAsk, BlockEvidence) {
		t.Errorf("Ask 应需要 evidence 块（required）")
	}
	if shouldFetchBlock(TaskAsk, BlockRelevantMessages) == false {
		t.Errorf("Ask 应需要 relevant_messages 块（required）")
	}
	// MemoryReview 不声明 followups 块。
	if shouldFetchBlock(TaskMemoryReview, BlockFollowups) {
		t.Errorf("MemoryReview 不应需要 followups 块")
	}
}
