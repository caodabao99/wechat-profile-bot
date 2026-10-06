package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

// seedActionOutcome 在统一 Action Ledger 里落一条已产出结果的行动，供策略历史聚合。
func seedActionOutcome(t *testing.T, db *sql.DB, cid int64, actionType, outcome, provenance string, now time.Time) {
	t.Helper()
	id, err := LogAction(db, cid, "manual", "", actionType, "行动:"+actionType, now)
	if err != nil {
		t.Fatalf("LogAction 失败: %v", err)
	}
	if err := SetActionOutcome(db, id, outcome, provenance, "seed", now); err != nil {
		t.Fatalf("SetActionOutcome 失败: %v", err)
	}
}

func TestMapActionTypeToStrategyType(t *testing.T) {
	cases := []struct {
		at   string
		want string
	}{
		{reasonImportantDate, StrategyBirthdayContact},
		{reasonUnresolvedTopic, StrategyTopicFollowup},
		{reasonOpenFollowup, StrategyProjectFollowup},
		{reasonGoalDeadline, StrategyProjectFollowup},
		{reasonHighRisk, StrategySupport},
		{reasonRecentPositiveChange, StrategyCelebration},
		{reasonReconnecting, StrategyLowPressureCheckin},
		{"maintain", StrategyCasualContact},
		{"totally_unknown_token", StrategyUnknown},
		{StrategyBirthdayContact, StrategyBirthdayContact}, // 已是策略类型 → 透传（可后续扩展）
	}
	for _, c := range cases {
		if got := MapActionTypeToStrategyType(c.at, nil); got != c.want {
			t.Errorf("MapActionTypeToStrategyType(%q)=%q，期望 %q", c.at, got, c.want)
		}
	}
	// 来源名无具体类型时，回退到 reason codes 里可识别的首个（与 decisionActionType 口径一致）。
	if got := MapActionTypeToStrategyType(ActionSourceSession, []string{reasonImportantDate}); got != StrategyBirthdayContact {
		t.Errorf("来源回退 reason codes 应识别为重要日子，实得 %q", got)
	}
}

func TestComputeStrategyHistoryAggregatesAndCILowSample(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()
	cid := regressionContact(t, db, "策略甲")

	// 少量样本（< interventionMinSample）→ 必须诚实标记 low_sample、不给强结论。
	for i := 0; i < 3; i++ {
		seedActionOutcome(t, db, cid, reasonImportantDate, ActionOutcomePositive, ActionProvenanceConfirmed, now)
	}
	seedActionOutcome(t, db, cid, reasonImportantDate, ActionOutcomeNegative, ActionProvenanceEstimated, now)

	hist, err := ComputeStrategyHistory(db, now)
	if err != nil {
		t.Fatal(err)
	}
	var bday *StrategyStat
	for i := range hist.Stats {
		if hist.Stats[i].StrategyType == StrategyBirthdayContact {
			bday = &hist.Stats[i]
		}
	}
	if bday == nil {
		t.Fatalf("应聚合出重要日子策略统计，实得 %+v", hist.Stats)
	}
	if bday.SampleCount != 4 || bday.Positive != 3 || bday.Negative != 1 {
		t.Fatalf("样本计数错误：Sample=%d pos=%d neg=%d", bday.SampleCount, bday.Positive, bday.Negative)
	}
	if bday.Confirmed != 3 || bday.Estimated != 1 {
		t.Fatalf("estimated/confirmed 必须分开计数：conf=%d est=%d", bday.Confirmed, bday.Estimated)
	}
	if !bday.LowSample {
		t.Fatalf("4 例应判为小样本")
	}
	if !strings.Contains(bday.Verdict, "样本太少") {
		t.Fatalf("小样本结论必须诚实降级，实得 %q", bday.Verdict)
	}
}

func TestStrategyVerdictCorrelationNotCausation(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()
	cid := regressionContact(t, db, "策略乙")
	// 造足量、全正向样本越过门槛，触发「历史上表现更好」措辞。
	n := interventionMinSample + 2
	for i := 0; i < n; i++ {
		seedActionOutcome(t, db, cid, reasonReconnecting, ActionOutcomePositive, ActionProvenanceConfirmed, now)
	}
	hist, err := ComputeStrategyHistory(db, now)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	forbidden := []string{"保证有效", "一定会", "导致了", "必然"}
	for _, s := range hist.Stats {
		for _, f := range forbidden {
			if strings.Contains(s.Verdict, f) {
				t.Fatalf("措辞违规（因果/保证）：%q", s.Verdict)
			}
		}
		if s.StrategyType == StrategyLowPressureCheckin && !s.LowSample {
			found = true
			if !strings.Contains(s.Verdict, "历史上表现") {
				t.Fatalf("足量样本应使用「历史上表现」措辞，实得 %q", s.Verdict)
			}
		}
	}
	if !found {
		t.Fatalf("应出现一个足量样本的策略统计")
	}
}

func TestStrategyBonusBounded(t *testing.T) {
	// 小样本 → 0（不给强结论）。
	if got := strategyBonus(StrategyStat{SampleCount: 5, Positive: 5, LowSample: true}); got != 0 {
		t.Fatalf("小样本加分应为 0，实得 %d", got)
	}
	// 足量全正向 → 有界正值。
	big := strategyBonus(StrategyStat{SampleCount: 20, Positive: 20, Confirmed: 20})
	if big < 1 || big > strategyScoreMaxBonus {
		t.Fatalf("正向 bonus 应在 [1,%d]，实得 %d", strategyScoreMaxBonus, big)
	}
	// 足量全负向 → 有界负值。
	neg := strategyBonus(StrategyStat{SampleCount: 20, Negative: 20, Confirmed: 20})
	if neg < strategyScoreMaxPenalty || neg > -1 {
		t.Fatalf("负向 bonus 应在 [%d,-1]，实得 %d", strategyScoreMaxPenalty, neg)
	}
}

func TestSurfaceDecisionsAnnotatesStrategyWithoutChangingPriority(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()
	cid := regressionContact(t, db, "策略丙")
	regressionMessages(t, db, cid, "换了新工作 项目上线了")
	// 造历史，让某策略类型有确认正向记录。
	for i := 0; i < interventionMinSample+1; i++ {
		seedActionOutcome(t, db, cid, reasonReconnecting, ActionOutcomePositive, ActionProvenanceConfirmed, now)
	}
	out, err := SurfaceDecisions(db, now, 5)
	if err != nil {
		t.Fatal(err)
	}
	// Priority 仍按降序（确定性排序未被策略层扰动）。
	prios := make([]int, len(out))
	for i, c := range out {
		prios[i] = c.Priority
	}
	if !sort.SliceIsSorted(prios, func(i, j int) bool { return prios[i] >= prios[j] }) {
		t.Fatalf("SurfaceDecisions 的 Priority 排序不应被策略层改变：%v", prios)
	}
	for _, c := range out {
		if c.StrategyType == "" {
			t.Fatalf("候选应被标注 StrategyType：%+v", c)
		}
	}
}

func TestStrategyHistoryAPIEndToEnd(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	// 无 LLM、无数据也应 200（核心页不 500/503）。
	w := callAPI(s, http.MethodGet, "/api/strategy/history", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/strategy/history 应 200，实得 %d %s", w.Code, w.Body.String())
	}
	var h StrategyHistory
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if h.DataNote == "" {
		t.Fatalf("应带诚实数据说明")
	}
	// 未知子路径 → 404。
	if w2 := callAPI(s, http.MethodGet, "/api/strategy/nope", ""); w2.Code != http.StatusNotFound {
		t.Fatalf("未知子路径应 404，实得 %d", w2.Code)
	}
}
