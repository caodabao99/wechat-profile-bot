package main

// 特性②「微信网址直开」单测：webBaseURL 优先、公网 IP 探测、探测不可用回退局域网。
// 命令路由返回含 http 的可点文本；配置缺省不报错。网络探测全部用桩替换，测试不触网。

import (
	"errors"
	"strings"
	"testing"
)

func withFakeLANProbe(t *testing.T, ip string, err error) {
	t.Helper()
	orig := firstLANIPv4
	firstLANIPv4 = func() (string, error) { return ip, err }
	t.Cleanup(func() { firstLANIPv4 = orig })
}

func withFakePublicProbe(t *testing.T, ip string, err error) {
	t.Helper()
	orig := getPublicIPv4
	getPublicIPv4 = func() (string, error) { return ip, err }
	t.Cleanup(func() { getPublicIPv4 = orig })
}

func TestWebPanelURLPrefersConfiguredBase(t *testing.T) {
	// 配置了 webBaseURL 时绝不触网
	withFakePublicProbe(t, "", errors.New("不应被调用"))
	cases := []struct{ in, want string }{
		{"https://panel.example.com", "https://panel.example.com/"},
		{"https://panel.example.com/", "https://panel.example.com/"},
		{"http://1.2.3.4:17965", "http://1.2.3.4:17965/"},
		{"panel.example.com", "http://panel.example.com/"}, // 缺 scheme 补 http
		{"  https://a.b/  ", "https://a.b/"},               // 去空白
	}
	for _, c := range cases {
		cfg := &Config{APIPort: 17965, WebBaseURL: c.in}
		url, source, err := WebPanelURL(cfg)
		if err != nil {
			t.Fatalf("in=%q 不应报错: %v", c.in, err)
		}
		if source != panelSourceConfig {
			t.Fatalf("in=%q 来源应为 config, got %q", c.in, source)
		}
		if url != c.want {
			t.Fatalf("in=%q 归一化错误: got %q want %q", c.in, url, c.want)
		}
	}
}

func TestWebPanelURLUsesPublicIP(t *testing.T) {
	withFakePublicProbe(t, "203.0.113.9", nil)
	withFakeLANProbe(t, "192.168.1.66", nil) // 即便局域网可探到，也应优先公网
	cfg := &Config{APIPort: 18000}
	url, source, err := WebPanelURL(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if source != panelSourcePublic {
		t.Fatalf("未配 webBaseURL 应优先公网探测, got source=%q", source)
	}
	if url != "http://203.0.113.9:18000/" {
		t.Fatalf("公网 URL 组装错误: %q", url)
	}

	// APIPort 非正数时回落默认端口
	withFakePublicProbe(t, "203.0.113.9", nil)
	cfg2 := &Config{APIPort: 0}
	url2, _, err := WebPanelURL(cfg2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(url2, ":17965/") {
		t.Fatalf("端口缺省应回落 17965: %q", url2)
	}
}

func TestWebPanelURLFallbackToLANWhenPublicFails(t *testing.T) {
	withFakePublicProbe(t, "", errors.New("未能探测到公网 IP"))
	withFakeLANProbe(t, "192.168.1.66", nil)
	cfg := &Config{APIPort: 18000}
	url, source, err := WebPanelURL(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if source != panelSourceLAN {
		t.Fatalf("公网失败应回退局域网, got source=%q", source)
	}
	if url != "http://192.168.1.66:18000/" {
		t.Fatalf("局域网 URL 组装错误: %q", url)
	}
}

func TestWebPanelURLBothFailReturnsError(t *testing.T) {
	withFakePublicProbe(t, "", errors.New("未能探测到公网 IP"))
	withFakeLANProbe(t, "", errors.New("未探测到可用的局域网 IPv4 地址"))
	cfg := &Config{APIPort: 17965}
	_, _, err := WebPanelURL(cfg)
	if err == nil {
		t.Fatal("公网与局域网都失败应返回错误，不得谎称正常")
	}
}

func TestWebPanelURLNilConfig(t *testing.T) {
	if _, _, err := WebPanelURL(nil); err == nil {
		t.Fatal("nil 配置应报错")
	}
}

func TestBotWebPanelCommandReturnsLink(t *testing.T) {
	// 公网可探到 → 回公网链接并提示放行端口
	withFakePublicProbe(t, "203.0.113.9", nil)
	b := &Bot{cfg: &Config{APIPort: 17965}}
	out := b.webPanelURL()
	if !strings.Contains(out, "http://203.0.113.9:17965/") {
		t.Fatalf("命令回复应含公网可点链接: %q", out)
	}
	if !strings.Contains(out, "安全组") || !strings.Contains(out, "17965") {
		t.Fatalf("公网地址应附端口/放行提示: %q", out)
	}

	// 公网失败、局域网可探 → 回局域网并附同网提示
	withFakePublicProbe(t, "", errors.New("未能探测到公网 IP"))
	withFakeLANProbe(t, "10.0.0.5", nil)
	out2 := b.webPanelURL()
	if !strings.Contains(out2, "http://10.0.0.5:17965/") {
		t.Fatalf("回退应含局域网链接: %q", out2)
	}
	if !strings.Contains(out2, "同一") {
		t.Fatalf("局域网回退应附同网提示: %q", out2)
	}

	// 配置了对外地址 → 用配置地址，无公网/局域网提示
	withFakePublicProbe(t, "", errors.New("不应被调用"))
	b.cfg = &Config{APIPort: 17965, WebBaseURL: "https://my.panel.dev/"}
	out3 := b.webPanelURL()
	if !strings.Contains(out3, "https://my.panel.dev/") {
		t.Fatalf("应使用配置地址: %q", out3)
	}
	if strings.Contains(out3, "自动探测") {
		t.Fatalf("配置了对外地址不应出现探测提示: %q", out3)
	}
}

func TestIsCommandRecognizesPanelAliases(t *testing.T) {
	for _, c := range []string{"面板", "网址", "地址"} {
		if !isCommand(c) {
			t.Fatalf("%q 应被识别为命令", c)
		}
	}
}
