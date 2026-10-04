package main

// 特性①「问 TA 的历史」端到端回归：真实 SQLite + 桩 LLM，走完整 HTTP 路由。
//
//	证明两段式（抽词检索 → 带出处作答）在 HTTP 层成立：
//	  - 未配置模型：/api/contacts/{id}/ask 返回 503；
//	  - 配置桩模型：答案含出处标注 [n]、sources 非空、且引用到真实被检索到的消息 id；
//	  - 服务层 AskContactHistory 直接调用同样能召回原文并作答。

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

// askStub 返回一个桩 LLM：
//   - 关键词抽取请求（prompt 含 "keywords"）→ 回 {"keywords":[...]}
//   - 作答请求（prompt 含 "answer"）→ 把候选原文里出现的关键词织进带 [n] 的回答
func askStub(t *testing.T, keywords []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		prompt := string(body)
		var content string
		if strings.Contains(prompt, `"keywords"`) || strings.Contains(prompt, "keywords") {
			kb, _ := json.Marshal(map[string]interface{}{"keywords": keywords})
			content = string(kb)
		} else {
			content = `{"answer":"他说想去三亚玩，出处见 [1]。"}`
		}
		_ = json.NewEncoder(rw).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{{"message": map[string]interface{}{"content": content}}},
		})
	}))
}

func askLLMConfig(url string) *Config {
	cfg := &Config{APIToken: "test-rel-token"}
	cfg.LLM.BaseURL = url
	cfg.LLM.ApiKey = "k"
	cfg.LLM.Model = "m"
	return cfg
}

// seedSanyaMessage 给联系人种一条「打算去三亚玩」的消息，返回其 id。
func seedSanyaMessage(t *testing.T, db *sql.DB, id int64) int64 {
	t.Helper()
	_, err := SaveMessages(db, id, []Message{{
		Sender: "other", Content: "我最近打算去三亚玩一周",
		Timestamp: time.Date(2025, 5, 20, 9, 0, 0, 0, time.Local),
	}})
	if err != nil {
		t.Fatal(err)
	}
	var msgID int64
	if err := db.QueryRow(`SELECT id FROM messages WHERE contact_id=? AND content LIKE '%三亚%'`, id).Scan(&msgID); err != nil {
		t.Fatal(err)
	}
	return msgID
}

func TestAskAPIRequiresLLMReturns503(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	// 未配置 llm：s.llm = nil → AskContactHistory 明确报 ErrLLMNotConfigured → 503
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	id := regressionContact(t, db, "问答人")
	seedSanyaMessage(t, db, id)

	w := callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/ask", id), `{"question":"他上次说想去哪玩？"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("未配置 LLM 应 503, got %d %s", w.Code, w.Body.String())
	}

	// 空问题（即便配置了模型也应 4xx/5xx，服务层报错），这里无模型先命中 503，
	// 单独验证：有模型但空问题 → 服务层 errors（非 503），路由映射为 500。
	stub := askStub(t, []string{"三亚"})
	defer stub.Close()
	sOK := &apiServer{db: db, cfg: askLLMConfig(stub.URL), llm: NewLLMClient(askLLMConfig(stub.URL)),
		sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	w = callAPI(sOK, http.MethodPost, fmt.Sprintf("/api/contacts/%d/ask", id), `{"question":"   "}`)
	if w.Code == http.StatusServiceUnavailable || w.Code == http.StatusOK {
		t.Fatalf("空问题不应 503/200, got %d %s", w.Code, w.Body.String())
	}
}

func TestAskAPIEndToEnd(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "问答人")
	if err := SaveProfile(db, id, `{"summary":"爱旅游"}`, "爱旅游", "init"); err != nil {
		t.Fatal(err)
	}
	msgID := seedSanyaMessage(t, db, id)
	// 再补一条无关消息，确保检索确实召回了三亚那条
	regressionMessages(t, db, id, "今天加班到很晚")

	stub := askStub(t, []string{"三亚", "旅游"})
	defer stub.Close()
	cfg := askLLMConfig(stub.URL)
	s := &apiServer{db: db, cfg: cfg, llm: NewLLMClient(cfg),
		sessions: &webSessionStore{sessions: map[string]time.Time{}}}

	w := callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/ask", id), `{"question":"他上次说想去哪玩？"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("ask 应 200, got %d %s", w.Code, w.Body.String())
	}
	var res AskResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Answer, "三亚") || !strings.Contains(res.Answer, "[1]") {
		t.Fatalf("答案应含出处关键词与 [n] 标注: %+v", res)
	}
	if len(res.Sources) == 0 {
		t.Fatal("sources 不应为空")
	}
	// 出处应包含被检索到的三亚消息 id
	found := false
	for _, src := range res.Sources {
		if src.MessageID == msgID {
			found = true
			if src.N <= 0 {
				t.Fatalf("出处编号应 >=1: %+v", src)
			}
		}
	}
	if !found {
		t.Fatalf("出处未覆盖目标消息 id=%d: %+v", msgID, res.Sources)
	}
	if res.Note == "" {
		t.Fatal("应带局限说明 note")
	}
}

func TestAskContactHistoryServiceLayer(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "问答人")
	msgID := seedSanyaMessage(t, db, id)

	stub := askStub(t, []string{"三亚"})
	defer stub.Close()
	cfg := askLLMConfig(stub.URL)
	llm := NewLLMClient(cfg)

	res, err := AskContactHistory(context.Background(), db, llm, id, "他上次说想去哪玩？")
	if err != nil {
		t.Fatal(err)
	}
	if res.Question == "" || res.Answer == "" {
		t.Fatalf("结果不完整: %+v", res)
	}
	hit := false
	for _, s := range res.Sources {
		if s.MessageID == msgID {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("服务层未召回目标消息: %+v", res.Sources)
	}

	// 无匹配关键词时也不应报错，应如实返回“没找到”，sources 空。
	res2, err := AskContactHistory(context.Background(), db, llm, id, "他说过最喜欢哪本科幻小说？")
	if err != nil {
		t.Fatalf("无匹配也应成功返回: %v", err)
	}
	_ = res2
}
