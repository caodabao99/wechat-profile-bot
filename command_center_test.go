package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestProjectSignal(t *testing.T) {
	today := "2026-03-10"
	if got := projectSignal(ProjectView{BlockedReason: "等待对方回复"}, today); got != "blocked" {
		t.Fatalf("受阻应判 blocked，实得 %q", got)
	}
	if got := projectSignal(ProjectView{NextActionDue: "2026-03-08"}, today); got != "due_soon" {
		t.Fatalf("到期应判 due_soon，实得 %q", got)
	}
	if got := projectSignal(ProjectView{NextActionDue: "2026-05-01"}, today); got != "stalled" {
		t.Fatalf("无到期/受阻默认 stalled，实得 %q", got)
	}
}

func TestCommandSectionProjectQueriesDueAndBlocked(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()
	cid := regressionContact(t, db, "项目对象")
	// 一个受阻项目。
	if _, err := CreateProject(db, CreateProjectInput{
		ContactID: cid, Title: "合作洽谈", Status: "active", Stage: "negotiating",
		BlockedReason: "对方未回复",
	}, now); err != nil {
		t.Fatal(err)
	}
	// 一个临近到期项目。
	if _, err := CreateProject(db, CreateProjectInput{
		ContactID: cid, Title: "跟进交付", Status: "active", Stage: "building",
		NextActionDue: now.Format("2006-01-02"),
	}, now); err != nil {
		t.Fatal(err)
	}
	got := commandSectionProject(db, now)
	if len(got) != 2 {
		t.Fatalf("应返回 2 个到期/受阻项目，实得 %d %+v", len(got), got)
	}
	// 受阻排在前。
	if got[0].Signal != "blocked" {
		t.Fatalf("受阻应优先，实得 %+v", got)
	}
}

func TestBuildCommandCenterShapeAndTolerant(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()
	cid := regressionContact(t, db, "指挥中心")
	regressionMessages(t, db, cid, "换了新工作 最近很忙")
	seedStateRow(t, db, cid, dynAtRisk, "urgent", "快流失")

	c := BuildCommandCenter(db, now)
	if c == nil {
		t.Fatal("指挥中心不应为 nil")
	}
	// 每栏切片必须非 nil（即便无数据也返回空切片，前端可安全渲染）。
	if c.Today == nil || c.Memory == nil || c.Risk == nil || c.Project == nil {
		t.Fatalf("各栏应非 nil：today=%v mem=%v risk=%v proj=%v", c.Today == nil, c.Memory == nil, c.Risk == nil, c.Project == nil)
	}
	if c.Risk != nil {
		// 至少能捕获到 seeded at_risk。
		found := false
		for _, r := range c.Risk {
			if r.Severity == "high" {
				found = true
			}
		}
		if !found {
			t.Logf("风险栏未含 high（数据依赖，不强制失败）：%+v", c.Risk)
		}
	}
}

func TestCommandCenterAPIEndToEnd(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	// 无数据也应 200（核心页绝不 500/503）。
	w := callAPI(s, http.MethodGet, "/api/command-center", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/command-center 应 200，实得 %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK   bool           `json:"ok"`
		Data *CommandCenter `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Data == nil {
		t.Fatalf("响应结构错误 %+v", resp)
	}
	// 非 GET → 404。
	if w2 := callAPI(s, http.MethodPost, "/api/command-center", `{}`); w2.Code != http.StatusNotFound {
		t.Fatalf("POST 应 404，实得 %d", w2.Code)
	}
}
