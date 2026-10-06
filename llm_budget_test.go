package main

// v6.3 P2c 日预算护栏验收。
//
// 钉死六件事：
//  1. 交互式标记的读写语义（含 nil ctx 安全、派生 ctx 继承）；
//  2. 超预算只拦后台批量：后台调用被拒且**根本不该触达模型**，交互式调用照常成功；
//  3. 预算不改变缓存语义：命中零成本，超预算照样能取回已算好的结果；
//  4. 预算快照的算术与「未设=不限」「负数归零」的口径；
//  5. 设置预算走 load→modify→save，不得把已有档案/密钥顺手清掉；缺字段要报 400；
//  6. 真接口层：用户点的情绪分析接口（走完整 s.route）在超预算时仍应触达模型，
//     而后台待跟进/开场白被拒时是普通错误 + 既有降级，不伪装成服务不可用。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInteractiveFlagSemantics(t *testing.T) {
	if isInteractiveCall(context.Background()) {
		t.Fatal("裸 ctx 应视为后台批量（可被预算拦）")
	}
	if !isInteractiveCall(withInteractiveCall(context.Background())) {
		t.Fatal("打了标记应判为交互式")
	}
	if !isInteractiveCall(withInteractiveCall(nil)) {
		t.Fatal("nil ctx 打标记必须安全，不得 panic")
	}
	if isInteractiveCall(nil) {
		t.Fatal("nil ctx 应安全返回 false")
	}
	// 标记应能随派生 ctx 传递（真实代码里都是 WithTimeout 派生）
	base := withInteractiveCall(context.Background())
	derived, cancel := context.WithTimeout(base, time.Second)
	defer cancel()
	if !isInteractiveCall(derived) {
		t.Fatal("派生 ctx 应继承交互式标记")
	}
	ctx2, cancel2 := newInteractiveCtx(time.Second)
	defer cancel2()
	if !isInteractiveCall(ctx2) {
		t.Fatal("newInteractiveCtx 应带交互式标记")
	}
}

// budgetOverDB 造一个「今日已用 1200、上限 1000」的库（已用尽）。
func budgetOverDB(t *testing.T) (*sql.DB, *LLMClient) {
	t.Helper()
	db := regressionDB(t)
	c := NewLLMClient(&Config{}).WithDB(db)
	c.logLLMCall(llmSpec{Model: "m"}, "", true, 200, llmUsage{Total: 1200}, 1)
	if err := saveLLMSettings(db, LLMSettings{DailyTokenBudget: 1000}); err != nil {
		t.Fatalf("设预算失败: %v", err)
	}
	return db, c
}

func TestBudgetBlocksBackgroundButNotInteractive(t *testing.T) {
	db, _ := budgetOverDB(t)
	server, calls := stubLLMServer(t, "结果")
	client := NewLLMClient(&Config{}).WithDB(db)
	client.apiKey, client.baseURL, client.model = "k", server.URL, "stub"

	id := regressionContact(t, db, "预算拦截")

	// 后台批量：应被拒，且模型一次都不该被触达（拒得干净，不产生任何消耗）
	_, err := callLLMCached(context.Background(), db, client, id, TaskTopic, "", "P-bg")
	if !errors.Is(err, errDailyBudgetExceeded) {
		t.Fatalf("后台调用超预算应被拒，实得 %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("被预算拒绝的调用不应触达模型，实得 calls=%d", calls.Load())
	}

	// 交互式：同一预算线下必须照常成功——预算的设计前提是不误伤用户操作
	out, err := callLLMCached(withInteractiveCall(context.Background()), db, client, id, TaskTopic, "", "P-ig")
	if err != nil {
		t.Fatalf("交互式调用不该被预算拦，实得 %v", err)
	}
	if out == "" || calls.Load() != 1 {
		t.Fatalf("交互式调用应真调一次并拿到结果，实得 out=%q calls=%d", out, calls.Load())
	}
}

func TestBudgetNeverBlocksCacheHits(t *testing.T) {
	db, _ := budgetOverDB(t)
	client := NewLLMClient(&Config{}).WithDB(db)
	client.apiKey, client.baseURL, client.model = "k", "https://example.invalid/v1", "stub"

	// 先按同一语义键把结果塞进缓存（等价于「上次已经算过」）
	const prompt = "P-warm"
	key := aiCacheKey{ContactID: 7, Task: string(TaskTopic), ContextVersion: "n/a", Model: client.modelOf(), PromptVersion: shortHash(prompt)}
	aiCachePut(db, key, "上次算好的结果")

	got, err := callLLMCached(context.Background(), db, client, 7, TaskTopic, "", prompt)
	if err != nil {
		t.Fatalf("缓存命中零成本，超预算也应放行，实得 %v", err)
	}
	if got != "上次算好的结果" {
		t.Fatalf("应取回缓存值，实得 %q", got)
	}
}

func TestBudgetStatusMathAndDefaults(t *testing.T) {
	db := regressionDB(t)

	// 未设预算：不限制，且后台调用照常允许
	st := llmBudgetStatus(db)
	if !st.Unlimited || st.Over || st.Limit != 0 {
		t.Fatalf("默认应为不限制，实得 %+v", st)
	}
	if !llmBudgetAllows(context.Background(), db) {
		t.Fatal("未设预算时后台调用应被允许")
	}

	// 有余额：remaining = limit - used
	if err := saveLLMSettings(db, LLMSettings{DailyTokenBudget: 5000}); err != nil {
		t.Fatal(err)
	}
	st = llmBudgetStatus(db)
	if st.Unlimited || st.Over || st.Limit != 5000 {
		t.Fatalf("应进入限额模式，实得 %+v", st)
	}
	if want := int64(5000) - st.UsedToday; st.Remaining != want {
		t.Fatalf("剩余应为 %d，实得 %d", want, st.Remaining)
	}

	// 用尽：Over 置位、剩余归零、后台被拦
	c := NewLLMClient(&Config{}).WithDB(db)
	c.logLLMCall(llmSpec{Model: "m"}, "", true, 200, llmUsage{Total: 9000}, 1)
	st = llmBudgetStatus(db)
	if !st.Over || st.Remaining != 0 {
		t.Fatalf("超线应 Over 且剩余归零，实得 %+v", st)
	}
	if llmBudgetAllows(context.Background(), db) {
		t.Fatal("超线后后台调用应被拒")
	}
	if !llmBudgetAllows(withInteractiveCall(context.Background()), db) {
		t.Fatal("超线后交互式调用仍应被允许")
	}

	// 负数按不限制处理（normalize 归零），不引入第三种语义
	if err := saveLLMSettings(db, LLMSettings{DailyTokenBudget: -5}); err != nil {
		t.Fatal(err)
	}
	if got := dailyTokenBudgetLimit(db); got != 0 {
		t.Fatalf("负数上限应归零为不限制，实得 %d", got)
	}
	if !llmBudgetStatus(db).Unlimited {
		t.Fatal("归零后状态应为不限制")
	}
}

func TestBudgetAPIRoundTripPreservesOtherSettings(t *testing.T) {
	db := regressionDB(t)
	// 先存一份带密钥的档案，验证设预算不会把它清掉
	seed := LLMSettings{
		ActiveProfileID: "p1",
		Profiles:        []LLMProfile{{ID: "p1", Label: "主力", BaseURL: "https://api.example.com/v1", APIKey: "secret-key", Model: "m1"}},
	}
	if err := saveLLMSettings(db, seed); err != nil {
		t.Fatal(err)
	}

	call := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/llm/budget", strings.NewReader(body))
		hLLMSetBudget(w, r, db)
		return w
	}

	w := call(`{"dailyTokenBudget":3000}`)
	if w.Code != http.StatusOK {
		t.Fatalf("设置预算应 200，实得 %d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		OK     bool            `json:"ok"`
		Budget LLMBudgetStatus `json:"budget"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Budget.Limit != 3000 || out.Budget.Unlimited {
		t.Fatalf("响应应回带预算快照，实得 %+v", out.Budget)
	}

	got, err := loadLLMSettings(db)
	if err != nil {
		t.Fatal(err)
	}
	if got.DailyTokenBudget != 3000 {
		t.Fatalf("库里预算应为 3000，实得 %d", got.DailyTokenBudget)
	}
	if len(got.Profiles) != 1 || got.Profiles[0].APIKey != "secret-key" || got.Profiles[0].BaseURL != "https://api.example.com/v1" {
		t.Fatalf("设预算不得动其它字段，实得 %+v", got.Profiles)
	}

	// 0 是有效动作（取消限制），不能和「没传」混为一谈
	if w := call(`{"dailyTokenBudget":0}`); w.Code != http.StatusOK {
		t.Fatalf("显式传 0 应被接受，实得 %d", w.Code)
	}
	if got, _ := loadLLMSettings(db); got.DailyTokenBudget != 0 {
		t.Fatalf("传 0 应清掉限制，实得 %d", got.DailyTokenBudget)
	}
	if w := call(`{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("缺字段应 400，实得 %d", w.Code)
	}
}

// 真接口层：用户点击的情绪分析在超预算时仍必须触达模型——
// 这条走的是完整 s.route，证明入口的交互式标记真的生效，而不只是单元里自己判自己。
func TestAPIUserActionSurvivesExhaustedBudget(t *testing.T) {
	db, _ := budgetOverDB(t)
	id := regressionContact(t, db, "点击不受拦")
	seedRecentMessages(t, db, id, "最近怎么样", "在忙项目", "改天聊", "好")
	if err := ensureAssistantTables(db); err != nil {
		t.Fatalf("建助手表: %v", err)
	}

	server, calls := stubLLMServer(t, `{"emotion":"平静","score":55,"summary":"语气偏淡","advice":"隔两天再聊","alert":false}`)
	client := NewLLMClient(&Config{}).WithDB(db)
	client.apiKey, client.baseURL, client.model = "k", server.URL, "stub"

	cfg := &Config{APIToken: "tok-budget"}
	s := &apiServer{db: db, cfg: cfg, llm: client, sessions: &webSessionStore{sessions: map[string]time.Time{}}}

	r := httptest.NewRequest(http.MethodPost, "/api/assistant/analyze-emotion", strings.NewReader(fmt.Sprintf(`{"contactId":%d}`, id)))
	r.Header.Set("Authorization", "Bearer tok-budget")
	w := httptest.NewRecorder()
	s.route(w, r)

	if calls.Load() == 0 {
		t.Fatalf("用户当场发起的调用被预算误伤：模型未被触达，响应 %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "预算") {
		t.Fatalf("用户请求不应收到预算拒绝，实得 %d %s", w.Code, w.Body.String())
	}
	// 不只要「没被预算拦」，还要整条链路真的成功（排除因为其它原因中途失败而假阴性）
	if w.Code != http.StatusOK {
		t.Fatalf("超预算下的用户点击应照常 200，实得 %d %s", w.Code, w.Body.String())
	}
}

// 后台批量被拒时是「普通错误 + 调用方跳过」，不是把额度用尽伪装成服务不可用。
func TestBackgroundBatchDegradesNotServiceUnavailable(t *testing.T) {
	db, _ := budgetOverDB(t)
	id := regressionContact(t, db, "后台被拦")
	seedRecentMessages(t, db, id, "帮我看看这个", "回头给你答复", "好的", "方案发我", "周一前")

	server, calls := stubLLMServer(t, `{"items":[]}`)
	client := NewLLMClient(&Config{}).WithDB(db)
	client.apiKey, client.baseURL, client.model = "k", server.URL, "stub"

	// 模拟调度器：裸 ctx 逐联系人抽取
	_, err := extractFollowups(context.Background(), db, client, id, time.Now(), 30)
	if !errors.Is(err, errDailyBudgetExceeded) {
		t.Fatalf("后台抽取超预算应被跳过，实得 %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("被跳过的后台任务不应烧额度，实得 calls=%d", calls.Load())
	}

	// 后台开场白：被拦时按既有降级返回空串，绝不向上抛 5xx 语义
	if draft := generateOutreachDraft(db, client, id, "cooling"); draft != "" {
		t.Fatalf("被预算拦下应返回空草稿（既有降级），实得 %q", draft)
	}
}
