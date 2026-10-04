package main

// 跨联系人关系图谱回归。
// 覆盖：同类事实交叉匹配、提到名字、幂等重建、按联系人过滤、API 全链路。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func itoa(v int64) string { return fmt.Sprintf("%d", v) }

// seedProfileFacts 给联系人存一份画像并抽取 active 事实。
func seedProfileFacts(t *testing.T, db *sql.DB, cid int64, profileJSON string) {
	t.Helper()
	if err := SaveProfile(db, cid, profileJSON, "测试画像", "seed"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ExtractProfileFacts(db, cid); err != nil {
		t.Fatal(err)
	}
}

func countConnections(t *testing.T, db *sql.DB, connType string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM contact_connections WHERE connection_type=?`, connType).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRebuildAllConnectionsSharedAndIdempotent(t *testing.T) {
	db := regressionDB(t)
	a := regressionContact(t, db, "阿哲")
	b := regressionContact(t, db, "小林")
	c := regressionContact(t, db, "路人")

	seedProfileFacts(t, db, a, `{"basic_info":{"occupation":"设计师","location":"成都"},"interests":["看展"],"important_facts":["同事老周"]}`)
	seedProfileFacts(t, db, b, `{"basic_info":{"occupation":"设计师","location":"成都"},"interests":["徒步"]}`)
	seedProfileFacts(t, db, c, `{"basic_info":{"location":"北京"}}`)

	n, err := RebuildAllConnections(db)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("应发现关联")
	}

	// a、b 同城 → shared_location；同职业 → shared_occupation
	var locDetail string
	var conf float64
	if err := db.QueryRow(
		`SELECT detail, confidence FROM contact_connections WHERE connection_type='shared_location' LIMIT 1`).
		Scan(&locDetail, &conf); err != nil {
		t.Fatalf("应有 shared_location: %v", err)
	}
	if locDetail != "成都" {
		t.Fatalf("detail 应为共同城市, got %s", locDetail)
	}
	if conf < 0.69 || conf > 0.71 {
		t.Fatalf("shared_location 可信度应≈0.7, got %v", conf)
	}
	if countConnections(t, db, "shared_occupation") == 0 {
		t.Fatal("同职业应有 shared_occupation")
	}
	// c 与任何人都不同城/不同兴趣 → a-c/b-c 不该有 location 连线
	var cross int
	db.QueryRow(`SELECT COUNT(*) FROM contact_connections
		WHERE connection_type='shared_location' AND (contact_a=? OR contact_b=?)`, c, c).Scan(&cross)
	if cross != 0 {
		t.Fatalf("北京路人不该进同城连线, got %d", cross)
	}

	// 幂等：重建两次结果一致（每对同类型只有一条）
	if _, err := RebuildAllConnections(db); err != nil {
		t.Fatal(err)
	}
	var total, distinct int
	db.QueryRow(`SELECT COUNT(*) FROM contact_connections`).Scan(&total)
	db.QueryRow(`SELECT COUNT(*) FROM (SELECT DISTINCT contact_a, contact_b, connection_type FROM contact_connections)`).Scan(&distinct)
	if total != distinct {
		t.Fatalf("重建后出现重复连线: total=%d distinct=%d", total, distinct)
	}
}

func TestRebuildAllConnectionsMentionedName(t *testing.T) {
	db := regressionDB(t)
	a := regressionContact(t, db, "李雷")
	b := regressionContact(t, db, "韩梅梅")

	// a 的画像里提到 b 的名字
	seedProfileFacts(t, db, a, `{"important_facts":["和韩梅梅一起负责新项目"]}`)
	seedProfileFacts(t, db, b, `{"basic_info":{"location":"深圳"}}`)

	if _, err := RebuildAllConnections(db); err != nil {
		t.Fatal(err)
	}
	low, high := a, b
	if low > high {
		low, high = high, low
	}
	var detail string
	var conf float64
	if err := db.QueryRow(
		`SELECT detail, confidence FROM contact_connections
		 WHERE contact_a=? AND contact_b=? AND connection_type='mentioned_name'`, low, high).
		Scan(&detail, &conf); err != nil {
		t.Fatalf("应有 mentioned_name 连线: %v", err)
	}
	if detail != "韩梅梅" {
		t.Fatalf("detail 应为被提到的名字, got %s", detail)
	}
	if conf < 0.79 || conf > 0.81 {
		t.Fatalf("mentioned_name 可信度应≈0.8, got %v", conf)
	}
}

func TestListConnectionsFilterAndNames(t *testing.T) {
	db := regressionDB(t)
	a := regressionContact(t, db, "甲")
	b := regressionContact(t, db, "乙")
	c := regressionContact(t, db, "丙")
	seedProfileFacts(t, db, a, `{"basic_info":{"location":"杭州"},"interests":["摄影"]}`)
	seedProfileFacts(t, db, b, `{"basic_info":{"location":"杭州"},"interests":["摄影"]}`)
	seedProfileFacts(t, db, c, `{"basic_info":{"location":"广州"},"interests":["钓鱼"]}`)
	if _, err := RebuildAllConnections(db); err != nil {
		t.Fatal(err)
	}

	all, err := ListConnections(db, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("全量列表不应为空")
	}
	// a↔b 有连线，涉及 c 的没有
	for _, v := range all {
		if v.ContactA == c || v.ContactB == c {
			t.Fatalf("丙应与任何人无连线: %+v", v)
		}
		if v.NameA == "" || v.NameB == "" {
			t.Fatalf("列表应带联系人名字: %+v", v)
		}
	}
	// 按 a 过滤能查到，按 c 过滤应为空
	onlyA, err := ListConnections(db, a, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyA) == 0 {
		t.Fatal("甲应有至少一条关联")
	}
	onlyC, err := ListConnections(db, c, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyC) != 0 {
		t.Fatalf("丙不该有关联: %+v", onlyC)
	}
	// confidence 降序
	for i := 1; i < len(all); i++ {
		if all[i-1].Confidence < all[i].Confidence {
			t.Fatalf("应按可信度降序: %+v", all)
		}
	}
}

func TestConnectionsAPIEndToEnd(t *testing.T) {
	db := regressionDB(t)
	a := regressionContact(t, db, "API阿哲")
	b := regressionContact(t, db, "API小林")
	seedProfileFacts(t, db, a, `{"basic_info":{"location":"武汉"}}`)
	seedProfileFacts(t, db, b, `{"basic_info":{"location":"武汉"}}`)

	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}

	// GET 全量（表空时自愈重建）
	w := callAPI(s, http.MethodGet, "/api/relationships/connections", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET connections: %d %s", w.Code, w.Body.String())
	}
	var list struct {
		Connections []ConnectionView `json:"connections"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Connections) == 0 {
		t.Fatal("自愈重建后应能查到武汉同城连线")
	}

	// POST 手动重建
	w = callAPI(s, http.MethodPost, "/api/relationships/connections/rebuild", "")
	if w.Code != http.StatusOK {
		t.Fatalf("POST rebuild: %d %s", w.Code, w.Body.String())
	}
	var rb struct {
		OK    bool `json:"ok"`
		Count int  `json:"count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rb); err != nil {
		t.Fatal(err)
	}
	if !rb.OK || rb.Count == 0 {
		t.Fatalf("重建返回不符: %+v", rb)
	}

	// 未知子路径 404
	w = callAPI(s, http.MethodGet, "/api/relationships/connections/whatever", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("未知子路径应 404, got %d", w.Code)
	}

	// 单联系人视角：合法 id 返回 200 + 列表
	w = callAPI(s, http.MethodGet, "/api/contacts/"+itoa(a)+"/connections", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET contact connections: %d %s", w.Code, w.Body.String())
	}
	var one struct {
		Connections []ConnectionView `json:"connections"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	if len(one.Connections) == 0 {
		t.Fatal("阿哲应有武汉同城关联")
	}
	// 非法 id 应 400，不该 500
	if c := callAPI(s, http.MethodGet, "/api/contacts/abc/connections", ""); c.Code != http.StatusBadRequest {
		t.Fatalf("非法 contactId 应 400, got %d", c.Code)
	}
}
