package main

// v6.3 §5.4 Context Debug 可观测层验收。
//
// 钉死三件「代码事实」，而非 README 声称：
//  1. 块计划与 Context Task Registry 单一来源一致（注册表改了，可观测层自动跟着改）；
//  2. 默认（未开 contextDebug）的可观测输出**绝不含消息正文**，而同上下文渲染后确实含正文
//     ——两者对照才证明隐私断言不是空断言；
//  3. 缓存探查只报告「同 context_version 的快照存在性」，恒不宣称必然命中。

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// seedRecentMessages 写入「当下时间」的互异消息，确保落在按天窗口内
// （regressionMessages 用固定旧时间戳，会被窗口挡在外面）。
func seedRecentMessages(t *testing.T, db *sql.DB, id int64, texts ...string) {
	t.Helper()
	ts := time.Now().Add(-time.Hour)
	for _, s := range texts {
		if _, err := SaveMessages(db, id, []Message{{Sender: "other", Content: s, Timestamp: ts}}); err != nil {
			t.Fatal(err)
		}
		ts = ts.Add(time.Minute)
	}
}

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
		why  string
	}{
		{"", 0, "空串"},
		{"你好世界", 4, "CJK 逐字计 1"},
		{"abcd", 1, "4 个非空白 ASCII 计 1"},
		{"abcde", 2, "不足 4 向上取整"},
		{"a b c d e f", 2, "空白不计"},
		{"你好 abcd", 3, "CJK 2 字计 2 + 拉丁 4 字符计 1"},
	}
	for _, c := range cases {
		if got := estimateTokens(c.in); got != c.want {
			t.Errorf("estimateTokens(%q)=%d，期望 %d（%s）", c.in, got, c.want, c.why)
		}
	}
}

// TestBlockPlanMatchesRegistry 证「块计划」派生自注册表：每个任务的 required/optional
// 与 taskSpec 完全一致，无多余、无遗漏。若有人在 registry 改声明而可观测层写死，本测试失败。
func TestBlockPlanMatchesRegistry(t *testing.T) {
	for _, task := range RegisteredTasks() {
		spec := taskSpec(task)
		cc := &ContactContext{Task: task, ContactID: 1}
		plan, _ := buildBlockPlan(cc)

		needOf := map[ContextBlock]string{}
		for _, p := range plan {
			needOf[p.Block] = p.Need
		}
		for _, b := range spec.Required {
			if needOf[b] != "required" {
				t.Fatalf("task=%q 块 %q 应为 required，实得 %q", task, b, needOf[b])
			}
		}
		optionalOnly := map[ContextBlock]bool{}
		for _, b := range spec.Optional {
			optionalOnly[b] = true
		}
		for _, b := range spec.Required {
			delete(optionalOnly, b) // required 优先，同块两者同列时不算 optional
		}
		for b := range optionalOnly {
			if needOf[b] != "optional" {
				t.Fatalf("task=%q 块 %q 应为 optional，实得 %q", task, b, needOf[b])
			}
		}
		for _, p := range plan {
			if p.Need == "-" {
				continue // 未声明块仅作计数披露，允许存在
			}
			if !shouldFetchBlock(task, p.Block) {
				t.Fatalf("task=%q 计划块 %q 不在注册表声明内（漂移）", task, p.Block)
			}
		}
	}
}

// TestBlockPlanSignalsMissingRequired 证最有信号量的一项：声明必需却为空的块被点名，
// 补齐数据后该告警消失。
func TestBlockPlanSignalsMissingRequired(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "块计划")

	cc, err := buildTaskContext(db, id, TaskNarrative, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, missing := buildBlockPlan(cc)
	if !containsBlock(missing, BlockRecentMessages) {
		t.Fatalf("空消息窗口内应点名 required 块 recent_messages，实得 %v", missing)
	}

	seedRecentMessages(t, db, id, "最近怎么样", "周末有空吗", "那个项目定了", "回头一起吃个饭")
	cc2, err := buildTaskContext(db, id, TaskNarrative, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	plan2, missing2 := buildBlockPlan(cc2)
	if containsBlock(missing2, BlockRecentMessages) {
		t.Fatalf("已有近期消息却仍报 recent_messages 缺失：%v", missing2)
	}
	var rm *contextBlockPlan
	for i := range plan2 {
		if plan2[i].Block == BlockRecentMessages {
			rm = &plan2[i]
		}
	}
	if rm == nil || !rm.Filled || rm.Count == 0 {
		t.Fatalf("块计划应显示 recent_messages 已填充且计数>0，实得 %+v", rm)
	}
}

func containsBlock(bs []ContextBlock, b ContextBlock) bool {
	for _, x := range bs {
		if x == b {
			return true
		}
	}
	return false
}

// TestContextDebugLeaksNoMessageContent 是 §5.4 的隐私主干：
// 先证明同一上下文的 rendered 里确实含有该私密串（断言非空转），
// 再证明可观测层（计数/指纹/估算）序列化后完全不含它。
func TestContextDebugLeaksNoMessageContent(t *testing.T) {
	const secret = "体检报告显示胃部有溃疡需要复查"
	db := regressionDB(t)
	id := regressionContact(t, db, "隐私可观测")
	seedRecentMessages(t, db, id, secret, "好的收到", "明天见", "谢谢")

	cc, err := buildTaskContext(db, id, TaskNarrative, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(RenderContextText(cc), secret) {
		t.Fatalf("前置不成立：rendered 未包含消息正文，本测试无法证明隐私（msgs=%d）", len(cc.RecentMessages))
	}

	obs := contextDebug(db, cc, nil, RenderContextText(cc))
	blob, err := json.Marshal(obs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), secret) {
		t.Fatalf("§5.4 可观测层泄漏消息正文：%s", string(blob))
	}
	// rendered 只用于本地估算，不得作为字段回流
	for k := range obs {
		if k == "rendered" || k == "context" {
			t.Fatalf("可观测层不应含字段 %q（正文外泄风险）", k)
		}
	}
}

// TestAICacheProbeReportsSnapshotNotHit 证缓存探查的诚实边界：
// 报告快照存在性（按 context_version / model 分层计数），但 hit_can_be_asserted 恒为 false。
func TestAICacheProbeReportsSnapshotNotHit(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "缓存探查")

	probe := aiCacheProbe(db, id, TaskNarrative, "cv-none", "m-1")
	if probe["table_present"] != true {
		t.Fatalf("表应存在（InitDB 已建），实得 %v", probe["table_present"])
	}
	if v, ok := probe["rows_for_task"].(int); !ok || v != 0 {
		t.Fatalf("新库应无快照，实得 %#v", probe["rows_for_task"])
	}

	// 先证「模型调用失败不写缓存」（缓存层既有降级哲学）：不可达域名→真调失败→快照仍为空。
	cfg := &Config{}
	cfg.LLM.BaseURL, cfg.LLM.ApiKey, cfg.LLM.Model = "https://example.invalid/v1", "k", "m-1"
	client := NewLLMClient(cfg)
	if _, err := callLLMCached(context.Background(), db, client, id, TaskNarrative, "cv-1", "同一 prompt"); err == nil {
		t.Fatal("不可达端点应报错（本测试依赖该前提验证失败不写缓存）")
	}
	if p := aiCacheProbe(db, id, TaskNarrative, "cv-1", "m-1"); p["rows_for_task"].(int) != 0 {
		t.Fatalf("模型调用失败不得写入缓存，实得快照 %v", p["rows_for_task"])
	}

	aiCachePut(db, aiCacheKey{ContactID: id, Task: string(TaskNarrative), ContextVersion: "cv-1", Model: "m-1", PromptVersion: shortHash("同一 prompt")}, "缓存响应内容")

	p := aiCacheProbe(db, id, TaskNarrative, "cv-1", "m-1")
	if v, ok := p["rows_for_task"].(int); !ok || v < 1 {
		t.Fatalf("rows_for_task 应 ≥1，实得 %#v", p["rows_for_task"])
	}
	if v, ok := p["rows_for_version"].(int); !ok || v != 1 {
		t.Fatalf("rows_for_version 应为 1，实得 %#v", p["rows_for_version"])
	}
	if v, ok := p["model_match_rows"].(int); !ok || v != 1 {
		t.Fatalf("model_match_rows 应为 1，实得 %#v", p["model_match_rows"])
	}
	if p["hit_can_be_asserted"] != false {
		t.Fatal("缓存命中不可被断言：hit_can_be_asserted 必须恒为 false")
	}
	// 响应正文绝不进可观测层
	blob, _ := json.Marshal(p)
	if strings.Contains(string(blob), "缓存响应内容") {
		t.Fatalf("缓存探查泄漏了 response 正文：%s", string(blob))
	}
}

// TestContactContextEndpointExposesObsWithoutContent 证接线：默认（debug=false）响应带 obs、
// 不带 rendered/context，且整份响应体不含正文；开 debug 时 obs 仍在。
func TestContactContextEndpointExposesObsWithoutContent(t *testing.T) {
	const secret = "合同细节对方要求下周前确认"
	db := regressionDB(t)
	id := regressionContact(t, db, "端点可观测")
	seedRecentMessages(t, db, id, secret, "收到", "好的", "明天聊")

	call := func(debug bool) (map[string]any, string) {
		s := &apiServer{db: db, cfg: &Config{ContextDebug: debug}}
		r := httptest.NewRequest("GET", "/api/contacts/1/context?task=narrative", nil)
		w := httptest.NewRecorder()
		s.routeContactContext(w, r, id)
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out, w.Body.String()
	}

	off, bodyOff := call(false)
	if _, ok := off["rendered"]; ok {
		t.Fatal("debug=false 不应返回 rendered")
	}
	obsAny, ok := off["obs"]
	if !ok {
		t.Fatalf("默认响应应带无正文可观测层 obs，实得键 %v", keysOf(off))
	}
	obs := obsAny.(map[string]any)
	for _, want := range []string{"block_plan", "token_estimate", "cache", "context_version", "model", "cacheable", "requires_llm", "registered"} {
		if _, ok := obs[want]; !ok {
			t.Fatalf("obs 缺字段 %q（§5.4 要求 source/version/token 估算/counts/cache 状态）", want)
		}
	}
	if len(obs["block_plan"].([]any)) == 0 {
		t.Fatal("obs.block_plan 不应为空")
	}
	if strings.Contains(bodyOff, secret) {
		t.Fatal("默认响应体泄漏消息正文")
	}

	on, _ := call(true)
	if _, ok := on["rendered"]; !ok {
		t.Fatal("debug=true 应返回 rendered")
	}
	if _, ok := on["obs"]; !ok {
		t.Fatal("debug=true 也应带 obs")
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
