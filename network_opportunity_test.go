package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestExtractOpportunityKeywords(t *testing.T) {
	kws := extractOpportunityKeywords("懂 AI 芯片供应链")
	join := strings.Join(kws, ",")
	// ASCII 词元小写。
	if !strings.Contains(join, "ai") {
		t.Fatalf("应含 ascii 词元 ai，实得 %v", kws)
	}
	// CJK bigram。
	if !strings.Contains(join, "芯片") || !strings.Contains(join, "供应") {
		t.Fatalf("应含中文二元组 芯片/供应，实得 %v", kws)
	}
	// 去重 + 上限。
	if len(kws) > 16 {
		t.Fatalf("关键词应截断至 16，实得 %d", len(kws))
	}
	seen := map[string]bool{}
	for _, k := range kws {
		if seen[k] {
			t.Fatalf("关键词不应重复：%q", k)
		}
		seen[k] = true
	}
	if extractOpportunityKeywords("   ") != nil {
		t.Fatalf("空白输入应返回 nil")
	}
}

func TestDiscoverOpportunitiesRecallAndLayers(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()
	cidA := regressionContact(t, db, "芯片专家")
	cidB := regressionContact(t, db, "跑友")

	// A：事实 + 项目 + 话题都命中「芯片供应链」。
	seedFact(t, db, cidA, "occupation", "芯片供应链专家", 0.9, 0.8, "active", "user", "2026-01-01", "")
	if _, err := CreateProject(db, CreateProjectInput{
		ContactID: cidA, Title: "AI芯片合作", Status: "active", Stage: "building",
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := ensureTopicHistory(db); err != nil {
		t.Fatal(err)
	}
	upsertTopicHistory(db, cidA, "2026-01-05", []TopicEntry{{Name: "芯片供应链趋势", Weight: 60, Status: "persistent"}}, now)
	seedStateRow(t, db, cidA, dynStable, "ok", "稳定")
	// B：不相关事实，应被排除。
	seedFact(t, db, cidB, "hobby", "喜欢马拉松跑步", 0.9, 0.8, "active", "user", "2026-01-01", "")

	res, err := DiscoverOpportunities(db, "芯片 供应链", now, 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.Layer != "DETERMINISTIC" {
		t.Fatalf("结果层应为 DETERMINISTIC，实得 %q", res.Layer)
	}
	if len(res.Matches) == 0 || res.Matches[0].ContactID != cidA {
		t.Fatalf("A 应为最佳匹配，实得 %+v", res.Matches)
	}
	m := res.Matches[0]
	if m.Name != "芯片专家" {
		t.Fatalf("联系人姓名应回填，实得 %q", m.Name)
	}
	// 分层：至少各有一个 FACT 与 INFERENCE。
	var hasFact, hasProject, hasTopic bool
	for _, e := range m.Evidence {
		switch e.Source {
		case "fact":
			hasFact = true
			if e.Layer != "FACT" {
				t.Fatalf("事实应标 FACT，实得 %q", e.Layer)
			}
		case "project":
			hasProject = true
			if e.Layer != "FACT" {
				t.Fatalf("项目应标 FACT，实得 %q", e.Layer)
			}
		case "topic":
			hasTopic = true
			if e.Layer != "INFERENCE" {
				t.Fatalf("话题应标 INFERENCE，实得 %q", e.Layer)
			}
		}
	}
	if !hasFact || !hasProject || !hasTopic {
		t.Fatalf("应同时召回事实/项目/话题三源，实得 fact=%v proj=%v topic=%v", hasFact, hasProject, hasTopic)
	}
	// B 不在结果里。
	for _, x := range res.Matches {
		if x.ContactID == cidB {
			t.Fatalf("不相关联系人不应被召回")
		}
	}
	// 关系强度与切入点非空。
	if !strings.Contains(m.RelationshipStrength, "亲密度") {
		t.Fatalf("应给出关系强度，实得 %q", m.RelationshipStrength)
	}
	if m.EntrySuggestion == "" {
		t.Fatalf("应给出推荐切入点")
	}
}

func TestDiscoverOpportunitiesEmptyAndNoMatch(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()
	if r, err := DiscoverOpportunities(db, "", now, 5); err != nil || len(r.Matches) != 0 {
		t.Fatalf("空查询应无匹配无错误：%v %+v", err, r)
	}
	regressionContact(t, db, "孤独者")
	r, err := DiscoverOpportunities(db, "量子计算拓扑", now, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Matches) != 0 {
		t.Fatalf("无匹配应返回空，实得 %+v", r.Matches)
	}
	if r.Note == "" {
		t.Fatalf("应带诚实说明")
	}
}

func TestOpportunityAPIEndToEnd(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	cid := regressionContact(t, db, "供应链朋友")
	seedFact(t, db, cid, "occupation", "负责芯片供应链", 0.9, 0.8, "active", "user", "2026-01-01", "")

	w := callAPI(s, http.MethodGet, "/api/opportunity?q="+url.QueryEscape("芯片供应链"), "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/opportunity 应 200，实得 %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK   bool               `json:"ok"`
		Data *OpportunityResult `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Data == nil || len(resp.Data.Matches) == 0 {
		t.Fatalf("应召回至少 1 人，实得 %+v", resp.Data)
	}
	// 非 GET → 404。
	if w2 := callAPI(s, http.MethodPost, "/api/opportunity", `{}`); w2.Code != http.StatusNotFound {
		t.Fatalf("POST 应 404，实得 %d", w2.Code)
	}
}
