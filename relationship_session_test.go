package main

// ═══════════════════════════════════════════════════════════════════════════
// Phase 5 验收：Relationship Session 编排层（§7）。核心承诺——不新建第二套
// decision/action/simulation，只编排已有能力；且模拟绝不冒充预测、估算绝不冒充确认。
//  1. §7.1 Before Brief 全部字段来自 Context Engine（可信/冲突事实、待办、状态…正确投影）。
//  2. §7.3 Rehearsal 结果必须显式标 SIMULATION、附「不代表真实预测」免责（铁律）。
//  3. §7.4 点执行 → 自动落 Action Ledger，source=relationship_session。
//  4. §7.5 用户反馈 5 档归一到既有结果极性；不新建第二套 outcome 真相。
//  5. §7.6 用户确认(confirmed) 后系统估算(estimated) 永不覆盖——二者绝不混淆（铁律）。
//  6. API 端到端：无 LLM 也能装配（核心页不 500/503）；不存在联系人 404。
// ═══════════════════════════════════════════════════════════════════════════

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestBuildRelationshipSessionProjectsContext(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "会话对象")
	regressionMessages(t, db, cid, "我最近换工作了", "周末一起爬山")
	// 可信事实 + 冲突事实各一，验证 §7.1 分栏投影。
	seedFact(t, db, cid, "occupation", "产品经理", 0.8, 0.6, "active", "ai", time.Now().Format(time.RFC3339), "")
	seedFact(t, db, cid, "location", "北京", 0.5, 0.5, "conflict", "ai", time.Now().Format(time.RFC3339), "")
	// 未完成事项。
	if err := ensureFollowupTables(db); err != nil {
		t.Fatal(err)
	}
	if _, err := AddFollowup(db, cid, "promise", "回给对方项目资料", "", ""); err != nil {
		t.Fatal(err)
	}

	sess, err := BuildRelationshipSession(db, cid, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if sess.Name != "会话对象" {
		t.Fatalf("联系人名字应回填，实得 %q", sess.Name)
	}
	if !hasSubstr(sess.Brief.ImportantFacts, "产品经理") {
		t.Fatalf("可信事实应投影到 important_facts，实得 %v", sess.Brief.ImportantFacts)
	}
	if !hasSubstr(sess.Brief.ConflictFacts, "北京") {
		t.Fatalf("冲突事实应投影到 conflict_facts，实得 %v", sess.Brief.ConflictFacts)
	}
	if !hasSubstr(sess.Brief.OpenItems, "回给对方项目资料") {
		t.Fatalf("待办应投影到 open_items，实得 %v", sess.Brief.OpenItems)
	}
	// §7.1 铁律：全部来自 Context Engine → ContextVersion 非空。
	if sess.ContextVersion == "" {
		t.Fatal("context_version 应非空（证明装配走 Context Engine 唯一入口）")
	}
	// §7.2 策略必须有推荐（有候选或诚实兜底），非空。
	if strings.TrimSpace(sess.Strategy.Recommended) == "" {
		t.Fatal("strategy.recommended 不应为空")
	}
}

func TestSessionRehearsalMarkedSimulation(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "预演对象")
	regressionMessages(t, db, cid, "在吗")
	sess, err := BuildRelationshipSession(db, cid, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if sess.Rehearsal.Mode != "SIMULATION" {
		t.Fatalf("§7.3 预演必须标 SIMULATION，实得 %q", sess.Rehearsal.Mode)
	}
	d := sess.Rehearsal.Disclaimer
	if d == "" || !strings.Contains(d, "SIMULATION") || !strings.Contains(d, "真实") {
		t.Fatalf("§7.3 免责必须声明是模拟、非真实预测，实得 %q", d)
	}
	// §7.6 长期观察窗口固定 7/14/30。
	if len(sess.ObservationDays) != 3 || sess.ObservationDays[0] != 7 || sess.ObservationDays[2] != 30 {
		t.Fatalf("观察窗口应为 [7,14,30]，实得 %v", sess.ObservationDays)
	}
}

func TestExecuteSessionActionWritesLedger(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "落账对象")
	id, err := ExecuteSessionAction(db, cid, "主动约对方喝咖啡聊项目", "session:"+itoa(cid), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if id <= 0 {
		t.Fatalf("应返回新行动 id，实得 %d", id)
	}
	e, err := GetActionLog(db, id)
	if err != nil {
		t.Fatal(err)
	}
	if e.Source != ActionSourceSession {
		t.Fatalf("§7.4 行动来源应为 relationship_session，实得 %q", e.Source)
	}
	if e.Status != ActionStatusGenerated {
		t.Fatalf("生命周期起点应为 generated，实得 %q", e.Status)
	}
}

func TestMapSessionOutcomeToExistingPolarity(t *testing.T) {
	cases := map[string]string{
		OutSmooth: ActionOutcomePositive, OutNormal: ActionOutcomeNeutral,
		OutPoor: ActionOutcomeNegative, OutFailed: ActionOutcomeNegative,
		OutUndecided: ActionOutcomeUnknown,
	}
	for label, want := range cases {
		got, ok := MapSessionOutcome(label)
		if !ok || got != want {
			t.Fatalf("%q 应映射 %q，实得 %q ok=%v", label, want, got, ok)
		}
	}
	if _, ok := MapSessionOutcome("瞎填的"); ok {
		t.Fatal("未知档应返回 false")
	}
}

func TestSessionOutcomeConfirmedNeverOverwrittenByEstimate(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "反馈对象")
	id, err := ExecuteSessionAction(db, cid, "介绍双方认识", "s", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// 进入 acted（估算观察只处理 acted/completed）。
	if err := TransitionActionStatus(db, id, ActionStatusActed, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	// §7.5 用户直接反馈「顺利」+ 备注 → confirmed。
	if err := RecordSessionOutcome(db, id, OutSmooth, "对方很热情，已约下次", time.Now()); err != nil {
		t.Fatal(err)
	}
	// §7.6 铁律：用户确认后，系统估算不得覆盖。
	if err := SetActionOutcome(db, id, ActionOutcomeNegative, ActionProvenanceEstimated, "系统想改写", time.Now()); err == nil {
		t.Fatal("confirmed 结果应拒绝被 estimated 覆盖")
	}
	e, _ := GetActionLog(db, id)
	if e.Outcome != ActionOutcomePositive || e.OutcomeProvenance != ActionProvenanceConfirmed {
		t.Fatalf("结果应仍是用户确认的 positive/confirmed，实得 %q/%q", e.Outcome, e.OutcomeProvenance)
	}
}

func TestRelationshipSessionAPIEndToEnd(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	cid := regressionContact(t, db, "API会话")
	regressionMessages(t, db, cid, "换了新工作")

	// GET：无 LLM 也应 200（核心页不 500/503）。
	w := callAPI(s, http.MethodGet, "/api/contacts/"+itoa(cid)+"/session", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET session 应 200，实得 %d %s", w.Code, w.Body.String())
	}
	var get struct {
		OK      bool `json:"ok"`
		Session struct {
			Rehearsal struct {
				Mode string `json:"mode"`
			} `json:"rehearsal"`
		} `json:"session"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &get); err != nil {
		t.Fatal(err)
	}
	if !get.OK || get.Session.Rehearsal.Mode != "SIMULATION" {
		t.Fatalf("响应应含 SIMULATION 标注，实得 %+v", get.Session.Rehearsal)
	}

	// POST execute → 拿到 action_log_id。
	we := callAPI(s, http.MethodPost, "/api/contacts/"+itoa(cid)+"/session/execute", `{"action_text":"约见面","source_ref":"api"}`)
	if we.Code != http.StatusOK {
		t.Fatalf("execute 应 200，实得 %d %s", we.Code, we.Body.String())
	}
	var exec struct {
		OK        bool  `json:"ok"`
		ActionLog int64 `json:"action_log_id"`
	}
	json.Unmarshal(we.Body.Bytes(), &exec)
	if !exec.OK || exec.ActionLog <= 0 {
		t.Fatalf("execute 应返回正 id，实得 %+v", exec)
	}

	// POST outcome → confirmed。
	wo := callAPI(s, http.MethodPost, "/api/contacts/"+itoa(cid)+"/session/outcome", `{"action_log_id":`+itoa(exec.ActionLog)+`,"label":"smooth","note":"顺利"}`)
	if wo.Code != http.StatusOK {
		t.Fatalf("outcome 应 200，实得 %d %s", wo.Code, wo.Body.String())
	}
	var oc struct {
		OK         bool   `json:"ok"`
		Outcome    string `json:"outcome"`
		Provenance string `json:"provenance"`
	}
	json.Unmarshal(wo.Body.Bytes(), &oc)
	if !oc.OK || oc.Outcome != ActionOutcomePositive || oc.Provenance != ActionProvenanceConfirmed {
		t.Fatalf("outcome 应 positive/confirmed，实得 %+v", oc)
	}

	// 非法档 → 400（非 500）。
	if wBad := callAPI(s, http.MethodPost, "/api/contacts/"+itoa(cid)+"/session/outcome", `{"action_log_id":`+itoa(exec.ActionLog)+`,"label":"乱填"}`); wBad.Code != http.StatusBadRequest {
		t.Fatalf("非法反馈档应 400，实得 %d", wBad.Code)
	}
	// 不存在联系人 → 404。
	if wNF := callAPI(s, http.MethodGet, "/api/contacts/999999/session", ""); wNF.Code != http.StatusNotFound {
		t.Fatalf("不存在联系人应 404，实得 %d", wNF.Code)
	}
}

// hasSubstr 判断切片里是否有任一字符串包含 sub。
func hasSubstr(list []string, sub string) bool {
	for _, v := range list {
		if strings.Contains(v, sub) {
			return true
		}
	}
	return false
}
