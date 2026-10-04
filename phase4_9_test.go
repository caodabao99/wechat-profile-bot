package main

// Phase 4-9 端到端 / 单元回归：可信画像事实、证据链、关系趋势、行动建议、回复前推演。
// 全部走真实 SQLite（regressionDB）+ 真实读写，不使用 mock 掩盖逻辑；LLM 相关用本地
// httptest 桩服务返回真实 JSON，验证「真实数据→服务→解析」全链路（§29 严禁假完成）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPhase4FactsDerivedFromProfile(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "阿明")
	pj := `{"basic_info":{"occupation":"供应链经理","location":"深圳","important_dates":["生日: 5月20日"]},` +
		`"personality":["细心","务实"],"communication_style":{"frequent_phrases":["收到","辛苦啦"]},` +
		`"interests":["跑步","咖啡"],"important_facts":["养了一只猫叫豆豆"],` +
		`"relationship":{"closeness":"比较熟"},"summary":"务实细心的供应链经理"}`
	if err := SaveProfile(db, id, pj, "务实细心的供应链经理", "init"); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	facts, err := GetFacts(db, id, false)
	if err != nil {
		t.Fatalf("GetFacts: %v", err)
	}
	// 期望至少：occupation+location+1个重要日子+2性格+2兴趣+1重要事实+2口头禅+1亲密度 = 11
	wantTypes := map[string]int{}
	for _, f := range facts {
		wantTypes[f.Type]++
		if f.Status != "active" {
			t.Fatalf("派生事实应为 active, got %s", f.Status)
		}
	}
	for _, typ := range []string{"occupation", "location", "important_date", "personality", "interest", "important_fact", "phrase", "closeness"} {
		if wantTypes[typ] == 0 {
			t.Fatalf("事实缺少类型 %q, 实际=%v", typ, wantTypes)
		}
	}
	if wantTypes["personality"] != 2 || wantTypes["interest"] != 2 || wantTypes["phrase"] != 2 {
		t.Fatalf("多值事实计数错误: %v", wantTypes)
	}
	// 重要日子应拆成 key=生日 value=5月20日
	var bdFound bool
	for _, f := range facts {
		if f.Type == "important_date" && f.Key == "生日" && f.Value == "5月20日" {
			bdFound = true
		}
	}
	if !bdFound {
		t.Fatal("重要日子未按 key/value 拆分")
	}
}

func TestPhase4FactsRetiredOnProfileChange(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "小红")
	p1 := `{"basic_info":{"occupation":"教师","location":"广州"},"summary":"a"}`
	if err := SaveProfile(db, id, p1, "a", "v1"); err != nil {
		t.Fatal(err)
	}
	// 新画像去掉了 location
	p2 := `{"basic_info":{"occupation":"教师"},"summary":"b"}`
	if err := SaveProfile(db, id, p2, "b", "v2"); err != nil {
		t.Fatal(err)
	}
	active, err := GetFacts(db, id, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range active {
		if f.Type == "location" {
			t.Fatal("已从画像移除的 location 不应仍是 active")
		}
	}
	all, err := GetFacts(db, id, true)
	if err != nil {
		t.Fatal(err)
	}
	var retiredLoc bool
	for _, f := range all {
		if f.Type == "location" && f.Status == "retired" {
			retiredLoc = true
		}
	}
	if !retiredLoc {
		t.Fatal("旧 location 应作为 retired 保留（可信度溯源历史）")
	}
}

func TestPhase5EvidenceAttachesAndBoostsConfidence(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "老李")
	pj := `{"interests":["跑步"],"summary":"喜欢跑步"}`
	if err := SaveProfile(db, id, pj, "喜欢跑步", "init"); err != nil {
		t.Fatal(err)
	}
	// 三条含「跑步」的真实消息 + 一条无关消息
	regressionMessages(t, db, id, "今天去跑步了", "周末也跑步", "一起跑步吗", "吃饭了吗")
	active, evidence, err := RebuildFactsAndEvidence(db, id)
	if err != nil {
		t.Fatal(err)
	}
	if active == 0 {
		t.Fatal("应有 active 事实")
	}
	if evidence != 3 {
		t.Fatalf("跑步事实应挂 3 条证据, got %d", evidence)
	}
	facts, err := GetFacts(db, id, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range facts {
		if f.Type == "interest" && f.Value == "跑步" {
			if len(f.Evidence) != 3 {
				t.Fatalf("证据条数=%d, 期望 3", len(f.Evidence))
			}
			// confidence = min(0.95, 0.6+0.08*3)=0.84
			if f.Confidence < 0.8 || f.Confidence > 0.85 {
				t.Fatalf("置信度应≈0.84, got %f", f.Confidence)
			}
			for _, ev := range f.Evidence {
				if ev.MessageID == 0 || ev.Snippet == "" {
					t.Fatal("证据缺少 messageId 或 snippet")
				}
			}
			return
		}
	}
	t.Fatal("未找到跑步事实")
}

func TestPhase5EvidenceIdempotent(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "王芳")
	if err := SaveProfile(db, id, `{"interests":["咖啡"],"summary":"x"}`, "x", "i"); err != nil {
		t.Fatal(err)
	}
	regressionMessages(t, db, id, "来杯咖啡", "咖啡真香")
	_, e1, err := RebuildFactsAndEvidence(db, id)
	if err != nil {
		t.Fatal(err)
	}
	_, e2, err := RebuildFactsAndEvidence(db, id)
	if err != nil {
		t.Fatal(err)
	}
	if e1 != 2 || e2 != 2 {
		t.Fatalf("重复重建证据应幂等(=2), got %d/%d", e1, e2)
	}
}

func TestPhase6ClassifyTrend(t *testing.T) {
	cases := []struct {
		name string
		t    RelationshipTrend
		want string
	}{
		{"new", RelationshipTrend{}, "new"},
		{"dormant_by_gap", RelationshipTrend{Recent30: 4, Prior30: 4, DaysSinceLast: 40}, "dormant"},
		{"cooling", RelationshipTrend{Recent30: 2, Prior30: 8, DaysSinceLast: 5}, "cooling"},
		{"warming", RelationshipTrend{Recent30: 12, Prior30: 4, DaysSinceLast: 2}, "warming"},
		{"stable", RelationshipTrend{Recent30: 6, Prior30: 6, DaysSinceLast: 3}, "stable"},
	}
	for _, c := range cases {
		got, _ := classifyTrend(&c.t)
		if got != c.want {
			t.Errorf("%s: classifyTrend=%s, want %s", c.name, got, c.want)
		}
	}
}

func TestPhase6TrendFromRealMessages(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "阿May")
	now := time.Now()
	// 前 30~60 天窗口：8 条；近 30 天窗口：2 条（最近一条 5 天前，保证 DaysSinceLast<30）
	// 内容必须逐条不同，否则同内容+同时间戳会被 msg_hash 去重成 1 条。
	for i := 0; i < 8; i++ {
		ts := now.AddDate(0, 0, -45)
		if _, err := SaveMessages(db, id, []Message{{Sender: "other", Content: "以前常聊(" + string(rune('a'+i)) + ")", Timestamp: ts}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		ts := now.AddDate(0, 0, -5)
		if _, err := SaveMessages(db, id, []Message{{Sender: "me", Content: "最近少联系(" + string(rune('a'+i)) + ")", Timestamp: ts}}); err != nil {
			t.Fatal(err)
		}
	}
	tr, err := GetRelationshipTrend(db, id)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Prior30 < 5 || tr.Recent30 > tr.Prior30/2 {
		t.Fatalf("窗口计数不符预期: recent=%d prior=%d", tr.Recent30, tr.Prior30)
	}
	if tr.State != "cooling" {
		t.Fatalf("应判定为 cooling, got %s (%s)", tr.State, tr.Summary)
	}
	if tr.LastActivityDay == "" {
		t.Fatal("应有最近活动日")
	}
}

func TestPhase7SuggestionsIdempotentAndStatus(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "降温的人")
	now := time.Now()
	for i := 0; i < 8; i++ {
		if _, err := SaveMessages(db, id, []Message{{Sender: "other", Content: "老消息(" + string(rune('a'+i)) + ")", Timestamp: now.AddDate(0, 0, -45)}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := SaveMessages(db, id, []Message{{Sender: "me", Content: "近期我发的", Timestamp: now.AddDate(0, 0, -5)}}); err != nil {
		t.Fatal(err)
	}
	// 无 LLM：只产出规则建议，draft 留空
	if _, err := GenerateActionSuggestions(db, nil, id); err != nil {
		t.Fatal(err)
	}
	list, err := ListSuggestions(db, false)
	if err != nil {
		t.Fatal(err)
	}
	var cool *SuggestionView
	for i := range list {
		if list[i].ContactID == id && list[i].Kind == "cooling" {
			cool = &list[i]
		}
	}
	if cool == nil {
		t.Fatalf("cooling 建议未生成, list=%+v", list)
	}
	if cool.WindowKey == "" {
		t.Fatal("建议应带 window_key 幂等锁")
	}
	// 再生成一次，表内该 (contact,kind,window) 只应有一条
	if _, err := GenerateActionSuggestions(db, nil, id); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM relationship_action_suggestions WHERE contact_id=? AND kind='cooling'`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("幂等失效, cooling 建议有 %d 条", n)
	}
	// 用户标记 done，再次生成不应重置其状态（窗口内幂等）
	if err := SetSuggestionStatus(db, cool.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateActionSuggestions(db, nil, id); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM relationship_action_suggestions WHERE id=?`, cool.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "done" {
		t.Fatalf("done 状态不应被重新生成覆盖, got %s", status)
	}
	// 默认列表只返回 open，done 需 includeHandled 才可见
	open, _ := ListSuggestions(db, false)
	for _, s := range open {
		if s.ID == cool.ID {
			t.Fatal("done 建议不应出现在默认(仅 open)列表")
		}
	}
	handled, _ := ListSuggestions(db, true)
	var found bool
	for _, s := range handled {
		if s.ID == cool.ID && s.Status == "done" {
			found = true
		}
	}
	if !found {
		t.Fatal("includeHandled 应能看到 done 建议")
	}
}

func TestPhase7SuggestionStatusValidation(t *testing.T) {
	db := regressionDB(t)
	if err := SetSuggestionStatus(db, 999999, "bogus"); err == nil {
		t.Fatal("非法状态应报错")
	}
}

func TestPhase9SimulateRequiresLLM(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "无模型")
	if err := SaveProfile(db, id, `{"summary":"x"}`, "x", "i"); err != nil {
		t.Fatal(err)
	}
	// llm 为 nil（未配置）：必须明确报错，不编造
	_, err := SimulateReply(context.Background(), db, nil, id, "要不要一起吃个饭", nil)
	if err != ErrLLMNotConfigured {
		t.Fatalf("未配置模型应返回 ErrLLMNotConfigured, got %v", err)
	}
	// 空草稿：先于模型检查报错
	if _, err := SimulateReply(context.Background(), db, nil, id, "   ", nil); err == nil {
		t.Fatal("空草稿应报错")
	}
}

func TestPhase9SimulateEndToEnd(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "推演对象")
	if err := SaveProfile(db, id, `{"summary":"务实, 喜欢跑步"}`, "务实, 喜欢跑步", "i"); err != nil {
		t.Fatal(err)
	}
	regressionMessages(t, db, id, "这周工作好忙", "有空一起跑步")

	// 桩 LLM：返回 OpenAI 兼容结构，content 为 replies JSON
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := `{"replies":[{"text":"好啊，周末走起","mood":"积极","rationale":"他一直想约你运动"},{"text":"最近有点累，改天吧","mood":"中性","rationale":"刚说工作忙"}]}`
		resp := map[string]interface{}{
			"choices": []map[string]interface{}{
				{"message": map[string]interface{}{"content": payload}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	cfg := &Config{}
	cfg.LLM.BaseURL = srv.URL
	cfg.LLM.ApiKey = "test-key"
	cfg.LLM.Model = "test-model"
	llm := NewLLMClient(cfg)

	recent, err := GetRecentMessages(db, id, 24)
	if err != nil {
		t.Fatal(err)
	}
	res, err := SimulateReply(context.Background(), db, llm, id, "周末要不要一起去跑步？", recent)
	if err != nil {
		t.Fatalf("SimulateReply: %v", err)
	}
	if res.ContactName == "" {
		t.Fatal("应回填联系人名称")
	}
	if len(res.Replies) != 2 {
		t.Fatalf("应解析出 2 种反应, got %d", len(res.Replies))
	}
	if res.Replies[0].Text == "" || res.Replies[0].Mood == "" {
		t.Fatal("反应缺少 text/mood")
	}
	if res.Note == "" {
		t.Fatal("应带「AI 模拟仅供参考」的免责标注")
	}
}

func TestPhase9ParseSimulatedRepliesTolerance(t *testing.T) {
	// 裸数组形式
	arr := `[{"text":"a","mood":"积极","rationale":"r"},{"text":"b","mood":"中性","rationale":"r"}]`
	got, err := parseSimulatedReplies(arr)
	if err != nil || len(got) != 2 {
		t.Fatalf("裸数组解析失败: %v %v", got, err)
	}
	// 超过 3 条应截断为 3，空 text 应被丢弃
	wrapper := `{"replies":[{"text":"1"},{"text":"2"},{"text":""},{"text":"3"},{"text":"4"}]}`
	got2, err := parseSimulatedReplies(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 3 {
		t.Fatalf("应最多保留 3 条非空反应, got %d", len(got2))
	}
	for _, r := range got2 {
		if r.Text == "" {
			t.Fatal("空 text 不应保留")
		}
	}
}
