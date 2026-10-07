package main

// P1 反向钉：此前出现「后端全有、网页零消费」且文档声称有 Web UI，长期没被发现，
// 成因就是缺这条链路的自动校验。双向钉住：
//   A) 前端(app.js/index.html)里出现的每个 /api/ 路径，其后端顶层域必须在路由表注册；
//   B) 后端注册的每个 /api/ 顶层域，必须「被前端消费」或「在 README 接口一览登记」——
//      既没接进界面、又不出现在文档里的端点，正是本次评审抓到的问题。
// 两侧清单都从源码/静态资源扫出，不维护手工名单，避免名单本身过期。

import (
	"fmt"
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

// TestNoDuplicateFunctionNamesInFrontend 钉住一类 node --check 根本抽不到的静默 bug：
// JS 允许同一作用域内重复的 function 声明，后者会**无声地覆盖**前者。
// 本项目就因此坑过一次：洞察页与详情页各有一个 loadTrend，后者覆盖前者，
// 导致洞察页拿 route.id=0 去请求 /api/contacts/0/trend（控制台 404，趋势 sparkline 一直取不到数据）。
// 规则：缩进相同（视为同一块作用域）的 function / async function 声明，名字不得重复。
func TestNoDuplicateFunctionNamesInFrontend(t *testing.T) {
	b, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("读 static/app.js 失败: %v", err)
	}
	re := regexp.MustCompile(`^([ \t]+)(?:async[ \t]+)?function[ \t]+([A-Za-z0-9_$]+)[ \t]*\(`)
	seen := map[string]string{}
	var dups []string
	for i, line := range strings.Split(string(b), "\n") {
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := m[1] + "\x00" + m[2]
		if prev, ok := seen[key]; ok {
			dups = append(dups, fmt.Sprintf("%s（第 %s 行与第 %d 行同缩进重名，后者会静默覆盖前者）", m[2], prev, i+1))
			continue
		}
		seen[key] = fmt.Sprint(i + 1)
	}
	if len(dups) > 0 {
		sort.Strings(dups)
		t.Fatalf("前端存在同作用域重名函数（会造成静覆盖）：%v", dups)
	}
	t.Logf("前端 %d 个函数声明无同缩进重名", len(seen))
}

// TestFrontendAPICallsHaveBackend 钉住：前端每个 /api/ 调用必须能找到后端路由。
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

// uiFieldContract 是「文档声称由网页使用的响应字段」契约表。
//
// 为何单独维护一份小表而不是自动扫描：文档里字段名淹在自然语句里，无法可靠地判定
// 哪个是「前端要用」的。而这类声明一旦脱离实现就会变成空话——投产前审计 F1 抓到的
// 正是：README 写「cooldown_seconds 供前端置灰按钮」，而 static/ 里对它 0 引用。
// 新增此类声明时在下表加一行即可，why 列就是它的存在理由。
var uiFieldContract = []struct{ field, why string }{
	{"cooldown_seconds", "重绑冷却期置灰按钮并显示剩余秒数"},
	{"session_expired", "提示会话已过期、需重新扫码"},
	{"logged_in", "显示当前登录状态"},
	{"qr_image", "直接作为 <img> src 展示重绑二维码"},
	{"accuracy", "模型能力成绩单的准确率"},
	{"verdict", "模型能力结论行"},
}

// TestDocumentedUIFieldsAreActuallyUsedByFrontend 双向钉：契约里的字段必须
// 同时出现在前端（真被使用）与 README（真被写清），两者缺一个就红。
func TestDocumentedUIFieldsAreActuallyUsedByFrontend(t *testing.T) {
	front := readFrontend(t)
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Skipf("读不到 README.md: %v", err)
	}
	doc := string(readme)

	var unusedInUI, missingInDoc []string
	for _, c := range uiFieldContract {
		if !strings.Contains(front, c.field) {
			unusedInUI = append(unusedInUI, c.field+"("+c.why+")")
		}
		if !strings.Contains(doc, c.field) {
			missingInDoc = append(missingInDoc, c.field)
		}
	}
	if len(unusedInUI) > 0 {
		t.Errorf("文档声称前端会用的字段在 static/ 里找不到引用（假 UI 承诺）：%v", unusedInUI)
	}
	if len(missingInDoc) > 0 {
		t.Errorf("契约字段未在 README 说明（用户无从得知该行为）：%v", missingInDoc)
	}
	t.Logf("契约：%d 个前端使用的响应字段均已在代码与 README 双侧对齐", len(uiFieldContract))
}
