package main

// 蓝图 §27 安全测试 —— 十类攻击面的回归钉。
//
// 每一项都针对「真实存在的防线」写可执行断言，而非空喊口号：
//   - SQL Injection：全仓参数化查询；注入串只当字面量，不改语义、不破坏表。
//   - FTS Query Injection：ftsPhraseMatch 把每个关键词裹成带引号短语并转义内部双引号，
//     等价参数绑定，用户输入的 AND/OR/NEAR/通配/列名都无法改变查询语法。
//   - SSRF / Proxy Abuse：模型 Base URL / 代理 URL 的协议白名单（拒 file/gopher/ftp/…）。
//   - API Key Leakage：带口令备份时旁路密钥文件加密为 *.enc，明文哨兵不外泄（§20 的备份侧）。
//   - Authorization：设了 apiToken 后，缺/错 Bearer 一律 401，正确才放行。
//   - Rate Limit：/api/ingest 超限返回 429 + Retry-After（LLM 账单保险丝）。
//   - Path Traversal：备份 zip 拒任意含路径分隔/`..`/目录的条目（防解压实穿越）。
//   - Backup Secret Leakage：同上加密旁路 + 体积/条目数上限（zip bomb）。
//   - CORS：默认不下发任何 Access-Control-Allow-Origin（同源策略即最强 CORS 姿态）。
//   - 以及 zip 文件数/解压体积上限，挡 zip bomb 拖垮服务。

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- 1. SQL Injection ----

func TestSecuritySQLInjectionTreatedAsLiteral(t *testing.T) {
	db := regressionDB(t)
	// 各类 SQLi 载荷作为「数据」写入，必须原样存储、绝不执行。
	payloads := []string{
		`'); DROP TABLE messages;--`,
		`" OR "1"="1`,
		`1; DELETE FROM contacts`,
		`' UNION SELECT settings_json FROM llm_settings--`,
	}
	var cid int64
	for i, p := range payloads {
		id, err := GetOrCreateContact(db, p)
		if err != nil {
			t.Fatalf("用注入串建联系人应成功(参数化): %v", err)
		}
		if i == 0 {
			cid = id
		}
		if _, err := SaveMessages(db, id, []Message{{Sender: "other", Content: p, Timestamp: time.Now()}}); err != nil {
			t.Fatalf("写入注入内容应成功: %v", err)
		}
	}

	// 表仍在、数据完好——注入串没有触发 DROP/DELETE。
	for _, tbl := range []string{"messages", "contacts", "ai_response_cache", "profile_facts"} {
		var n int
		if err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s`, tbl)).Scan(&n); err != nil {
			t.Fatalf("表 %s 应仍存在（注入不应破坏 schema）: %v", tbl, err)
		}
	}

	// 回读：内容逐字等于载荷，未被动过手脚。
	var got string
	if err := db.QueryRow(`SELECT content FROM messages WHERE content=?`, payloads[0]).Scan(&got); err != nil {
		t.Fatalf("应按字面量存回注入内容: %v", err)
	}
	if got != payloads[0] {
		t.Fatalf("内容被篡改: got %q want %q", got, payloads[0])
	}

	// 以注入串作为搜索关键词也不应报错或命中全部。
	res, err := SearchMessages(db, SearchOptions{Query: payloads[1], ContactID: cid, Limit: 10})
	if err != nil {
		t.Fatalf("注入型搜索词应被安全处理而非报错: %v", err)
	}
	if res.Total == len(payloads) {
		t.Fatalf("注入搜索不应匹配到所有行")
	}
}

// ---- 2. FTS Query Injection ----

func TestSecurityFTSQueryIsNeutralized(t *testing.T) {
	// ftsPhraseMatch 必须把每个关键词裹成带引号短语，转义内部双引号，用 AND 连接。
	cases := map[string]string{
		`a" OR "1"="1`:   `"a"" OR ""1""=""1"`, // 双引号被翻倍 → 整体是一个短语，OR 失效
		`NEAR(evil, 10)`: `"NEAR(evil, 10)"`,   // 括号/NEAR 被裹进短语，不当作运算符
		`*`:              `"*"`,                // 通配符被引号裹住
		`col:value`:      `"col:value"`,        // 列名查询语法被裹住
		`foo bar`:        `"foo bar"`,          // 空格短语
	}
	for in, want := range cases {
		got := ftsPhraseMatch([]string{in})
		if got != want {
			t.Fatalf("ftsPhraseMatch(%q)=%q want %q", in, got, want)
		}
	}
	// 多关键词：各自成短语，AND 连接，任一都不会泄漏裸语法。
	got := ftsPhraseMatch([]string{`x" OR "1"="1`, "y"})
	if got != `"x"" OR ""1""=""1" AND "y"` {
		t.Fatalf("多词拼接结果异常: %q", got)
	}
}

// ---- 3/9. SSRF & Proxy Abuse：协议白名单 ----

func TestSecurityURLSchemeAllowlist(t *testing.T) {
	// 代理 URL 只准 http/https/socks5，挡掉 file/gopher/ftp 等被滥用作内网探针/协议攻击。
	okProxy := []string{"", "http://127.0.0.1:7890", "https://proxy.example.com:8443", "socks5://10.0.0.1:1080"}
	for _, s := range okProxy {
		if err := validateLLMProxyURL(s); err != nil {
			t.Fatalf("合法代理 URL 应通过 %q: %v", s, err)
		}
	}
	badProxy := []string{"ftp://host", "file:///etc/passwd", "gopher://127.0.0.1:11211", "://x", "dict://localhost:11111"}
	for _, s := range badProxy {
		if err := validateLLMProxyURL(s); err == nil {
			t.Fatalf("危险协议应被拒 %q", s)
		}
	}
	// Base URL 协议护栏：拒带凭据 userinfo、非 http(s) 协议。
	if err := validateLLMBaseURL("baseURL", "http://user:pass@internal-meta/"); err == nil {
		t.Fatal("带 userinfo 的 Base URL 应被拒")
	}
	if err := validateLLMBaseURL("baseURL", "file:///etc/passwd"); err == nil {
		t.Fatal("file:// Base URL 应被拒")
	}
}

// ---- 4/8. Backup Secret Leakage：带口令备份加密旁路 ----

func TestSecurityBackupSecretsEncryptedWithPassword(t *testing.T) {
	db := regressionDB(t)
	dir := t.TempDir()
	const sentinel = "sk-LEAKME-1234567890abcdef"
	cfgJSON, _ := json.Marshal(map[string]any{"llm": map[string]any{"apiKey": sentinel}})
	if err := os.WriteFile(filepath.Join(dir, "config.json"), cfgJSON, 0600); err != nil {
		t.Fatal(err)
	}

	zipPath, cleanup, err := BuildBackupZipWithPassword(db, dir, []string{"config.json"}, "hunter2")
	if err != nil {
		t.Fatalf("带口令备份失败: %v", err)
	}
	defer cleanup()
	defer os.Remove(zipPath)

	names, plaintext := zipEntries(zipPath)
	if !containsStr(names, "config.json.enc") {
		t.Fatalf("旁路密钥文件应加密为 config.json.enc，实际条目=%v", names)
	}
	if containsStr(names, "config.json") {
		t.Fatalf("加密备份不应再含明文 config.json 条目")
	}
	if strings.Contains(plaintext, sentinel) {
		t.Fatal("备份中出现明文 API Key，密钥外泄！")
	}

	// 对照：无口令时旁路以明文打包（用户自担风险，非缺陷），确保上面断言不是恒真。
	zip2, cleanup2, err := BuildBackupZipWithPassword(db, dir, []string{"config.json"}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup2()
	defer os.Remove(zip2)
	names2, plain2 := zipEntries(zip2)
	if !containsStr(names2, "config.json") || !strings.Contains(plain2, sentinel) {
		t.Fatalf("无口令备份应含明文 config.json 与哨兵（对照失效）: names=%v", names2)
	}
}

// ---- 7. Path Traversal：备份 zip 拒穿越条目 ----

func TestSecurityBackupRejectsTraversalEntries(t *testing.T) {
	for _, bad := range []string{"../evil.txt", "sub/evil.txt", "a\\b.txt", ".."} {
		zipPath := writeZipWithEntry(t, bad, []byte("x"))
		_, _, _, err := extractBackupZip(zipPath)
		if err == nil || !strings.Contains(err.Error(), "非法路径") {
			t.Fatalf("穿越条目 %q 应被拒并提示非法路径, got err=%v", bad, err)
		}
		os.Remove(zipPath)
	}
}

// ---- 10. Zip Bomb：文件数上限 ----

func TestSecurityBackupEnforcesEntryLimit(t *testing.T) {
	// 造 backupMaxEntries+1 个合法纯文件名条目 → 应在解析阶段以「文件数超过上限」拒绝。
	dir := t.TempDir()
	zp := filepath.Join(dir, "bomb.zip")
	f, err := os.Create(zp)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for i := 0; i <= backupMaxEntries; i++ {
		w, _ := zw.Create(fmt.Sprintf("f%03d.bin", i))
		_, _ = w.Write([]byte("x"))
	}
	_ = zw.Close()
	_ = f.Close()

	_, _, _, err = extractBackupZip(zp)
	if err == nil || !strings.Contains(err.Error(), "文件数") {
		t.Fatalf("超过条目上限应被拒, got err=%v", err)
	}
}

// ---- 9. CORS：默认不下发宽松跨域头 ----

func TestSecurityNoPermissiveCORS(t *testing.T) {
	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))

	// 绝不出现允许任意源的跨域头（尤其带凭据的 *）。
	for _, k := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials"} {
		if v := rec.Header().Get(k); v != "" {
			t.Fatalf("不应下发宽松 CORS 头 %s=%q", k, v)
		}
	}
	// 基础安全头必须在位。
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("缺 X-Content-Type-Options")
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("缺 X-Frame-Options")
	}
}

// ---- 5. Authorization：缺/错 Token → 401 ----

func TestSecurityAuthorizationRequired(t *testing.T) {
	db := regressionDB(t)
	s := &apiServer{
		db:       db,
		cfg:      &Config{APIToken: "secTok"},
		sessions: &webSessionStore{sessions: map[string]time.Time{}},
	}
	hit := func(auth string) int {
		r := httptest.NewRequest("GET", "/api/status", nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		s.route(w, r)
		return w.Code
	}
	if c := hit(""); c != http.StatusUnauthorized {
		t.Fatalf("无凭证应 401, got %d", c)
	}
	if c := hit("Bearer wrong"); c != http.StatusUnauthorized {
		t.Fatalf("错 token 应 401, got %d", c)
	}
	if c := hit("Bearer secTok"); c == http.StatusUnauthorized {
		t.Fatalf("正确 token 不应 401")
	}
}

// ---- 6. Rate Limit：/api/ingest 超限 429 ----

func TestSecurityIngestRateLimit(t *testing.T) {
	db := regressionDB(t)
	s := &apiServer{
		db:       db,
		cfg:      &Config{APIToken: "secTok"},
		sessions: &webSessionStore{sessions: map[string]time.Time{}},
		ingestRL: newRateLimiter(2, time.Minute),
		// guard 留 nil：RecordDenied/RecordBannedHit 均 nil 安全，429 路径只记审计不依赖它。
	}
	codes := []int{}
	for i := 0; i < 3; i++ {
		// 空 text 在限流之后早退为 400，无需真实 LLM；第三次应被限流器挡在 429。
		r := httptest.NewRequest("POST", "/api/ingest", strings.NewReader(`{"text":"","analyze":false}`))
		r.Header.Set("Authorization", "Bearer secTok")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.route(w, r)
		codes = append(codes, w.Code)
		if i == 2 {
			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("第 3 次(超限)应 429, got %d (all=%v)", w.Code, codes)
			}
			if w.Header().Get("Retry-After") == "" {
				t.Fatal("429 应带 Retry-After 头")
			}
		}
	}
}

// ---- helpers ----

// zipEntries 返回 zip 内所有条目名与「拼接后的文本内容」，供明文泄漏扫描。
func zipEntries(zipPath string) (names []string, plaintext string) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, ""
	}
	defer zr.Close()
	var buf bytes.Buffer
	for _, f := range zr.File {
		names = append(names, f.Name)
		if strings.HasSuffix(f.Name, backupEncSuffix) {
			continue // 密文不纳入明文扫描
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		_, _ = buf.ReadFrom(rc)
		rc.Close()
	}
	return names, buf.String()
}

// writeZipWithEntry 造一个只含指定条目名的 zip，返回路径。
func writeZipWithEntry(t *testing.T, entryName string, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	zp := filepath.Join(dir, "t.zip")
	f, err := os.Create(zp)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create(entryName)
	if err != nil {
		_ = f.Close()
		// 某些非法名 zw.Create 可能直接报错；视为「无法构造」，让调用方据 extract 结果判定。
		_ = zw.Close()
		return zp
	}
	_, _ = w.Write(data)
	_ = zw.Close()
	_ = f.Close()
	return zp
}

func containsStr(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
