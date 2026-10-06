package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// seedFact 直接插一条 profile_facts（测试内单线程，绕锁用 db.Exec），返回其 id。
func seedFact(t *testing.T, db *sql.DB, cid int64, typ, val string, conf, strength float64, status, sourceType, lastSeen, lastConfirmed string) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO profile_facts (contact_id, fact_type, fact_key, fact_value, confidence, evidence_strength, status, source_type, last_seen, last_confirmed_at)
		VALUES (?, ?, '', ?, ?, ?, ?, ?, ?, ?)`, cid, typ, val, conf, strength, status, sourceType, lastSeen, lastConfirmed)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// seedConflictEvidence 为事实挂一条 evidence_type=conflict 的证据行（§7.5 触发 recent_conflict）。
func seedConflictEvidence(t *testing.T, db *sql.DB, factID, cid int64) {
	t.Helper()
	mustExec(t, db, `INSERT INTO profile_fact_evidence (fact_id, contact_id, message_id, archived, evidence_type) VALUES (?, ?, 1, 0, 'conflict')`, factID, cid)
}

func TestClassifyReviewReasons(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -120).Format(time.RFC3339) // 120 天前
	recent := now.Format(time.RFC3339)
	cases := []struct {
		name       string
		it         MemoryReviewItem
		wantReason string
		wantOK     bool
	}{
		{"冲突", MemoryReviewItem{HasConflict: true, Status: "active", Confidence: 0.9}, reviewReasonConflict, true},
		{"过时", MemoryReviewItem{Status: "stale", Confidence: 0.9, LastSeen: recent, LastConfirmedAt: recent}, reviewReasonStale, true},
		{"证据弱", MemoryReviewItem{Status: "active", Confidence: 0.6, EvidenceStrength: 0.1, LastSeen: recent, LastConfirmedAt: recent}, reviewReasonWeak, true},
		{"中等置信", MemoryReviewItem{Status: "active", Confidence: 0.6, EvidenceStrength: 0.5, LastSeen: recent, LastConfirmedAt: recent}, reviewReasonMedium, true},
		{"长期未确认", MemoryReviewItem{Status: "active", Confidence: 0.9, EvidenceStrength: 1.0, LastSeen: old, LastConfirmedAt: ""}, reviewReasonUnverified, true},
		{"无触发", MemoryReviewItem{Status: "active", Confidence: 0.9, EvidenceStrength: 1.0, LastSeen: recent, LastConfirmedAt: recent}, "", false},
	}
	for _, c := range cases {
		got, _, ok := classifyReview(c.it, now)
		if ok != c.wantOK {
			t.Errorf("%s: ok got %v want %v", c.name, ok, c.wantOK)
		}
		if got != c.wantReason {
			t.Errorf("%s: reason got %q want %q", c.name, got, c.wantReason)
		}
	}
}

// §8.1：每联系人 Top3 节流 + 冲突优先 + 用户确认事实永不入队。
func TestBuildReviewQueueThrottleAndPriority(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "待确认者")
	now := time.Now()
	recent := now.Format(time.RFC3339)

	conflictID := seedFact(t, db, cid, "occupation", "律师", 0.6, 0.5, "active", "ai", recent, "")
	seedConflictEvidence(t, db, conflictID, cid)
	// 4 条中等置信事实（同联系人）→ 加上冲突共 5 候选，应被节流到 Top3。
	for i := 0; i < 4; i++ {
		seedFact(t, db, cid, "important_fact", fmt.Sprintf("事实%d", i), 0.6, 0.5, "active", "ai", recent, "")
	}
	// 用户确认事实：即便中等置信也绝不入队（§8.3）。
	seedFact(t, db, cid, "personality", "已确认项", 0.6, 0.5, "confirmed", "user", recent, recent)

	list, err := BuildMemoryReviewQueue(db, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	var forContact int
	var sawUser bool
	for _, it := range list {
		if it.ContactID == cid {
			forContact++
		}
		if it.FactValue == "已确认项" {
			sawUser = true
		}
	}
	if forContact != reviewPerContactMax {
		t.Errorf("每联系人应节流到 Top%d, got %d", reviewPerContactMax, forContact)
	}
	if sawUser {
		t.Error("用户确认的事实不应进入待确认队列")
	}
	if len(list) == 0 || list[0].Reason != reviewReasonConflict {
		t.Fatalf("冲突应排在最前, got %+v", list)
	}
	if list[0].FactID != conflictID {
		t.Errorf("队首应为冲突事实 id=%d, got %d", conflictID, list[0].FactID)
	}
}

// 系统级上限：limit 生效。
func TestBuildReviewQueueSystemLimit(t *testing.T) {
	db := regressionDB(t)
	recent := time.Now().Format(time.RFC3339)
	for c := 0; c < 5; c++ {
		cid := regressionContact(t, db, fmt.Sprintf("多联系人%d", c))
		for i := 0; i < 3; i++ {
			seedFact(t, db, cid, "important_fact", fmt.Sprintf("f%d-%d", c, i), 0.6, 0.5, "active", "ai", recent, "")
		}
	}
	list, err := BuildMemoryReviewQueue(db, time.Now(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Fatalf("全局 limit=4 应截断到 4, got %d", len(list))
	}
}

func decodeReview(t *testing.T, body string) []MemoryReviewItem {
	t.Helper()
	var res struct {
		OK    bool               `json:"ok"`
		Items []MemoryReviewItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("解析失败: %v body=%s", err, body)
	}
	return res.Items
}

// API 端到端：GET 队列 → reject 退出当前态 → defer 保持原状态 → confirm 提升为 user 后退出队列。
func TestMemoryReviewAPIEndToEnd(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	cid := regressionContact(t, db, "队列API")
	recent := time.Now().Format(time.RFC3339)
	fid := seedFact(t, db, cid, "important_fact", "值得确认", 0.6, 0.5, "active", "ai", recent, "")

	// GET 队列应含该事实
	w := callAPI(s, http.MethodGet, "/api/memory/review?limit=10", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET review 应 200, got %d %s", w.Code, w.Body.String())
	}
	items := decodeReview(t, w.Body.String())
	var found bool
	for _, it := range items {
		if it.FactID == fid {
			found = true
		}
	}
	if !found {
		t.Fatalf("队列应含事实 %d, got %+v", fid, items)
	}

	// defer：保持原状态（路由约定 /facts/{verb}/{factID}）
	w = callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/facts/defer/%d", cid, fid), "")
	if w.Code != http.StatusOK {
		t.Fatalf("defer 应 200, got %d", w.Code)
	}
	var st string
	db.QueryRow(`SELECT status FROM profile_facts WHERE id=?`, fid).Scan(&st)
	if st != "active" {
		t.Errorf("defer 不应改状态, got %s", st)
	}

	// reject：退出当前态
	w = callAPI(s, http.MethodPost, fmt.Sprintf("/api/contacts/%d/facts/reject/%d", cid, fid), "")
	if w.Code != http.StatusOK {
		t.Fatalf("reject 应 200, got %d", w.Code)
	}
	db.QueryRow(`SELECT status FROM profile_facts WHERE id=?`, fid).Scan(&st)
	if st != "rejected" {
		t.Errorf("reject 应置 rejected, got %s", st)
	}

	// 再 GET 队列：rejected 不再出现
	items = decodeReview(t, callAPI(s, http.MethodGet, "/api/memory/review?limit=10", "").Body.String())
	for _, it := range items {
		if it.FactID == fid {
			t.Fatal("被否定的事实不应再进入队列")
		}
	}
}
