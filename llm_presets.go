package main

// 模型预设目录（v6.2）：内置国内外主流「OpenAI 兼容」接口，供网页端一键填充 baseURL/
// 默认模型名/地区/是否走代理，用户只需补 apiKey（本地 Ollama 免密钥）。纯数据、可脱库单测。
//
// 设计要点：
//   - 只做「填表助手」，不改变运行链路——用户仍可自定义任意 OpenAI 兼容接口；
//   - region 仅用于默认代理建议与分组展示（foreign 默认 useProxy=true），非安全/合规判定；
//   - baseURL 一律不带末尾斜杠、不含 /chat/completions（调用时由 LLMClient 拼接）。

// ModelPreset 一个可选的模型服务商预设。
type ModelPreset struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`    // 中文名
	Provider string   `json:"provider"` // 归类标识
	Region   string   `json:"region"`   // domestic|foreign
	BaseURL  string   `json:"baseURL"`  // OpenAI 兼容接口地址
	Models   []string `json:"models"`   // 常见模型名，供下拉/默认
	UseProxy bool     `json:"useProxy"` // 默认是否走代理（国外建议 true）
	NeedsKey bool     `json:"needsKey"` // 是否需要 API Key（本地 Ollama=false）
}

// llmPresets 返回内置预设目录。顺序即前端展示顺序：国内在前、国外在后、本地最后。
func llmPresets() []ModelPreset {
	return []ModelPreset{
		// —— 国内（默认不走代理）——
		{Key: "deepseek", Label: "DeepSeek", Provider: "deepseek", Region: regionDomestic, BaseURL: "https://api.deepseek.com", Models: []string{"deepseek-chat", "deepseek-reasoner"}, NeedsKey: true},
		{Key: "dashscope", Label: "阿里云百炼（通义千问）", Provider: "dashscope", Region: regionDomestic, BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", Models: []string{"qwen-plus", "qwen-max", "qwen-turbo"}, NeedsKey: true},
		{Key: "zhipu", Label: "智谱 GLM", Provider: "zhipu", Region: regionDomestic, BaseURL: "https://open.bigmodel.cn/api/paas/v4", Models: []string{"glm-4-plus", "glm-4-air"}, NeedsKey: true},
		{Key: "volcengine", Label: "火山引擎（豆包）", Provider: "volcengine", Region: regionDomestic, BaseURL: "https://ark.cn-beijing.volces.com/api/v3", Models: []string{"doubao-pro-32k", "doubao-lite-32k"}, NeedsKey: true},
		{Key: "moonshot", Label: "月之暗面 Kimi", Provider: "moonshot", Region: regionDomestic, BaseURL: "https://api.moonshot.cn/v1", Models: []string{"moonshot-v1-8k", "moonshot-v1-32k"}, NeedsKey: true},
		{Key: "siliconflow", Label: "硅基流动 SiliconFlow", Provider: "siliconflow", Region: regionDomestic, BaseURL: "https://api.siliconflow.cn/v1", Models: []string{"deepseek-ai/DeepSeek-V3", "Qwen/Qwen2.5-7B-Instruct"}, NeedsKey: true},

		// —— 国外（默认走代理）——
		{Key: "openai", Label: "OpenAI", Provider: "openai", Region: regionForeign, BaseURL: "https://api.openai.com/v1", Models: []string{"gpt-4o-mini", "gpt-4o"}, UseProxy: true, NeedsKey: true},
		{Key: "gemini", Label: "Google Gemini（兼容模式）", Provider: "gemini", Region: regionForeign, BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", Models: []string{"gemini-2.0-flash", "gemini-1.5-pro"}, UseProxy: true, NeedsKey: true},
		{Key: "groq", Label: "Groq", Provider: "groq", Region: regionForeign, BaseURL: "https://api.groq.com/openai/v1", Models: []string{"llama-3.3-70b-versatile", "mixtral-8x7b-32768"}, UseProxy: true, NeedsKey: true},
		{Key: "mistral", Label: "Mistral AI", Provider: "mistral", Region: regionForeign, BaseURL: "https://api.mistral.ai/v1", Models: []string{"mistral-large-latest", "mistral-small-latest"}, UseProxy: true, NeedsKey: true},
		{Key: "xai", Label: "xAI Grok", Provider: "xai", Region: regionForeign, BaseURL: "https://api.x.ai/v1", Models: []string{"grok-2-latest"}, UseProxy: true, NeedsKey: true},

		// —— 本地/自建（免密钥、不走代理）——
		{Key: "ollama", Label: "Ollama（本地）", Provider: "ollama", Region: regionDomestic, BaseURL: "http://127.0.0.1:11434/v1", Models: []string{"qwen2.5", "llama3.1", "deepseek-r1"}, NeedsKey: false},
	}
}

// presetByKey 按 key 查找预设。
func presetByKey(key string) (ModelPreset, bool) {
	for _, p := range llmPresets() {
		if p.Key == key {
			return p, true
		}
	}
	return ModelPreset{}, false
}

// presetToProfile 把预设映射为一个「待补密钥」的档案草稿（前端填充表单用）。
func presetToProfile(p ModelPreset, model string) LLMProfile {
	if model == "" && len(p.Models) > 0 {
		model = p.Models[0]
	}
	label := p.Label
	if label != "" {
		label = label + " · " + model
	}
	return LLMProfile{Label: label, Provider: p.Provider, BaseURL: p.BaseURL, Model: model, Region: p.Region, UseProxy: p.UseProxy, DisableThinking: true}
}
