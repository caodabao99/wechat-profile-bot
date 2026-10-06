package main

// Phase 8b 联系人深分页二阶段（蓝图 §11.3/§11.4）回归。
// 覆盖：last_updated_unix 触发器随 last_updated 同步、keyset 游标遍历与旧 offset 参照
// 顺序等价且不重不漏、legacy 不可解析时间（unix=0）排最后、includeTotal 仅首屏 opt-in
// 才 COUNT、以及 HTTP cursor 路由端到端。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// setContactUpdated 通过 UPDATE 改 last_updated，借 _au 触发器同步 last_updated_unix，用于受控排序。
func setContactUpdated(t *testing.T, db *sql.DB, id int64, ts time.Time) {
	t.Helper()
	if _, err := db.Exec(`UPDATE contacts SET last_updated=? WHERE id=?`, ts.Format(time.RFC3339), id); err != nil {
		t.Fatal(err)
	}
}

func readLastUpdatedUnix(t *testing.T, db *sql.DB, id int64) int64 {
	t.Helper()
	var u int64
	if err := db.QueryRow(`SELECT last_updated_unix FROM contacts WHERE id=?`, id).Scan(&u); err != nil {
		t.Fatal(err)
	}
	return u
}

// TestContactsTriggerSyncsLastUpdatedUnix 证明 UPDATE last_updated 会经 _au 触发器同步整型列，
// 且不可解析/空 last_updated 归 0（排序落最后，与原 strftime NULL 行为一致）。
func TestContactsTriggerSyncsLastUpdatedUnix(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "阿明")
	ts := time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC)
	setContactUpdated(t, db, id, ts)
	if got := readLastUpdatedUnix(t, db, id); got != ts.Unix() {
		t.Fatalf("UPDATE 触发器未同步 last_updated_unix: got=%d want=%d", got, ts.Unix())
	}
	// 改成不可解析的空串 → 触发器应把 last_updated_unix 归 0
	if _, err := db.Exec(`UPDATE contacts SET last_updated='' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if got := readLastUpdatedUnix(t, db, id); got != 0 {
		t.Fatalf("空 last_updated 应使 last_updated_unix=0，得 %d", got)
	}
}

// TestContactsCursorMatchesOffsetOrder 证明逐页 keyset 游标遍历得到的联系人序列，
// 与单次大 limit 的 offset 参照完全一致（同序、不重、不漏）。
func TestContactsCursorMatchesOffsetOrder(t *testing.T) {
	db := regressionDB(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const n = 11
	for i := 0; i < n; i++ {
		id := regressionContact(t, db, fmt.Sprintf("联系人%02d", i))
		setContactUpdated(t, db, id, base.Add(time.Duration(i)*time.Hour))
	}

	ref, _, err := GetContactsPageFiltered(db, false, "", nil, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	refIDs := make([]int64, 0, len(ref))
	for _, c := range ref {
		refIDs = append(refIDs, c.ID)
	}

	var gotIDs []int64
	seen := map[int64]bool{}
	var bu, bi int64
	for page := 0; page < 1000; page++ {
		list, _, hasMore, nu, ni, err := GetContactsPageCursor(db, false, "", nil, bu, bi, 3, false)
		if err != nil {
			t.Fatalf("游标第 %d 页出错: %v", page, err)
		}
		if len(list) > 3 {
			t.Fatalf("第 %d 页返回 %d 条，超过 limit=3", page, len(list))
		}
		for _, c := range list {
			if seen[c.ID] {
				t.Fatalf("游标翻页出现重复联系人 id=%d", c.ID)
			}
			seen[c.ID] = true
			gotIDs = append(gotIDs, c.ID)
		}
		if !hasMore {
			break
		}
		if nu == 0 && ni == 0 {
			t.Fatalf("hasMore=true 却未返回有效游标（第 %d 页）", page)
		}
		bu, bi = nu, ni
	}
	if len(gotIDs) != n {
		t.Fatalf("游标遍历应含 %d 条，实得 %d", n, len(gotIDs))
	}
	if fmt.Sprint(gotIDs) != fmt.Sprint(refIDs) {
		t.Fatalf("游标遍历与 offset 参照顺序不一致\n got=%v\nwant=%v", gotIDs, refIDs)
	}
}

// TestContactsCursorLegacyZeroSortsLast 证明 last_updated 不可解析（last_updated_unix=0）的
// 历史联系人排在全最后，与旧路径 strftime NULL 在 DESC 下排末一致。
func TestContactsCursorLegacyZeroSortsLast(t *testing.T) {
	db := regressionDB(t)
	base := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	valid := regressionContact(t, db, "有效")
	setContactUpdated(t, db, valid, base)
	legacy := regressionContact(t, db, "遗留")
	if _, err := db.Exec(`UPDATE contacts SET last_updated='' WHERE id=?`, legacy); err != nil {
		t.Fatal(err)
	}
	if u := readLastUpdatedUnix(t, db, legacy); u != 0 {
		t.Fatalf("遗留空时间应归 0，得 %d", u)
	}
	list, _, _, _, _, err := GetContactsPageCursor(db, false, "", nil, 0, 0, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != valid || list[1].ID != legacy {
		t.Fatalf("遗留 0 值应排最后，得 %#v", list)
	}
}

// TestContactsCursorIncludeTotal 证明 total 仅在 includeTotal=true 时经 COUNT(*) 返回，
// 默认 false 跳过 COUNT（total=0），hasMore 由多取一条推断。
func TestContactsCursorIncludeTotal(t *testing.T) {
	db := regressionDB(t)
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		id := regressionContact(t, db, fmt.Sprintf("C%d", i))
		setContactUpdated(t, db, id, base.Add(time.Duration(i)*time.Hour))
	}
	if _, total, _, _, _, err := GetContactsPageCursor(db, false, "", nil, 0, 0, 2, false); err != nil {
		t.Fatal(err)
	} else if total != 0 {
		t.Fatalf("includeTotal=false 不应 COUNT，期望 total=0，得 %d", total)
	}
	_, total2, hasMore, _, _, err := GetContactsPageCursor(db, false, "", nil, 0, 0, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if total2 != 5 {
		t.Fatalf("includeTotal=true 应返回全量匹配总数 5，得 %d", total2)
	}
	if !hasMore {
		t.Fatal("5 条 limit=2 首屏应 hasMore=true")
	}
}

// TestContactsCursorAPIEndToEnd 走真实 HTTP 路由验证 cursor=1 端点：
// 首屏带 includeTotal 返回总数与游标，翻页不带 includeTotal 则 total=0 且不重复。
func TestContactsCursorAPIEndToEnd(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	const n = 7
	for i := 0; i < n; i++ {
		id := regressionContact(t, db, fmt.Sprintf("U%d", i))
		setContactUpdated(t, db, id, base.Add(time.Duration(i)*time.Hour))
	}

	type page struct {
		List     []contactJSON `json:"list"`
		Total    int           `json:"total"`
		HasMore  bool          `json:"hasMore"`
		NextUnix int64         `json:"nextUnix"`
		NextID   int64         `json:"nextID"`
	}

	w := callAPI(s, http.MethodGet, "/api/contacts?cursor=1&limit=3&includeTotal=1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("首屏 code=%d body=%s", w.Code, w.Body.String())
	}
	var p1 page
	if err := json.Unmarshal(w.Body.Bytes(), &p1); err != nil {
		t.Fatal(err)
	}
	if p1.Total != n {
		t.Fatalf("首屏 total 应=%d，得 %d", n, p1.Total)
	}
	if len(p1.List) != 3 || !p1.HasMore || p1.NextID == 0 {
		t.Fatalf("首屏异常 len=%d hasMore=%v nextID=%d", len(p1.List), p1.HasMore, p1.NextID)
	}

	// 翻页：不带 includeTotal → total 应为 0（免 COUNT），且不与首屏重复
	w2 := callAPI(s, http.MethodGet,
		fmt.Sprintf("/api/contacts?cursor=1&limit=3&beforeUnix=%d&beforeID=%d", p1.NextUnix, p1.NextID), "")
	if w2.Code != http.StatusOK {
		t.Fatalf("翻页 code=%d body=%s", w2.Code, w2.Body.String())
	}
	var p2 page
	if err := json.Unmarshal(w2.Body.Bytes(), &p2); err != nil {
		t.Fatal(err)
	}
	if p2.Total != 0 {
		t.Fatalf("翻页不应 COUNT，total 应=0，得 %d", p2.Total)
	}
	seen := map[int64]bool{}
	for _, c := range p1.List {
		seen[c.ID] = true
	}
	for _, c := range p2.List {
		if seen[c.ID] {
			t.Fatalf("翻页与首屏出现重复联系人 id=%d", c.ID)
		}
		seen[c.ID] = true
	}
}
