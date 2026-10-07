package main

// 投产前审计修复的回归测试（批次 A：C9 去重前置丢消息 / C1 会话过期死端 / C2 panic 无隔离）。
// 全部离线：iLink 用 httptest 假服务端，不碰真实网络与凭据。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeILinkServer 返回一个只读 getupdates 的假 iLink 服务端；响应可注入，并记录收到的游标。
func fakeILinkServer(t *testing.T, resp string, gotCursor *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if buf, ok := body["get_updates_buf"].(string); ok && gotCursor != nil {
			*gotCursor = buf
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
}

func newTestILinkClient(t *testing.T, baseURL string) *ILinkClient {
	t.Helper()
	c := NewILinkClient(filepath.Join(t.TempDir(), "ilink_credentials.json"))
	c.baseURL = baseURL
	t.Cleanup(c.Shutdown)
	return c
}

// A1：游标必须是「后置提交」——GetUpdates 只记待提交，CommitCursor 才推进。
// 这是不丢消息的核心：处理失败/崩溃时旧游标仍在，服务端会重放。
func TestCursorCommittedOnlyAfterCommit(t *testing.T) {
	var seen string
	srv := fakeILinkServer(t, `{"ret":0,"get_updates_buf":"B1","msgs":[]}`, &seen)
	defer srv.Close()
	c := newTestILinkClient(t, srv.URL)

	if _, err := c.GetUpdates(); err != nil {
		t.Fatalf("GetUpdates 失败: %v", err)
	}
	if c.CommittedCursor() != "" {
		t.Fatalf("未提交前不应推进已提交游标，实得 %q", c.CommittedCursor())
	}
	if c.PendingCursor() != "B1" {
		t.Fatalf("待提交游标应为 B1，实得 %q", c.PendingCursor())
	}
	if err := c.CommitCursor(); err != nil {
		t.Fatalf("提交游标失败: %v", err)
	}
	if c.CommittedCursor() != "B1" || c.PendingCursor() != "" {
		t.Fatalf("提交后 committed 应为 B1、pending 应清空，实得 %q / %q", c.CommittedCursor(), c.PendingCursor())
	}
	// 下次请求必须带上已提交游标（服务端据此只给其后的消息）
	if _, err := c.GetUpdates(); err != nil {
		t.Fatalf("第二次 GetUpdates 失败: %v", err)
	}
	if seen != "B1" {
		t.Fatalf("第二次请求应带游标 B1，实得 %q", seen)
	}
	// 空提交是安全空操作（无待提交游标时不得落盘、不得推进）
	if err := (&ILinkClient{}).CommitCursor(); err != nil {
		t.Fatalf("无待提交游标时 CommitCursor 应为空操作，实得错误: %v", err)
	}
}

// A2：游标必须落盘并能读回——否则重启后是空游标，按协议会重放整段历史。
func TestCursorPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cred := filepath.Join(dir, "ilink_credentials.json")

	srv := fakeILinkServer(t, `{"ret":0,"get_updates_buf":"C42","msgs":[]}`, nil)
	defer srv.Close()

	c1 := NewILinkClient(cred)
	c1.baseURL = srv.URL
	if _, err := c1.GetUpdates(); err != nil {
		t.Fatal(err)
	}
	if err := c1.CommitCursor(); err != nil {
		t.Fatal(err)
	}
	c1.Shutdown()

	// 模拟重启：新客户端读回落盘游标
	c2 := NewILinkClient(cred)
	defer c2.Shutdown()
	c2.baseURL = srv.URL
	c2.LoadCursor()
	if c2.CommittedCursor() != "C42" {
		t.Fatalf("重启后应恢复游标 C42，实得 %q（丢失即意味着历史重放或消息漏收）", c2.CommittedCursor())
	}
}

// A3：已处理滑窗只在处理成功后生效，绝不前置标记（前置标记=处理失败即永久丢消息）。
func TestMarkProcessedOnlyAfterSuccess(t *testing.T) {
	c := newTestILinkClient(t, "http://127.0.0.1:1")
	defer c.Shutdown()
	key := "12345"
	if c.AlreadyProcessed(key) {
		t.Fatal("未经处理的 key 不应被视为已处理")
	}
	c.MarkProcessed(key)
	if !c.AlreadyProcessed(key) {
		t.Fatal("MarkProcessed 后应命中滑窗")
	}
	c.MarkProcessed("") // 空 key 应安全忽略
	if c.AlreadyProcessed("") {
		t.Fatal("空 key 永远不应算已处理")
	}
}

// A4：messageKey 兜底逻辑。
func TestMessageKeyFallback(t *testing.T) {
	if got := messageKey(&ILinkMessage{MessageID: json.Number("77"), FromUserID: "u@im.wechat"}); got != "77" {
		t.Fatalf("有 message_id 应直接用之，实得 %q", got)
	}
	if got := messageKey(&ILinkMessage{MessageID: json.Number("0"), FromUserID: "u@im.wechat", CreateTimeMs: 900}); got != "u@im.wechat-900" {
		t.Fatalf("message_id=0 应退回 from+时间戳，实得 %q", got)
	}
	if got := messageKey(nil); got != "" {
		t.Fatalf("nil 消息应返回空串，实得 %q", got)
	}
}

// A5：账本是重放安全网——done 跳过、失败可重试、达上限判毒丸。
func TestIngestLedgerIdempotencyAndPoison(t *testing.T) {
	db := regressionDB(t)
	if err := ensureIngestLedger(db); err != nil {
		t.Fatal(err)
	}
	key := "msg-1"

	if p, _ := ingestShouldProcess(db, key); !p {
		t.Fatal("全新 key 应需处理")
	}
	if err := ingestMarkDone(db, key, "u1"); err != nil {
		t.Fatal(err)
	}
	if p, _ := ingestShouldProcess(db, key); p {
		t.Fatal("done 的 key 重放时必须跳过，否则会重复入库")
	}

	// 失败累加：前两次仍可重试，第三次判毒丸
	k2 := "msg-bad"
	for i := 1; i <= 3; i++ {
		n, err := ingestMarkFailed(db, k2, "u1", "handler panic")
		if err != nil {
			t.Fatalf("第 %d 次标记失败: %v", i, err)
		}
		if n != i {
			t.Fatalf("attempts 应累加为 %d，实得 %d", i, n)
		}
	}
	if p, _ := ingestShouldProcess(db, k2); p {
		t.Fatalf("达到重试上限(%d)后不应再处理，否则坏消息会无限重放", ingestMaxAttempts)
	}
	if !ingestPoisoned(db, k2) {
		t.Fatal("该 key 应被判定为毒丸")
	}
	if ingestPoisoned(db, "不存在") {
		t.Fatal("未知 key 不应被判毒丸")
	}

	// done 之后再次标记失败应能覆盖状态（同一 key 语义变化时不留下陈旧 done）
	if _, err := ingestMarkFailed(db, key, "u1", "重放后又失败"); err != nil {
		t.Fatal(err)
	}

	// 空 key / 无稳定键：宁可照常处理也不丢消息
	if p, _ := ingestShouldProcess(db, ""); !p {
		t.Fatal("空 key 应照常处理")
	}
	if err := ingestMarkDone(db, "", "u1"); err != nil {
		t.Fatalf("空 key 标记应安全无操作: %v", err)
	}
}

// A6：账本会过期清理，不随运行年限无限增长。
func TestIngestPruneRemovesOldRows(t *testing.T) {
	db := regressionDB(t)
	if err := ensureIngestLedger(db); err != nil {
		t.Fatal(err)
	}
	if err := ingestMarkDone(db, "fresh", "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ingest_ledger(msg_key, from_user, status, attempts, updated_at)
		VALUES('ancient','u1','done',1, datetime('now','-30 days'))`); err != nil {
		t.Fatal(err)
	}
	n, err := ingestPrune(db)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应只清理 30 天前的 1 行，实得 %d", n)
	}
	var left int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ingest_ledger WHERE msg_key='fresh'`).Scan(&left); err != nil || left != 1 {
		t.Fatalf("新行不应被清理，实得 left=%d err=%v", left, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM ingest_ledger WHERE msg_key='ancient'`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("30 天前的行应被清掉，实得 left=%d err=%v", left, err)
	}
}

// A7：panic 隔离——单条消息处理内部崩溃不得带走整个进程。
func TestSafeHandleMessageRecoversPanic(t *testing.T) {
	// 故意构造会 panic 的 bot：client 为 nil，status() 里解引用即崩
	b := NewBot(nil, nil, nil, &Config{})
	reply, ok := b.SafeHandleMessage(&ILinkMessage{
		FromUserID: "u1",
		ItemList:   []ILinkItem{{Type: 1, Text: &ILinkText{Text: "状态"}}},
	})
	if ok {
		t.Fatal("panic 应被判定为处理失败（调用方据此不提交游标，交给重放）")
	}
	if reply == "" {
		t.Fatal("失败时应给用户一句可读的兜底回复")
	}
	// 走到这里没崩，说明 recover 生效：进程仍存活
}

// A8：重绑 API 的契约与降级（未初始化客户端不 panic；未知子路径 404；方法限制 405）。
func TestWechatRebindAPIContract(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}},
		client: newTestILinkClient(t, "http://127.0.0.1:1")}

	w := callAPI(s, http.MethodGet, "/api/wechat/bind", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/wechat/bind 应 200，实得 %d body=%s", w.Code, w.Body.String())
	}
	var snap map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("响应应是合法 JSON: %v", err)
	}
	if snap["status"] != "idle" {
		t.Fatalf("初始状态应为 idle，实得 %v", snap["status"])
	}
	if _, ok := snap["session_expired"]; !ok {
		t.Fatal("快照必须包含 session_expired，网页据此提示需重新扫码")
	}

	if code := callAPI(s, http.MethodGet, "/api/wechat/rebind", "").Code; code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/wechat/rebind 应 405，实得 %d", code)
	}
	if code := callAPI(s, http.MethodGet, "/api/wechat/unknown", "").Code; code != http.StatusNotFound {
		t.Fatalf("未知子路径应 404，实得 %d", code)
	}
}

// A9：AllowProbe 能让「会话过期」不再成为死端——清位后仍可继续发请求。
func TestAllowProbeClearsStickyExpiry(t *testing.T) {
	c := newTestILinkClient(t, "http://127.0.0.1:1")
	defer c.Shutdown()
	c.mu.Lock()
	c.sessionExpired = true
	c.mu.Unlock()
	if !c.SessionExpired() {
		t.Fatal("置位后应为已过期")
	}
	c.AllowProbe()
	if c.SessionExpired() {
		t.Fatal("AllowProbe 后应清位，使主循环还能再探测一次（否则重登消息永远收不到）")
	}
}

// A10：ClearCredentials 必须真删凭据文件——只清内存的话重启仍会跳过扫码、再次 -14。
func TestClearCredentialsRemovesFiles(t *testing.T) {
	dir := t.TempDir()
	cred := filepath.Join(dir, "ilink_credentials.json")
	c := NewILinkClient(cred)
	defer c.Shutdown()
	c.mu.Lock()
	c.botToken, c.botID, c.userID, c.cursor = "tok", "bot", "user", "C1"
	c.mu.Unlock()
	if err := c.SaveCredentials(); err != nil {
		t.Fatal(err)
	}
	if !c.IsLoggedIn() {
		t.Fatal("前置条件：应已登录")
	}
	if err := c.ClearCredentials(); err != nil {
		t.Fatalf("清理凭据失败: %v", err)
	}
	if c.IsLoggedIn() {
		t.Fatal("内存会话应被清空")
	}
	if _, err := os.Stat(cred); !os.IsNotExist(err) {
		t.Fatalf("凭据文件必须真删（否则重启仍跳过扫码、再次 -14），实得 err=%v", err)
	}
	// 重绑后必须从头拉：旧会话的游标对新会话无意义
	if c.CommittedCursor() != "" {
		t.Fatalf("清理后游标应归零，实得 %q", c.CommittedCursor())
	}
	// 重复清理应安全（文件已不存在不算错）
	if err := c.ClearCredentials(); err != nil {
		t.Fatalf("重复清理应幂等，实得: %v", err)
	}
}
