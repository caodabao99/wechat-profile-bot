package main

// ═══════════════════════════════════════════════════════════════════════════
// 模型与代理运行时配置（v6.2 网页端「模型与代理」）
//
// 目标：让网页端**无需重启**即可
//   - 在多个大模型档案（国内/国外/自定义）之间切换活动模型；
//   - 逐档案决定是否关闭推理/思考模式（disableThinking）；
//   - 配置「仅模型调用」走的网络代理（其余微信/邮件等功能不受影响）；
//   - 记录并统计各模型的调用量。
//
// 设计红线（与既有架构一致）：
//   - 增量复用：LLMClient 仍是全链路唯一 HTTP 出口，调用点不改；只是在 CallContext
//     里把「启动时固定配置」升级为「运行时解析活动档案」，未配置时回落 config.json，
//     老部署零感知。
//   - 配置持久化走懒建单行 JSON 表 llm_settings（仿 portfolio_settings/assistant_settings）：
//     Type=config、参与备份、不可重建、含密钥故按 contact 无关；恢复缺表则保留主库。
//   - 密钥安全：对外一律打码（apiKeyMask），真实密钥只在保存且回传值等于打码时保留原值
//     （与 calendarKeyMask/smtpPassMask 同纪律）。
//   - 代理只作用于模型 HTTP 调用：LLMClient 自带 resty 客户端，微信 iLink/SMTP 各自独立，
//     不经此处，故「模型走代理、其他走国内」天然成立。
// ═══════════════════════════════════════════════════════════════════════════

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// apiKeyMask 是对外展示/回填时替代真实密钥的占位串。保存时若前端回传此值，保留库里原密钥。
const apiKeyMask = "******"

// 模型档案的地区与预设来源。
const (
	regionDomestic = "domestic" // 国内模型
	regionForeign  = "foreign"  // 国外模型（默认走代理）
)

// LLMProfile 一个大模型档案：一套可切换的接口配置（provider 兼容 OpenAI chat/completions）。
type LLMProfile struct {
	ID              string `json:"id"`
	Label           string `json:"label"`           // 用户可读名，如「DeepSeek 对话」
	Provider        string `json:"provider"`        // deepseek/dashscope/zhipu/volcengine/moonshot/openai/custom...
	BaseURL         string `json:"baseURL"`         // OpenAI 兼容接口地址（不带末尾斜杠）
	APIKey          string `json:"apiKey"`          // 密钥（对外打码）
	Model           string `json:"model"`           // 模型名
	DisableThinking bool   `json:"disableThinking"` // 关闭推理/思考模式
	Region          string `json:"region"`          // domestic|foreign
	UseProxy        bool   `json:"useProxy"`        // 本档案是否经全局代理访问（国外模型建议 true）
}

// LLMProxySite 一次代理连通性探测的站点结果。
type LLMProxySite struct {
	URL       string `json:"url"`
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
}

// LLMProxy 全局代理设置与最近一次连通性测试结果（结果仅用于状态页展示，不作为正确性依赖）。
type LLMProxy struct {
	Enabled  bool           `json:"enabled"`
	URL      string         `json:"url"`     // http://host:port 或 socks5://host:port
	NoProxy  string         `json:"noProxy"` // 逗号分隔的不走代理的域名/IP
	TestedAt string         `json:"testedAt,omitempty"`
	EgressIP string         `json:"egressIp,omitempty"` // 经代理看到的出口 IP
	DirectIP string         `json:"directIp,omitempty"` // 不经代理的出口 IP（对照）
	Sites    []LLMProxySite `json:"sites,omitempty"`    // 站点延迟探测结果
}

// LLMSettings 模型与代理总设置（存 llm_settings 单行 JSON）。
type LLMSettings struct {
	ActiveProfileID string       `json:"activeProfileId"`
	Profiles        []LLMProfile `json:"profiles"`
	Proxy           LLMProxy     `json:"proxy"`
	// DailyTokenBudget 单日「真实模型调用」的 token 上限（缓存命中不计）。
	// 0（默认）= 不限制；负数在 normalize 里归零，不引入第三种语义。
	// 只拦后台批量派生任务，用户当场发起的调用永远不被预算拦（见 llm_budget.go）。
	DailyTokenBudget int64 `json:"dailyTokenBudget,omitempty"`
	// v7.0 §6 Cost Control Plane：周/月全局预算 + 单联系人日预算 + 任务级 Model Router 策略。
	// 均为 token 上限，0=不限制，负数归零；只作用于后台批量，不拦交互式调用。
	WeeklyTokenBudget          int64 `json:"weeklyTokenBudget,omitempty"`
	MonthlyTokenBudget         int64 `json:"monthlyTokenBudget,omitempty"`
	PerContactDailyTokenBudget int64 `json:"perContactDailyTokenBudget,omitempty"`
	// TaskPolicies 任务级 Model Router：key = ContextTask 值，value = 该任务的模型/参数/预算策略。
	// 空（默认）= 该任务走活动档案、沿用 registry 缓存策略——未配置时与 v6.x 行为完全一致。
	TaskPolicies map[string]TaskModelPolicy `json:"taskPolicies,omitempty"`
}

// llmSpec 是运行时解析出的、供单次调用使用的有效配置。
type llmSpec struct {
	ProfileID       string
	Label           string
	Provider        string
	BaseURL         string
	APIKey          string
	Model           string
	DisableThinking bool
	Region          string
	UseProxy        bool
	ProxyURL        string // 仅当 UseProxy 且全局代理启用且档案需要时非空
	// v7.0 Model Router 生成参数覆盖（来自任务策略）：nil/0 = 不覆盖，沿用内置默认。
	Temperature *float64
	MaxTokens   int
}

// ensureLLMSettingsTable 懒建单行配置表（幂等 DDL，自持 dbMu，不 bump user_version）。仿 portfolio_settings。
func ensureLLMSettingsTable(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS llm_settings (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		settings_json TEXT NOT NULL,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	return err
}

// newProfileID 生成短随机档案 ID（12 hex）。
func newProfileID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "p-custom"
	}
	return "p-" + hex.EncodeToString(b)
}

// seedProfilesFromConfig 用 config.json 的 llm 段合成一个初始档案，保证老部署升级后
// 网页端能看到「当前正在用的模型」并可继续编辑（region 由是否国外接口粗略判断）。
func seedProfilesFromConfig() []LLMProfile {
	if config == nil {
		return nil
	}
	cfg := config.LLM
	if strings.TrimSpace(cfg.BaseURL) == "" && strings.TrimSpace(cfg.Model) == "" {
		return nil
	}
	region := regionDomestic
	if isForeignBaseURL(cfg.BaseURL) {
		region = regionForeign
	}
	return []LLMProfile{{
		ID:              "p-default",
		Label:           "默认（来自 config.json）",
		Provider:        "custom",
		BaseURL:         strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		APIKey:          cfg.ApiKey,
		Model:           strings.TrimSpace(cfg.Model),
		DisableThinking: cfg.DisableThinking,
		Region:          region,
		UseProxy:        region == regionForeign,
	}}
}

// isForeignBaseURL 粗略判断接口域名是否为国外主流（用于默认地区/代理建议，非安全判定）。
func isForeignBaseURL(baseURL string) bool {
	u := strings.ToLower(baseURL)
	foreign := []string{"openai.com", "anthropic.com", "gemini", "googleapis.com", "groq.com", "mistral.ai", "together", "cohere", "x.ai", "huggingface"}
	for _, f := range foreign {
		if strings.Contains(u, f) {
			return true
		}
	}
	return false
}

func defaultLLMSettings() LLMSettings {
	s := LLMSettings{Profiles: seedProfilesFromConfig()}
	if len(s.Profiles) > 0 {
		s.ActiveProfileID = s.Profiles[0].ID
	}
	return s
}

// normalize 去空白、末尾斜杠、补 ID、清理不存在的活动指针。
func (s *LLMSettings) normalize() {
	for i := range s.Profiles {
		s.Profiles[i].BaseURL = strings.TrimRight(strings.TrimSpace(s.Profiles[i].BaseURL), "/")
		s.Profiles[i].Label = strings.TrimSpace(s.Profiles[i].Label)
		s.Profiles[i].Model = strings.TrimSpace(s.Profiles[i].Model)
		if s.Profiles[i].ID == "" {
			s.Profiles[i].ID = newProfileID()
		}
		if s.Profiles[i].Provider == "" {
			s.Profiles[i].Provider = "custom"
		}
		if s.Profiles[i].Region != regionForeign {
			s.Profiles[i].Region = regionDomestic
		}
	}
	if s.ActiveProfileID != "" && s.activeProfile() == nil {
		s.ActiveProfileID = ""
	}
	if s.ActiveProfileID == "" && len(s.Profiles) > 0 {
		s.ActiveProfileID = s.Profiles[0].ID
	}
	s.Proxy.URL = strings.TrimSpace(s.Proxy.URL)
	if s.DailyTokenBudget < 0 {
		s.DailyTokenBudget = 0 // 负数无意义，归零即「不限制」
	}
	if s.WeeklyTokenBudget < 0 {
		s.WeeklyTokenBudget = 0
	}
	if s.MonthlyTokenBudget < 0 {
		s.MonthlyTokenBudget = 0
	}
	if s.PerContactDailyTokenBudget < 0 {
		s.PerContactDailyTokenBudget = 0
	}
	normalizeTaskPolicies(s)
}

// activeProfile 返回当前活动档案（找不到返回 nil）。
func (s *LLMSettings) activeProfile() *LLMProfile {
	for i := range s.Profiles {
		if s.Profiles[i].ID == s.ActiveProfileID {
			return &s.Profiles[i]
		}
	}
	return nil
}

// profileByID 按 ID 返回档案（找不到返回 nil）。供 Model Router 解析主/备档案。
func (s *LLMSettings) profileByID(id string) *LLMProfile {
	if id == "" {
		return nil
	}
	for i := range s.Profiles {
		if s.Profiles[i].ID == id {
			return &s.Profiles[i]
		}
	}
	return nil
}

// usable 报告档案是否配齐了发起调用所需的字段。
func (p *LLMProfile) usable() bool {
	return p != nil && strings.TrimSpace(p.APIKey) != "" && strings.TrimSpace(p.BaseURL) != "" && strings.TrimSpace(p.Model) != ""
}

// loadLLMSettings 读取设置（表缺失/无行/脏数据→回落 config.json 合成的默认设置）。
func loadLLMSettings(db *sql.DB) (LLMSettings, error) {
	if err := ensureLLMSettingsTable(db); err != nil {
		return defaultLLMSettings(), err
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	var raw string
	row := db.QueryRow(`SELECT settings_json FROM llm_settings WHERE id = 1`)
	if err := row.Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			def := defaultLLMSettings()
			def.normalize()
			return def, nil
		}
		def := defaultLLMSettings()
		def.normalize()
		return def, err
	}
	var s LLMSettings
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		def := defaultLLMSettings()
		def.normalize()
		return def, nil
	}
	// 库里完全没有档案时（例如旧版本先建了空行），用 config.json 兜底显示。
	if len(s.Profiles) == 0 {
		s.Profiles = seedProfilesFromConfig()
	}
	s.normalize()
	return s, nil
}

// validateLLMBaseURL 校验单个模型端点：仅允许 http/https、必须有主机名、禁止 userinfo 掩盖主机。
// 空串放行（回落 config.json 启动配置，属合法语义）。返回人读可懂的拒绝原因。
func validateLLMBaseURL(field, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("%s 不是合法的 URL（需含 http(s):// 主机名）", field)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("%s 仅支持 http/https 协议，实得 %q", field, u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("%s 不允许在 URL 内嵌账号密码（user:pass@host）", field)
	}
	return nil
}

// validateLLMProxyURL 校验代理端点：允许 http/https/socks5；空串放行（未启用）。
func validateLLMProxyURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return errors.New("代理地址不是合法的 URL（需含协议与主机名）")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5":
	default:
		return fmt.Errorf("代理仅支持 http/https/socks5 协议，实得 %q", u.Scheme)
	}
	return nil
}

// saveLLMSettings 保存设置（合并打码密钥：前端回传 apiKeyMask 视为「不改动」沿用原值）。
func saveLLMSettings(db *sql.DB, next LLMSettings) error {
	// 先读旧值以还原被掩码的密钥
	prev, _ := loadLLMSettings(db)
	byID := map[string]string{}
	for _, p := range prev.Profiles {
		byID[p.ID] = p.APIKey
	}
	for i := range next.Profiles {
		if next.Profiles[i].APIKey == apiKeyMask {
			if old, ok := byID[next.Profiles[i].ID]; ok {
				next.Profiles[i].APIKey = old
			} else {
				next.Profiles[i].APIKey = ""
			}
		}
	}
	next.normalize()
	// v6.3 安全收口：在任何写库/生效前校验端点（SSRF/协议处理器护栏）。
	// 只校非空值，合法 http(s) 端点不受影响；不合法直接拒绝，不落库、不改活动配置。
	for _, p := range next.Profiles {
		if err := validateLLMBaseURL("模型接口地址", p.BaseURL); err != nil {
			return err
		}
	}
	if err := validateLLMProxyURL(next.Proxy.URL); err != nil {
		return err
	}
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := ensureLLMSettingsTable(db); err != nil {
		return err
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err = db.Exec(
		`INSERT INTO llm_settings (id, settings_json, updated_at) VALUES (1, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(id) DO UPDATE SET settings_json=excluded.settings_json, updated_at=excluded.updated_at`,
		string(b))
	return err
}

// resolveSpec 把「活动档案 + config.json 兑底」解析为单次调用有效配置。
// 优先级：DB 活动档案（可用）→ config.json 启动配置。
func (c *LLMClient) resolveSpec() llmSpec {
	if c.db != nil {
		if s, err := loadLLMSettings(c.db); err == nil {
			if p := s.activeProfile(); p != nil && p.APIKey != "" && p.BaseURL != "" && p.Model != "" {
				return specFromProfile(p, &s)
			}
		}
	}
	// 回落启动时 config.json（保持老部署与测试 NewLLMClient(cfg) 语义不变）
	return c.startupSpec()
}

// specFromProfile 把指定档案 + 全局代理设置组装为单次调用配置（活动档案与路由档案共用）。
func specFromProfile(p *LLMProfile, s *LLMSettings) llmSpec {
	useProxy := p.UseProxy && s.Proxy.Enabled && strings.TrimSpace(s.Proxy.URL) != ""
	proxyURL := ""
	if useProxy {
		proxyURL = strings.TrimSpace(s.Proxy.URL)
	}
	return llmSpec{
		ProfileID:       p.ID,
		Label:           p.Label,
		Provider:        p.Provider,
		BaseURL:         p.BaseURL,
		APIKey:          p.APIKey,
		Model:           p.Model,
		DisableThinking: p.DisableThinking,
		Region:          p.Region,
		UseProxy:        useProxy,
		ProxyURL:        proxyURL,
	}
}

// startupSpec 由 LLMClient 构造时的固定字段构成。
func (c *LLMClient) startupSpec() llmSpec {
	return llmSpec{
		Label:           "config.json",
		Provider:        "custom",
		BaseURL:         c.baseURL,
		APIKey:          c.apiKey,
		Model:           c.model,
		DisableThinking: c.disableThinking,
	}
}
