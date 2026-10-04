package main

// 全端点活体扫描：把真实 mux（withSecurity + /api/）跑起来，喂完整种子数据后，
// 逐个端点发真实 HTTP 请求，验证「功能是否真的可达、能否正确处理」。
//
// 与既有测试的分工：既有单测/冒烟只覆盖了约一半 handler（覆盖率 47.6%，56 个 handler 0%）。
// 本扫描补上「所有功能都真被点了一遍」这一层：
//   - want200=true ：确定性端点（CRUD/读/搜索/标签/洞察聚合/归档状态/备份日志/助手看板/模式/状态），
//     必须返回 2xx，且能解析为 JSON（下载类除外）。返回 4xx/5xx 视为功能异常。
//   - want200=false：依赖 LLM 的端点（改写/推演/情绪/祝福语/立即巡检/待跟进扫描/画像重生成/
//     摄入…），无真实模型无法验证「输出质量」，但至少证明：路由命中(非 404)、handler 运行、
//     不 panic(不出现连接中断)。返回 4xx/5xx 的「优雅失败」可接受，404/崩溃不可接受。
//
// 一个错误就 t.Errorf（不 Fatal），整轮跑完一次性报出所有端点结果，形成完整功能体检表。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestZZFeatureSweepLive(t *testing.T) {
	db := regressionDB(t)
	ensureAllValueAddedTables(t, db) // 标签/时间线/待跟进表
	for _, fn := range []func(*sql.DB) error{ensureAssistantTables, ensureArchiveTables} {
		if err := fn(db); err != nil {
			t.Fatalf("建表失败: %v", err)
		}
	}
	// LLM 指向一个立即失败的本地地址：依赖模型的端点会走「优雅失败」分支，不会挂起或 panic。
	deadLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no model in sweep", http.StatusBadGateway)
	}))
	defer deadLLM.Close()

	cfg := &Config{APIToken: "sweep-token"}
	cfg.LLM.BaseURL = deadLLM.URL
	cfg.LLM.ApiKey = "sweep"
	cfg.LLM.Model = "sweep"
	s := &apiServer{
		db:       db,
		cfg:      cfg,
		llm:      NewLLMClient(cfg),
		sessions: &webSessionStore{sessions: map[string]time.Time{}},
		guard:    newSecurityGuard(),
		ingestRL: newRateLimiter(1000, time.Minute),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", s.route)
	ts := httptest.NewServer(withSecurity(mux, s.guard, cfg.TrustedProxies))
	defer ts.Close()

	// ---- 种一份完整数据 ----
	id := regressionContact(t, db, "体检人")
	pj := `{"basic_info":{"occupation":"医生","location":"北京","important_dates":["1990-05-01"]},"interests":["攀岩"],"important_facts":["有个女儿"],"summary":"医生"}`
	if err := SaveProfile(db, id, pj, "医生", "seed"); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	regressionMessages(t, db, id, "我是医生", "在北京", "周末去攀岩")
	now := time.Now()
	for i := 0; i < 6; i++ {
		if _, err := SaveMessages(db, id, []Message{{Sender: "other", Content: fmt.Sprintf("攀岩训练第%d次记录", i), Timestamp: now.AddDate(0, 0, -20)}}); err != nil {
			t.Fatalf("SaveMessages: %v", err)
		}
	}
	if _, _, err := RebuildFactsAndEvidence(db, id); err != nil {
		t.Fatalf("RebuildFacts: %v", err)
	}
	if _, err := RebuildDailyMetrics(db, id); err != nil {
		t.Fatalf("RebuildDailyMetrics: %v", err)
	}
	if err := saveAssistantSettings(db, defaultAssistantSettings()); err != nil {
		t.Fatalf("seed assistant settings: %v", err)
	}

	cid := fmt.Sprintf("%d", id)
	do := func(method, path, body string) (int, string, error) {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, ts.URL+path, rd)
		req.Header.Set("Authorization", "Bearer sweep-token")
		resp, err := ts.Client().Do(req)
		if err != nil {
			return 0, "", err // 连接中断 ≈ handler panic
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), nil
	}

	type c struct {
		name   string
		method string
		path   string
		body   string
		want   bool // true=必须 2xx 且(默认)可解析 JSON
		jsonOK bool // want=true 时是否额外校验响应为合法 JSON
	}
	cases := []c{
		// —— 确定性只读 / 写：必须 2xx ——
		{"联系人列表", "GET", "/api/contacts", "", true, true},
		{"联系人详情", "GET", "/api/contacts/" + cid, "", true, true},
		{"联系人消息", "GET", "/api/contacts/" + cid + "/messages?limit=20&offset=0", "", true, true},
		{"联系人统计", "GET", "/api/contacts/" + cid + "/stats", "", true, true},
		{"联系人历史", "GET", "/api/contacts/" + cid + "/history", "", true, true},
		{"事实证据", "GET", "/api/contacts/" + cid + "/facts", "", true, true},
		{"关系趋势", "GET", "/api/contacts/" + cid + "/trend", "", true, true},
		{"时间线", "GET", "/api/contacts/" + cid + "/timeline", "", true, true},
		{"联系人标签GET", "GET", "/api/contacts/" + cid + "/tags", "", true, true},
		{"设置备注", "PUT", "/api/contacts/" + cid + "/remark", `{"remark":"体检备注"}`, true, true},
		{"设置名字", "PUT", "/api/contacts/" + cid + "/name", `{"name":"体检人改名"}`, true, true},

		{"标签列表", "GET", "/api/tags", "", true, true},
		{"标签创建", "POST", "/api/tags", `{"name":"体检标签"}`, true, true},

		{"搜索FTS状态", "GET", "/api/search/fts-status", "", true, true},
		{"搜索FTS重建", "POST", "/api/search/fts-rebuild", "", true, true},
		{"搜索消息", "GET", "/api/search/messages?q=攀岩&limit=10&offset=0", "", true, true},

		{"洞察-重复", "GET", "/api/insights/duplicates", "", true, true},
		{"洞察-社交", "GET", "/api/insights/social", "", true, true},
		{"洞察-报告", "GET", "/api/insights/report", "", true, false},

		{"助手看板", "GET", "/api/assistant/dashboard", "", true, true},
		{"助手设置读", "GET", "/api/assistant/settings", "", true, true},
		{"助手邮件日志", "GET", "/api/assistant/email-log", "", true, true},
		{"助手待跟进列表", "GET", "/api/assistant/followups", "", true, true},
		{"助手待跟进新增", "POST", "/api/assistant/followups", fmt.Sprintf(`{"contactId":%d,"kind":"other","content":"体检跟进"}`, id), true, true},
		{"日历订阅密钥", "GET", "/api/assistant/calendar/key", "", true, true},
		{"周计划看板", "GET", "/api/assistant/weekly-plan", "", true, true},
		{"关系图谱全量", "GET", "/api/relationships/connections", "", true, true},
		{"关系图谱单联系人", "GET", "/api/contacts/" + cid + "/connections", "", true, true},
		{"关系图谱重建", "POST", "/api/relationships/connections/rebuild", "", true, true},

		{"人生总览", "GET", "/api/life/state", "", true, true},
		{"人生推演", "GET", "/api/life/projection", "", true, true},
		{"人生年表", "GET", "/api/life/timeline", "", true, true},

		{"高阶洞察-社交网络", "GET", "/api/insight/network", "", true, true},
		{"高阶洞察-自我画像", "GET", "/api/insight/self", "", true, true},
		{"高阶洞察-干预学习", "GET", "/api/insight/intervention", "", true, true},
		{"高阶洞察-本周简报", "GET", "/api/insight/briefing", "", true, true},
		{"高阶洞察-手动重算", "POST", "/api/insight/recompute", "", true, true},

		{"归档状态", "GET", "/api/archive/status", "", true, true},
		{"系统状态", "GET", "/api/status", "", true, true},
		{"备份日志", "GET", "/api/backup/logs", "", true, true},

		// —— 依赖 LLM / 副作用：仅要求路由命中(非 404) 且不崩溃 ——
		{"改写(需LLM)", "POST", "/api/contacts/" + cid + "/rewrite", `{}`, false, false},
		{"草稿检查(需LLM)", "POST", "/api/contacts/" + cid + "/review-draft", `{}`, false, false},
		{"画像变化(需LLM)", "POST", "/api/contacts/" + cid + "/profile-changes", `{}`, false, false},
		{"意图分析(需LLM)", "POST", "/api/contacts/" + cid + "/analyze", `{}`, false, false},
		{"画像重生成(需LLM)", "POST", "/api/contacts/" + cid + "/regenerate", `{}`, false, false},
		{"补充画像(需LLM)", "POST", "/api/contacts/" + cid + "/supplement", `{}`, false, false},
		{"问答ask(需LLM)", "POST", "/api/contacts/" + cid + "/ask", `{}`, false, false},
		{"推演上下文", "POST", "/api/contacts/" + cid + "/rehearsal/context", `{}`, false, false},
		{"推演轮次(需LLM)", "POST", "/api/contacts/" + cid + "/rehearsal/turn", `{}`, false, false},
		{"推演复盘(需LLM)", "POST", "/api/contacts/" + cid + "/rehearsal/review", `{}`, false, false},
		{"情绪分析(需LLM)", "POST", "/api/assistant/analyze-emotion", `{}`, false, false},
		{"立即巡检(需LLM)", "POST", "/api/assistant/run-now", `{"kind":"daily"}`, false, false},
		{"祝福语(需LLM)", "POST", "/api/assistant/blessing", `{}`, false, false},
		{"待跟进扫描(需LLM)", "POST", "/api/assistant/followups/scan", `{}`, false, false},
		{"周期报告(可能需LLM)", "GET", "/api/insights/period-report?period=week", "", false, false},
		{"摄入粘贴(需LLM)", "POST", "/api/ingest", `{"text":"没有联系人头的乱码内容"}`, false, false},
		{"周计划重生(需LLM)", "POST", "/api/assistant/weekly-plan", "", false, false},
	}

	pass := 0
	for _, tc := range cases {
		code, body, err := do(tc.method, tc.path, tc.body)
		if err != nil {
			t.Errorf("[崩溃/连接中断] %s %s %s: %v", tc.method, tc.path, "("+tc.name+")", err)
			continue
		}
		switch {
		case tc.want:
			if code < 200 || code >= 300 {
				t.Errorf("[功能异常] %s %s %s → %d: %s", tc.method, tc.path, "("+tc.name+")", code, truncate(body))
				continue
			}
			if tc.jsonOK && !validJSON(body) {
				t.Errorf("[非JSON] %s %s %s → %d: %s", tc.method, tc.path, "("+tc.name+")", code, truncate(body))
				continue
			}
			pass++
		default:
			if code == http.StatusNotFound {
				t.Errorf("[路由缺失] %s %s %s → 404（端点未接线/路径错误）", tc.method, tc.path, "("+tc.name+")")
				continue
			}
			// 依赖 LLM：非 404 即视为「可达且 handler 已运行」，具体输出质量需真实模型
			pass++
		}
	}
	t.Logf("活体扫描完成：%d/%d 端点通过（确定性端点已验证 2xx+JSON；LLM 端点仅验证可达/不崩）", pass, len(cases))
}

func validJSON(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	var v interface{}
	return json.Unmarshal([]byte(s), &v) == nil
}

func truncate(s string) string {
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}
