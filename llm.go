package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
)

// LLMClient 封装 OpenAI 兼容的 chat/completions 调用。
// 启动时以 config.json 的 llm 段为固定配置；接入 db 后升级为「运行时解析活动档案」——
// 网页端切换模型/开关推理/配置代理无需重启即生效，未配置档案时回落 config.json。
type LLMClient struct {
	apiKey          string // 启动兜底：config.json llm.apiKey
	baseURL         string // 启动兜底：config.json llm.baseURL
	model           string // 启动兜底：config.json llm.model
	disableThinking bool   // 启动兜底：config.json llm.disableThinking
	db              *sql.DB
	http            *resty.Client // 直连客户端（不走代理）

	proxyMu  sync.Mutex    // 保护 proxyCli/proxyURL
	proxyURL string        // 当前代理客户端对应的 URL（变更则重建）
	proxyCli *resty.Client // 走代理的客户端（缓存）
}

// NewLLMClient 根据配置创建 LLM 客户端（db 为 nil 时只用 config.json）。
func NewLLMClient(cfg *Config) *LLMClient {
	base := strings.TrimRight(strings.TrimSpace(cfg.LLM.BaseURL), "/")
	return &LLMClient{
		apiKey:          cfg.LLM.ApiKey,
		baseURL:         base,
		model:           cfg.LLM.Model,
		disableThinking: cfg.LLM.DisableThinking,
		http: resty.New().
			SetTimeout(60*time.Second).
			SetHeader("Content-Type", "application/json"),
	}
}

// WithDB 注入数据库，使 LLMClient 具备运行时切模型/代理/用量统计能力。返回自身便于链式调用。
func (c *LLMClient) WithDB(db *sql.DB) *LLMClient {
	c.db = db
	return c
}

// clientFor 返回是否走代理对应的 resty 客户端。代理客户端仅在 URL 变化时重建，
// 并发调用共享只读客户端，绝不在调用中改写直连客户端的传输层（避免数据竞态）。
func (c *LLMClient) clientFor(useProxy bool, proxyURL string) *resty.Client {
	if !useProxy || strings.TrimSpace(proxyURL) == "" {
		return c.http
	}
	c.proxyMu.Lock()
	defer c.proxyMu.Unlock()
	if c.proxyCli == nil || c.proxyURL != proxyURL {
		cl := resty.New().
			SetTimeout(60*time.Second).
			SetHeader("Content-Type", "application/json").
			SetProxy(proxyURL)
		c.proxyCli = cl
		c.proxyURL = proxyURL
	}
	return c.proxyCli
}

// configured 报告模型是否真正可用（配了 apiKey 与 baseURL）。运行时解析活动档案。
func (c *LLMClient) configured() bool {
	if c == nil {
		return false
	}
	spec := c.resolveSpec()
	return strings.TrimSpace(spec.APIKey) != "" && strings.TrimSpace(spec.BaseURL) != ""
}

// chatResponse 是接口返回中我们关心的字段
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// Call 发送一次对话请求，要求模型输出 JSON。
// 网络或接口失败时自动重试一次。
func (c *LLMClient) Call(prompt string) (string, error) {
	return c.CallContext(context.Background(), prompt)
}

// isRetryableStatus 判断 HTTP 状态码是否值得重试。
// 401/403/404/400 属于配置或请求本身的问题，重试必然得到同样的结果；
// 早先这里对所有 IsError() 一律重试，导致每次配置错误都要白等 2 秒再叠加一次
// 完整超时（客户端超时 60s，最坏一次调用 ~122s 才返回错误）。
// 只有 429（限流）和 5xx（服务端故障）才可能有瞬态性。
func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// CallContext 与 Call 相同，但接受 ctx 用于取消/超时控制。
// HTTP 请求走 resty 的 SetContext，重试等待也感知 ctx，
// 避免调用方已经放弃后仍在后台占用 LLM 配额。
// 每次调用都**运行时解析活动档案**（模型/密钥/接口/推理开关/是否走代理），
// 故网页端切换模型后紧接着的调用即用新配置，无需重启。
// 每次真实模型调用（含失败）都记一笔用量日志（缓存命中不会走到这里，故统计的是真实 API 消耗）。
func (c *LLMClient) CallContext(ctx context.Context, prompt string) (string, error) {
	spec := c.resolveSpec()
	start := time.Now()
	content, status, usage, err := c.doCallContext(ctx, spec, prompt)
	c.logLLMCall(spec, err == nil, status, usage, time.Since(start).Milliseconds())
	return content, err
}

// doCallContext 是 CallContext 的执行体（不含用量记录），返回最终内容/HTTP 状态/token 用量/错误。
func (c *LLMClient) doCallContext(ctx context.Context, spec llmSpec, prompt string) (string, int, llmUsage, error) {
	cli := c.clientFor(spec.UseProxy && spec.ProxyURL != "", spec.ProxyURL)

	body := map[string]interface{}{
		"model": spec.Model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"temperature": 0.3,
		"response_format": map[string]string{
			"type": "json_object",
		},
	}
	// 关闭推理思考：百炼兼容模式认 enable_thinking，DeepSeek 官方/V4 认 thinking.type，
	// 两个参数一起发，不支持的平台会忽略。
	if spec.DisableThinking {
		body["enable_thinking"] = false
		body["thinking"] = map[string]string{"type": "disabled"}
	}

	endpoint := spec.BaseURL + "/chat/completions"
	var lastErr error
	var lastStatus int
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", lastStatus, llmUsage{}, ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}

		resp, err := cli.R().
			SetContext(ctx).
			SetHeader("Authorization", "Bearer "+spec.APIKey).
			SetBody(body).
			Post(endpoint)
		if err != nil {
			lastErr = fmt.Errorf("请求模型接口失败: %w", err)
			continue
		}
		lastStatus = resp.StatusCode()
		if resp.IsError() {
			lastErr = fmt.Errorf("模型接口返回 %d: %s", lastStatus,
				strings.TrimSpace(string(resp.Body())))
			if !isRetryableStatus(lastStatus) {
				return "", lastStatus, llmUsage{}, lastErr
			}
			continue
		}

		var out chatResponse
		if err := json.Unmarshal(resp.Body(), &out); err != nil {
			lastErr = fmt.Errorf("解析模型返回失败: %w", err)
			continue
		}
		if out.Error != nil {
			lastErr = errors.New("模型接口报错: " + out.Error.Message)
			continue
		}
		if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
			lastErr = errors.New("模型返回内容为空")
			continue
		}
		var u llmUsage
		if out.Usage != nil {
			u = llmUsage{Prompt: out.Usage.PromptTokens, Completion: out.Usage.CompletionTokens, Total: out.Usage.TotalTokens}
		}
		return out.Choices[0].Message.Content, lastStatus, u, nil
	}
	return "", lastStatus, llmUsage{}, lastErr
}

// ExtractJSON 从模型返回文本中提取第一个 { 到最后一个 } 的内容，
// 容忍 ```json 代码块包裹和多余解释文字。
func ExtractJSON(s string) string {
	s = strings.TrimSpace(s)
	// 去掉常见的 markdown 代码块围栏
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```JSON")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}
