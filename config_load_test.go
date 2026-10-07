package main

// LoadConfig 的首启动集成测试：直接覆盖「NAS/容器上启动不了」那条真实故障路径。
// 测试进程里 configPath() 落在编译出的测试二进制同目录（临时目录），可安全走完整个流程。
// 每个用例都会恢复全局 config 并删掉生成的文件，避免污染同包其它用例。

import (
	"os"
	"testing"
)

// withCleanConfig 清空当前 config 文件、保存并在使用后恢复全局 config。
func withCleanConfig(t *testing.T) {
	t.Helper()
	saved := config
	p := configPath()
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		t.Fatalf("前置清理 %s 失败: %v", p, err)
	}
	t.Cleanup(func() {
		_ = os.Remove(p)
		config = saved
	})
}

// A. 首启动：config.json 不存在时**不得返回错误**（返回错误 → main 里 os.Exit(1) →
// NAS 上面板永远起不来），并且要按环境变量把配置写出来、权限收到 0600。
func TestLoadConfigFirstRunContinuesInsteadOfFailing(t *testing.T) {
	withCleanConfig(t)
	p := configPath()

	t.Setenv("WEPB_MY_NAME", "昵称A")
	t.Setenv("WEPB_API_TOKEN", "token-abc")
	t.Setenv("WEPB_LLM_MODEL", "glm-4.7-flash")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("首启动不应报错（报错就等于 main 退出、NAS 起不来），实得: %v", err)
	}
	if cfg.MyName != "昵称A" || cfg.APIToken != "token-abc" || cfg.LLM.Model != "glm-4.7-flash" {
		t.Fatalf("环境变量未生效：%+v", cfg)
	}
	if cfg.APIPort != 17965 || cfg.Profile.ColdStartCount != 20 {
		t.Fatalf("默认值丢失：apiPort=%d coldStart=%d", cfg.APIPort, cfg.Profile.ColdStartCount)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("应已生成 %s: %v", p, err)
	}
	// 里面有 apiToken，必须仅属主可读写（umask 兜底后仍期望 0600）
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Fatalf("config.json 权限应为 0600，实得 %v", perm)
	}
}

// B. 密钥仍是占位值：不得报错退出，且把占位 Key 规范化为空（否则会拿 sk-xxx 去撞接口吃 401）。
func TestLoadConfigPlaceholderKeyDegradesNotFatal(t *testing.T) {
	withCleanConfig(t)

	t.Setenv("WEPB_MY_NAME", "")
	t.Setenv("WEPB_API_TOKEN", "token-def") // 不传 WEPB_LLM_API_KEY → 保留模板占位 sk-xxx

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("占位密钥不应导致启动失败，实得: %v", err)
	}
	if cfg.LLM.ApiKey != "" {
		t.Fatalf("占位 sk-xxx 应被规范化为空，实得 %q", cfg.LLM.ApiKey)
	}
	if cfg.APIToken != "token-def" {
		t.Fatalf("apiToken 引导失败：%q", cfg.APIToken)
	}
}

// C. 真正损坏的 config.json 仍必须报错：不能把"配置坏了"悄悄当默认值跑起来。
func TestLoadConfigStillFailsOnCorruptFile(t *testing.T) {
	withCleanConfig(t)

	if err := os.WriteFile(configPath(), []byte("{ 这不是合法 JSON "), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(); err == nil {
		t.Fatal("损坏的 config.json 应返回错误，却被静默接受了")
	}
}

// E. createInitialConfigIfAbsent 必须幂等：第二次不得覆盖已有文件。
// （--init-config 与 LoadConfig 共用它；若会覆盖，用户手改的配置就会被冲掉）
func TestCreateInitialConfigIfAbsentIsIdempotent(t *testing.T) {
	withCleanConfig(t)

	p, created, err := createInitialConfigIfAbsent()
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("首次应判为新建")
	}

	mine := `{"myName":"手工改过的","apiToken":"keep-me","llm":{"apiKey":"sk-real"}}`
	if err := os.WriteFile(p, []byte(mine), 0600); err != nil {
		t.Fatal(err)
	}
	_, created2, err := createInitialConfigIfAbsent()
	if err != nil {
		t.Fatal(err)
	}
	if created2 {
		t.Fatal("文件已存在时不应报“新建”")
	}
	data, err := os.ReadFile(p)
	if err != nil || string(data) != mine {
		t.Fatalf("已有配置被覆盖了：%s err=%v", data, err)
	}
}

// D. 已有配置不被环境变量覆盖：容器重启不能把用户在网页/文件里改好的配置冲掉。
func TestLoadConfigEnvDoesNotOverrideExistingFile(t *testing.T) {
	withCleanConfig(t)

	t.Setenv("WEPB_MY_NAME", "首次昵称")
	if _, err := LoadConfig(); err != nil {
		t.Fatal(err)
	}
	// 第二次启动（文件已存在）时改环境变量，不应影响已落盘的配置
	t.Setenv("WEPB_MY_NAME", "后来的昵称")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MyName != "首次昵称" {
		t.Fatalf("环境变量不应覆盖已存在的 config.json，实得 myName=%q", cfg.MyName)
	}
}
