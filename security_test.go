package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestGuard 构造一个只作用于临时目录的 securityGuard：封禁名单落在 temp，
// 安全日志丢弃到 io.Discard，不依赖全局 dbPath，也不污染任何真实文件。
func newTestGuard(t *testing.T, path string) *securityGuard {
	t.Helper()
	return &securityGuard{
		fails:  make(map[string]int),
		banned: make(map[string]banRecord),
		hitRL:  newRateLimiter(1, time.Minute),
		denyRL: newRateLimiter(5, time.Minute),
		secLog: slog.New(slog.NewTextHandler(io.Discard, nil)),
		path:   path,
	}
}

// 达到 maxAuthFailures 前不封禁，达到即永久封禁，并清零失败计数。
func TestSecurityGuard_BanAfterThreshold(t *testing.T) {
	g := newTestGuard(t, filepath.Join(t.TempDir(), "banned_ips.json"))
	const ip = "203.0.113.9"

	for i := 1; i < maxAuthFailures; i++ {
		g.RecordAuthFailure(ip, "token 错误")
		if g.IsBanned(ip) {
			t.Fatalf("第 %d 次失败就封禁了，应达 %d 次才封", i, maxAuthFailures)
		}
	}
	// 阈值内的失败被计数
	if g.fails[ip] != maxAuthFailures-1 {
		t.Fatalf("失败计数 = %d, 期望 %d", g.fails[ip], maxAuthFailures-1)
	}
	// 最后一次触达阈值 → 封禁
	g.RecordAuthFailure(ip, "动态码错误")
	if !g.IsBanned(ip) {
		t.Fatalf("达 %d 次失败后应永久封禁", maxAuthFailures)
	}
	if _, stillCounting := g.fails[ip]; stillCounting {
		t.Fatalf("封禁后应清零该 IP 的失败计数")
	}
	rec := g.banned[ip]
	if rec.Fails != maxAuthFailures || rec.Reason != "动态码错误" {
		t.Fatalf("封禁记录不符：fails=%d reason=%q", rec.Fails, rec.Reason)
	}
}

// 封禁名单持久化到磁盘，重启（新 guard 载入同一路径）后依然生效。
func TestSecurityGuard_PersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "banned_ips.json")
	g := newTestGuard(t, path)
	const ip = "198.51.100.7"
	for i := 0; i < maxAuthFailures; i++ {
		g.RecordAuthFailure(ip, "爆破")
	}

	g2 := newTestGuard(t, path)
	g2.loadBans()
	if !g2.IsBanned(ip) {
		t.Fatalf("重新载入后封禁应仍然生效（持久化失效）")
	}
	list := g2.ListBans()
	if len(list) != 1 || list[0].IP != ip {
		t.Fatalf("ListBans 返回异常：%+v", list)
	}
}

// Unban 解除封禁；对未封禁 IP 返回 false（幂等、不误删）。
func TestSecurityGuard_Unban(t *testing.T) {
	path := filepath.Join(t.TempDir(), "banned_ips.json")
	g := newTestGuard(t, path)
	const ip = "203.0.113.20"
	for i := 0; i < maxAuthFailures; i++ {
		g.RecordAuthFailure(ip, "爆破")
	}
	if !g.IsBanned(ip) {
		t.Fatal("前置：应已封禁")
	}
	if !g.Unban(ip) {
		t.Fatal("Unban 应返回 true")
	}
	if g.IsBanned(ip) {
		t.Fatal("Unban 后不应再是封禁状态")
	}
	if g.Unban(ip) {
		t.Fatal("对未封禁 IP 再 Unban 应返回 false")
	}
	// 解除后应落盘：新 guard 载入不应再含该 IP
	g2 := newTestGuard(t, path)
	g2.loadBans()
	if g2.IsBanned(ip) {
		t.Fatal("解封应持久化，重启后不应恢复封禁")
	}
}

// 登录成功清零失败计数，避免"偶尔输错 + 长期累积"把正常用户逼到封禁。
func TestSecurityGuard_SuccessResetsFailCount(t *testing.T) {
	g := newTestGuard(t, filepath.Join(t.TempDir(), "banned_ips.json"))
	const ip = "203.0.113.30"
	for i := 0; i < 5; i++ {
		g.RecordAuthFailure(ip, "手滑")
	}
	g.RecordAuthSuccess(ip) // 成功 → 计数清零
	if g.fails[ip] != 0 {
		t.Fatalf("登录成功后失败计数应清零，实际 %d", g.fails[ip])
	}
	// 再失败 5 次：若未清零则 5+5=10 已封；正确行为是仍计数到 5，未封
	for i := 0; i < 5; i++ {
		g.RecordAuthFailure(ip, "手滑")
	}
	if g.IsBanned(ip) {
		t.Fatal("成功清零后重新累计未到阈值，不应封禁")
	}
}

// 空 IP 一律被 guard 忽略（不计入、不崩溃）。
func TestSecurityGuard_IgnoresEmptyIP(t *testing.T) {
	g := newTestGuard(t, filepath.Join(t.TempDir(), "banned_ips.json"))
	g.RecordAuthFailure("", "x")
	if len(g.fails) != 0 {
		t.Fatal("空 IP 不应被计数")
	}
	if g.IsBanned("") {
		t.Fatal("空 IP 不应被视为封禁")
	}
}

// 固定窗口限流器：窗口内达到上限后拒绝，跨 key 相互独立，nil/空 key 放行。
func TestRateLimiter_Allow(t *testing.T) {
	l := newRateLimiter(2, time.Minute)
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("第 1 次应放行")
	}
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("第 2 次应放行（=limit）")
	}
	ok, retry := l.Allow("k")
	if ok {
		t.Fatal("第 3 次（超 limit）应被拒绝")
	}
	if retry < 1 {
		t.Fatalf("拒绝时应返回正的等待秒数，实际 %d", retry)
	}
	// 不同 key 独立计数
	if ok, _ := l.Allow("other"); !ok {
		t.Fatal("另一个 key 首次应放行，不受 k 的计数影响")
	}
	// 空 key 与 nil 限流器：直接放行（放行优于误伤）
	if ok, _ := l.Allow(""); !ok {
		t.Fatal("空 key 应放行")
	}
	var nilL *rateLimiter
	if ok, _ := nilL.Allow("k"); !ok {
		t.Fatal("nil 限流器应放行")
	}
}

// securityHeaders 中间件给所有响应挂上基础安全头，并透传下游处理。
func TestSecurityHeaders(t *testing.T) {
	called := false
	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte("ok"))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/anything", nil))

	if !called {
		t.Fatal("下游 handler 未被调用（中间件吞掉了请求）")
	}
	checks := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for k, want := range checks {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("响应头 %s = %q, 期望 %q", k, got, want)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP 缺少 default-src 'self'：%q", csp)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("响应体被篡改：%q", rec.Body.String())
	}
}

// 保证测试用到的封禁文件权限为 0600（含 IP 与原因，属敏感审计数据）。
func TestSecurityGuard_BanFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "banned_ips.json")
	g := newTestGuard(t, path)
	for i := 0; i < maxAuthFailures; i++ {
		g.RecordAuthFailure("203.0.113.1", "爆破")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("封禁文件应存在：%v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Fatalf("封禁文件权限 = %o, 期望 600", perm)
	}
}
