package main

// V7 §18 AI Evaluation Lab —— 可离线、确定性、零真实用户数据的评测基础设施。
//
// 为什么能离线：黄金用例把「模型在某输入上的输出样本」连同「期望」一起录进 fixtures，
// 评测器只做确定性的结构化判定（JSON 合法性、必填字段覆盖、期望片段召回、证据归属计数、
// 幻觉探针、时延/Token 采集）。接真实模型时，runner 用模型的实时输出替换 model_outputs 里的
// 样本即可复用同一套指标 / 模型对比 / Prompt 回归逻辑——评测口径与线上完全一致，但 CI 无需联网、
// 无需密钥、无需真实聊天数据。
//
// 单一来源：JSON 抽取直接复用 llm.go 的 ExtractJSON（全仓唯一的 LLM 输出取 JSON 逻辑），
// 绝不另写一份「剥代码围栏 + 找大括号」的样板。
//
// 指标命名对齐蓝图 §18.2；§18.3 模型对比、§18.4 Prompt 回归都是对聚合报告上的纯函数。

import (
	_ "embed"
	"encoding/json"
	"sort"
	"strings"
)

//go:embed tests/evaldata/golden_cases.json
var goldenCasesJSON []byte

// §18.2 指标名。
const (
	MetricJSONValidity        = "json_validity"
	MetricFieldCoverage       = "required_field_coverage" // 关系状态等结构化字段齐备度
	MetricSnippetRecall       = "snippet_recall"          // 事实/冲突/决策相关/上下文召回的期望命中
	MetricEvidenceAttribution = "evidence_attribution"    // 证据归属充分度
	MetricHallucination       = "hallucination"           // 越高越差（命中禁用断言）
)

// EvalExpect 是某用例的期望契约。
type EvalExpect struct {
	MustValidJSON     bool     `json:"must_be_valid_json"`
	RequiredFields    []string `json:"required_fields"`
	ExpectedSnippets  []string `json:"expected_snippets"`
	ForbiddenSnippets []string `json:"forbidden_snippets"`
	MinEvidenceRefs   int      `json:"min_evidence_refs"`
}

// EvalSample 是某模型在该用例上的录制输出（含时延/Token，供成本类指标）。
type EvalSample struct {
	Output    string `json:"output"`
	LatencyMs int    `json:"latency_ms"`
	Tokens    int    `json:"tokens"`
}

// GoldenCase 一条黄金用例。
type GoldenCase struct {
	ID           string                `json:"id"`
	Task         string                `json:"task"`
	Category     string                `json:"category"`
	Input        string                `json:"input"`
	Expect       EvalExpect            `json:"expect"`
	ModelOutputs map[string]EvalSample `json:"model_outputs"`
}

// GoldenSet 是整个黄金集。
type GoldenSet struct {
	Version string       `json:"version"`
	Models  []string     `json:"models"`
	Cases   []GoldenCase `json:"cases"`
}

// LoadGoldenCases 解析内嵌黄金集。
func LoadGoldenCases() (*GoldenSet, error) {
	var gs GoldenSet
	if err := json.Unmarshal(goldenCasesJSON, &gs); err != nil {
		return nil, err
	}
	return &gs, nil
}

// CaseScore 单用例 × 单模型的打分。
type CaseScore struct {
	CaseID    string
	Category  string
	Metrics   map[string]float64 // 仅含本例「适用」的指标
	Applied   map[string]bool
	Pass      bool
	JSONValid bool
	LatencyMs int
	Tokens    int
}

// scoreCase 对一条用例按某模型的录制输出做确定性评分。缺该模型输出 → 视为坏输出。
func scoreCase(c GoldenCase, model string) CaseScore {
	s := CaseScore{CaseID: c.ID, Category: c.Category, Metrics: map[string]float64{}, Applied: map[string]bool{}}
	sample, has := c.ModelOutputs[model]
	text := sample.Output
	extracted := ExtractJSON(text)
	valid := has && json.Valid([]byte(extracted))
	s.JSONValid = valid

	s.Applied[MetricJSONValidity] = true
	if valid {
		s.Metrics[MetricJSONValidity] = 1
	} else {
		s.Metrics[MetricJSONValidity] = 0
	}

	var obj map[string]interface{}
	if valid {
		_ = json.Unmarshal([]byte(extracted), &obj)
	}

	if len(c.Expect.RequiredFields) > 0 {
		s.Applied[MetricFieldCoverage] = true
		hit := 0
		for _, f := range c.Expect.RequiredFields {
			if _, ok := obj[f]; ok {
				hit++
			}
		}
		s.Metrics[MetricFieldCoverage] = evalRatio(hit, len(c.Expect.RequiredFields))
	}

	if len(c.Expect.ExpectedSnippets) > 0 {
		s.Applied[MetricSnippetRecall] = true
		low := strings.ToLower(text)
		hit := 0
		for _, sn := range c.Expect.ExpectedSnippets {
			if strings.Contains(low, strings.ToLower(sn)) {
				hit++
			}
		}
		s.Metrics[MetricSnippetRecall] = evalRatio(hit, len(c.Expect.ExpectedSnippets))
	}

	if c.Expect.MinEvidenceRefs > 0 {
		s.Applied[MetricEvidenceAttribution] = true
		n := 0
		if arr, ok := obj["evidence"].([]interface{}); ok {
			n = len(arr)
		}
		v := float64(n) / float64(c.Expect.MinEvidenceRefs)
		if v > 1 {
			v = 1
		}
		s.Metrics[MetricEvidenceAttribution] = v
	}

	if len(c.Expect.ForbiddenSnippets) > 0 {
		s.Applied[MetricHallucination] = true
		low := strings.ToLower(text)
		viol := 0
		for _, fb := range c.Expect.ForbiddenSnippets {
			if strings.Contains(low, strings.ToLower(fb)) {
				viol++
			}
		}
		if viol > 0 {
			s.Metrics[MetricHallucination] = 1
		} else {
			s.Metrics[MetricHallucination] = 0
		}
	}

	if has {
		s.LatencyMs = sample.LatencyMs
		s.Tokens = sample.Tokens
	}

	// Pass 判据：JSON 合法 + 字段全覆盖 + 期望片段全覆盖 + 证据充分 + 无幻觉。
	s.Pass = valid
	if c.Expect.MustValidJSON && !valid {
		s.Pass = false
	}
	if s.Applied[MetricFieldCoverage] && s.Metrics[MetricFieldCoverage] < 1 {
		s.Pass = false
	}
	if s.Applied[MetricSnippetRecall] && s.Metrics[MetricSnippetRecall] < 1 {
		s.Pass = false
	}
	if s.Applied[MetricEvidenceAttribution] && s.Metrics[MetricEvidenceAttribution] < 1 {
		s.Pass = false
	}
	if s.Applied[MetricHallucination] && s.Metrics[MetricHallucination] > 0 {
		s.Pass = false
	}
	return s
}

// MetricAgg 一项指标的聚合（均值 × 适用计数）。
type MetricAgg struct {
	Mean  float64
	Count int
}

// EvalReport 某模型在整套黄金集聚合后的评测报告。
type EvalReport struct {
	Model         string               `json:"model"`
	TotalCases    int                  `json:"totalCases"`
	PassedCases   int                  `json:"passedCases"`
	Accuracy      float64              `json:"accuracy"`
	FailureRate   float64              `json:"failureRate"`
	JSONValidity  float64              `json:"jsonValidity"`
	Hallucination float64              `json:"hallucination"`
	AvgLatencyMs  float64              `json:"avgLatencyMs"`
	TotalTokens   int                  `json:"totalTokens"`
	Metrics       map[string]MetricAgg `json:"metrics"`
}

// RunEvaluation 对某模型跑完整黄金集，产出聚合报告。
func RunEvaluation(gs *GoldenSet, model string) *EvalReport {
	r := &EvalReport{Model: model, Metrics: map[string]MetricAgg{}}
	latSum, tokSum, passed := 0, 0, 0
	for _, c := range gs.Cases {
		cs := scoreCase(c, model)
		r.TotalCases++
		latSum += cs.LatencyMs
		tokSum += cs.Tokens
		if cs.Pass {
			passed++
		}
		for m, v := range cs.Metrics {
			if !cs.Applied[m] {
				continue
			}
			agg := r.Metrics[m]
			agg.Mean += v
			agg.Count++
			r.Metrics[m] = agg
		}
	}
	r.PassedCases = passed
	r.Accuracy = evalRatio(passed, r.TotalCases)
	r.FailureRate = 1 - r.Accuracy
	r.AvgLatencyMs = evalRatio(latSum, r.TotalCases)
	r.TotalTokens = tokSum
	for m, agg := range r.Metrics {
		agg.Mean = agg.Mean / float64(agg.Count)
		r.Metrics[m] = agg
	}
	r.JSONValidity = r.Metrics[MetricJSONValidity].Mean
	r.Hallucination = r.Metrics[MetricHallucination].Mean
	return r
}

// ModelCompareRow §18.3 模型对比的一行。
type ModelCompareRow struct {
	Model        string  `json:"model"`
	Accuracy     float64 `json:"accuracy"`
	FailureRate  float64 `json:"failureRate"`
	AvgLatencyMs float64 `json:"avgLatencyMs"`
	TotalTokens  int     `json:"totalTokens"`
}

// CompareModels 汇总多模型对比，按准确率降序 → 失败率升序 → Token 升序 → 名升序（确定性）。
func CompareModels(reports []*EvalReport) []ModelCompareRow {
	rows := make([]ModelCompareRow, 0, len(reports))
	for _, r := range reports {
		rows = append(rows, ModelCompareRow{
			Model: r.Model, Accuracy: r.Accuracy, FailureRate: r.FailureRate,
			AvgLatencyMs: r.AvgLatencyMs, TotalTokens: r.TotalTokens,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Accuracy != rows[j].Accuracy {
			return rows[i].Accuracy > rows[j].Accuracy
		}
		if rows[i].FailureRate != rows[j].FailureRate {
			return rows[i].FailureRate < rows[j].FailureRate
		}
		if rows[i].TotalTokens != rows[j].TotalTokens {
			return rows[i].TotalTokens < rows[j].TotalTokens
		}
		return rows[i].Model < rows[j].Model
	})
	return rows
}

// RegressionResult §18.4 的回归判定结果。
type RegressionResult struct {
	IsRegression     bool     `json:"isRegression"`
	RegressedMetrics []string `json:"regressedMetrics"`
}

// DetectRegression 拿候选报告对比基线：任一关键指标劣化超过 tolerance 即标记 REGRESSION。
// 越高越好的指标：json_validity / 字段覆盖 / 片段召回 / 证据归属 / 准确率；
// 越低越好的指标：hallucination / failure_rate。命中即禁止直接发布（由调用方据此拦截）。
func DetectRegression(baseline, candidate *EvalReport, tolerance float64) RegressionResult {
	var bad []string
	higherBetter := func(metric string, b, c float64) {
		if c < b-tolerance {
			bad = append(bad, metric)
		}
	}
	higherBetter(MetricJSONValidity, baseline.JSONValidity, candidate.JSONValidity)
	higherBetter("accuracy", baseline.Accuracy, candidate.Accuracy)
	for _, m := range []string{MetricFieldCoverage, MetricSnippetRecall, MetricEvidenceAttribution} {
		higherBetter(m, baseline.Metrics[m].Mean, candidate.Metrics[m].Mean)
	}
	// 越低越好。
	if candidate.Hallucination > baseline.Hallucination+tolerance {
		bad = append(bad, MetricHallucination)
	}
	if candidate.FailureRate > baseline.FailureRate+tolerance {
		bad = append(bad, "failure_rate")
	}
	return RegressionResult{IsRegression: len(bad) > 0, RegressedMetrics: bad}
}

// evalRatio 安全除法（分母 0 → 0）。
func evalRatio(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}
