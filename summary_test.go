package main

// v4.9.0「智能回顾摘要」端到端回归：真实 SQLite + 桩 LLM，走完整 HTTP 路由。
//
// 证明：
//   - 未配置模型 → POST /api/contacts/{id}/summary 返回 503；
//   - 配置桩模型 → 200 且 overview/topics/todos 正确解析、sources 覆盖真实消息 id、note 非空；
//   - 窗口内消息不足（<4 条）→ 空摘要 + note，不硬编、不报错；
//   - 桩返回坏 JSON → 回退到「原文摘录」overview，topics/todos 为空数组，不 panic。

import (
	"context"
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

// summaryStub 桩 LLM：按 prompt 里出现的字面 key 分派响应。
//   - badJSON=true → 返回一段非 JSON 文本，触发回退分支
//   - 否则返回一份合规的 {overview, topics, todos} 结构
func summaryStub(t *testing.T, badJSON bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		prompt := string(body)
		var content string
		switch {
		case badJSON:
			content = "这是一段模型返回的自由文本，不是 JSON：他们聊了旅行和工作。"
		case strings.Contains(prompt, "overview") && strings.Contains(prompt, "todos"):
			content = `{"overview":"这段时间你们主要聊了下个月的旅行安排和最近的项目压力，出处见 [1][2]。","topics":["旅行","工作","健身"],"todos":[{"text":"把三亚酒店选择发给对方","owner":"我","ref":"[1]"},{"text":"对方答应周末回电话","owner":"对方","ref":"[2]"},{"text":"非法 ref 应被清空","owner":"未知角色","ref":"[999]"}]}`
		default:
			content = `{"overview":"(未匹配) 桩兜底","topics":[],"todos":[]}`
		}
		_ = json.NewEncoder(rw).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{{"message": map[string]interface{}{"content": content}}},
		})
	}))
}

func summaryLLMConfig(url string) *Config {
	cfg := &Config{APIToken: "test-rel-token"}
	cfg.LLM.BaseURL = url
	cfg.LLM.ApiKey = "k"
	cfg.LLM.Model = "m"
	return cfg
}

// seedRecentConversation 种 6 条近 N 天内的消息，返回其数据库 id 列表（按插入顺序）。
func seedRecentConversation(t *testing.T, db *sql.DB, cid int64) []int64 {
	t.Helper()
	base := time.Now().Add(-3 * 24 * time.Hour)
	contents := []string{
		"下个月想去三亚玩一周，你有推荐酒店吗？",
		"我这边可以帮你查一下，稍等我今晚整理几个",
		"最近项目上线压力大，周末约个球放松一下",
		"好，我这边周日有空，你定时间",
		"上次你借我的 200 块，下周方便还一下不",
		"嗯，我记着呢，下周转账给你",
	}
	ids := make([]int64, 0, len(contents))
	for i, text := range contents {
		sender := "other"
		if i%2 == 1 {
			sender = "me"
		}
		vaMsg(t, db, cid, sender, text, base.Add(time.Duration(i)*time.Hour))
		var id int64
		if err := db.QueryRow(`SELECT id FROM messages WHERE contact_id=? AND content=?`, cid, text).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestSummaryAPIRequiresLLMReturns503(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	// 未配置 llm：s.llm = nil → SummarizeContact 返回 ErrLLMNotConfigured → 503
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	id := regressionContact(t, db, "摘要人")
	seedRecentConversation(t, db, id)

	w := callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/summary", id), `{"days":30}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("未配置 LLM 应 503, got %d %s", w.Code, w.Body.String())
	}
}

func TestSummaryAPIEndToEnd(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "摘要人")
	ids := seedRecentConversation(t, db, id)
	// 给画像摘要一个非空值，验证 prompt 组装不炸
	if err := SaveProfile(db, id, `{"summary":"常一起出游"}`, "常一起出游", "init"); err != nil {
		t.Fatal(err)
	}

	stub := summaryStub(t, false)
	defer stub.Close()
	cfg := summaryLLMConfig(stub.URL)
	s := &apiServer{db: db, cfg: cfg, llm: NewLLMClient(cfg),
		sessions: &webSessionStore{sessions: map[string]time.Time{}}}

	w := callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/summary", id), `{"days":30}`)
	if w.Code != http.StatusOK {
		t.Fatalf("summary 应 200, got %d %s", w.Code, w.Body.String())
	}
	var res ContactSummary
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.ContactID != id || res.Name == "" {
		t.Fatalf("contactId/name 不对: %+v", res)
	}
	if res.From == "" || res.To == "" {
		t.Fatalf("from/to 窗口日期不应为空: %+v", res)
	}
	if !strings.Contains(res.Overview, "旅行") || !strings.Contains(res.Overview, "[1]") {
		t.Fatalf("overview 应含关键词与 [n] 标注: %q", res.Overview)
	}
	if len(res.Topics) != 3 {
		t.Fatalf("topics 期望 3 项, got %+v", res.Topics)
	}
	// 桩给了 3 条 todos，其中第三条 owner="未知角色" → 归一为 "我"; ref "[999]" 越界 → 清空
	if len(res.Todos) != 3 {
		t.Fatalf("todos 期望 3 项, got %+v", res.Todos)
	}
	if res.Todos[0].Owner != "我" || res.Todos[0].Ref != "[1]" {
		t.Fatalf("第一条 todo owner/ref 不对: %+v", res.Todos[0])
	}
	if res.Todos[1].Owner != "对方" || res.Todos[1].Ref != "[2]" {
		t.Fatalf("第二条 todo owner/ref 不对: %+v", res.Todos[1])
	}
	if res.Todos[2].Owner != "我" {
		t.Fatalf("未知 owner 应归一为「我」: %+v", res.Todos[2])
	}
	if res.Todos[2].Ref != "" {
		t.Fatalf("越界 ref 应清空, got %q", res.Todos[2].Ref)
	}
	if len(res.Sources) == 0 {
		t.Fatal("sources 不应为空")
	}
	// 出处应覆盖真实消息 id（窗口内 6 条全部纳入）
	hit := map[int64]bool{}
	for _, src := range res.Sources {
		hit[src.MessageID] = true
		if src.N <= 0 {
			t.Fatalf("出处编号应 >=1: %+v", src)
		}
	}
	for _, want := range ids {
		if !hit[want] {
			t.Fatalf("sources 未覆盖消息 id=%d", want)
		}
	}
	if res.Note == "" {
		t.Fatal("应带局限性说明 note")
	}
}

func TestSummaryInsufficientMessagesReturnsEmpty(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "稀疏人")
	// 只种 2 条消息（阈值 4），应走「原文不足」分支
	base := time.Now().Add(-2 * 24 * time.Hour)
	vaMsg(t, db, id, "other", "只有一条", base)
	vaMsg(t, db, id, "me", "只有一条回复", base.Add(time.Hour))

	stub := summaryStub(t, false)
	defer stub.Close()
	cfg := summaryLLMConfig(stub.URL)
	s := &apiServer{db: db, cfg: cfg, llm: NewLLMClient(cfg),
		sessions: &webSessionStore{sessions: map[string]time.Time{}}}

	w := callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/summary", id), `{"days":30}`)
	if w.Code != http.StatusOK {
		t.Fatalf("消息不足也应 200（如实返回空摘要）, got %d %s", w.Code, w.Body.String())
	}
	var res ContactSummary
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Overview != "" {
		t.Fatalf("消息不足时 overview 应为空, got %q", res.Overview)
	}
	if len(res.Topics) != 0 || len(res.Todos) != 0 || len(res.Sources) != 0 {
		t.Fatalf("空摘要应三数组皆空: %+v", res)
	}
	if !strings.Contains(res.Note, "不足") {
		t.Fatalf("note 应说明不足, got %q", res.Note)
	}
}

func TestSummaryBadJSONFallsBackGracefully(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "摘要人")
	seedRecentConversation(t, db, id)

	stub := summaryStub(t, true) // 返回非 JSON 文本
	defer stub.Close()
	cfg := summaryLLMConfig(stub.URL)
	s := &apiServer{db: db, cfg: cfg, llm: NewLLMClient(cfg),
		sessions: &webSessionStore{sessions: map[string]time.Time{}}}

	w := callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/summary", id), `{"days":30}`)
	if w.Code != http.StatusOK {
		t.Fatalf("坏 JSON 应回退 200, got %d %s", w.Code, w.Body.String())
	}
	var res ContactSummary
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	// 回退：overview 以「（模型未返回有效结构化摘要」开头，不新增信息
	if !strings.HasPrefix(res.Overview, "（模型未返回有效结构化摘要") {
		t.Fatalf("坏 JSON 回退 overview 应以原文摘录提示开头, got %q", res.Overview)
	}
	if len(res.Topics) != 0 {
		t.Fatalf("回退 topics 应为空, got %+v", res.Topics)
	}
	if len(res.Todos) != 0 {
		t.Fatalf("回退 todos 应为空, got %+v", res.Todos)
	}
	if len(res.Sources) == 0 {
		t.Fatal("回退时 sources 仍应包含窗口内原文")
	}
}

// 服务层：直接调 SummarizeContact，覆盖 days 边界（clamp）与联系人不存在路径。
func TestSummaryServiceLayerClampAndMissing(t *testing.T) {
	db := regressionDB(t)
	stub := summaryStub(t, false)
	defer stub.Close()
	llm := NewLLMClient(summaryLLMConfig(stub.URL))

	// 联系人不存在 → 报错
	if _, err := SummarizeContact(newCtx(), db, llm, 999999, 30); err == nil {
		t.Fatal("联系人不存在应报错")
	}

	// days=0 → 落默认 30；days 巨大 → 夹到 365
	id := regressionContact(t, db, "摘要人")
	seedRecentConversation(t, db, id)
	res, err := SummarizeContact(newCtx(), db, llm, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 由 From/To 反推窗口，确保至少覆盖到已插入的 3 天前消息
	from := parseSummaryDate(res.From)
	if time.Since(from) < 24*time.Hour {
		t.Fatalf("days=0 未回落默认窗口: from=%s", res.From)
	}
	res2, err := SummarizeContact(newCtx(), db, llm, id, summaryMaxDays*10)
	if err != nil {
		t.Fatal(err)
	}
	from2 := parseSummaryDate(res2.From)
	if days := int(time.Since(from2) / (24 * time.Hour)); days > summaryMaxDays+2 || days < summaryMaxDays-2 {
		t.Fatalf("days=%d 未夹到 summaryMaxDays=%d", days, summaryMaxDays)
	}
}

func parseSummaryDate(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return time.Now().AddDate(0, 0, -summaryDefaultDays)
	}
	return t
}

func newCtx() context.Context { return context.Background() }
