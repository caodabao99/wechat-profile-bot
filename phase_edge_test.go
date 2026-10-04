package main

// Phase 4-9 边界/对抗用例：LIKE 通配符安全、API 空事实自愈重建、LLM 起草、公开服务入口。
// 全部真实 SQLite / 真实 HTTP，覆盖常规 happy-path 未触及的分支。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 事实值含 LIKE 通配符时，必须按字面匹配，不能被 % / _ 放大命中范围。
func TestPhase5EvidenceWildcardSafety(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "通配符")
	// 兴趣值 "50%_off" —— % 和 _ 都是 LIKE 通配符，必须被转义成字面
	if err := SaveProfile(db, id, `{"interests":["50%_off"],"summary":"x"}`, "x", "i"); err != nil {
		t.Fatal(err)
	}
	// 清掉 SaveProfile 钩子产生的证据，手动加消息后重建
	dbMu.Lock()
	db.Exec(`DELETE FROM profile_fact_evidence WHERE contact_id=?`, id)
	dbMu.Unlock()
	// 字面命中：真含 "50%_off"
	regressionMessages(t, db, id, "全场 50%_off 划算")
	// 诱饵：若不转义，%_ 会误命中 "50Xoff" / "500off" 等
	regressionMessages(t, db, id, "价格 500off 与 50Xoff 都不该命中")

	n, err := AttachFactEvidence(db, id)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("通配符应严格字面匹配只命中 1 条, got %d", n)
	}
}

// facts 表被清空但画像仍在时，GET /facts 应自愈重建（覆盖 contactHasProfileJSON + 自愈分支）。
func TestFactsAPISelfHealWhenEmpty(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	id := regressionContact(t, db, "自愈")
	if err := SaveProfile(db, id, `{"basic_info":{"occupation":"律师"},"summary":"律师"}`, "律师", "i"); err != nil {
		t.Fatal(err)
	}
	// 模拟恢复后/老库：派生事实表被清空，但 profile_json 仍在
	dbMu.Lock()
	db.Exec(`DELETE FROM profile_fact_evidence WHERE contact_id=?`, id)
	db.Exec(`DELETE FROM profile_facts WHERE contact_id=?`, id)
	dbMu.Unlock()
	// 直接确认确实空了
	var cnt int
	db.QueryRow(`SELECT COUNT(*) FROM profile_facts WHERE contact_id=?`, id).Scan(&cnt)
	if cnt != 0 {
		t.Fatalf("前置条件：事实应已被清空, got %d", cnt)
	}

	w := callAPI(s, http.MethodGet, fmt.Sprintf("/api/contacts/%d/facts", id), "")
	if w.Code != http.StatusOK {
		t.Fatalf("facts: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Facts []FactView `json:"facts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Facts) == 0 {
		t.Fatalf("空事实表应触发自愈重建并返回事实, body=%s", w.Body.String())
	}
	var found bool
	for _, f := range resp.Facts {
		if f.Type == "occupation" && f.Value == "律师" {
			found = true
		}
	}
	if !found {
		t.Fatal("自愈重建后应包含 occupation=律师")
	}
	// 无画像联系人：不应报错，返回空数组即可
	id2 := regressionContact(t, db, "空白人")
	w = callAPI(s, http.MethodGet, fmt.Sprintf("/api/contacts/%d/facts", id2), "")
	if w.Code != http.StatusOK {
		t.Fatalf("无画像 facts 应 200, got %d %s", w.Code, w.Body.String())
	}
	var resp2 struct {
		Facts []FactView `json:"facts"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp2)
	if len(resp2.Facts) != 0 {
		t.Fatalf("无画像应返回空事实, got %d", len(resp2.Facts))
	}
}

// 有 LLM 时，降温建议应被 fillDrafts 补上一段开场白（覆盖 fillDrafts + parseDraft）。
func TestPhase7FillDraftsWithLLM(t *testing.T) {
	db := regressionDB(t)
	stub := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		_ = json.NewEncoder(rw).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"message": map[string]interface{}{"content": `{"draft":"嗨，最近怎么样？好久没聊了"}`}},
			},
		})
	}))
	defer stub.Close()
	cfg := &Config{}
	cfg.LLM.BaseURL = stub.URL
	cfg.LLM.ApiKey = "k"
	cfg.LLM.Model = "m"
	llm := NewLLMClient(cfg)

	id := regressionContact(t, db, "需要开场白")
	now := time.Now()
	for i := 0; i < 8; i++ {
		if _, err := SaveMessages(db, id, []Message{{Sender: "other", Content: fmt.Sprintf("旧(%d)", i), Timestamp: now.AddDate(0, 0, -45)}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := SaveMessages(db, id, []Message{{Sender: "me", Content: "近", Timestamp: now.AddDate(0, 0, -5)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateActionSuggestions(db, llm, id); err != nil {
		t.Fatal(err)
	}
	list, err := ListSuggestions(db, false)
	if err != nil {
		t.Fatal(err)
	}
	var drafted bool
	for _, sg := range list {
		if sg.ContactID == id && sg.Kind == "cooling" {
			if sg.Draft != "" {
				drafted = true
			} else {
				t.Fatalf("降温建议应由 LLM 补上 draft, 实际为空: %+v", sg)
			}
		}
	}
	if !drafted {
		t.Fatal("未找到被填充 draft 的 cooling 建议")
	}
}

// 公开服务入口 ExtractProfileFacts / AttachFactEvidence 独立可用（不靠 Rebuild 组合）。
func TestPhase4PublicFactWrappers(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "公开入口")
	if err := SaveProfile(db, id, `{"interests":["读书"],"summary":"x"}`, "x", "i"); err != nil {
		t.Fatal(err)
	}
	// 清掉钩子产物，单独走两个公开 wrapper
	dbMu.Lock()
	db.Exec(`DELETE FROM profile_fact_evidence WHERE contact_id=?`, id)
	db.Exec(`DELETE FROM profile_facts WHERE contact_id=?`, id)
	dbMu.Unlock()
	active, _, err := ExtractProfileFacts(db, id)
	if err != nil || active == 0 {
		t.Fatalf("ExtractProfileFacts: active=%d err=%v", active, err)
	}
	regressionMessages(t, db, id, "爱读书", "在读一本书")
	ev, err := AttachFactEvidence(db, id)
	if err != nil || ev == 0 {
		t.Fatalf("AttachFactEvidence: ev=%d err=%v", ev, err)
	}
}
