package main

// ═══════════════════════════════════════════════════════════════════════════
// Phase 3 验收：Memory / Evidence 3.0（§10 Memory Maintenance + §11 Evidence Chain）。
//
// 钉死这些「智能层级提升」的承诺，而非只测 happy path：
//  1. §11.2 证据评分综合六因子，且铁律「纯关键词命中不得评 high/medium」成立；
//     用户确认封顶、反向证据降档、无证据判 insufficient。
//  2. §11 证据链八问：谁说的 / 何时 / 原文 / 为何支持 / 等级 / 反向证据 / 第一人称 / 是否仍有效，
//     全部确定性地从既有证据行装配，只读、无 LLM、事实不存在返回 404 不 500。
//  3. §10.3 Temporal Memory：同槽位当前值 + 被取代历史按生效先后串成时间线，当前值恒在末位。
//  4. §10.2 merge：把近重复合并到 canonical——迁移证据、其余标 superseded，绝不删除（溯源不断）。
//  5. §10.1 新增检测：low_evidence / never_confirmed 出现在提案里。
// ═══════════════════════════════════════════════════════════════════════════

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// seedEvidenceRow 直接插一条证据行（测试用，绕开 LLM/检索）。
func seedEvidenceRow(t *testing.T, db *sql.DB, factID, contactID, msgID int64, evType, matchType, quote, msgTime string) {
	t.Helper()
	direct := 0
	if evType == EvDirect {
		direct = 1
	}
	ss := 0.4
	if matchType == "exact" {
		ss = 0.8
	}
	_, err := db.Exec(`INSERT INTO profile_fact_evidence
		(fact_id, contact_id, message_id, archived, snippet, msg_time, match_type, support_strength, quote, is_direct_support, evidence_type)
		VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?)`,
		factID, contactID, msgID, quote, msgTime, matchType, ss, quote, direct, evType)
	if err != nil {
		t.Fatalf("seed evidence: %v", err)
	}
}

func evView(typ, match, quote, msgTime string) FactEvidenceView {
	return FactEvidenceView{EvidenceType: typ, MatchType: match, Quote: quote, Snippet: quote, MsgTime: msgTime}
}

func TestAssessEvidencePureKeywordNeverHigh(t *testing.T) {
	now := time.Now()
	// 全部 topic_related / weak_context：铁律要求不得 high/medium。
	kw := []FactEvidenceView{
		evView(EvTopicRelated, "contextual", "他最近在弄律师的事", now.Add(-10*time.Hour).Format(time.RFC3339)),
		evView(EvWeakContext, "contextual", "法律", now.Add(-10*time.Hour).Format(time.RFC3339)),
	}
	a := AssessEvidence("occupation", "ai", "active", kw, "", nil, now)
	if a.Directness != 0 {
		t.Fatalf("纯关键词 directness 应为 0，实得 %v", a.Directness)
	}
	if a.Grade == GradeHigh || a.Grade == GradeMedium {
		t.Fatalf("§11.2 铁律：纯关键词不得评 high/medium，实得 %+v", a)
	}
	if a.Score >= 0.7 {
		t.Fatalf("纯关键词分数不得达 high 阈值，实得 %v", a.Score)
	}
}

func TestAssessEvidenceDirectFirstPersonHigh(t *testing.T) {
	now := time.Now()
	recent := now.Add(-2 * time.Hour).Format(time.RFC3339)
	evs := []FactEvidenceView{
		evView(EvDirect, "exact", "我是律师，在深圳", recent),
		evView(EvDirect, "exact", "我做律师好几年了", recent),
	}
	a := AssessEvidence("occupation", "ai", "active", evs, "", nil, now)
	if a.Grade != GradeHigh {
		t.Fatalf("近期多条第一人称精确直接证据应评 high，实得 %+v", a)
	}
	if a.Directness != 1 || a.Consistency <= 0.5 {
		t.Fatalf("directness 应=1、consistency 应随条数上升，实得 %+v", a)
	}
}

func TestAssessEvidenceConflictDowngrades(t *testing.T) {
	now := time.Now()
	recent := now.Add(-time.Hour).Format(time.RFC3339)
	evs := []FactEvidenceView{evView(EvDirect, "exact", "我是律师", recent)}
	conflict := []FactEvidenceView{evView(EvConflict, "contextual", "我辞职创业了", recent)}
	a := AssessEvidence("occupation", "ai", "active", evs, "", conflict, now)
	if a.Grade != GradeConflicted {
		t.Fatalf("存在第一人称反向证据应降为 conflicted，实得 %+v", a)
	}
	if a.ConflictPenalty < 0.5 {
		t.Fatalf("第一人称冲突惩罚应 ≥0.5，实得 %v", a.ConflictPenalty)
	}
}

func TestAssessEvidenceUserConfirmedCeils(t *testing.T) {
	a := AssessEvidence("occupation", "user", "confirmed", nil, "", nil, time.Now())
	// 无支撑证据但用户确认：score 仍应封顶（不被 insufficient 逻辑拉低）。
	if !a.UserConfirmed || a.Score < 0.9 {
		t.Fatalf("用户确认应封顶高可信，实得 %+v", a)
	}
}

func TestAssembleChainEightQuestions(t *testing.T) {
	now := time.Now()
	recent := now.Add(-3 * time.Hour).Format(time.RFC3339)
	all := []FactEvidenceView{
		evView(EvDirect, "exact", "我在深圳做律师", recent),
		evView(EvConflict, "contextual", "他早就不做律师了", recent),
	}
	c := assembleChain(7, 99, "occupation", "", "律师", "active", "ai", "", all, now)
	if c.FactValue != "律师" || c.ContactID != 7 || c.FactID != 99 {
		t.Fatalf("基本字段应回填，实得 %+v", c)
	}
	if !c.FirstPerson {
		t.Fatal("含第一人称直接证据应标 first_person")
	}
	if c.WhoSaid != "本人（第一人称）" {
		t.Fatalf("第一人称应判『本人』，实得 %q", c.WhoSaid)
	}
	if len(c.OriginalTexts) != 1 || len(c.CounterEvidence) != 1 {
		t.Fatalf("原文/反向证据应各 1 条，实得 sup=%d counter=%d", len(c.OriginalTexts), len(c.CounterEvidence))
	}
	if !c.StillValid {
		t.Fatal("active 且 valid_until 空应判仍有效")
	}
	if c.When != recent {
		t.Fatalf("when 应为最近支撑证据时间，实得 %q", c.When)
	}
	if !strings.Contains(c.WhySupports, "直接") {
		t.Fatalf("why_supports 应描述直接支撑，实得 %q", c.WhySupports)
	}
}

func TestBuildEvidenceChainEndToEnd(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "链主")
	fid := seedFact(t, db, cid, "occupation", "律师", 0.7, 0.5, "active", "ai", time.Now().Format(time.RFC3339), "")
	recent := time.Now().Add(-time.Hour).Format(time.RFC3339)
	seedEvidenceRow(t, db, fid, cid, 1001, EvDirect, "exact", "我是律师", recent)

	chain, err := BuildEvidenceChain(db, cid, fid, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if chain.FactValue != "律师" || len(chain.OriginalTexts) != 1 {
		t.Fatalf("应装配出 1 条原文，实得 %+v", chain)
	}
	// 事实不存在 → ErrNoRows（调用方 404，不 500）。
	if _, err := BuildEvidenceChain(db, cid, 999999, time.Now()); err != sql.ErrNoRows {
		t.Fatalf("不存在事实应返回 sql.ErrNoRows，实得 %v", err)
	}
}

func TestBuildFactTimeline(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "职业史")
	// 旧值 superseded（销售 2023），新值 active（创业者 2025-now）。
	old := seedFact(t, db, cid, "occupation", "销售", 0.7, 0.5, "superseded", "ai", "2023-01-01T00:00:00Z", "")
	newf := seedFact(t, db, cid, "occupation", "创业者", 0.7, 0.5, "active", "ai", "2025-01-01T00:00:00Z", "")
	if _, err := db.Exec(`UPDATE profile_facts SET valid_from='2023-01-01T00:00:00Z', valid_until='2025-01-01T00:00:00Z', superseded_by=? WHERE id=?`, newf, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE profile_facts SET valid_from='2025-01-01T00:00:00Z' WHERE id=?`, newf); err != nil {
		t.Fatal(err)
	}

	tl, err := BuildFactTimeline(db, cid, "occupation", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Slots) != 2 {
		t.Fatalf("应有 2 个历史版本，实得 %d", len(tl.Slots))
	}
	// 当前值恒在末位。
	last := tl.Slots[len(tl.Slots)-1]
	if !last.Current || last.Value != "创业者" {
		t.Fatalf("末位应为当前值『创业者』，实得 %+v", last)
	}
	first := tl.Slots[0]
	if first.Current || first.Value != "销售" || first.ValidUntil == "" || first.SupersededBy == nil || *first.SupersededBy != newf {
		t.Fatalf("首位应为被取代的『销售』且带时效/指向新值，实得 %+v", first)
	}
}

func TestMergeFactsMigratesEvidenceAndSupersedes(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "合并主")
	keep := seedFact(t, db, cid, "interest", "跑步", 0.7, 0.5, "active", "ai", time.Now().Format(time.RFC3339), "")
	loser := seedFact(t, db, cid, "interest", "喜欢跑步", 0.6, 0.3, "active", "ai", time.Now().Format(time.RFC3339), "")
	recent := time.Now().Format(time.RFC3339)
	seedEvidenceRow(t, db, keep, cid, 1, EvDirect, "exact", "我爱跑步", recent)
	seedEvidenceRow(t, db, loser, cid, 2, EvDirect, "exact", "喜欢跑步", recent)
	// 一条与 keep 重复 (message_id,archived) 的证据：合并时应被去重丢弃，不撞唯一约束。
	seedEvidenceRow(t, db, loser, cid, 1, EvDirect, "exact", "我爱跑步", recent)

	if err := MergeFacts(db, keep, []int64{loser}); err != nil {
		t.Fatal(err)
	}
	// loser 标 superseded、不删除、指向 keep。
	var status string
	var sup sql.NullInt64
	if err := db.QueryRow(`SELECT status, superseded_by FROM profile_facts WHERE id=?`, loser).Scan(&status, &sup); err != nil {
		t.Fatal(err)
	}
	if status != "superseded" || !sup.Valid || sup.Int64 != keep {
		t.Fatalf("loser 应 superseded→keep，实得 status=%q sup=%v", status, sup)
	}
	// keep 现在拥有 message 1 与 2 两条证据（重复的 message 1 被去重、不外迁第二份）。
	var cntMsg2, cntMsg1 int
	db.QueryRow(`SELECT COUNT(*) FROM profile_fact_evidence WHERE fact_id=? AND message_id=2`, keep).Scan(&cntMsg2)
	db.QueryRow(`SELECT COUNT(*) FROM profile_fact_evidence WHERE fact_id=? AND message_id=1`, keep).Scan(&cntMsg1)
	if cntMsg2 != 1 || cntMsg1 != 1 {
		t.Fatalf("keep 应恰好持有 msg1×1、msg2×1 证据，实得 msg1=%d msg2=%d", cntMsg1, cntMsg2)
	}
	// loser 名下不再有证据行。
	var leftover int
	db.QueryRow(`SELECT COUNT(*) FROM profile_fact_evidence WHERE fact_id=?`, loser).Scan(&leftover)
	if leftover != 0 {
		t.Fatalf("loser 证据应全部迁走，残留 %d", leftover)
	}
}

func TestConsolidationDetectsLowEvidenceAndNeverConfirmed(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "检测田")
	recent := time.Now().Format(time.RFC3339)
	old := time.Now().AddDate(0, 0, -200).Format(time.RFC3339)
	// low evidence：active、非 user、strength < 0.34。
	lowID := seedFact(t, db, cid, "important_fact", "证据薄弱项", 0.6, 0.1, "active", "ai", recent, recent)
	// never confirmed：active、非 user、last_confirmed 空、last_seen 很旧。
	neverID := seedFact(t, db, cid, "important_fact", "从未确认项", 0.6, 0.5, "active", "ai", old, "")

	p, err := BuildConsolidationProposal(db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sawLow, sawNever := false, false
	for _, it := range p.Items {
		if it.Kind == KindLowEvidence && len(it.Facts) > 0 && it.Facts[0].ID == lowID {
			sawLow = true
		}
		if it.Kind == KindNeverConfirmed && len(it.Facts) > 0 && it.Facts[0].ID == neverID {
			sawNever = true
		}
	}
	if !sawLow {
		t.Fatalf("提案应含 low_evidence 检测，Summary=%+v", p.Summary)
	}
	if !sawNever {
		t.Fatalf("提案应含 never_confirmed 检测，Summary=%+v", p.Summary)
	}
	if p.Summary.LowEvidence == 0 || p.Summary.NeverConfirmed == 0 {
		t.Fatalf("汇总计数应 >0，实得 %+v", p.Summary)
	}
}

func TestMemoryAPIEvidenceChainAndTimeline(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"} // 与 callAPI 硬编码 Bearer 对齐
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	cid := regressionContact(t, db, "API主")
	fid := seedFact(t, db, cid, "occupation", "工程师", 0.7, 0.5, "active", "ai", time.Now().Format(time.RFC3339), "")
	recent := time.Now().Format(time.RFC3339)
	seedEvidenceRow(t, db, fid, cid, 500, EvDirect, "exact", "我是工程师", recent)

	// GET evidence-chain
	w := callAPI(s, http.MethodGet, "/api/contacts/"+itoa(cid)+"/facts/evidence-chain/"+itoa(fid), "")
	if w.Code != http.StatusOK {
		t.Fatalf("evidence-chain 应 200，实得 %d %s", w.Code, w.Body.String())
	}
	var ec struct {
		OK    bool                `json:"ok"`
		Chain EvidenceChainAnswer `json:"chain"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ec); err != nil {
		t.Fatal(err)
	}
	if !ec.OK || ec.Chain.FactValue != "工程师" || len(ec.Chain.OriginalTexts) != 1 {
		t.Fatalf("证据链响应不完整，实得 %+v", ec.Chain)
	}

	// GET timeline
	w = callAPI(s, http.MethodGet, "/api/contacts/"+itoa(cid)+"/facts/timeline?factType=occupation", "")
	if w.Code != http.StatusOK {
		t.Fatalf("timeline 应 200，实得 %d %s", w.Code, w.Body.String())
	}
	var tl struct {
		OK       bool         `json:"ok"`
		Timeline FactTimeline `json:"timeline"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tl); err != nil {
		t.Fatal(err)
	}
	if !tl.OK || len(tl.Timeline.Slots) != 1 || tl.Timeline.Slots[0].Value != "工程师" {
		t.Fatalf("时间线应含 1 个当前版本，实得 %+v", tl.Timeline)
	}

	// 缺 factType → 400
	if w = callAPI(s, http.MethodGet, "/api/contacts/"+itoa(cid)+"/facts/timeline", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("缺 factType 应 400，实得 %d", w.Code)
	}
	// 不存在事实的 evidence-chain → 404（非 500）
	if w = callAPI(s, http.MethodGet, "/api/contacts/"+itoa(cid)+"/facts/evidence-chain/999999", ""); w.Code != http.StatusNotFound {
		t.Fatalf("不存在事实应 404，实得 %d %s", w.Code, w.Body.String())
	}
}
