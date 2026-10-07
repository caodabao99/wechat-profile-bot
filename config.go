package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// LLMConfig 大模型接口配置
type LLMConfig struct {
	ApiKey          string `json:"apiKey"`              // API Key
	BaseURL         string `json:"baseURL"`             // 接口地址，如 https://api.deepseek.com
	Model           string `json:"model"`               // 模型名称
	DisableThinking bool   `json:"disableThinking"`     // 关闭推理思考模式（适用于 deepseek-v4/qwen3 等默认开启思考的模型）
	ExtraBody       string `json:"extraBody,omitempty"` // 自定义请求体参数（JSON 对象），与网页档案同义：兼容各厂商思考/推理参数命名差异
}

// ProfileConfig 画像生成策略
type ProfileConfig struct {
	ColdStartCount int `json:"coldStartCount"` // 累计多少条对方消息后首次生成画像
	UpdateInterval int `json:"updateInterval"` // 之后每新增多少条对方消息更新一次画像
}

// Config 程序总配置
type Config struct {
	MyName         string        `json:"myName"`         // 我自己的微信昵称，用于区分消息发送方
	APIPort        int           `json:"apiPort"`        // REST API 端口（供 Windows 桌面版远程调用）；不填按 17965，填负数表示禁用 API
	APIToken       string        `json:"apiToken"`       // REST API 认证 Token，桌面端调用时需在请求头携带
	APIWhitelist   []string      `json:"apiWhitelist"`   // API 访问 IP 白名单（如 ["1.2.3.4", "192.168.1.0/24"]），空数组=不限制
	TrustedProxies []string      `json:"trustedProxies"` // 可信反向代理 IP/CIDR（如 ["127.0.0.1", "10.0.0.0/8"]）；仅直连 IP 命中时才采纳 X-Forwarded-For，为空=忽略该头
	WebBaseURL     string        `json:"webBaseURL"`     // 管理面板对外可达的完整基础地址（如 https://your-domain/ 或 http://公网IP:端口/），供微信「网址」命令直开；留空则自动探测服务器公网出口 IP，探测不可用才回退局域网 IP
	LLM            LLMConfig     `json:"llm"`
	Profile        ProfileConfig `json:"profile"`
	// ContextDebug 控制可观测端点 GET /api/contacts/{id}/context 是否返回完整上下文与 rendered 文本。
	// rendered 含大量私人聊天与推理上下文，生产默认 false（仅返回结构化摘要）；排障时才开。
	ContextDebug bool `json:"contextDebug"`
}

// config 全局配置单例
var config *Config

// 首次运行自动生成的配置模板（带注释说明每个配置项用途，JSON 解析时 _comment 键会被忽略）
const defaultConfigTemplate = `{
  "_comment1": "myName: 你的微信昵称，必须与微信里显示的昵称完全一致，用于区分聊天记录中哪些是你发的",
  "myName": "你的微信昵称",
  "_comment2": "apiPort: REST API 端口，供 Windows 桌面版远程调用。默认值 17965（非常用端口，降低被扫描风险）；不需要桌面版远程访问时填 -1 可完全禁用 API 服务",
  "apiPort": 17965,
  "_comment3": "apiToken: REST API 认证令牌（Bearer Token），桌面端调用时通过 Authorization 请求头传递。留空则启动时自动生成一个随机令牌并写回本文件，之后保持不变",
  "apiToken": "",
  "_comment4": "apiWhitelist: API 访问 IP 白名单。留空或空数组表示不限制；填写后仅允许列表内的 IP/CIDR 段访问，其他 IP 一律拒绝。如 [\"1.2.3.4\", \"192.168.1.0/24\"]",
  "apiWhitelist": [],
  "_comment4b": "trustedProxies: 可信反向代理列表，仅在 nginx/Caddy 等反代后部署时填写，内容为反代服务器的来源 IP 或网段（同机反代填 [\"127.0.0.1\"]）。命中时白名单/封禁/限流按 X-Forwarded-For 里的真实访客 IP 判定；直连 IP 不在此列表时一律忽略 X-Forwarded-For（防止伪造）。注意不要把 0.0.0.0/0 之类大网段填进来，否则等于信任所有人伪造的头",
  "trustedProxies": [],
  "_comment4c": "webBaseURL: 管理面板的对外可访问完整地址，供微信里发「网址」命令后直接点开。部署在云端时可不填，程序会自动探测服务器公网出口 IP（拼成 http://公网IP:apiPort/）；要外网可访问需你在云主机安全组/防火墙放行 apiPort，或在路由器做端口映射，跨公网建议填带 https 的域名。若公网探测不可用（如本机在 NAT 后且无回显服务），才回退自动探测局域网 IP（仅同网可开）",
  "webBaseURL": "",
  "_comment5": "llm: 大模型配置。apiKey 填入你的密钥；baseURL 为 OpenAI 兼容接口地址；model 为模型名称；disableThinking 默认 true 关闭思考/推理模式——本程序不需要推理，开着只会拖慢响应、多耗 token。deepseek-v4-flash、qwen3.8-flash 等默认开思考的模型必须保持 true；不支持该参数的接口会自动忽略",
  "llm": {
    "apiKey": "sk-xxx",
    "baseURL": "https://api.deepseek.com",
    "model": "deepseek-chat",
    "disableThinking": true
  },
  "_comment6": "profile: 画像生成规则。coldStartCount 表示累计对方消息达到该条数后首次生成画像；updateInterval 表示画像生成后，对方消息每新增该条数就自动更新一次画像",
  "profile": {
    "coldStartCount": 20,
    "updateInterval": 10
  }
}
`

// configPath 返回配置文件路径；Docker 场景优先用 /config 目录
func configPath() string {
	if _, err := os.Stat("/config"); err == nil {
		return "/config/config.json"
	}
	exe, err := os.Executable()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(filepath.Dir(exe), "config.json")
}

// LoadConfig 读取程序同目录下的 config.json。
//
// 首次启动**不再**「写模板 + 报错退出」：那种行为在 NAS/容器里是死循环——进程退了，
// 用户既打不开网页面板（没服务），也没法在无人值守环境里改文件，表观就是「启动不了」。
// 现在按内置默认值 + 环境变量引导生成一份可用配置并继续启动；大模型未配置时相关
// 功能自动降级，用户可在网页「模型与代理」页补配，或直接改 config.json 后重启。
func LoadConfig() (*Config, error) {
	p, created, err := createInitialConfigIfAbsent()
	if err != nil {
		return nil, err
	}
	if created {
		slog.Info("首次启动：已按默认值与环境变量生成配置文件，继续启动（各项可在网页管理界面修改）", "path", p)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("解析 config.json 失败: %w", err)
	}

	// 兜底默认值，避免用户把某些数字字段删空
	if c.Profile.ColdStartCount <= 0 {
		c.Profile.ColdStartCount = 20
	}
	if c.Profile.UpdateInterval <= 0 {
		c.Profile.UpdateInterval = 10
	}
	if c.APIPort == 0 {
		// 未填写时按默认端口启动；显式填负数表示禁用 API
		c.APIPort = 17965
	}

	// 密钥未填不再是致命错误（无终端的容器里退出 = 再也进不去面板）：把占位值
	// 规范化为空，让 LLM 客户端按「未配置」优雅降级（不会拿 sk-xxx 去撞接口吃 401），
	// 用户可在网页「模型与代理」页填 Key，也可改 config.json 后重启。
	if k := strings.TrimSpace(c.LLM.ApiKey); k == "" || k == "sk-xxx" {
		c.LLM.ApiKey = ""
		slog.Warn("尚未配置大模型 API Key：画像与意图分析会提示不可用，入库与其余功能不受影响；可在网页「模型与代理」页填写后直接生效")
	}
	if n := strings.TrimSpace(c.MyName); n == "" || n == "你的微信昵称" {
		fmt.Fprintf(os.Stderr, "警告: myName 未填写（当前 %q），将无法区分聊天记录里哪些消息是你发的，画像质量会明显下降\n", c.MyName)
	}

	// 自动生成随机 API Token（若留空）并写回配置文件
	if c.APIToken == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return nil, fmt.Errorf("生成 API Token 失败: %w", err)
		}
		c.APIToken = hex.EncodeToString(b)
		if err := saveConfigPreserveComments(p, &c); err != nil {
			fmt.Fprintf(os.Stderr, "警告: 自动生成的 API Token 未写回配置文件: %v\n", err)
		} else {
			slog.Info("已自动生成 API Token 并写入配置文件，请将其填入桌面端 config.json 的 remote.apiToken", "path", p)
		}
	}

	config = &c
	return &c, nil
}

// createInitialConfigIfAbsent 在 config.json 不存在时按默认值 + WEPB_* 环境变量生成它。
// 返回（路径, 是否本次新建, 错误）。LoadConfig 与 --init-config 子命令共用同一函数，
// 避免两处逻辑漂移（打包脚本就依赖过“无配置时写模板”这一行为）。
//
// 权限 0600：文件里会含 llm.apiKey 与 apiToken，填完真实密钥后它就是凭据，不能同机其他用户可读。
func createInitialConfigIfAbsent() (string, bool, error) {
	p := configPath()
	if _, err := os.Stat(p); err == nil {
		return p, false, nil
	}
	if err := writeFileAtomic(p, []byte(initialConfigContent())); err != nil {
		return p, false, fmt.Errorf("配置文件不存在，且自动创建失败: %w", err)
	}
	return p, true, nil
}

// initConfigFromCLI 处理 --init-config：只生成（或报告已存在）配置模板后退出。
// 用途：NAS/容器里先落一份可读可改的 config.json 再编辑；也让打包脚本能依赖一个
// 明确的“写完就退”入口，而不靠“首启动写模板后退出”这种旧行为（v7.4.0 已改掉）。
func initConfigFromCLI() {
	p, created, err := createInitialConfigIfAbsent()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if created {
		fmt.Printf("已生成配置模板：%s\n请编辑其中的 myName 与 llm.apiKey 后启动程序（也可直接启动，在网页里配置）\n", p)
		return
	}
	fmt.Printf("配置文件已存在，未作修改：%s\n", p)
}

// initialConfigContent 生成首启动配置：在内置模板上做**定点字符串替换**（而不是反
// 序列化再 Marshal），这样 _comment 说明与字段顺序都保留，用户打开文件仍看得懂。
//
// 环境变量只用于无人值守部署（NAS/容器）引导，与 qb-stream 的约定一致：
// WEPB_MY_NAME / WEPB_API_TOKEN / WEPB_LLM_API_KEY / WEPB_LLM_BASE_URL / WEPB_LLM_MODEL。
// 配置一旦生成就以 config.json 为准（环境变量不再覆盖），避免网页改完又被容器重启重置。
func initialConfigContent() string {
	out := defaultConfigTemplate
	for _, r := range []struct{ key, env string }{
		{"myName", "WEPB_MY_NAME"},
		{"apiToken", "WEPB_API_TOKEN"},
		{"apiKey", "WEPB_LLM_API_KEY"},
		{"baseURL", "WEPB_LLM_BASE_URL"},
		{"model", "WEPB_LLM_MODEL"},
	} {
		if v := strings.TrimSpace(os.Getenv(r.env)); v != "" {
			out = setJSONStringField(out, r.key, v)
		}
	}
	return out
}

// setJSONStringField 把 `"key": "..."` 的值换成 v（v 里的反斜杠与引号转义）。
func setJSONStringField(content, key, v string) string {
	re := regexp.MustCompile(`("` + regexp.QuoteMeta(key) + `"\s*:\s*)"[^"]*"`)
	esc := strings.ReplaceAll(v, `\`, `\\`)
	esc = strings.ReplaceAll(esc, `"`, `\"`)
	return re.ReplaceAllString(content, `${1}"`+esc+`"`)
}

// saveConfigPreserveComments 仅替换配置文件中的 apiToken 字段值，保留 _comment 等注释。
// 避免 json.MarshalIndent 覆盖整个文件导致注释丢失。
func saveConfigPreserveComments(path string, c *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	content := string(data)
	// 匹配 "apiToken": "" 或 "apiToken": "xxx"，替换为新 token
	re := regexp.MustCompile(`("apiToken"\s*:\s*)"[^"]*"`)
	newContent := re.ReplaceAllString(content, `${1}"`+c.APIToken+`"`)
	if newContent == content {
		// 没匹配到（用户可能把 apiToken 这行删了），降级为整文件覆盖。
		// 必须传真实的 c：以前这里传的是 &Config{APIToken: token}，
		// 会把 myName / llm / profile 全部清零，用户重启后发现配置全没了。
		return saveConfig(path, c)
	}
	return writeFileAtomic(path, []byte(newContent))
}

// saveConfig 将配置写回 JSON 文件（用于自动生成 token 后持久化）
func saveConfig(path string, c *Config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}
