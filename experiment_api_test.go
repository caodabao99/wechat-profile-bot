package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func decodeExperiment(t *testing.T, body string) *Experiment {
	t.Helper()
	var res struct {
		Experiment *Experiment `json:"experiment"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("解析响应失败: %v body=%s", err, body)
	}
	return res.Experiment
}

// TestExperimentAPIEndToEnd 走真实 HTTP 路由：建→列→start→measure→结论，及越权/非法周期。
func TestExperimentAPIEndToEnd(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	cid := regressionContact(t, db, "实验人")
	// 路由内部用 time.Now()（真实钟 ~2026-10 > end 2026-01-15），obs 用满整窗，结论确定。

	// 铺数据：base 日均 1.0，obs 日均 4.0 → improved
	seedWindow(t, db, cid, "2026-01-01", 7, 1, 0)
	seedWindow(t, db, cid, "2026-01-08", 8, 3, 1)

	// 非法周期 → 400
	if w := callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/experiments", cid), `{"goal":"g","duration_days":0,"start_date":"2026-01-08"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("duration<=0 应 400, got %d %s", w.Code, w.Body.String())
	}

	// 新建（draft）
	w := callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/experiments", cid), `{"goal":"改善关系","strategy":"低压力联系","avoid_strategy":"连续追问","duration_days":7,"start_date":"2026-01-08"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("新建应 201, got %d %s", w.Code, w.Body.String())
	}
	created := decodeExperiment(t, w.Body.String())
	if created == nil || created.Status != ExpStatusDraft || created.ContactID != cid {
		t.Fatalf("新建应 draft: %+v", created)
	}

	// 列表
	w = callAPI(s, http.MethodGet, fmt.Sprintf("/api/contacts/%d/experiments", cid), "")
	if w.Code != http.StatusOK {
		t.Fatalf("列表应 200, got %d", w.Code)
	}
	var listRes struct {
		Experiments []Experiment `json:"experiments"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listRes); err != nil {
		t.Fatal(err)
	}
	if len(listRes.Experiments) != 1 {
		t.Fatalf("应列出 1 条, got %d", len(listRes.Experiments))
	}

	// draft 直接 measure → measured:false（尚未开始）
	w = callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/experiments/%d/measure", cid, created.ID), "")
	if w.Code != http.StatusOK {
		t.Fatalf("draft measure 应 200, got %d %s", w.Code, w.Body.String())
	}
	var skipRes struct {
		Measured bool `json:"measured"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &skipRes); err != nil {
		t.Fatal(err)
	}
	if skipRes.Measured {
		t.Fatal("draft 不应被测量")
	}

	// start → running
	w = callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/experiments/%d/start", cid, created.ID), "")
	if w.Code != http.StatusOK {
		t.Fatalf("start 应 200, got %d %s", w.Code, w.Body.String())
	}
	if got := decodeExperiment(t, w.Body.String()); got.Status != ExpStatusRunning {
		t.Fatalf("start 后应 running, got %+v", got)
	}

	// measure → improved
	w = callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/experiments/%d/measure", cid, created.ID), "")
	if w.Code != http.StatusOK {
		t.Fatalf("measure 应 200, got %d %s", w.Code, w.Body.String())
	}
	measured := decodeExperiment(t, w.Body.String())
	if measured.Conclusion != ExpConclusionImproved || measured.Status != ExpStatusCompleted {
		t.Fatalf("应 improved+completed: %+v", measured)
	}

	// 越权：用别的联系人路径操作该实验 → 404
	other := regressionContact(t, db, "无关人")
	w = callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/experiments/%d/start", other, created.ID), "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("越权应 404, got %d %s", w.Code, w.Body.String())
	}

	// 未知 verb → 404
	w = callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/experiments/%d/bogus", cid, created.ID), "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("未知 verb 应 404, got %d", w.Code)
	}
}

// TestExperimentAPICancelAbandon 验证 cancel/abandon 状态迁移经路由生效。
func TestExperimentAPICancelAbandon(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	cid := regressionContact(t, db, "取消实验")
	w := callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/experiments", cid), `{"goal":"g","duration_days":14,"start_date":"2026-01-08"}`)
	created := decodeExperiment(t, w.Body.String())

	if w = callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/experiments/%d/abandon", cid, created.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("abandon 应 200, got %d %s", w.Code, w.Body.String())
	}
	if got := decodeExperiment(t, w.Body.String()); got.Status != ExpStatusAbandoned {
		t.Fatalf("应 abandoned, got %+v", got)
	}
	if w = callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/experiments/%d/cancel", cid, created.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("cancel 应 200, got %d", w.Code)
	}
	if got := decodeExperiment(t, w.Body.String()); got.Status != ExpStatusCancelled {
		t.Fatalf("应 cancelled, got %+v", got)
	}
}
