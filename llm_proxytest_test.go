package main

// Phase D（网络代理连通性测试）回归：
//   - fetchEgressIP 兼容纯文本与 {"ip":...} JSON，端点缺失返回哨兵错误；
//   - runProxyTest：直连取 IP + 站点延迟；Enabled=false 不测经代理出口；
//   - 失败哲学：不可达代理/站点记为数据、不 panic、不报错；
//   - POST /api/llm/proxy/test 端到端（注入 httptest 目标，绝不碰真实外网），结果落库供状态页；
//   - POST /api/llm/model/test：活动模型可达返回 ok=true，未配置返回 ok=false（均 200）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchEgressIP(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("203.0.113.9\n"))
	}))
	defer plain.Close()
	js := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ip":"198.51.100.7"}`))
	}))
	defer js.Close()

	c := httpClientFor("", 2*time.Second)
	if ip, err := fetchEgressIP(c, plain.URL); err != nil || ip != "203.0.113.9" {
		t.Fatalf("纯文本解析失败 ip=%q err=%v", ip, err)
	}
	if ip, err := fetchEgressIP(c, js.URL); err != nil || ip != "198.51.100.7" {
		t.Fatalf("JSON 解析失败 ip=%q err=%v", ip, err)
	}
	if _, err := fetchEgressIP(c, "  "); err != errNoIPEndpoint {
		t.Fatalf("空端点应返回哨兵错误，得 %v", err)
	}
}

func TestRunProxyTestDirectAndGracefulFailure(t *testing.T) {
	ipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("1.2.3.4"))
	}))
	defer ipSrv.Close()
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer okSrv.Close()
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer errSrv.Close()

	// 直连（Enabled=false）：取直连 IP，测站点，不测经代理出口。
	out := runProxyTest(LLMProxy{Enabled: false}, ProxyTestOptions{
		IPDirectURL: ipSrv.URL,
		SiteURLs:    []string{okSrv.URL, errSrv.URL},
		Timeout:     2 * time.Second,
	})
	if out.DirectIP != "1.2.3.4" {
		t.Fatalf("直连出口 IP 应为 1.2.3.4，得 %q", out.DirectIP)
	}
	if out.EgressIP != "" {
		t.Fatalf("未启用代理不应有经代理出口 IP，得 %q", out.EgressIP)
	}
	if len(out.Sites) != 2 || !out.Sites[0].OK || out.Sites[1].OK {
		t.Fatalf("站点可达性判定错误，得 %#v", out.Sites)
	}

	// 失败哲学：不可达代理不 panic，出口 IP 记空、站点记不可达。
	bad := runProxyTest(LLMProxy{Enabled: true, URL: "http://127.0.0.1:1"}, ProxyTestOptions{
		IPDirectURL:   ipSrv.URL,
		IPViaProxyURL: ipSrv.URL,
		SiteURLs:      []string{okSrv.URL},
		Timeout:       2 * time.Second,
	})
	if bad.EgressIP != "" {
		t.Fatalf("不可达代理应得空出口 IP，得 %q", bad.EgressIP)
	}
	if len(bad.Sites) != 1 || bad.Sites[0].OK {
		t.Fatalf("不可达代理下站点应记为失败，得 %#v", bad.Sites)
	}
}

func TestLLMProxyTestAPIEndToEnd(t *testing.T) {
	db := regressionDB(t)
	ipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("9.9.9.9"))
	}))
	defer ipSrv.Close()
	siteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer siteSrv.Close()

	// 注入探测目标，测试结束恢复默认。
	old := proxyTestOptionsFn
	proxyTestOptionsFn = func() ProxyTestOptions {
		return ProxyTestOptions{IPDirectURL: ipSrv.URL, SiteURLs: []string{siteSrv.URL}, Timeout: 2 * time.Second}
	}
	defer func() { proxyTestOptionsFn = old }()

	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}

	w := callAPI(s, http.MethodPost, "/api/llm/proxy/test", `{"enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST proxy/test: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK    bool     `json:"ok"`
		Proxy LLMProxy `json:"proxy"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Proxy.DirectIP != "9.9.9.9" {
		t.Fatalf("应返回直连 IP 9.9.9.9，得 %#v", resp.Proxy)
	}
	if len(resp.Proxy.Sites) != 1 || !resp.Proxy.Sites[0].OK {
		t.Fatalf("站点延迟结果应可达，得 %#v", resp.Proxy.Sites)
	}
	// 结果应持久化到 llm_settings，供状态页读取。
	saved, err := loadLLMSettings(db)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Proxy.TestedAt == "" || saved.Proxy.DirectIP != "9.9.9.9" {
		t.Fatalf("测试结果应落库，得 %#v", saved.Proxy)
	}
}

func TestLLMModelTestAPI(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}

	// 未配置活动模型：ok=false，不报错。
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	s.llm = NewLLMClient(&Config{}).WithDB(db)
	w := callAPI(s, http.MethodPost, "/api/llm/model/test", "")
	if w.Code != http.StatusOK {
		t.Fatalf("未配置模型也应 200: %d %s", w.Code, w.Body.String())
	}
	var unconf struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &unconf); err != nil {
		t.Fatal(err)
	}
	if unconf.OK {
		t.Fatalf("未配置模型应 ok=false，得 %#v", unconf)
	}

	// 配置活动模型（指向 httptest mock）：可达 → ok=true。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()
	if err := saveLLMSettings(db, LLMSettings{
		ActiveProfileID: "m",
		Profiles:        []LLMProfile{{ID: "m", Label: "Mock", Provider: "custom", BaseURL: srv.URL, APIKey: "sk-x", Model: "mock-1"}},
	}); err != nil {
		t.Fatal(err)
	}
	s.llm = NewLLMClient(&Config{}).WithDB(db)
	w2 := callAPI(s, http.MethodPost, "/api/llm/model/test", "")
	var reach struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &reach); err != nil {
		t.Fatal(err)
	}
	if w2.Code != http.StatusOK || !reach.OK {
		t.Fatalf("模型可达应 ok=true，得 code=%d body=%s", w2.Code, w2.Body.String())
	}
}
