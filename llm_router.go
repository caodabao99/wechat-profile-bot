package main

// ═══════════════════════════════════════════════════════════════════════════
// §5 P0：Task-level AI Model Router（PERSONAL RELATIONSHIP OS 3.0）
//
// 蓝图要的不是「再多一个模型管理页」，而是把「每个 AI 任务用哪个模型、失败退到哪个、
// 采样参数、超时、隐私约束、任务预算」收敛成一处**确定性**策略：
//   - 路由决策全部由代码按 (task → policy) 查表得出，绝不让 LLM 决定「该用哪个模型」；
//   - 未配置策略的任务完全沿用既有「活动档案」路径，行为与 v6.x 逐字节一致（向后兼容）；
//   - primary 失败 → fallback 档案 → 确定性降级（由调用方负责），LLM 故障绝不使核心页 500。
//
// 单一事实来源纪律：缓存与否仍以 Context Task Registry 的 Cacheable 为准（见 ai_cache.go），
// 本文件不重复定义缓存开关；隐私 local_only 只是路由约束（优先本地档案），不改变数据本身。
// ═══════════════════════════════════════════════════════════════════════════

import (
	"errors"
	"net/url"
	"strings"
)

// TaskModelPolicy 任务级模型路由策略（蓝图 §5.1）。零值 = 无覆盖：主模型走活动档案、
// 无 fallback、生成参数用内置默认、预算不设限。指针/零值字段一律「未设置」语义。
type TaskModelPolicy struct {
	PrimaryProfileID  string   `json:"primaryProfileId,omitempty"`  // 主模型档案 ID；空=活动档案
	FallbackProfileID string   `json:"fallbackProfileId,omitempty"` // 备用档案 ID；primary 失败时改用
	MaxTokens         int      `json:"maxTokens,omitempty"`         // 输出 token 上限；0=不覆盖
	Temperature       *float64 `json:"temperature,omitempty"`       // 采样温度；nil=不覆盖（0 是合法值故用指针）
	TimeoutSec        int      `json:"timeoutSec,omitempty"`        // 单次调用超时秒；0=沿用调用方 ctx
	PrivacyMode       string   `json:"privacyMode,omitempty"`       // "" | "local_only"
	BudgetLimitTokens int64    `json:"budgetLimitTokens,omitempty"` // 该任务当日 token 上限；0=不限（§6 任务预算）
}

// privacyLocalOnly 唯一的隐私模式取值：只允许走本地/自建档案，绝不外发远端 API。
const privacyLocalOnly = "local_only"

// errNoLocalProfile 声明了 local_only 但环境里没有可用的本地档案：调用方按确定性降级处理，
// 绝不退回到远端模型（否则违背隐私约束）。
var errNoLocalProfile = errors.New("该任务要求仅本地模型处理(local_only)，但未配置可用的本地模型档案，已跳过远端调用")

// normalizeTaskPolicies 清洗策略：去空白、负预算/负 token 归零、温度夹到 [0,2]、删空 key。
// 未知任务名保留（前向兼容：将来新增 task 的预置策略不会因当前版本不认识而被丢弃）。
func normalizeTaskPolicies(s *LLMSettings) {
	if len(s.TaskPolicies) == 0 {
		s.TaskPolicies = nil
		return
	}
	clean := make(map[string]TaskModelPolicy, len(s.TaskPolicies))
	for k, p := range s.TaskPolicies {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		p.PrimaryProfileID = strings.TrimSpace(p.PrimaryProfileID)
		p.FallbackProfileID = strings.TrimSpace(p.FallbackProfileID)
		p.PrivacyMode = strings.TrimSpace(p.PrivacyMode)
		if p.BudgetLimitTokens < 0 {
			p.BudgetLimitTokens = 0
		}
		if p.MaxTokens < 0 {
			p.MaxTokens = 0
		}
		if p.TimeoutSec < 0 {
			p.TimeoutSec = 0
		}
		if p.Temperature != nil {
			t := *p.Temperature
			if t < 0 {
				t = 0
			}
			if t > 2 {
				t = 2
			}
			p.Temperature = &t
		}
		clean[key] = p
	}
	if len(clean) == 0 {
		s.TaskPolicies = nil
		return
	}
	s.TaskPolicies = clean
}

// taskPolicyFor 取任务策略；无任务名或未配置返回零值 + false。
func taskPolicyFor(s *LLMSettings, task ContextTask) (TaskModelPolicy, bool) {
	if task == "" || s == nil || s.TaskPolicies == nil {
		return TaskModelPolicy{}, false
	}
	p, ok := s.TaskPolicies[string(task)]
	return p, ok
}

// isLocalEndpoint 判断档案接口是否指向本地/回环地址（用于 local_only 隐私校验）。
// ollama 预设的 127.0.0.1 属本地；公网主机即便带 "local" 字样也不算。
func isLocalEndpoint(p *LLMProfile) bool {
	if p == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(p.Provider), "ollama") {
		return true
	}
	u, err := url.Parse(strings.TrimSpace(p.BaseURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "0.0.0.0"
}

// resolvedRoute 是经任务策略解析出的一次调用路由：主档案 spec + 可选 fallback spec + 策略。
type resolvedRoute struct {
	Policy      TaskModelPolicy
	Primary     llmSpec
	Fallback    llmSpec
	HasPrimary  bool // 主档案可用（配齐 key/base/model）
	HasFallback bool
}

// resolveRoute 依任务策略解析调用路由。无 db / 无策略 / 策略未指定档案时，主档案回落
// 活动档案（即既有 resolveSpec 行为），从而对未接入路由的任务零影响。
func (c *LLMClient) resolveRoute(task ContextTask) resolvedRoute {
	var route resolvedRoute
	if c.db == nil {
		route.Primary = c.resolveSpec()
		route.HasPrimary = true
		return route
	}
	s, err := loadLLMSettings(c.db)
	if err != nil {
		route.Primary = c.resolveSpec()
		route.HasPrimary = true
		return route
	}
	policy, has := taskPolicyFor(&s, task)
	route.Policy = policy

	// 主档案：策略指定则用之，否则活动档案。
	if has && policy.PrimaryProfileID != "" {
		if p := s.profileByID(policy.PrimaryProfileID); p.usable() {
			route.Primary = specFromProfile(p, &s)
			route.HasPrimary = true
		}
	}
	if !route.HasPrimary {
		route.Primary = c.resolveSpec()
		route.HasPrimary = true
	}

	// 隐私 local_only：主档案必须是本地；否则尝试改选任一本地可用档案；仍无 → 标记不可用。
	if policy.PrivacyMode == privacyLocalOnly {
		if !isLocalEndpoint(s.profileByID(route.Primary.ProfileID)) {
			if lp := firstLocalProfile(&s); lp != nil {
				route.Primary = specFromProfile(lp, &s)
			} else {
				route.HasPrimary = false // 触发 errNoLocalProfile，调用方确定性降级
			}
		}
	}

	// 备用档案：仅在明确配置、且与主档案不同、且可用时启用。
	if has && policy.FallbackProfileID != "" && policy.FallbackProfileID != route.Primary.ProfileID {
		if p := s.profileByID(policy.FallbackProfileID); p.usable() {
			route.Fallback = specFromProfile(p, &s)
			route.HasFallback = true
		}
	}
	return route
}

// firstLocalProfile 返回第一个可用的本地/自建档案（用于 local_only 自动改选）。
func firstLocalProfile(s *LLMSettings) *LLMProfile {
	for i := range s.Profiles {
		if s.Profiles[i].usable() && isLocalEndpoint(&s.Profiles[i]) {
			return &s.Profiles[i]
		}
	}
	return nil
}

// applyGenerationParams 把策略的 max_tokens/temperature 盖到 spec 上（供 doCallContext 取用）。
func applyGenerationParams(spec llmSpec, policy TaskModelPolicy) llmSpec {
	spec.MaxTokens = policy.MaxTokens
	spec.Temperature = policy.Temperature
	return spec
}

// ── 蓝图 §5.2 建议初始策略：按任务给出能力档位提示（供 UI 预填 / 文档，非强制绑定）──

// ModelTier 是任务建议的模型档位（人类可读，指导用户为任务挑选主/备档案）。
type ModelTier string

const (
	TierHighQuality   ModelTier = "high_quality"  // 高质量
	TierMidHigh       ModelTier = "mid_high"      // 中高质量
	TierDeterministic ModelTier = "deterministic" // 确定性优先，模型仅解释
	TierLight         ModelTier = "light"         // 轻量
	TierLocalOnly     ModelTier = "local_only"    // 敏感，仅本地
)

// taskTierHint 返回任务在蓝图 §5.2 里的建议档位。未列出任务回落「中高质量」。
// 这是**建议**，不是路由强制——实际用哪个档案由用户在 TaskPolicies 里绑定。
func taskTierHint(task ContextTask) (ModelTier, string) {
	switch task {
	case TaskProfile, TaskSimulation:
		return TierHighQuality, "高质量模型：画像与推演质量优先"
	case TaskAsk, TaskCoach, TaskNarrative:
		return TierMidHigh, "中高质量：兼顾质量与成本"
	case TaskDecision, TaskBriefing, TaskMemoryReview:
		return TierDeterministic, "确定性优先：排序/判定由代码完成，模型只负责解释"
	case TaskFollowup, TaskEmotion, TaskOutreach, TaskSummary, TaskTopic, TaskIntent, TaskBlessing:
		return TierLight, "轻量模型：后台派生、量大且可缓存"
	default:
		return TierMidHigh, "中高质量（默认建议）"
	}
}

// ModelPolicyView 是对外暴露的「任务 → 有效策略 + 建议档位」视图（供成本/路由面板展示，
// 单一来源仍是 LLMSettings.TaskPolicies + Context Task Registry，前端不另建映射）。
type ModelPolicyView struct {
	Task            ContextTask `json:"task"`
	Label           string      `json:"label"`
	Registered      bool        `json:"registered"`
	CacheEnabled    bool        `json:"cacheEnabled"`  // 单一来源 = registry.Cacheable
	SuggestedTier   ModelTier   `json:"suggestedTier"` // §5.2 建议档位
	SuggestedReason string      `json:"suggestedReason"`
	Configured      bool        `json:"configured"`      // 用户是否已为该任务绑定策略
	PrimaryProfile  string      `json:"primaryProfile"`  // 已绑定的主档案 ID（空=活动档案）
	FallbackProfile string      `json:"fallbackProfile"` // 已绑定的备档案 ID
	PrivacyMode     string      `json:"privacyMode"`
	MaxTokens       int         `json:"maxTokens"`
	Temperature     *float64    `json:"temperature,omitempty"`
	TimeoutSec      int         `json:"timeoutSec"`
	BudgetLimit     int64       `json:"budgetLimitTokens"`
}

// ModelPolicyViews 组装全部注册任务 + 已配置但未注册的任务的策略视图（供路由/成本面板）。
func ModelPolicyViews(settings LLMSettings) []ModelPolicyView {
	seen := map[string]bool{}
	var out []ModelPolicyView
	add := func(task ContextTask) {
		if seen[string(task)] {
			return
		}
		seen[string(task)] = true
		v := ModelPolicyView{
			Task:         task,
			Registered:   isTaskRegistered(task),
			CacheEnabled: taskSpec(task).Cacheable,
		}
		if v.Registered {
			v.Label = taskSpec(task).Label
		} else {
			v.Label = string(task)
		}
		v.SuggestedTier, v.SuggestedReason = taskTierHint(task)
		if p, ok := taskPolicyFor(&settings, task); ok {
			v.Configured = true
			v.PrimaryProfile = p.PrimaryProfileID
			v.FallbackProfile = p.FallbackProfileID
			v.PrivacyMode = p.PrivacyMode
			v.MaxTokens = p.MaxTokens
			v.Temperature = p.Temperature
			v.TimeoutSec = p.TimeoutSec
			v.BudgetLimit = p.BudgetLimitTokens
		}
		out = append(out, v)
	}
	for _, t := range RegisteredTasks() {
		add(t)
	}
	// 已配置策略但尚未登记在 registry 的任务也如实呈现（不隐藏）。
	for t := range settings.TaskPolicies {
		add(ContextTask(t))
	}
	return out
}
