package main

// V7 §18 AI Evaluation Lab + §19 缓存/Prompt/Model 治理 的离线确定性验收。
//
// 钉死两件事：
//  1. §18：黄金集覆盖全部任务类型；评分器 / 聚合报告 / 模型对比 / Prompt 回归在
//     「理想 model-a / 一般 model-b / 坏输出 model-c」三档样本上给出正确、可复现的判定。
//     评测全程零 LLM、零网络、零真实用户数据。
//  2. §19：AI 响应缓存的语义主键 = (contact_id, task, context_version, model, prompt_version)
//     五分量缺一不可——任一分量缺失即不启用缓存（防误命中/脏写），且切换模型/提示词会改变键，
//     跨模型、跨 Prompt 结果天然不串用。
//
// 指标命名对齐蓝图 §18.2，若将来重命名会同时打破这里的断言（正是我们想要的锁定）。

import (
	"database/sql"
	"strings"
	"testing"
)

// mustLoadGolden 载入黄金集，失败即致命。
func mustLoadGolden(t *testing.T) *GoldenSet {
	t.Helper()
	gs, err := LoadGoldenCases()
	if err != nil {
		t.Fatalf("LoadGoldenCases 失败：%v", err)
	}
	return gs
}

// §18.1 黄金集规模与覆盖面：≥20 例、≥11 类任务、每例三档样本齐备。
func TestGoldenSetCoversAllTaskTypes(t *testing.T) {
	gs := mustLoadGolden(t)
	if len(gs.Cases) < 20 {
		t.Fatalf("黄金用例应 ≥20，实得 %d", len(gs.Cases))
	}
	cats := map[string]bool{}
	for _, c := range gs.Cases {
		cats[c.Category] = true
		for _, m := range gs.Models {
			if _, ok := c.ModelOutputs[m]; !ok {
				t.Fatalf("用例 %q 缺模型 %q 的录制输出", c.ID, m)
			}
		}
	}
	if len(cats) < 11 {
		seen := make([]string, 0, len(cats))
		for k := range cats {
			seen = append(seen, k)
		}
		t.Fatalf("任务类型应 ≥11，实得 %d（%v）", len(cats), seen)
	}
	if len(gs.Models) != 3 {
		t.Fatalf("应恰好三档模型 model-a/b/c，实得 %v", gs.Models)
	}
}

// §18.2 理想模型 model-a 全过，且产出对齐蓝图的指标名。
func TestIdealModelScoresPerfect(t *testing.T) {
	gs := mustLoadGolden(t)
	r := RunEvaluation(gs, "model-a")
	scored := 0
	for _, c := range gs.Cases {
		if !c.Expect.Deterministic {
			scored++
		}
	}
	if r.TotalCases != scored {
		t.Fatalf("TotalCases=%d 应为 LLM 用例数 %d", r.TotalCases, scored)
	}
	if r.Accuracy != 1.0 {
		t.Fatalf("model-a 准确率应为 1.0，实得 %g（指标=%v）", r.Accuracy, r.Metrics)
	}
	if r.JSONValidity != 1.0 {
		t.Fatalf("model-a json_validity 应为 1.0，实得 %g", r.JSONValidity)
	}
	if r.Hallucination != 0 {
		t.Fatalf("model-a hallucination 应为 0，实得 %g", r.Hallucination)
	}
	// §18.2 关键指标名必须出现在聚合里。
	for _, m := range []string{
		MetricJSONValidity, MetricFieldCoverage, MetricSnippetRecall,
		MetricEvidenceAttribution, MetricHallucination,
	} {
		if _, ok := r.Metrics[m]; !ok {
			t.Fatalf("聚合缺指标 %q", m)
		}
	}
}

// 坏输出模型 model-c：JSON 合法率为 0，全部失败。
func TestBadModelFailsJSON(t *testing.T) {
	gs := mustLoadGolden(t)
	r := RunEvaluation(gs, "model-c")
	if r.JSONValidity != 0 {
		t.Fatalf("model-c json_validity 应为 0，实得 %g", r.JSONValidity)
	}
	if r.FailureRate != 1.0 {
		t.Fatalf("model-c 失败率应为 1.0，实得 %g", r.FailureRate)
	}
}

// 一般模型 model-b：介于两档之间（既有通过也有失败），证明评分器可区分质量梯度。
func TestMidModelSitsInBetween(t *testing.T) {
	gs := mustLoadGolden(t)
	r := RunEvaluation(gs, "model-b")
	if !(r.Accuracy > 0 && r.Accuracy < 1) {
		t.Fatalf("model-b 准确率应落在 (0,1) 区间，实得 %g", r.Accuracy)
	}
}

// §18.3 模型对比按准确率降序，理想模型排首位。
func TestCompareModelsRanking(t *testing.T) {
	gs := mustLoadGolden(t)
	rows := CompareModels([]*EvalReport{
		RunEvaluation(gs, "model-c"),
		RunEvaluation(gs, "model-b"),
		RunEvaluation(gs, "model-a"),
	})
	if len(rows) != 3 {
		t.Fatalf("应对比 3 个模型，实得 %d", len(rows))
	}
	if rows[0].Model != "model-a" {
		t.Fatalf("model-a 应排首位，实得 %q", rows[0].Model)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Accuracy < rows[i].Accuracy {
			t.Fatalf("排序非降序：%v", rows)
		}
	}
}

// §18.4 Prompt/模型回归：候选相对基线劣化超容差即标 REGRESSION 并列出劣化指标。
func TestDetectRegressionFlagsDegradation(t *testing.T) {
	gs := mustLoadGolden(t)
	baseline := RunEvaluation(gs, "model-a")
	candidate := RunEvaluation(gs, "model-c")
	res := DetectRegression(baseline, candidate, 0.05)
	if !res.IsRegression {
		t.Fatalf("理想→坏输出应判为回归")
	}
	if !strSliceHas(res.RegressedMetrics, MetricJSONValidity) {
		t.Fatalf("应列出 json_validity 回归，实得 %v", res.RegressedMetrics)
	}
	// 同模型自比不应回归。
	same := DetectRegression(baseline, RunEvaluation(gs, "model-a"), 0.05)
	if same.IsRegression {
		t.Fatalf("相同模型不应判为回归：%v", same.RegressedMetrics)
	}
}

// §19 缓存键语义有效性：五分量缺一不可。
func TestCacheKeyRequiresAllFiveComponents(t *testing.T) {
	full := aiCacheKey{ContactID: 1, Task: "fact_extraction", ContextVersion: "cv", Model: "m", PromptVersion: "pv"}
	if !full.valid() {
		t.Fatalf("完整键应判为 valid")
	}
	// 逐个削掉任一分量都必须失效。
	weaken := func(mut func(*aiCacheKey), field string) {
		k := full
		mut(&k)
		if k.valid() {
			t.Fatalf("缺失 %q 时键不应 valid", field)
		}
	}
	weaken(func(k *aiCacheKey) { k.ContactID = 0 }, "contact_id")
	weaken(func(k *aiCacheKey) { k.Task = "" }, "task")
	weaken(func(k *aiCacheKey) { k.ContextVersion = "" }, "context_version")
	weaken(func(k *aiCacheKey) { k.Model = "" }, "model")
	weaken(func(k *aiCacheKey) { k.PromptVersion = "" }, "prompt_version")
}

// §19 缓存主键治理：ai_response_cache 的 PRIMARY KEY 必须恰好是这五列，
// 从而保证切模型 / 改 Prompt 天然改变键、跨维度结果不串用。
func TestCachePrimaryKeyIsFiveComponents(t *testing.T) {
	db := regressionDB(t)
	var ddl string
	err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='ai_response_cache'`).Scan(&ddl)
	if err == sql.ErrNoRows {
		t.Fatalf("ai_response_cache 表不存在")
	}
	if err != nil {
		t.Fatalf("读取 DDL 失败：%v", err)
	}
	upper := strings.ToUpper(ddl)
	pkIdx := strings.Index(upper, "PRIMARY KEY")
	if pkIdx < 0 {
		t.Fatalf("缓存表无 PRIMARY KEY：%s", ddl)
	}
	pk := strings.ToLower(ddl[pkIdx:])
	for _, col := range []string{"contact_id", "task", "context_version", "model", "prompt_version"} {
		if !strings.Contains(pk, col) {
			t.Fatalf("主键缺列 %q：%s", col, pk)
		}
	}
}

func strSliceHas(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
