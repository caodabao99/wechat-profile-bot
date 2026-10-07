package main

// 首启动配置引导的回归测试（投产：NAS/容器部署「启动不了」的根因修复）。
//
// 旧行为：config.json 不存在时只写模板然后 return error → main 打印"请填写后重新启动"并
// os.Exit(1)；在没终端的 NAS 上进程反复退出，面板永远打不开。现改为：按默认值 + WEPB_*
// 环境变量生成配置并**继续启动**；未配大模型 Key 也只降级不退出。

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInitialConfigContentFromEnv(t *testing.T) {
	t.Setenv("WEPB_MY_NAME", "大宝")
	t.Setenv("WEPB_API_TOKEN", "tok-with-\"quote\"-and-\\slash")
	t.Setenv("WEPB_LLM_API_KEY", "sk-real-key")
	t.Setenv("WEPB_LLM_BASE_URL", "https://open.bigmodel.cn/api/paas/v4")
	t.Setenv("WEPB_LLM_MODEL", "glm-4.7-flash")

	content := initialConfigContent()

	// 必须是合法 JSON，否则用户首启动就拿到一个打不开的文件
	var c Config
	if err := json.Unmarshal([]byte(content), &c); err != nil {
		t.Fatalf("生成的配置不是合法 JSON: %v\n%s", err, content[:200])
	}
	if c.MyName != "大宝" {
		t.Fatalf("myName 未被环境变量引导，实得 %q", c.MyName)
	}
	if c.APIToken != `tok-with-"quote"-and-\slash` {
		t.Fatalf("apiToken 引导或转义有误，实得 %q", c.APIToken)
	}
	if c.LLM.ApiKey != "sk-real-key" || c.LLM.Model != "glm-4.7-flash" ||
		c.LLM.BaseURL != "https://open.bigmodel.cn/api/paas/v4" {
		t.Fatalf("llm 字段引导有误：%+v", c.LLM)
	}
	// 默认值与注释必须保留（用户打开文件要看得懂，也不能丢 _comment 说明）
	if !strings.Contains(content, "_comment1") {
		t.Error("生成的配置丢了 _comment 说明字段")
	}
	if c.Profile.ColdStartCount != 20 || c.APIPort != 17965 {
		t.Fatalf("默认值被改坏：coldStart=%d apiPort=%d", c.Profile.ColdStartCount, c.APIPort)
	}
	if !c.LLM.DisableThinking {
		t.Error("disableThinking 默认应为 true")
	}
}

func TestInitialConfigContentWithoutEnvKeepsPlaceholders(t *testing.T) {
	// 全空环境变量（t.Setenv 设空串等价未设）：必须仍是合法 JSON、保持占位提示
	t.Setenv("WEPB_MY_NAME", "")
	t.Setenv("WEPB_API_TOKEN", "")
	t.Setenv("WEPB_LLM_API_KEY", "")
	content := initialConfigContent()
	var c Config
	if err := json.Unmarshal([]byte(content), &c); err != nil {
		t.Fatalf("无环境变量时生成的配置非法: %v", err)
	}
	if c.MyName != "你的微信昵称" || c.LLM.ApiKey != "sk-xxx" {
		t.Fatalf("未设环境变量时应保留模板占位值，实得 myName=%q apiKey=%q", c.MyName, c.LLM.ApiKey)
	}
}

func TestSetJSONStringFieldOnlyHitsIntendedKey(t *testing.T) {
	src := `{"_comment3":"apiToken: 说明里的 apiToken 字样","apiToken": "", "llm":{"apiKey":"sk-xxx"}}`
	got := setJSONStringField(src, "apiToken", "abc")
	if !strings.Contains(got, `"apiToken": "abc"`) {
		t.Fatalf("目标字段未被替换：%s", got)
	}
	// 关键：值必须是 abc（16 进制风格），不能被替换到 _comment3 的文本里
	if strings.Contains(got, `"abc"`) == false {
		t.Fatal("替换结果异常")
	}
	if !strings.Contains(got, `"apiKey":"sk-xxx"`) {
		t.Fatalf("其它字段被误改：%s", got)
	}
	if strings.Count(got, "abc") != 1 {
		t.Fatalf("出现多余替换：%s", got)
	}
}
