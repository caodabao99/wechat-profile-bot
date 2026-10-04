package main

// 关系驾驶端至端冒烟：走完整 mux（withSecurity + /api/ + /assets/ + /）的真实 TCP 监听。
// 这是唯一覆盖静态资源服务、index.html 内含驾驶舱、Bearer 鉴权闸、以及
// facts/trend/suggestions 端点在真实网络栈上返回正确 JSON 的集成测试。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestZZLiveServerSmoke(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "smoke-token"}
	s := &apiServer{
		db:       db,
		cfg:      cfg,
		sessions: &webSessionStore{sessions: map[string]time.Time{}},
		guard:    newSecurityGuard(),
		ingestRL: newRateLimiter(100, time.Minute),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", s.route)
	mux.HandleFunc("/assets/", handleAssets)
	mux.HandleFunc("/", handleWebUI)
	ts := httptest.NewServer(withSecurity(mux, s.guard, cfg.TrustedProxies))
	defer ts.Close()

	do := func(method, path, body string, auth bool) (int, string) {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, ts.URL+path, rd)
		if auth {
			req.Header.Set("Authorization", "Bearer smoke-token")
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s 请求失败: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// 1. 静态资源
	if code, _ := do("GET", "/assets/app.js", "", false); code != 200 {
		t.Fatalf("app.js 应 200, got %d", code)
	}

	// 2. index.html 含驾驶舱页签
	if code, body := do("GET", "/", "", false); code != 200 || !strings.Contains(body, "驾驶舱") {
		t.Fatalf("index.html 应 200 且含「驾驶舱」, code=%d contains=%v", code, strings.Contains(body, "驾驶舱"))
	}

	// 3. 未带 token 的 API 应 401
	if code, _ := do("GET", "/api/relationships/suggestions", "", false); code != 401 {
		t.Fatalf("未授权应 401, got %d", code)
	}

	// 4. 造真实数据：联系人 + 画像 + 消息（降温场景）
	id := regressionContact(t, db, "冒烟人")
	pj := `{"basic_info":{"occupation":"医生","location":"北京"},"interests":["攀岩"],"important_facts":["有个女儿"],"summary":"医生"}`
	if err := SaveProfile(db, id, pj, "医生", "i"); err != nil {
		t.Fatal(err)
	}
	regressionMessages(t, db, id, "我是医生", "在北京", "周末去攀岩")
	now := time.Now()
	for i := 0; i < 8; i++ {
		if _, err := SaveMessages(db, id, []Message{{Sender: "other", Content: fmt.Sprintf("老消息%d(%c)", i, 'a'+i), Timestamp: now.AddDate(0, 0, -45)}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := SaveMessages(db, id, []Message{{Sender: "me", Content: "最近少了", Timestamp: now.AddDate(0, 0, -5)}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RebuildFactsAndEvidence(db, id); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildDailyMetrics(db, id); err != nil {
		t.Fatal(err)
	}

	// 5. GET facts（带 token）→ 有事实、有证据
	code, body := do("GET", fmt.Sprintf("/api/contacts/%d/facts", id), "", true)
	if code != 200 {
		t.Fatalf("facts: %d %s", code, body)
	}
	var fr struct {
		Facts []FactView `json:"facts"`
	}
	if err := json.Unmarshal([]byte(body), &fr); err != nil || len(fr.Facts) == 0 {
		t.Fatalf("facts 解析失败或为空: n=%d err=%v body=%s", len(fr.Facts), err, body)
	}
	var evTotal int
	for _, f := range fr.Facts {
		evTotal += len(f.Evidence)
	}
	t.Logf("facts=%d 证据合计=%d", len(fr.Facts), evTotal)

	// 6. GET trend → cooling
	code, body = do("GET", fmt.Sprintf("/api/contacts/%d/trend", id), "", true)
	var tr RelationshipTrend
	if code != 200 || json.Unmarshal([]byte(body), &tr) != nil {
		t.Fatalf("trend: %d %s", code, body)
	}
	t.Logf("trend state=%s recent=%d prior=%d", tr.State, tr.Recent30, tr.Prior30)

	// 7. POST generate → 返回建议
	code, body = do("POST", "/api/relationships/suggestions/generate", fmt.Sprintf(`{"contactId":%d}`, id), true)
	if code != 200 {
		t.Fatalf("generate: %d %s", code, body)
	}
	var gr struct {
		Generated   int              `json:"generated"`
		Suggestions []SuggestionView `json:"suggestions"`
	}
	if err := json.Unmarshal([]byte(body), &gr); err != nil {
		t.Fatalf("generate 解析失败: %v %s", err, body)
	}
	var kinds []string
	for _, sg := range gr.Suggestions {
		kinds = append(kinds, sg.Kind+":"+sg.Status)
	}
	t.Logf("generate http code=%d 建议=%v", code, kinds)
}
