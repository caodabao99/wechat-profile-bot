package main

// 代理连通性测试（v6.2）。目的：让用户在配置「仅模型调用走的代理」后，能一键验证——
//   - 经代理看到的出口 IP（以及不经代理的直连出口 IP 作对照，证明代理确实改了出口）；
//   - 到若干代表性站点的连通性与延迟；
//   - 当前活动模型接口是否可达。
//
// 设计红线：
//   - 网络目标可注入：runProxyTest 接受 ProxyTestOptions（IP 端点/站点列表），默认走公网地址，
//     测试用例用 httptest 服务器替换，绝不依赖真实外网。
//   - 失败哲学：探测失败是「数据」不是「错误」——单项失败记入结果、绝不 503、绝不 panic；整体有超时上限。
//   - 只影响模型调用：代理仅注入 LLMClient 的 resty 客户端（PhaseA 已实现）；此处测试用的临时
//     http.Client 不外溢到微信 iLink/SMTP，故「其他功能仍走国内网络」天然成立。

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// errNoIPEndpoint IP 探测端点未提供（注入选项为空时的哨兵，不作为对外错误）。
var errNoIPEndpoint = errors.New("IP 探测端点未提供")

// proxyTestOptionsFn 供 handler 获取探测目标；默认走公网，测试用例临时替换为 httptest 地址。
var proxyTestOptionsFn = defaultProxyTestOptions

// ProxyTestOptions 注入探测目标（测试用 httptest 覆盖，生产用默认公网）。
type ProxyTestOptions struct {
	IPDirectURL   string        // 不经代理取出口 IP
	IPViaProxyURL string        // 经代理取出口 IP
	SiteURLs      []string      // 代表性站点，逐个测延迟
	Timeout       time.Duration // 单请求超时
}

// defaultProxyTestOptions 生产默认：出口 IP 用 api.ipify.org（返回纯文本 IP），
// 站点覆盖国内外主流（用于感知代理是否可用/快慢）。
func defaultProxyTestOptions() ProxyTestOptions {
	return ProxyTestOptions{
		IPDirectURL:   "https://api.ipify.org",
		IPViaProxyURL: "https://api.ipify.org",
		SiteURLs: []string{
			"https://www.google.com/generate_204",
			"https://api.openai.com/v1/models",
			"https://www.cloudflare.com/cdn-cgi/trace",
		},
		Timeout: 8 * time.Second,
	}
}

// httpClientFor 构造带（或不带）代理的临时 http 客户端。proxyURL 为空或解析失败→直连。
func httpClientFor(proxyURL string, timeout time.Duration) *http.Client {
	tr := &http.Transport{}
	if pu := strings.TrimSpace(proxyURL); pu != "" {
		if u, err := url.Parse(pu); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

// fetchEgressIP GET 一个端点并尽力从中提取 IP（兼容纯文本与 {"ip":...} JSON）。
func fetchEgressIP(client *http.Client, endpoint string) (string, error) {
	if strings.TrimSpace(endpoint) == "" {
		return "", errNoIPEndpoint
	}
	resp, err := client.Get(endpoint)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	s := strings.TrimSpace(string(b))
	// 尝试 JSON：ipinfo/ip.sb 等
	if strings.HasPrefix(s, "{") {
		var j map[string]interface{}
		if json.Unmarshal([]byte(s), &j) == nil {
			if ip, ok := j["ip"].(string); ok {
				return strings.TrimSpace(ip), nil
			}
		}
	}
	// 纯文本：取第一行
	if nl := strings.IndexAny(s, "\r\n"); nl >= 0 {
		s = s[:nl]
	}
	return strings.TrimSpace(s), nil
}

// probeSite 测一个站点连通性与延迟（GET，2xx/3xx/4xx 皆视为「可达」，仅传输失败/5xx 记不可达）。
func probeSite(client *http.Client, site string, timeout time.Duration) LLMProxySite {
	res := LLMProxySite{URL: site}
	c := *client
	c.Timeout = timeout
	start := time.Now()
	resp, err := c.Get(site)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	res.OK = resp.StatusCode < 500
	if !res.OK {
		res.Error = "HTTP " + http.StatusText(resp.StatusCode)
	}
	return res
}

// ProxyTestOutcome 一次代理探测的结果（写入 settings.Proxy 供状态页展示）。
type ProxyTestOutcome struct {
	TestedAt string
	EgressIP string
	DirectIP string
	Sites    []LLMProxySite
}

// runProxyTest 执行探测。proxy.Enabled=false 时仅测直连出口 + 直连站点延迟。
func runProxyTest(proxy LLMProxy, opts ProxyTestOptions) ProxyTestOutcome {
	if opts.Timeout == 0 {
		opts.Timeout = 8 * time.Second
	}
	direct := httpClientFor("", opts.Timeout)
	out := ProxyTestOutcome{TestedAt: time.Now().Format(time.RFC3339)}

	out.DirectIP, _ = fetchEgressIP(direct, opts.IPDirectURL)

	viaClient := direct
	if proxy.Enabled && strings.TrimSpace(proxy.URL) != "" {
		viaClient = httpClientFor(proxy.URL, opts.Timeout)
		out.EgressIP, _ = fetchEgressIP(viaClient, opts.IPViaProxyURL)
	}

	for _, site := range opts.SiteURLs {
		out.Sites = append(out.Sites, probeSite(viaClient, site, opts.Timeout))
	}
	return out
}
