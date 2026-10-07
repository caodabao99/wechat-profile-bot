package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 本文件是 API 服务的安全防护层，包含三块能力：
//
//  1. 登录失败计数 + IP 永久封禁（securityGuard）
//     同一 IP 在网页登录通道（/api/auth/token、/api/auth/2fa）累计失败 10 次即永久封禁，
//     封禁后该 IP 访问任何路径都直接 403。封禁名单持久化到 banned_ips.json，重启不丢失。
//
//  2. 独立安全日志（security.log）
//     所有认证失败、封禁、限流命中都写进这个单独的文件，与 bot.log 分开，方便审计和排查
//     "是不是有人在爆破我"。文件权限 0600。
//
//  3. 接口限流（rateLimiter）
//     给 /api/ingest 这类会真实消耗 LLM 额度的接口装保险丝，防止 apiToken 泄露后被人
//     写循环刷爆账单。
//
// 设计取舍（重要）：
//   - 只有【登录通道】的失败才计入封禁计数。普通 /api/ 请求带错 token 只记日志、不计数。
//     原因：桌面端远程模式如果配了过期 token，会短时间内高频重试，若计入封禁会把用户
//     自己的 IP 永久封死，属于"安全机制反而搞挂正常功能"。而登录通道的 token 是 128 位
//     随机数，暴力破解在数学上不可行，计数封禁主要是防自动化脚本无脑扫。
//   - 封禁是永久的（按需求），但提供 `--unban <ip>` 命令行开关自救，避免误封自己后
//     只能上服务器删文件。
//   - IP 一律取 RemoteAddr，不信任 X-Forwarded-For 等可伪造的头（与 checkIPWhitelist 一致）。

const (
	// maxAuthFailures 同一 IP 登录失败达到此次数即永久封禁
	maxAuthFailures = 10
	// securityLogName 独立安全日志文件名（与数据库同目录）
	securityLogName = "security.log"
	// bannedIPsFileName 封禁名单持久化文件名（与数据库同目录）
	bannedIPsFileName = "banned_ips.json"
	// ingestRateLimit /api/ingest 每分钟最大请求数（按 token 维度，无 token 时按 IP）
	ingestRateLimit = 120
	// ingestRateWindow 限流统计窗口
	ingestRateWindow = time.Minute
)

// banRecord 一条封禁记录（同时用于持久化和日志输出）
type banRecord struct {
	IP       string `json:"ip"`
	Reason   string `json:"reason"`
	Fails    int    `json:"fails"`
	BannedAt string `json:"banned_at"`
}

// securityGuard 登录失败计数与 IP 封禁管理
type securityGuard struct {
	mu     sync.Mutex
	fails  map[string]int       // IP -> 未达阈值的累计失败次数
	banned map[string]banRecord // IP -> 永久封禁记录
	secLog *slog.Logger         // 独立安全日志（security.log）
	hitRL  *rateLimiter         // "已封禁 IP 仍在访问"的日志节流器
	denyRL *rateLimiter         // "白名单/Token 拒绝"的日志节流器（防扫描器刷爆日志）
	path   string               // banned_ips.json 路径
}

// securityLogPath 返回安全日志文件路径（与数据库同目录）
func securityLogPath() string {
	return filepath.Join(filepath.Dir(dbPath()), securityLogName)
}

// bannedIPsPath 返回封禁名单文件路径
func bannedIPsPath() string {
	return filepath.Join(filepath.Dir(dbPath()), bannedIPsFileName)
}

// newSecurityGuard 初始化安全防护：打开独立日志文件、载入已有封禁名单。
// 任何一步失败都只降级（退回主日志 / 空名单），不阻断服务启动——
// 安全组件自身出问题不应该让整个 bot 起不来。
func newSecurityGuard() *securityGuard {
	g := &securityGuard{
		fails:  make(map[string]int),
		banned: make(map[string]banRecord),
		hitRL:  newRateLimiter(1, time.Minute), // 每个被封 IP 每分钟最多记一条"仍在访问"
		denyRL: newRateLimiter(5, time.Minute), // 每个 IP 每类拒绝每分钟最多记 5 条
		path:   bannedIPsPath(),
	}

	// 独立安全日志。打开失败时退回 slog 默认输出，功能不受影响
	logPath := securityLogPath()
	// 先轮转再打开：security.log 记录登录失败/封禁/限流/429，长时间被探测时增速很快，
	// 不设上限会吃满磁盘（投产前审计 N5）
	if rotated, err := rotateIfNeeded(logPath, logMaxBytes, logKeep); err != nil {
		slog.Warn("security.log 轮转失败，本次跳过", "err", err)
	} else if rotated {
		slog.Info("security.log 已轮转", "file", logPath, "keep", logKeep)
	}
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); err == nil {
		g.secLog = slog.New(slog.NewTextHandler(f, nil))
		slog.Info("安全日志已启用", "file", logPath)
	} else {
		slog.Warn("无法打开安全日志文件，安全事件将只记录到主日志", "path", logPath, "err", err)
		g.secLog = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	g.loadBans()
	return g
}

// log 写独立安全日志，同时把关键级别（Warn/Error）镜像到主日志，
// 这样运维只看 bot.log 也能发现"有人在爆破"和"某 IP 已被封禁"
func (g *securityGuard) log(level slog.Level, msg string, kv ...interface{}) {
	g.secLog.Log(nil, level, msg, kv...) //nolint:staticcheck // ctx 仅用于 Handler 扩展，这里不需要
	if level >= slog.LevelWarn {
		slog.Log(nil, level, "[安全] "+msg, kv...) //nolint:staticcheck
	}
}

// loadBans 从磁盘载入封禁名单。文件不存在是首次启动的正常情况。
func (g *securityGuard) loadBans() {
	data, err := os.ReadFile(g.path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("封禁名单读取失败，本次以空名单启动", "path", g.path, "err", err)
			g.log(slog.LevelWarn, "封禁名单读取失败", "path", g.path, "err", err)
		}
		return
	}
	var list []banRecord
	if err := json.Unmarshal(data, &list); err != nil {
		// 名单损坏不能当成"没有封禁"，否则等于自动解封所有人。
		// 保留原文件为 .corrupt 便于人工检查，然后以空名单启动。
		slog.Error("封禁名单解析失败，已备份为 .corrupt，本次以空名单启动", "path", g.path, "err", err)
		g.log(slog.LevelError, "封禁名单解析失败", "path", g.path, "err", err)
		_ = os.Rename(g.path, g.path+".corrupt")
		return
	}
	for _, r := range list {
		if r.IP == "" {
			continue
		}
		g.banned[r.IP] = r
	}
	if len(g.banned) > 0 {
		slog.Warn("已载入 IP 封禁名单", "count", len(g.banned))
		g.log(slog.LevelInfo, "封禁名单已载入", "count", len(g.banned))
	}
}

// saveBans 持久化封禁名单（原子写 + 0600）。调用方需持有 g.mu。
func (g *securityGuard) saveBans() {
	list := make([]banRecord, 0, len(g.banned))
	for _, r := range g.banned {
		list = append(list, r)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		g.log(slog.LevelError, "封禁名单序列化失败", "err", err)
		return
	}
	tmp := g.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		g.log(slog.LevelError, "封禁名单写入失败", "err", err)
		return
	}
	if err := os.Rename(tmp, g.path); err != nil {
		g.log(slog.LevelError, "封禁名单落盘失败", "err", err)
	}
}

// IsBanned 判断 IP 是否已被永久封禁
func (g *securityGuard) IsBanned(ip string) bool {
	if g == nil || ip == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.banned[ip]
	return ok
}

// RecordBannedHit 记录一次"已封禁 IP 仍在访问"。
// 做每分钟一条的日志节流：被封的机器往往会持续重试，不节流会把 security.log 刷爆，
// 反而淹没有价值的记录。
func (g *securityGuard) RecordBannedHit(ip, path string) {
	if g == nil {
		return
	}
	if g.hitRL != nil {
		if ok, _ := g.hitRL.Allow("banhit:" + ip); !ok {
			return
		}
	}
	g.log(slog.LevelWarn, "已封禁 IP 访问被拒绝", "ip", ip, "path", path)
}

// RecordAuthFailure 记录一次登录失败。累计达到 maxAuthFailures 时永久封禁该 IP。
// reason 用于日志说明失败在哪一步（token 错误 / 动态码错误）。
func (g *securityGuard) RecordAuthFailure(ip, reason string) {
	if g == nil || ip == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	if _, ok := g.banned[ip]; ok {
		// 已封禁的 IP 还在试，只记日志，不重复封禁
		g.log(slog.LevelInfo, "已封禁 IP 仍在尝试登录", "ip", ip, "reason", reason)
		return
	}

	g.fails[ip]++
	n := g.fails[ip]
	remain := maxAuthFailures - n
	g.log(slog.LevelWarn, "登录失败", "ip", ip, "reason", reason, "fails", n, "remaining", remain)

	if n < maxAuthFailures {
		return
	}

	rec := banRecord{
		IP:       ip,
		Reason:   reason,
		Fails:    n,
		BannedAt: time.Now().Format("2006-01-02 15:04:05"),
	}
	g.banned[ip] = rec
	delete(g.fails, ip)
	g.saveBans()
	g.log(slog.LevelError, "IP 已永久封禁",
		"ip", ip, "fails", n, "reason", reason,
		"hint", "如为误封，在服务器执行: wechat-profile-bot --unban "+ip)
}

// RecordAuthSuccess 登录成功后清除该 IP 的失败计数。
// 避免"偶尔输错几次 + 长期累积"把正常用户慢慢推向封禁。
func (g *securityGuard) RecordAuthSuccess(ip string) {
	if g == nil || ip == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if n, ok := g.fails[ip]; ok && n > 0 {
		delete(g.fails, ip)
		g.log(slog.LevelInfo, "登录成功，失败计数已清零", "ip", ip, "prev_fails", n)
	}
}

// RecordDenied 记录一次访问拒绝（IP 不在白名单 / 普通 API 请求 token 无效）。
// 只审计、不计入封禁计数——否则桌面端配了过期 token 高频重试，会把自己 IP 永久封死。
// 内部按 "IP+类型" 每分钟 5 条节流：公网端口天天被扫描器敲，不节流的话
// security.log 会被无意义的探测记录刷满，真正有价值的爆破痕迹反而被淹没。
func (g *securityGuard) RecordDenied(ip, path, kind string) {
	if g == nil {
		return
	}
	if g.denyRL != nil {
		if ok, _ := g.denyRL.Allow("deny:" + kind + ":" + ip); !ok {
			return
		}
	}
	g.log(slog.LevelWarn, "访问被拒绝", "ip", ip, "path", path, "kind", kind)
}

// RecordEvent 记录一条安全审计事件（非失败类，例如关闭 2FA、导出含密钥的备份）。
// 走 Warn 级别，会同时镜像到主日志，方便事后追溯"谁在什么时候动了安全设置"。
func (g *securityGuard) RecordEvent(ip, event string) {
	if g == nil {
		return
	}
	g.log(slog.LevelWarn, event, "ip", ip)
}

// ListBans 返回全部封禁记录（按封禁时间排序，供 --list-bans 使用）
func (g *securityGuard) ListBans() []banRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	list := make([]banRecord, 0, len(g.banned))
	for _, r := range g.banned {
		list = append(list, r)
	}
	// 简单插入排序，名单通常只有个位数条目
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].BannedAt < list[j-1].BannedAt; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	return list
}

// Unban 解除某个 IP 的封禁。返回是否确实移除了记录。
// 同时用于 --unban 命令行和运行期解封。
func (g *securityGuard) Unban(ip string) bool {
	ip = strings.TrimSpace(ip)
	if g == nil || ip == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.banned[ip]; !ok {
		return false
	}
	delete(g.banned, ip)
	delete(g.fails, ip)
	g.saveBans()
	g.log(slog.LevelWarn, "IP 封禁已解除", "ip", ip)
	return true
}

// ---------------------------------------------------------------------------
// 限流器
// ---------------------------------------------------------------------------

// rateBucket 一个 key 在当前窗口内的计数
type rateBucket struct {
	windowStart time.Time
	count       int
}

// rateLimiter 固定窗口计数器。够用且实现简单：
// 精确令牌桶需要每个 key 一个定时器，对于"防刷爆账单"这个目标没必要。
type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	buckets map[string]*rateBucket
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, buckets: make(map[string]*rateBucket)}
}

// Allow 判断 key 是否还能放行。返回是否放行 + 需要等待的秒数（放行时为 0）。
func (l *rateLimiter) Allow(key string) (bool, int) {
	if l == nil || key == "" {
		return true, 0
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	// 顺手清理过期窗口，防止 key 数量无上限增长（不同 IP 会不断产生新 key）
	if len(l.buckets) > 512 {
		for k, b := range l.buckets {
			if now.Sub(b.windowStart) > l.window {
				delete(l.buckets, k)
			}
		}
	}

	b, ok := l.buckets[key]
	if !ok || now.Sub(b.windowStart) >= l.window {
		l.buckets[key] = &rateBucket{windowStart: now, count: 1}
		return true, 0
	}
	b.count++
	if b.count > l.limit {
		retry := int(l.window.Seconds()-now.Sub(b.windowStart).Seconds()) + 1
		if retry < 1 {
			retry = 1
		}
		return false, retry
	}
	return true, 0
}

// securityHeaders 给所有响应加上基础安全头。
//
//	X-Content-Type-Options: nosniff —— 禁止浏览器猜测 MIME 类型，防止把上传内容当脚本执行
//	X-Frame-Options: DENY           —— 禁止被 iframe 嵌套，防点击劫持/界面套壳钓鱼
//	Referrer-Policy                 —— 不把带 token 的 URL 泄露给第三方
//	CSP                             —— 只允许同源脚本/样式；vue.global.prod.js 是内联加载的同源文件，
//	                                 页面里的 Vue 模板编译需要 'unsafe-eval'，故保留该项
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'")
		next.ServeHTTP(w, r)
	})
}

// openGuardOffline 为命令行子命令（--unban / --list-bans）构造一个不启动服务的 guard。
// 安全日志指向 io.Discard：改名单这种一次性操作没必要往 security.log 里追加。
func openGuardOffline() *securityGuard {
	g := &securityGuard{
		fails:  make(map[string]int),
		banned: make(map[string]banRecord),
		secLog: slog.New(slog.NewTextHandler(io.Discard, nil)),
		path:   bannedIPsPath(),
	}
	g.loadBans()
	return g
}

// handleSecurityCLI 处理安全相关的命令行开关。
// 返回 true 表示已处理完毕，main 应直接返回（不启动服务）。
// 这是永久封禁机制的自救通道：万一误封了自己的 IP，不用去手工编辑 JSON 文件。
func handleSecurityCLI() bool {
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--unban":
			if i+1 >= len(args) {
				fmt.Println("用法: wechat-profile-bot --unban <ip>")
				os.Exit(2)
			}
			unbanFromCLI(args[i+1])
			return true
		case "--unban-all":
			unbanAllFromCLI()
			return true
		case "--list-bans":
			listBansFromCLI()
			return true
		case "--help", "-h":
			printUsage()
			return true
		}
	}
	return false
}

// printUsage 打印命令行用法
func printUsage() {
	fmt.Println(`wechat-profile-bot —— 微信联系人画像记录服务端

直接运行（无参数）即启动服务：扫码登录 → 长轮询收消息 → 提供 REST API 和网页管理界面。

安全相关开关（执行完立即退出，不启动服务）：
  --list-bans        查看被永久封禁的 IP 名单
  --unban <ip>       解除某个 IP 的封禁（误封自己时用这个自救）
  --unban-all        清空整个封禁名单
  --help, -h         显示本帮助

相关文件（均与数据库同目录）：
  config.json             配置（apiToken、模型 Key 等）
  security.log            安全日志：登录失败、封禁、限流、白名单拒绝
  banned_ips.json         封禁名单（永久生效，重启不丢失）
  bot.log                 运行日志`)
}

// unbanFromCLI 命令行解封入口：不启动服务，只改名单文件后退出。
func unbanFromCLI(ip string) {
	ip = strings.TrimSpace(ip)
	g := openGuardOffline()
	if g.Unban(ip) {
		fmt.Printf("已解除封禁: %s\n", ip)
	} else {
		fmt.Printf("该 IP 不在封禁名单中: %s\n", ip)
	}
}

// unbanAllFromCLI 清空整个封禁名单
func unbanAllFromCLI() {
	g := openGuardOffline()
	n := len(g.ListBans())
	if n == 0 {
		fmt.Println("封禁名单本来就是空的")
		return
	}
	g.mu.Lock()
	g.banned = make(map[string]banRecord)
	g.fails = make(map[string]int)
	g.saveBans()
	g.mu.Unlock()
	fmt.Printf("已清空封禁名单，共解除 %d 个 IP\n", n)
}

// listBansFromCLI 命令行查看封禁名单
func listBansFromCLI() {
	g := openGuardOffline()
	list := g.ListBans()
	if len(list) == 0 {
		fmt.Println("封禁名单为空")
		return
	}
	fmt.Printf("共 %d 条封禁记录:\n", len(list))
	for _, r := range list {
		fmt.Printf("  %-16s  %s  失败%d次  原因: %s\n", r.IP, r.BannedAt, r.Fails, r.Reason)
	}
	fmt.Printf("\n名单文件: %s\n安全日志: %s\n", g.path, securityLogPath())
}
