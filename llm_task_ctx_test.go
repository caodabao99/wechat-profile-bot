package main

// v6.3 P2a/P2b 任务级用量归因与成本面板验收。
//
// 钉死五件事（都是可执行断言，非文档声称）：
//  1. task 经 ctx 传播，接管原语 callLLMCached 一行不改签名就让 14 个调用点带上归因；
//  2. 缓存命中不产生用量日志（统计口径 = 真实 API 消耗），二次调用不应多出计数；
//  3. 未接管的裸调点记为「未归因」并照常计入分组——不隐藏成本，且任务合计必须与窗口总量对得上；
//  4. 老库（llm_call_log 无 task 列）经懒建路径自动补列，历史行回落空串而非报错；
//  5. 聚合行带人类可读标签且源自 registry，未登记/未归因如实标注（前端不存在第二份映射）。

import (
	"context"
	"strings"
	"testing"
)

func TestWithLLMTaskRoundTrip(t *testing.T) {
	ctx := withLLMTask(context.Background(), TaskNarrative)
	if got := llmTaskFromContext(ctx); got != string(TaskNarrative) {
		t.Fatalf("ctx 应带回任务名，实得 %q", got)
	}
	// 空任务不写入：避免给未接管路径制造「空 task 值」噪声
	if got := llmTaskFromContext(withLLMTask(context.Background(), "")); got != "" {
		t.Fatalf("空任务不应写入 ctx，实得 %q", got)
	}
	if got := llmTaskFromContext(context.Background()); got != "" {
		t.Fatalf("无 ctx 值应返回空串，实得 %q", got)
	}
	if got := llmTaskFromContext(nil); got != "" {
		t.Fatalf("nil ctx 必须安全返回空串，实得 %q", got)
	}
}

// TestCallLLMCachedAttributesTask 端到端证归因来自真实调用链，并顺带证「命中不计数」。
func TestCallLLMCachedAttributesTask(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "归因")

	server, calls := stubLLMServer(t, "摘要内容")
	cfg := &Config{}
	cfg.LLM.BaseURL, cfg.LLM.ApiKey, cfg.LLM.Model = server.URL, "test-key", "stub-model"
	client := NewLLMClient(cfg).WithDB(db)

	if _, err := callLLMCached(context.Background(), db, client, id, TaskSummary, "", "同一 prompt"); err != nil {
		t.Fatal(err)
	}
	u, err := ComputeLLMUsage(db)
	if err != nil {
		t.Fatal(err)
	}
	if got := taskCallsByName(u, string(TaskSummary)); got != 1 {
		t.Fatalf("真调一次应记 task=%q 调用 1 次，实得 %d（byTask=%v）", TaskSummary, got, u.ByTask)
	}

	// 第二次同输入命中缓存：模型不应被再触达，用量日志也不应多出一行
	if _, err := callLLMCached(context.Background(), db, client, id, TaskSummary, "", "同一 prompt"); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("缓存命中不应触达模型，实得 calls=%d", n)
	}
	u2, _ := ComputeLLMUsage(db)
	if got := taskCallsByName(u2, string(TaskSummary)); got != 1 {
		t.Fatalf("缓存命中不得计入用量（口径=真实消耗），实得 %d", got)
	}
}

// TestUnattributedCallsRemainVisible 证「未归因」成组且总账对得上。
func TestUnattributedCallsRemainVisible(t *testing.T) {
	db := regressionDB(t)
	c := NewLLMClient(&Config{}).WithDB(db)
	spec := llmSpec{ProfileID: "p", Model: "m-x", Provider: "openai"}

	c.logLLMCall(spec, string(TaskNarrative), true, 200, llmUsage{Total: 7}, 10)
	c.logLLMCall(spec, "", true, 200, llmUsage{Total: 3}, 10) // 裸调点：无归因
	c.logLLMCall(spec, "", false, 500, llmUsage{}, 10)

	u, err := ComputeLLMUsage(db)
	if err != nil {
		t.Fatal(err)
	}
	if taskCallsByName(u, unattributedTaskKey) != 2 {
		t.Fatalf("未归因调用应聚合成一组且计 2 次，实得 %v", u.ByTask)
	}
	// 对账：各任务分组之和 == 窗口总量（证明没有行被丢掉）
	sum := 0
	for _, row := range u.ByTask {
		sum += row.Calls
	}
	if sum != u.Month.Calls {
		t.Fatalf("任务分组合计(%d)应与窗口总量(%d)一致，说明有调用未被归因视图覆盖", sum, u.Month.Calls)
	}
	for _, row := range u.ByTask {
		if row.Task == unattributedTaskKey && row.Registered {
			t.Fatal("未归因组不应被当作已登记任务")
		}
		if row.Task == string(TaskNarrative) && !row.Registered {
			t.Fatal("narrative 已登记，Registered 应为 true")
		}
	}
}

// TestEnsureCallLogAddsTaskColumnOnLegacyDB 证老库升级路径：先建 v6.2 形状的表（无 task 列）
// 并写入历史行，再走懒建函数，task 列应自动出现、历史行回落空串、写入带归因不报错。
func TestEnsureCallLogAddsTaskColumnOnLegacyDB(t *testing.T) {
	db := regressionDB(t)
	if _, err := db.Exec(`DROP TABLE IF EXISTS llm_call_log`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE llm_call_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL,
		profile_id TEXT NOT NULL DEFAULT '', profile_label TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
		region TEXT NOT NULL DEFAULT '', ok INTEGER NOT NULL DEFAULT 0,
		http_status INTEGER NOT NULL DEFAULT 0, prompt_tokens INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0,
		latency_ms INTEGER NOT NULL DEFAULT 0, used_proxy INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO llm_call_log (ts, model, ok) VALUES (strftime('%s','now'), 'legacy', 1)`); err != nil {
		t.Fatal(err)
	}

	if err := ensureLLMCallLogTable(db); err != nil {
		t.Fatalf("懒建/补列应成功: %v", err)
	}
	var cols int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('llm_call_log') WHERE name='task'`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 1 {
		t.Fatal("老库应被自动补上 task 列")
	}
	// 历史行回落空串 → 归入「未归因」而非丢失
	c := NewLLMClient(&Config{}).WithDB(db)
	c.logLLMCall(llmSpec{Model: "new"}, string(TaskTopic), true, 200, llmUsage{Total: 1}, 5)
	u, err := ComputeLLMUsage(db)
	if err != nil {
		t.Fatal(err)
	}
	if taskCallsByName(u, string(TaskTopic)) != 1 || taskCallsByName(u, unattributedTaskKey) != 1 {
		t.Fatalf("应同时看到新归因行与老行(未归因)，实得 %v", u.ByTask)
	}
}

// taskCallsByName 从聚合视图里取指定任务名的调用次数（无则 0）。
func taskCallsByName(u LLMUsage, name string) int {
	for _, row := range u.ByTask {
		if row.Task == name {
			return row.Calls
		}
	}
	return 0
}

// P2b 验收：任务聚合行带人类可读标签，且标签唯一来源是 registry；
// 未登记任务与未归因调用如实标注，不伪装成正常任务——
// 前端直显服务端标签，因此不得存在第二份映射。
func TestTaskUsageRowsCarryHonestLabels(t *testing.T) {
	db := regressionDB(t)
	c := NewLLMClient(&Config{}).WithDB(db)
	c.logLLMCall(llmSpec{Model: "m"}, string(TaskCoach), true, 200, llmUsage{Total: 3}, 5)
	c.logLLMCall(llmSpec{Model: "m"}, "legacy_task", true, 200, llmUsage{Total: 3}, 5)
	c.logLLMCall(llmSpec{Model: "m"}, "", true, 200, llmUsage{Total: 3}, 5)

	u, err := ComputeLLMUsage(db)
	if err != nil {
		t.Fatal(err)
	}
	label := map[string]string{}
	reg := map[string]bool{}
	for _, row := range u.ByTask {
		label[row.Task] = row.Label
		reg[row.Task] = row.Registered
	}
	// 已登记：标签必等于 registry 的 Label
	if got, want := label[string(TaskCoach)], taskSpec(TaskCoach).Label; got != want {
		t.Fatalf("coach 标签 = %q，应为 registry 的 %q", got, want)
	}
	if !reg[string(TaskCoach)] {
		t.Fatal("coach 应标为已登记")
	}
	// 未登记：保留原名并明确标注，不得显示为「默认」
	if got := label["legacy_task"]; got != "legacy_task（未登记）" {
		t.Fatalf("未登记任务标签 = %q，应带（未登记）后缀", got)
	}
	if reg["legacy_task"] {
		t.Fatal("legacy_task 不应被判为已登记")
	}
	// 未归因：文案不伪装成一个真任务
	if got := label[unattributedTaskKey]; got != "未接管调用（无任务归因）" {
		t.Fatalf("未归因标签 = %q", got)
	}
	// 任何行不得空标签（前端会渲染空白行）
	for _, row := range u.ByTask {
		if strings.TrimSpace(row.Label) == "" {
			t.Fatalf("任务 %q 标签为空", row.Task)
		}
	}
}
