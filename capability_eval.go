package main

// 网页端「模型能力测试」：用当前配置的模型跑一批能力探针（抽取/情绪含过度报警陷阱/
// 跟进含过度抽取陷阱/冲突/幻觉拒答/时间推理），复用修好的 Eval Lab 评分器给出「行不行」的量化结论。
//
// 安全：走既有鉴权（进入 route() 已通过 IP 白名单 + Bearer/会话）；单次调用超时 + 全局长超时；
// single-flight 互斥防并发重复烧 token；模型未配置时优雅返回不 500（§32）。全部虚拟数据，零真实用户聊天。

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"sync"
	"time"
)

// llmCaller 抽象一次模型调用：生产 *LLMClient 满足；离线测试注入 stub，令本功能可被确定性覆盖。
type llmCaller interface {
	CallContext(ctx context.Context, prompt string) (string, error)
}

// capabilityEvalMu：同一时刻只允许一个能力测试在跑（避免并发把配额打爆）。
var capabilityEvalMu sync.Mutex

// capProbe 一条能力探针。PromptKey 非空时走生产 RenderPrompt（真实上线提示词），否则用 Input 指令。
type capProbe struct {
	ID, Task, PromptKey string
	Vars                map[string]string
	Input               string
	Expect              EvalExpect
}

func capabilityProbes() []capProbe {
	return []capProbe{
		{ID: "emo-low", Task: "emotion_analyze", PromptKey: "emotion_analyze",
			Vars: map[string]string{"name": "测试", "emotionHint": "",
				"messages": "对方: 最近真的好累\n对方: 感觉做什么都没意义\n对方: 天天失眠\n对方: 不想跟任何人说话"},
			Expect: EvalExpect{MustValidJSON: true, RequiredFields: []string{"emotion", "alert"},
				ExpectedSnippets: []string{"低落|疲惫|消极|累", "true"}}},
		{ID: "emo-neutral-trap", Task: "emotion_analyze", PromptKey: "emotion_analyze",
			Vars: map[string]string{"name": "测试", "emotionHint": "",
				"messages": "对方: 明天上午10点开会可以吗\n我: 可以\n对方: 好的收到"},
			Expect: EvalExpect{MustValidJSON: true, RequiredFields: []string{"emotion", "alert"},
				ExpectedSnippets:  []string{"中性|平稳|正常", "false"},
				ForbiddenSnippets: []string{"低落", "焦虑", "愤怒", "消极"}}},
		{ID: "followup-trap", Task: "followup_extract", PromptKey: "followup_extract",
			Vars: map[string]string{"name": "测试", "maxPerContact": "8",
				"messages": "对方[03-01 10:00]: 周末那个展你去吗，带上我呗\n我[03-01 10:05]: 行，票我来订\n对方[03-02 09:00]: 上次借你的200块啥时候还呀\n对方[03-02 09:10]: 对了你发我的资料我收到啦，谢谢"},
			Expect: EvalExpect{MustValidJSON: true, RequiredFields: []string{"items"},
				ExpectedSnippets: []string{"question", "promise", "money"}, ForbiddenSnippets: []string{"资料"}}},
		{ID: "fact-hallucination", Task: "fact_extraction",
			Input: `从下面微信对话抽取【对方】的稳定事实，只输出JSON对象 {"facts":["..."]}。严格只依据原文，原文没说的绝不要臆造成因/病情/机构。` +
				"\n\n对话：\n对方: 我妈上周做了个手术，现在恢复得还行\n我: 那就好\n对方: 有我爸照顾",
			Expect: EvalExpect{MustValidJSON: true, RequiredFields: []string{"facts"},
				ExpectedSnippets: []string{"手术"}, ForbiddenSnippets: []string{"癌症", "化疗", "肿瘤", "ICU"}}},
		{ID: "missing-refusal", Task: "fact_extraction",
			Input: `从对话抽取【对方】的【家庭住址】、【婚姻状况】、【孩子信息】，只输出JSON {"address":"...","marital_status":"...","children":"..."}。` +
				`对话里完全没提到的项必须填 "未提及"，绝不猜测编造。\n\n对话：\n对方: 今天团建去海边，晒爆了\n我: 爽啊`,
			Expect: EvalExpect{MustValidJSON: true, RequiredFields: []string{"address", "marital_status", "children"},
				ExpectedSnippets: []string{"未提及"}}},
		{ID: "conflict", Task: "detect_conflict",
			Input: `下面关于【对方】的两条说法是否互相矛盾？只输出JSON {"conflict":true,"explain":"..."}（不矛盾则 false）。\n\n对话：\n对方(3月): 我在戒糖，甜的一点不碰\n对方(9月): 天天一杯奶茶，根本戒不掉`,
			Expect: EvalExpect{MustValidJSON: true, RequiredFields: []string{"conflict"},
				ExpectedSnippets: []string{"true|矛盾|冲突|不一致|前后不"}}},
		{ID: "temporal", Task: "qa_answer",
			Input: `已知对话发生在2026年3月。对方说"下个月"去出差，推算出差月份与目的地。只输出JSON {"travel_month":"YYYY-MM","destination":"..."}。` +
				`\n\n对话：\n对方: 我下个月要出差去成都，待一周`,
			Expect: EvalExpect{MustValidJSON: true, RequiredFields: []string{"travel_month", "destination"},
				ExpectedSnippets: []string{"2026-04", "成都"}}},
	}
}

// CapabilityCaseResult 单条探针结果（供前端逐条展示）。
type CapabilityCaseResult struct {
	ID           string             `json:"id"`
	Task         string             `json:"task"`
	Pass         bool               `json:"pass"`
	JSONValid    bool               `json:"jsonValid"`
	LatencyMs    int64              `json:"latencyMs"`
	Metrics      map[string]float64 `json:"metrics,omitempty"`
	Missed       []string           `json:"missed,omitempty"`       // 未命中的期望片段（诊断）
	HitForbidden []string           `json:"hitForbidden,omitempty"` // 命中的禁用片段（疑似幻觉）
	Raw          string             `json:"raw"`
	Error        string             `json:"error,omitempty"`
}

// CapabilityReport 总报告。
type CapabilityReport struct {
	OK               bool                   `json:"ok"`
	Error            string                 `json:"error,omitempty"`
	Model            string                 `json:"model"`
	Total            int                    `json:"total"`
	Ran              int                    `json:"ran"`              // 实际完成并评分的探针数
	SkippedRateLimit int                    `json:"skippedRateLimit"` // 因限流未完成
	Passed           int                    `json:"passed"`
	Accuracy         float64                `json:"accuracy"`
	JSONValidity     float64                `json:"jsonValidity"`
	Hallucination    float64                `json:"hallucination"`
	Verdict          string                 `json:"verdict"`
	Cases            []CapabilityCaseResult `json:"cases"`
}

// RunCapabilityEval 用给定模型跑完全部探针，产出量化报告。call 生产为 *LLMClient，测试为 stub。
func RunCapabilityEval(ctx context.Context, db *sql.DB, call llmCaller, model string) CapabilityReport {
	probes := capabilityProbes()
	rep := CapabilityReport{OK: true, Model: model, Total: len(probes)}
	var jsonOK, passed, hallucApplied, hallucHit int
	for _, pr := range probes {
		prompt := pr.Input
		if pr.PromptKey != "" {
			if rp, err := RenderPrompt(db, pr.PromptKey, pr.Vars); err == nil {
				prompt = rp
			}
		}
		cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		start := time.Now()
		raw, err := call.CallContext(cctx, prompt)
		lat := time.Since(start).Milliseconds()
		cancel()

		res := CapabilityCaseResult{ID: pr.ID, Task: pr.Task, LatencyMs: lat, Raw: clipOut(raw, 400)}
		if err != nil && strings.Contains(err.Error(), "429") {
			res.Error = "限流未完成"
			rep.SkippedRateLimit++
			rep.Cases = append(rep.Cases, res)
			continue
		}
		if err != nil {
			res.Error = err.Error()
			raw = ""
		}
		g := GoldenCase{ID: pr.ID, Task: pr.Task, Expect: pr.Expect,
			ModelOutputs: map[string]EvalSample{"live": {Output: raw}}}
		cs := scoreCase(g, "live")
		res.Pass = cs.Pass
		res.JSONValid = cs.JSONValid
		res.Metrics = cs.Metrics
		res.Missed = missingSnippets(raw, pr.Expect.ExpectedSnippets)
		res.HitForbidden = hitForbidden(raw, pr.Expect.ForbiddenSnippets)
		rep.Ran++
		if cs.JSONValid {
			jsonOK++
		}
		if cs.Pass {
			passed++
		}
		if cs.Applied[MetricHallucination] {
			hallucApplied++
			if cs.Metrics[MetricHallucination] > 0 {
				hallucHit++
			}
		}
		rep.Cases = append(rep.Cases, res)
	}
	if rep.Ran > 0 {
		rep.Passed = passed
		rep.Accuracy = capRound2(float64(passed) / float64(rep.Ran))
		rep.JSONValidity = capRound2(float64(jsonOK) / float64(rep.Ran))
		if hallucApplied > 0 {
			rep.Hallucination = capRound2(float64(hallucHit) / float64(hallucApplied))
		}
		rep.Verdict = capabilityVerdict(rep)
	} else {
		rep.Verdict = "未能完成任何探针（可能模型不可达或持续限流）"
	}
	return rep
}

func capabilityVerdict(r CapabilityReport) string {
	switch {
	case r.Accuracy >= 0.8 && r.Hallucination == 0:
		return "能力良好：抽取准确、无编造，可用于生产。"
	case r.Accuracy >= 0.8:
		return "整体良好，但检出疑似幻觉，关键抽取建议加人工复核。"
	case r.Accuracy >= 0.5:
		return "基本可用但有明显短板（见下方失败项），暂不建议无人复核用于关键事实。"
	default:
		return "能力不足：多数探针未通过，该模型不适合本产品的关系信息抽取。"
	}
}

// missingSnippets 返回未命中的期望片段（每个片段含「|」同义备选，任一命中即算命中）。
func missingSnippets(raw string, expect []string) []string {
	low := strings.ToLower(raw)
	var miss []string
	for _, sn := range expect {
		if !snippetHit(low, sn) {
			miss = append(miss, sn)
		}
	}
	return miss
}

func hitForbidden(raw string, forb []string) []string {
	low := strings.ToLower(raw)
	var hit []string
	for _, fb := range forb {
		if forbiddenHit(low, fb) {
			hit = append(hit, fb)
		}
	}
	return hit
}

func capRound2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

func clipOut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// hLLMCapabilityEval POST /api/llm/capability：用当前活动模型跑能力测试并返回报告。
func (s *apiServer) hLLMCapabilityEval(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	if s.llm == nil || !s.llm.configured() {
		writeJSON(w, http.StatusOK, CapabilityReport{OK: false,
			Error: "未配置可用的活动模型：请先在「模型与代理」填写并选择活动模型，再运行能力测试。"})
		return
	}
	if !capabilityEvalMu.TryLock() {
		writeErr(w, http.StatusTooManyRequests, "模型能力测试正在进行中，请等待上一次完成后再试。")
		return
	}
	defer capabilityEvalMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	rep := RunCapabilityEval(ctx, s.db, s.llm, s.llm.ActiveModelName())
	writeJSON(w, http.StatusOK, rep)
}
