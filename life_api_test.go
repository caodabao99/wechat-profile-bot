package main

// 人生模拟器 HTTP 层回归：三端点自愈现算、鉴权、方法限制、未知子路径 404、手动重算异步返回。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLifeAPIEndToEnd(t *testing.T) {
	db := regressionAssistantDB(t)
	if err := saveAssistantSettings(db, defaultAssistantSettings()); err != nil {
		t.Fatal(err)
	}
	// 造一个有互动的联系人，让 state/projection/timeline 都能现算出非空结果
	cid := regressionContact(t, db, "推演对象")
	now := time.Now()
	for d := 0; d < 8; d++ {
		day := now.AddDate(0, 0, -d)
		seedMessage(t, db, cid, "me", "在忙吗", fmt.Sprintf("lifeapi-me-%d", d), day)
		seedMessage(t, db, cid, "other", "还好啦最近有点事", fmt.Sprintf("lifeapi-other-%d", d), day)
	}

	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}

	// 未授权 → 401
	noauth := httptest.NewRequest(http.MethodGet, "/api/life/state", nil)
	wno := httptest.NewRecorder()
	s.route(wno, noauth)
	if wno.Code != http.StatusUnauthorized {
		t.Fatalf("未授权应 401, got %d", wno.Code)
	}

	// GET state：自愈现算，应 200 且带 state.portfolio.contactCount
	w := callAPI(s, http.MethodGet, "/api/life/state", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET state: %d %s", w.Code, w.Body.String())
	}
	var stateResp struct {
		Enabled bool `json:"enabled"`
		State   struct {
			Portfolio struct {
				ContactCount int `json:"contactCount"`
			} `json:"portfolio"`
		} `json:"state"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &stateResp); err != nil {
		t.Fatal(err)
	}
	if !stateResp.Enabled {
		t.Error("enabled 应为 true")
	}

	// GET projection：应 200 且有 3 条策略
	w = callAPI(s, http.MethodGet, "/api/life/projection", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET projection: %d %s", w.Code, w.Body.String())
	}
	var projResp struct {
		Projection struct {
			HorizonDays int `json:"horizonDays"`
			Strategies  []struct {
				Key string `json:"key"`
			} `json:"strategies"`
		} `json:"projection"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &projResp); err != nil {
		t.Fatal(err)
	}
	if len(projResp.Projection.Strategies) != 3 {
		t.Errorf("应有 3 条自动策略, got %d", len(projResp.Projection.Strategies))
	}

	// GET timeline：应 200 且 narrative 可解析
	w = callAPI(s, http.MethodGet, "/api/life/timeline", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET timeline: %d %s", w.Code, w.Body.String())
	}
	if !validJSON(w.Body.String()) {
		t.Errorf("timeline 响应非 JSON: %s", truncate(w.Body.String()))
	}

	// 方法限制：GET 端点不接受 POST
	if c := callAPI(s, http.MethodPost, "/api/life/state", ""); c.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST state 应 405, got %d", c.Code)
	}

	// 未知子路径 404
	if c := callAPI(s, http.MethodGet, "/api/life/nope", ""); c.Code != http.StatusNotFound {
		t.Errorf("未知子路径应 404, got %d", c.Code)
	}
	// 过深层级 404（对齐 weekly-plan 教训）
	if c := callAPI(s, http.MethodGet, "/api/life/state/deep", ""); c.Code != http.StatusNotFound {
		t.Errorf("过深子路径应 404, got %d", c.Code)
	}
	// 空子路径 404
	if c := callAPI(s, http.MethodGet, "/api/life", ""); c.Code != http.StatusNotFound {
		t.Errorf("/api/life 应 404, got %d", c.Code)
	}

	// POST recompute：异步、立即 200
	w = callAPI(s, http.MethodPost, "/api/life/recompute", "")
	if w.Code != http.StatusOK {
		t.Fatalf("POST recompute: %d %s", w.Code, w.Body.String())
	}
	var rec struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil || !rec.OK {
		t.Fatalf("recompute 响应不符: %s err=%v", w.Body.String(), err)
	}
	// GET recompute 应 405
	if c := callAPI(s, http.MethodGet, "/api/life/recompute", ""); c.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET recompute 应 405, got %d", c.Code)
	}
	// 给后台任务收尾时间，避免关库后 goroutine 仍用 db 干扰其它测试
	time.Sleep(500 * time.Millisecond)
}
