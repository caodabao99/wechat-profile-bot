package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// 微信内一键打开网页面板（特性②）。
//
// 现实约束：程序无法自行穿透 NAT。外网直开必须有一个「可达地址」——端口映射 /
// DDNS / 内网穿透 / Tailscale 等由部署方提供，写进 config.json 的 webBaseURL。
// 没配 webBaseURL 时，回退自动探测本机局域网 IPv4，拼成 http://<lan-ip>:<port>/，
// 这只在「与服务器同一 WiFi/内网」时可点开后直接进面板。
//
// 安全：面板本身有 Bearer Token + 2FA，这里只回地址、不签发免登录魔法链接；
// 链接出现在聊天里等于暴露入口地址，依赖鉴权兜底。跨网建议 https。

// defaultFirstLANIPv4 取首个「已启用、非回环」网卡上的非回环 IPv4 地址。
// 排除 127.0.0.0/8 与 169.254.0.0/16（链路本地，多为未取到 DHCP 时的临时地址）。
func defaultFirstLANIPv4() (string, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("枚举网卡失败: %w", err)
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.To4() == nil {
				continue
			}
			if ip.IsLinkLocalUnicast() {
				continue
			}
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("未探测到可用的局域网 IPv4 地址")
}

// firstLANIPv4 是可在测试中替换的探测入口。
var firstLANIPv4 = defaultFirstLANIPv4

// 面板地址来源，供调用方给出准确提示。
const (
	panelSourceConfig = "config" // config.json 显式配置的对外地址
	panelSourcePublic = "public" // 探测到的服务器公网出口 IP
	panelSourceLAN    = "lan"    // 回退局域网 IP（仅同网可开）
)

// publicIPEndpoints 返回裸公网 IP 的第三方回显服务（就近取一，任一可用即可）。
var publicIPEndpoints = []string{
	"https://api.ipify.org",
	"https://ifconfig.me/ip",
	"https://icanhazip.com",
	"https://ip.sb",
}

// defaultGetPublicIPv4 通过外部回显服务探测本机公网出口 IPv4。
// 云主机网卡上通常是内网地址（公网经 NAT），只能靠「我看到的对外 IP」来判断。
// 各端点并发探测、共享一个短超时：任一命中合法公网单播地址即采纳，全部失败才报错。
// 串行逐个试最坏需 N×timeout，会阻塞微信消息处理循环；并发 + 2s 上限把最坏耗时锁在约 2s。
func defaultGetPublicIPv4() (string, error) {
	const perProbeTimeout = 2 * time.Second
	client := &http.Client{Timeout: perProbeTimeout}
	ctx, cancel := context.WithTimeout(context.Background(), perProbeTimeout)
	defer cancel()

	type result struct{ ip string }
	// 缓冲=端点数：即使提前返回、不再排空，各 goroutine 也能写入而不阻塞泄漏（ctx 到期后它们快速自终）。
	ch := make(chan result, len(publicIPEndpoints))
	for _, ep := range publicIPEndpoints {
		go func(ep string) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep, nil)
			if err != nil {
				ch <- result{}
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				ch <- result{}
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
			ip := net.ParseIP(strings.TrimSpace(string(body)))
			if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
				ch <- result{}
				return
			}
			ch <- result{ip: ip.String()}
		}(ep)
	}
	for range publicIPEndpoints {
		if r := <-ch; r.ip != "" {
			return r.ip, nil
		}
	}
	return "", errors.New("未能探测到公网 IP")
}

// getPublicIPv4 是可在测试中替换的公网探测入口。
var getPublicIPv4 = defaultGetPublicIPv4

// normalizeBaseURL 补全缺失的 scheme（默认 http://）并确保以 / 结尾，
// 使拼进去的链接是可直接点击的完整基础地址。
func normalizeBaseURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	if !strings.HasSuffix(u, "/") {
		u += "/"
	}
	return u
}

// WebPanelURL 返回管理面板的可点地址与其来源。
//   - cfg.WebBaseURL 非空：用配置的对外可达地址（推荐 https），来源 config；
//   - 否则探测服务器公网出口 IPv4 + apiPort 拼成 http://<public-ip>:<port>/，来源 public
//     （部署在云端时即用此地址；端口映射/安全组放行由部署方在服务器侧配置）；
//   - 公网探测失败才回退局域网 IPv4（来源 lan，仅同网可开）；
//   - 两者都失败返回错误，调用方应如实告知，不得谎称「一切正常」。
func WebPanelURL(cfg *Config) (url string, source string, err error) {
	if cfg == nil {
		return "", "", errors.New("配置为空")
	}
	if base := normalizeBaseURL(cfg.WebBaseURL); base != "" {
		return base, panelSourceConfig, nil
	}
	port := cfg.APIPort
	if port <= 0 {
		port = 17965
	}
	if pip, e := getPublicIPv4(); e == nil && pip != "" {
		pub := fmt.Sprintf("http://%s:%d/", pip, port)
		// 投产前审计 C7：面板全程 HTTP，而这里会把「http://公网IP」主动推给用户。
		// 跨公网时 apiToken、TOTP 密钥与全库聊天明文都走http，拿到 token 即可绕过 2FA
		// 直接调用全部 API（包括导出含密钥的备份）。不能只提「微信会告警」这种轻描写。
		slog.Warn("面板地址自动探测为公网明文 HTTP，建议配置 HTTPS 反代后再使用",
			"url", pub, "risk", "apiToken/TOTP/聊天内容明文过网；公网裸奔时等同于凭证可被窃取")
		return pub, panelSourcePublic, nil
	}
	if ip, e := firstLANIPv4(); e == nil && ip != "" {
		return fmt.Sprintf("http://%s:%d/", ip, port), panelSourceLAN, nil
	}
	return "", "", errors.New("无法确定面板地址：公网探测与局域网探测均失败，请在 config.json 配置 webBaseURL")
}
