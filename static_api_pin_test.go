package main

// P1 反向钉：此前出现「后端全有、网页零消费」且文档声称有 Web UI，长期没被发现，
// 成因就是缺这条链路的自动校验。双向钉住：
//   A) 前端(app.js/index.html)里出现的每个 /api/ 路径，其后端顶层域必须在路由表注册；
//   B) 后端注册的每个 /api/ 顶层域，必须「被前端消费」或「在 README 接口一览登记」——
//      既没接进界面、又不出现在文档里的端点，正是本次评审抓到的问题。
// 两侧清单都从源码/静态资源扫出，不维护手工名单，避免名单本身过期。

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	reFrontAPI = regexp.MustCompile(`/api/[A-Za-z0-9_./{}$-]+`)
	reGoDomain = regexp.MustCompile(`parts\[0\]\s*==\s*"([a-z0-9.][a-z0-9._-]*)"`)
	reGoExactP = regexp.MustCompile(`p\s*==\s*"([a-z0-9.][a-z0-9._-]*)"`)
)

// apiDocExempt 是天然不经网页的合法例外（必须写理由，防止悄悄扩表）。
var apiDocExempt = map[string]string{
	"auth":         "网页登录通道（/api/auth/*），形式特殊",
	"calendar.ics": "日历订阅直链，由日历客户端带订阅密钥访问，不经 app.js",
	"ingest":       "桌面端与网页共用但由后端内部/桌面调用为主，网页粘贴走 smart-paste 端点",
}

// readFrontend 读前端两类静态资源（走 embed，保证与发布产物一致）。
func readFrontend(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	for _, f := range []string{"static/app.js", "static/index.html"} {
		b, err := staticFS.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", f, err)
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// readNonTestGoSource 拼接本包所有非测试 Go 源码（磁盘读取，测试进程 CWD 即包目录）。
func readNonTestGoSource(t *testing.T) string {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil || len(names) == 0 {
		t.Skipf("无法枚举 Go 源文件: err=%v n=%d", err, len(names))
	}
	var sb strings.Builder
	n := 0
	for _, f := range names {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		sb.Write(b)
		sb.WriteByte('\n')
		n++
	}
	if n < 50 {
		t.Fatalf("非测试 Go 源文件枚举数异常偏低(%d)，检查枚举逻辑", n)
	}
	return sb.String()
}

// registeredDomains 从 Go 源码抽取所有 /api/ 顶层域（parts[0]=="x" 与整路径精确匹配 p=="x"）。
func registeredDomains(t *testing.T) map[string]bool {
	t.Helper()
	src := readNonTestGoSource(t)
	set := map[string]bool{}
	for _, m := range reGoDomain.FindAllStringSubmatch(src, -1) {
		set[m[1]] = true
	}
	for _, m := range reGoExactP.FindAllStringSubmatch(src, -1) {
		set[m[1]] = true
	}
	if len(set) < 15 {
		t.Fatalf("后端顶层域抽取数异常偏低(%d)，检查正则是否失效", len(set))
	}
	return set
}

// normalizeAPIPath 处理模板串与尾随标点，返回去重后的前端调用集合。
func normalizeAPIPath(raw string) string {
	s := strings.TrimRight(raw, ".,;)\"'`")
	if i := strings.Index(s, "${"); i >= 0 { // 模板串截到变量前
		s = s[:i]
	}
	return strings.TrimRight(s, "/")
}

func TestFrontendAPICallsHaveBackend(t *testing.T) {
	reg := registeredDomains(t)
	front := readFrontend(t)

	seen := map[string]bool{}
	var missing []string
	for _, m := range reFrontAPI.FindAllString(front, -1) {
		p := normalizeAPIPath(m)
		dom := strings.TrimPrefix(p, "/api/")
		if i := strings.Index(dom, "/"); i >= 0 {
			dom = dom[:i]
		}
		if dom == "" || seen[p] {
			continue
		}
		seen[p] = true
		if !reg[dom] {
			missing = append(missing, p)
		}
	}
	if len(seen) < 30 {
		t.Fatalf("前端去重 API 调用数异常偏低(%d)，检查正则是否失效", len(seen))
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("前端调用了 %d 个后端未注册的路径（路由被删/改名？）：%v", len(missing), missing)
	}
	t.Logf("A) 前端 %d 个去重 /api/ 调用全部有后端注册", len(seen))
}

func TestRegisteredAPIDomainsAreConsumedOrDocumented(t *testing.T) {
	reg := registeredDomains(t)
	front := readFrontend(t)

	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Skipf("读不到 README.md: %v", err)
	}
	doc := string(readme)

	var orphans []string
	for dom := range reg {
		needle := "/api/" + dom
		if strings.Contains(front, needle) || strings.Contains(doc, needle) {
			continue
		}
		if _, ok := apiDocExempt[dom]; ok {
			continue
		}
		orphans = append(orphans, dom)
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		t.Fatalf("这些后端 API 域既未被前端消费、也未登记 README 接口一览（文档与实现脱节）：%v", orphans)
	}
	t.Logf("B) %d 个后端 API 域均「已接入前端」或「已登记 README」", len(reg))
}
