package main

// Phase 8/10 API 全链路回归：真实数据→DB→服务→HTTP 路由→JSON 响应。
// 用真实 SQLite + 真实 http 路由（s.route），证明端点可用、鉴权生效、自愈与幂等在 HTTP 层成立。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func callAPI(s *apiServer, method, path, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer test-rel-token")
	w := httptest.NewRecorder()
	s.route(w, r)
	return w
}

func TestRelationshipAPIEndToEnd(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}

	// 未登录（无 Bearer）应被拒
	noauth := httptest.NewRequest(http.MethodGet, "/api/relationships/suggestions", nil)
	wno := httptest.NewRecorder()
	s.route(wno, noauth)
	if wno.Code != http.StatusUnauthorized {
		t.Fatalf("未授权应 401, got %d", wno.Code)
	}

	id := regressionContact(t, db, "驾驶舱")
	pj := `{"basic_info":{"occupation":"设计师","location":"成都"},"interests":["看展"],"summary":"设计师"}`
	if err := SaveProfile(db, id, pj, "设计师", "init"); err != nil {
		t.Fatal(err)
	}
	regressionMessages(t, db, id, "我是设计师", "在成都", "周末去看展")

	// GET facts：应返回派生事实
	w := callAPI(s, http.MethodGet, fmt.Sprintf("/api/contacts/%d/facts", id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET facts: %d %s", w.Code, w.Body.String())
	}
	var factsResp struct {
		Facts []FactView `json:"facts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &factsResp); err != nil {
		t.Fatal(err)
	}
	if len(factsResp.Facts) == 0 {
		t.Fatal("facts 端点应返回事实")
	}

	// POST facts/rebuild
	w = callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/facts/rebuild", id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("rebuild facts: %d %s", w.Code, w.Body.String())
	}

	// GET trend
	w = callAPI(s, http.MethodGet, fmt.Sprintf("/api/contacts/%d/trend", id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET trend: %d %s", w.Code, w.Body.String())
	}
	var trend RelationshipTrend
	if err := json.Unmarshal(w.Body.Bytes(), &trend); err != nil {
		t.Fatal(err)
	}
	if trend.ContactID != id {
		t.Fatalf("trend 联系人 ID 不符: %+v", trend)
	}

	// 生成建议（该联系人近期有互动，未必触发 cooling/silence，只验证端点通）
	w = callAPI(s, http.MethodPost, "/api/relationships/suggestions/generate", fmt.Sprintf(`{"contactId":%d}`, id))
	if w.Code != http.StatusOK {
		t.Fatalf("generate suggestions: %d %s", w.Code, w.Body.String())
	}

	// 列表端点
	w = callAPI(s, http.MethodGet, "/api/relationships/suggestions", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list suggestions: %d %s", w.Code, w.Body.String())
	}
	var listResp struct {
		Suggestions []SuggestionView `json:"suggestions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}

	// 不存在的联系人应 404
	w = callAPI(s, http.MethodGet, "/api/contacts/999999/facts", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("不存在联系人应 404, got %d", w.Code)
	}
}

func TestSuggestionStatusAPI(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}

	// 造一个 cooling 联系人：前 30~60 天密集、近 30 天骤减
	id := regressionContact(t, db, "降温X")
	now := time.Now()
	for i := 0; i < 8; i++ {
		if _, err := SaveMessages(db, id, []Message{{Sender: "other", Content: "老(" + string(rune('a'+i)) + ")", Timestamp: now.AddDate(0, 0, -45)}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := SaveMessages(db, id, []Message{{Sender: "me", Content: "近", Timestamp: now.AddDate(0, 0, -5)}}); err != nil {
		t.Fatal(err)
	}
	w := callAPI(s, http.MethodPost, fmt.Sprintf("/api/relationships/suggestions/generate?contactId=%d", id), fmt.Sprintf(`{"contactId":%d}`, id))
	if w.Code != http.StatusOK {
		t.Fatalf("generate: %d %s", w.Code, w.Body.String())
	}
	var genResp struct {
		Suggestions []SuggestionView `json:"suggestions"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &genResp)
	var targetID int64
	for _, sg := range genResp.Suggestions {
		if sg.ContactID == id && sg.Kind == "cooling" {
			targetID = sg.ID
		}
	}
	if targetID == 0 {
		t.Fatalf("应生成 cooling 建议, resp=%s", w.Body.String())
	}
	// 置为 done
	w = callAPI(s, http.MethodPost, fmt.Sprintf("/api/relationships/suggestions/%d/status", targetID), `{"status":"done"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("set status: %d %s", w.Code, w.Body.String())
	}
	// 非法状态应 400
	w = callAPI(s, http.MethodPost, fmt.Sprintf("/api/relationships/suggestions/%d/status", targetID), `{"status":"bogus"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法状态应 400, got %d", w.Code)
	}
}

func TestSimulateAPIRequiresLLMAndSucceeds(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}

	// 未配置 llm：s.llm=nil → 503
	sNoLLM := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	id := regressionContact(t, db, "推演人")
	if err := SaveProfile(db, id, `{"summary":"务实"}`, "务实", "i"); err != nil {
		t.Fatal(err)
	}
	w := callAPI(sNoLLM, http.MethodPost, fmt.Sprintf("/api/contacts/%d/rehearsal/simulate", id), `{"draft":"周末一起吃饭？"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("未配置 LLM 应 503, got %d %s", w.Code, w.Body.String())
	}

	// 配置桩 LLM：端到端返回推演结果
	stub := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		payload := `{"replies":[{"text":"好呀","mood":"积极","rationale":"他喜欢美食"},{"text":"这周忙","mood":"中性","rationale":"最近加班"}]}`
		_ = json.NewEncoder(rw).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{{"message": map[string]interface{}{"content": payload}}},
		})
	}))
	defer stub.Close()
	cfg2 := &Config{APIToken: "test-rel-token"}
	cfg2.LLM.BaseURL = stub.URL
	cfg2.LLM.ApiKey = "k"
	cfg2.LLM.Model = "m"
	sOK := &apiServer{db: db, cfg: cfg2, llm: NewLLMClient(cfg2), sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	w = callAPI(sOK, http.MethodPost, fmt.Sprintf("/api/contacts/%d/rehearsal/simulate", id), `{"draft":"周末一起吃饭？"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("simulate 应 200, got %d %s", w.Code, w.Body.String())
	}
	var res SimulationResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Replies) != 2 || res.Note == "" {
		t.Fatalf("推演结果不完整: %+v", res)
	}
	// 空草稿应 500（服务层报错）
	w = callAPI(sOK, http.MethodPost, fmt.Sprintf("/api/contacts/%d/rehearsal/simulate", id), `{"draft":"  "}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("空草稿应 500, got %d", w.Code)
	}
}
