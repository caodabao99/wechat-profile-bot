package main

// 任务级用量归因（蓝图 §6 AI Router 的第一块地基）。
//
// 问题：llm_call_log 记了「哪个模型、多少 token、成没成功」，却没记「为哪个 AI 任务」——
// 于是无法回答「叙事花了多少、摘要花了多少」，而任务级预算/路由/成本面板全都依赖这个维度。
//
// 做法（刻意选最小侵入）：task 经 context 传播，而非给 CallContext 加参数。
//   - callLLMCached 这一**单一接管原语**在真调前把 task 塞进 ctx → 已接管的 14 处调用点
//     自动获得归因，不必逐个改签名；
//   - 未接管的裸调点（ping、交互式改写/检查等）拿不到 task → 记为空串，据实呈现为「未归因」，
//     而不是伪造一个值。
//
// 纪律：归因是观测层，绝不参与业务判定；写失败静默忽略（与既有用量统计一致）。

import (
	"context"
)

// llmTaskCtxKey 是 ctx 中承载「本次调用所属 AI 任务」的键（私有类型，避免与第三方键冲突）。
type llmTaskCtxKey struct{}

// withLLMTask 把任务写入 ctx，供深层 CallContext 记录用量归因。task 为空时原样返回（不写噪声）。
func withLLMTask(ctx context.Context, task ContextTask) context.Context {
	if task == "" {
		return ctx
	}
	return context.WithValue(ctx, llmTaskCtxKey{}, string(task))
}

// llmTaskFromContext 取回本次调用的任务名；无则返回空串（=未归因）。
func llmTaskFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if s, ok := ctx.Value(llmTaskCtxKey{}).(string); ok {
		return s
	}
	return ""
}

// unattributedTaskKey 是聚合视图里代表「未归因调用」的分组名（不隐藏这部分成本）。
const unattributedTaskKey = "(未归因)"
