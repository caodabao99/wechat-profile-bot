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
	ApiKey          string `json:"apiKey"`          // API Key
	BaseURL         string `json:"baseURL"`         // 接口地址，如 https://api.deepseek.com
	Model           string `json:"model"`           // 模型名称
	DisableThinking bool   `json:"disableThinking"` // 关闭推理思考模式（适用于 deepseek-v4/qwen3 等默认开启思考的模型）
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
	LLM            LLMConfig     `json:"llm"`
	Profile        ProfileConfig `json:"profile"`
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

// LoadConfig 读取程序同目录下的 config.json；
// 文件不存在时自动生成一份默认配置并返回错误，提示用户填写后重启。
func LoadConfig() (*Config, error) {
	p := configPath()
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			// 0600：模板里含 llm.apiKey 占位符，用户填完真实密钥后这个文件就是凭据，
			// 不能对同机器其他用户可读
			if werr := os.WriteFile(p, []byte(defaultConfigTemplate), 0600); werr != nil {
				return nil, fmt.Errorf("配置文件不存在，且自动创建失败: %w", werr)
			}
			return nil, fmt.Errorf("配置文件不存在，已在程序目录生成默认配置：\n%s\n\n请填写 myName 和 llm.apiKey 后重新启动程序", p)
		}
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

	// 占位符校验：apiKey 没填的话，用户要等到第一次生成画像才看到一条 401，
	// 中间入库的消息全都没有画像。启动时就拦下来更省事。
	if k := strings.TrimSpace(c.LLM.ApiKey); k == "" || k == "sk-xxx" {
		return nil, fmt.Errorf("config.json 里 llm.apiKey 还是占位值，请填写真实的大模型 API Key 后重新启动")
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
