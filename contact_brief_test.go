package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestDeterministicAvoidItems(t *testing.T) {
	// 降温 → 应避免施压。
	cooling := &ContactContext{CurrentRelationshipState: &RelationshipStateView{DynamicState: dynCooling}}
	if !hasSubstr(deterministicAvoidItems(cooling), "施压") {
		t.Fatalf("降温态应给出避免施压的提示，实得 %+v", deterministicAvoidItems(cooling))
	}
	// 冲突事实 → 应提示先澄清。
	conflict := &ContactContext{
		CurrentRelationshipState: &RelationshipStateView{DynamicState: dynStable},
		ConflictingFacts:         []FactView{{Type: "preference", Key: "k", Value: "v"}},
	}
	if !hasSubstr(deterministicAvoidItems(conflict), "冲突事实") {
		t.Fatalf("有冲突事实应提示，实得 %+v", deterministicAvoidItems(conflict))
	}
	// 无状态 → 诚实留白。
	if got := deterministicAvoidItems(&ContactContext{}); len(got) != 0 {
		t.Fatalf("无可依据信号应留空，实得 %+v", got)
	}
	if got := deterministicAvoidItems(nil); len(got) != 0 {
		t.Fatalf("nil 上下文应留空，实得 %+v", got)
	}
}

func TestRecentTopicNames(t *testing.T) {
	cc := &ContactContext{RecentTopics: &ContactTopics{Weeks: []TopicWeek{
		{WeekStart: "2026-01-05", Topics: []TopicEntry{{Name: "老话题"}}},
		{WeekStart: "2026-02-02", Topics: []TopicEntry{{Name: "AI创业"}, {Name: "跑步"}}},
	}}}
	got := recentTopicNames(cc)
	// 只取最近一周。
	if len(got) != 2 || got[0] != "AI创业" {
		t.Fatalf("应取最近一周主题，实得 %+v", got)
	}
	if len(recentTopicNames(&ContactContext{})) != 0 {
		t.Fatalf("无主题历史应空")
	}
}

func TestContactBriefAPIEndToEnd(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	cid := regressionContact(t, db, "简报对象")
	seedStateRow(t, db, cid, dynCooling, "watching", "近期降温")

	w := callAPI(s, http.MethodGet, "/api/contacts/"+itoa(cid)+"/brief", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET brief 应 200，实得 %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK    bool          `json:"ok"`
		Brief *ContactBrief `json:"brief"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Brief == nil {
		t.Fatalf("响应结构错误 %+v", resp)
	}
	if resp.Brief.Layer != "DETERMINISTIC" {
		t.Fatalf("简报应为纯确定性层，实得 %q", resp.Brief.Layer)
	}
	if !hasSubstr(resp.Brief.AvoidItems, "施压") {
		t.Fatalf("降温简报应含避免施压，实得 %+v", resp.Brief.AvoidItems)
	}
	// 不存在联系人 → 404（非 500）。
	if w2 := callAPI(s, http.MethodGet, "/api/contacts/999999/brief", ""); w2.Code != http.StatusNotFound {
		t.Fatalf("不存在联系人应 404，实得 %d", w2.Code)
	}
}
