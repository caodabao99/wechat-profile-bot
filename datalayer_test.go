package main

// 数据层登记表（registry.go）+ 统一指标层（metrics.go）的单测：
//   - TableRegistry/GetTableMeta/IsContactScoped 元数据正确；
//   - contactCleanupTables 全部已登记且含 contact_id（回归护栏：防止 contact_connections
//     这类「双列/无 contact_id 表」被误纳入级联清理，导致删联系人报 no such column 回滚）；
//   - medianSeconds 纯函数的空/奇/偶确定性；
//   - GetAggregatedMetrics 的小时直方、条数、回复延迟中位、归档感知、窗口边界、空数据诚实降级。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTableRegistryBasics(t *testing.T) {
	reg := TableRegistry()
	if len(reg) == 0 {
		t.Fatal("登记表不应为空")
	}
	// 表名唯一。
	seen := map[string]bool{}
	for _, m := range reg {
		if m.Name == "" {
			t.Fatal("登记表存在空名")
		}
		if seen[m.Name] {
			t.Fatalf("表名重复登记: %s", m.Name)
		}
		seen[m.Name] = true
		switch m.Type {
		case "core", "derived", "cache", "audit", "config":
		default:
			t.Errorf("表 %s 的 type 非法: %q", m.Name, m.Type)
		}
	}
	// 已知核心表可查到，未知表返回 nil。
	if GetTableMeta("messages") == nil {
		t.Error("messages 应已登记")
	}
	if GetTableMeta("__no_such_table__") != nil {
		t.Error("未知表应返回 nil")
	}
	// TableRegistry 应返回值拷贝：改返回 slice 不影响内部登记。
	if len(reg) > 0 {
		reg[0].Name = "MUTATED"
		if GetTableMeta("MUTATED") != nil {
			t.Error("TableRegistry 应返回拷贝，不应泄漏内部数组引用")
		}
	}
}

func TestCleanupTablesRegisteredAndContactScoped(t *testing.T) {
	// 关键回归护栏：contactCleanupTables 会逐个生成 DELETE FROM <t> WHERE contact_id=?，
	// 因此每一项都必须①已登记 ②确有 contact_id 列。
	for _, name := range contactCleanupTables {
		m := GetTableMeta(name)
		if m == nil {
			t.Errorf("cleanup 表 %s 未登记（GetTableMeta=nil，会被静默跳过，清理缺失）", name)
			continue
		}
		if !m.HasContactID {
			t.Errorf("cleanup 表 %s 无 contact_id 列却被纳入级联清理，将触发 no such column 回滚", name)
		}
	}
	// contact_connections 用 contact_a/contact_b 双列，绝不能在 cleanup 清单里。
	for _, name := range contactCleanupTables {
		if name == "contact_connections" {
			t.Error("contact_connections 不应出现在 contactCleanupTables（由 contact.go 专用 DELETE 处理）")
		}
	}
}

func TestIsContactScoped(t *testing.T) {
	cases := map[string]bool{
		"messages":            true,
		"followup_items":      true,
		"relationship_goals":  true,
		"contacts":            false, // 主表本体
		"contact_connections": false, // 双列
		"prompt_templates":    false, // 全局配置
		"__no_such_table__":   false, // 未登记
	}
	for name, want := range cases {
		if got := IsContactScoped(name); got != want {
			t.Errorf("IsContactScoped(%s)=%v, 期望 %v", name, got, want)
		}
	}
}

func TestMedianSeconds(t *testing.T) {
	if got := medianSeconds(nil); got != 0 {
		t.Errorf("空样本应为 0, got %d", got)
	}
	if got := medianSeconds([]int64{300, 100, 200}); got != 200 { // 排序 100,200,300 → 中位 200
		t.Errorf("奇数中位应 200, got %d", got)
	}
	if got := medianSeconds([]int64{10, 40, 20, 30}); got != 25 { // 排序 10,20,30,40 → (20+30)/2=25
		t.Errorf("偶数中位应 25, got %d", got)
	}
}

// seedMsg 用 SaveMessages 落一条内容唯一（防 msg_hash 去重）的消息。
func seedMsg(t *testing.T, db *sql.DB, id int64, sender string, at time.Time, tag string) {
	t.Helper()
	if _, err := SaveMessages(db, id, []Message{{Sender: sender, Content: "c-" + tag, Timestamp: at}}); err != nil {
		t.Fatal(err)
	}
}

func TestGetAggregatedMetricsHourHistLatency(t *testing.T) {
	db := regressionAssistantDB(t)
	now := time.Now()
	id := regressionContact(t, db, "指标-时段")

	// 3 天，每天对方 10:00 发言、我方 10:05 回复（间隔恒 300s）。
	for i := 1; i <= 3; i++ {
		d := now.AddDate(0, 0, -i)
		other := time.Date(d.Year(), d.Month(), d.Day(), 10, 0, 0, 0, now.Location())
		me := other.Add(5 * time.Minute)
		seedMsg(t, db, id, "other", other, "o"+string(rune('a'+i)))
		seedMsg(t, db, id, "me", me, "m"+string(rune('a'+i)))
	}

	m, err := GetAggregatedMetrics(db, int(id), coachTimingWindowDay)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("无错时不应返回 nil")
	}
	if m.OtherCount != 3 || m.MeCount != 3 {
		t.Errorf("条数应 3/3, got %d/%d", m.OtherCount, m.MeCount)
	}
	if m.OtherHourHist[10] != 3 {
		t.Errorf("对方 10 点直方应 3, got %d", m.OtherHourHist[10])
	}
	if m.MeHourHist[10] != 3 {
		t.Errorf("我方 10 点直方应 3, got %d", m.MeHourHist[10])
	}
	if m.ReplyLatencyP50 != 300 {
		t.Errorf("回复延迟中位应 300 秒, got %d", m.ReplyLatencyP50)
	}
}

func TestGetAggregatedMetricsArchiveAware(t *testing.T) {
	db := regressionAssistantDB(t)
	now := time.Now()
	id := regressionContact(t, db, "指标-归档")

	// 确保归档表存在（regressionAssistantDB 不预建它；缺表时 GetAggregatedMetrics 本就会降级）。
	if err := ensureArchiveTables(db); err != nil {
		t.Fatal(err)
	}
	seedMsg(t, db, id, "other", now.Add(-time.Hour), "live")
	// 直接落一条归档消息（列与 messages 同构），验证统计把 messages_archive 并入。
	if _, err := db.Exec(
		`INSERT INTO messages_archive (contact_id, sender, content, msg_hash, msg_time, msg_unix)
		 VALUES (?, 'other', '归档内容', 'h-arch-1', ?, ?)`,
		id, now.Add(-48*time.Hour), now.Add(-48*time.Hour).Unix(),
	); err != nil {
		t.Fatal(err)
	}

	m, err := GetAggregatedMetrics(db, int(id), coachTimingWindowDay)
	if err != nil {
		t.Fatal(err)
	}
	if m.OtherCount != 2 {
		t.Errorf("应计入归档那一条, OtherCount 期望 2, got %d", m.OtherCount)
	}
}

func TestGetAggregatedMetricsWindowBoundary(t *testing.T) {
	db := regressionAssistantDB(t)
	now := time.Now()
	id := regressionContact(t, db, "指标-窗口")

	seedMsg(t, db, id, "other", now.AddDate(0, 0, -2), "recent")    // 窗口内
	seedMsg(t, db, id, "other", now.AddDate(0, 0, -400), "longago") // 窗口外（180 天）

	m, err := GetAggregatedMetrics(db, int(id), coachTimingWindowDay)
	if err != nil {
		t.Fatal(err)
	}
	if m.OtherCount != 1 {
		t.Errorf("窗口外应被排除, OtherCount 期望 1, got %d", m.OtherCount)
	}
}

func TestGetAggregatedMetricsEmpty(t *testing.T) {
	db := regressionAssistantDB(t)
	id := regressionContact(t, db, "指标-空")

	m, err := GetAggregatedMetrics(db, int(id), coachTimingWindowDay)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("空数据也应返回非 nil（诚实降级，调用方不必判空指针）")
	}
	if len(m.OtherHourHist) != 0 || m.OtherCount != 0 || m.ReplyLatencyP50 != 0 {
		t.Errorf("空数据应全零, got %+v", m)
	}
}

// ---- 活体扫描（真实 HTTP）：验证 metrics 计数自愈 + /api/system/table-registry 新端点 ----

// liveDo 向真实 mux 发一个带 Bearer 鉴权的 GET，返回状态码与响应体。
func liveDo(t *testing.T, url, token, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s 请求失败: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestZZLiveMetricsSelfHealAndRegistry 验证：刚升级、metrics 表为空时，/api/status 与
// /api/system/data-report 会自愈重建日聚合再计数（而非报 0），并验证 table-registry 可达。
func TestZZLiveMetricsSelfHealAndRegistry(t *testing.T) {
	db := regressionDB(t)
	for _, fn := range []func(*sql.DB) error{ensureAssistantTables, ensureArchiveTables} {
		if err := fn(db); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &Config{APIToken: "dl-token"}
	s := &apiServer{
		db:       db,
		cfg:      cfg,
		llm:      NewLLMClient(cfg),
		sessions: &webSessionStore{sessions: map[string]time.Time{}},
		guard:    newSecurityGuard(),
		ingestRL: newRateLimiter(1000, time.Minute),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", s.route)
	ts := httptest.NewServer(withSecurity(mux, s.guard, cfg.TrustedProxies))
	defer ts.Close()

	// 种消息但故意不重建日聚合：模拟刚升级、metrics 表为空的库。
	id := regressionContact(t, db, "自愈活体")
	for i := 0; i < 5; i++ {
		if _, err := SaveMessages(db, id, []Message{{Sender: "other", Content: fmt.Sprintf("自愈活体消息%d", i), Timestamp: time.Now().AddDate(0, 0, -i)}}); err != nil {
			t.Fatal(err)
		}
	}
	var mc int
	if err := db.QueryRow(`SELECT COUNT(*) FROM relationship_daily_metrics`).Scan(&mc); err != nil {
		t.Fatal(err)
	}
	if mc != 0 {
		t.Fatalf("测试前提：metrics 应为空, got %d", mc)
	}

	// /api/status 应自愈重建 → messages > 0。
	code, body := liveDo(t, ts.URL, "dl-token", "/api/status")
	if code != http.StatusOK {
		t.Fatalf("status → %d: %s", code, body)
	}
	var st struct {
		Messages int64 `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("status 解析失败: %v body=%.160s", err, body)
	}
	if st.Messages == 0 {
		t.Errorf("status 应自愈计数 >0（刚升级 metrics 空），实得 0；body=%.160s", body)
	}

	// /api/system/data-report 同理自愈。
	code, body = liveDo(t, ts.URL, "dl-token", "/api/system/data-report")
	if code != http.StatusOK {
		t.Fatalf("data-report → %d: %s", code, body)
	}
	var rep struct {
		MessagesTotal int64 `json:"messagesTotal"`
	}
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("data-report 解析失败: %v", err)
	}
	if rep.MessagesTotal == 0 {
		t.Errorf("data-report messagesTotal 应自愈 >0, 实得 0")
	}

	// /api/system/table-registry：新端点须 200 且返回含核心表的 JSON 数组。
	code, body = liveDo(t, ts.URL, "dl-token", "/api/system/table-registry")
	if code != http.StatusOK {
		t.Fatalf("table-registry → %d: %s", code, body)
	}
	var reg []TableMeta
	if err := json.Unmarshal([]byte(body), &reg); err != nil {
		t.Fatalf("table-registry 非合法 TableMeta 数组: %v body=%.120s", err, body)
	}
	names := map[string]bool{}
	for _, m := range reg {
		names[m.Name] = true
	}
	for _, need := range []string{"messages", "relationship_daily_metrics", "contact_connections"} {
		if !names[need] {
			t.Errorf("table-registry 应含 %s", need)
		}
	}
}
