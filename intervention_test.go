package main

// 高阶洞察 · Phase C 证据化干预学习回归。
// 覆盖：Wilson 区间性质、成功率、样本不足诚实降级、判决文案、按置信下界排序、
//       kindLabel/dataNote/bestActions，以及整层重算（pending 不计入）+ 缓存幂等。

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestWilsonIntervalProperties(t *testing.T) {
	lo, hi := wilsonInterval(0.5, 100, interventionWilsonZ)
	if !(lo > 0 && lo < 0.5 && hi > 0.5 && hi < 1) {
		t.Errorf("p=0.5,n=100 区间应包裹 0.5 且在 (0,1) 内, got [%v,%v]", lo, hi)
	}
	// 样本越多区间越窄
	lo2, hi2 := wilsonInterval(0.5, 1000, interventionWilsonZ)
	if (hi2 - lo2) >= (hi - lo) {
		t.Errorf("n=1000 应比 n=100 更窄: [%v,%v] vs [%v,%v]", lo2, hi2, lo, hi)
	}
	// n=0 退化
	if l, h := wilsonInterval(0.5, 0, interventionWilsonZ); l != 0 || h != 0 {
		t.Errorf("n=0 应返回 0,0, got [%v,%v]", l, h)
	}
	// 边界裁剪到 [0,1]
	if _, h := wilsonInterval(1.0, 20, interventionWilsonZ); h > 1 {
		t.Errorf("上界不应超过 1, got %v", h)
	}
	if l, _ := wilsonInterval(0.0, 20, interventionWilsonZ); l < 0 {
		t.Errorf("下界不应低于 0, got %v", l)
	}
}

func TestFinalizeLearning(t *testing.T) {
	// N=0 不动
	zero := &InterventionLearning{Kind: "cooling", Segment: "全部"}
	finalizeLearning(zero)
	if zero.SuccessRate != 0 || zero.LowSample {
		t.Errorf("N=0 应保持原样, got %+v", zero)
	}
	// 样本充足：15/20 → 75%
	ok := &InterventionLearning{Kind: "cooling", Segment: "全部", N: 20, Improved: 15}
	finalizeLearning(ok)
	if ok.SuccessRate != 75 || ok.LowSample {
		t.Errorf("20 样本不应低样本, rate=%v low=%v", ok.SuccessRate, ok.LowSample)
	}
	if ok.Verdict == "" {
		t.Error("应有判决文案")
	}
	// 样本不足：3 例 → LowSample
	few := &InterventionLearning{Kind: "birthday", Segment: "全部", N: 3, Improved: 2}
	finalizeLearning(few)
	if !few.LowSample {
		t.Errorf("3 例应标低样本, got %+v", few)
	}
	if !strings.Contains(few.Verdict, "样本偏少") {
		t.Errorf("低样本判决应诚实标注, got %s", few.Verdict)
	}
}

func TestVerdictBranches(t *testing.T) {
	// 稳定有效：置信下界 >= 50
	stable := &InterventionLearning{Kind: "cooling", Segment: "朋友", N: 40, Improved: 34, SuccessRate: 85, WilsonLow: 70}
	if got := verdict(stable); !strings.Contains(got, "稳定有效") {
		t.Errorf("高置信下界应判稳定有效, got %s", got)
	}
	// 少见成效：成功率 <= 25 且不低样本
	poor := &InterventionLearning{Kind: "silence", Segment: "同事", N: 30, Improved: 5, SuccessRate: 16.7, WilsonLow: 8}
	if got := verdict(poor); !strings.Contains(got, "少见成效") {
		t.Errorf("低成功率应判少见成效, got %s", got)
	}
}

func TestSortLearningsOrder(t *testing.T) {
	list := []InterventionLearning{
		{Kind: "birthday", Segment: "全部", N: 5, WilsonLow: 90, LowSample: true},
		{Kind: "cooling", Segment: "全部", N: 30, WilsonLow: 50, LowSample: false},
		{Kind: "silence", Segment: "全部", N: 30, WilsonLow: 60, LowSample: false},
	}
	sortLearnings(list)
	// 非低样本在前
	if list[0].LowSample || list[1].LowSample || !list[2].LowSample {
		t.Fatalf("充足样本应排在低样本之前: %+v", list)
	}
	// 充足样本内部按置信下界降序：silence(60) 先于 cooling(50)
	if list[0].Kind != "silence" || list[1].Kind != "cooling" {
		t.Errorf("应按置信下界降序, got %s,%s", list[0].Kind, list[1].Kind)
	}
}

func TestKindLabelAndDataNoteAndBestActions(t *testing.T) {
	if kindLabel("cooling") != "主动破冰问候" {
		t.Errorf("cooling 标签错误: %s", kindLabel("cooling"))
	}
	if kindLabel("unknown_kind") != "unknown_kind" {
		t.Errorf("未知 kind 应原样返回: %s", kindLabel("unknown_kind"))
	}
	if dataNote(0) == "" {
		t.Error("0 样本应有引导说明")
	}
	if !strings.Contains(dataNote(5), "还不够") {
		t.Errorf("少样本说明应提示不足, got %s", dataNote(5))
	}
	// bestActions 只取样本充足且置信下界>=40 的
	bs := bestActions([]InterventionLearning{
		{KindLabel: "主动破冰问候", N: 30, SuccessRate: 80, WilsonLow: 65, LowSample: false},
		{KindLabel: "久未联系后重启话题", N: 4, WilsonLow: 55, LowSample: true},
		{KindLabel: "低效动作", N: 30, SuccessRate: 20, WilsonLow: 10, LowSample: false},
	})
	if len(bs) != 1 || !strings.Contains(bs[0], "主动破冰问候") {
		t.Errorf("只应选出高置信有效动作, got %+v", bs)
	}
}

func TestComputeInterventionEmptyAndStale(t *testing.T) {
	db := regressionAssistantDB(t)
	now := time.Now()
	if err := ComputeInterventionLearning(db, now); err != nil {
		t.Fatal(err)
	}
	ins, genAt, err := GetCachedIntervention(db)
	if err != nil || ins == nil {
		t.Fatalf("应读回缓存: %v", err)
	}
	if ins.TotalResolved != 0 {
		t.Errorf("空库已回测总数应为 0, got %d", ins.TotalResolved)
	}
	if ins.DataNote == "" {
		t.Error("空库应有引导说明")
	}
	if IsInterventionStale(genAt, now) || !IsInterventionStale(genAt, now.Add(8*24*time.Hour)) {
		t.Error("新鲜度判定不符")
	}
}

func TestComputeInterventionAggregationExcludesPending(t *testing.T) {
	db := regressionAssistantDB(t)
	now := time.Now()
	cid := regressionContact(t, db, "干预对象")
	// 建一条 cooling 建议
	var suggID int64
	if _, err := db.Exec(`INSERT INTO relationship_action_suggestions (contact_id, kind, window_key) VALUES (?, 'cooling', 'test-win')`, cid); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT id FROM relationship_action_suggestions WHERE contact_id=? AND window_key='test-win'`, cid).Scan(&suggID); err != nil {
		t.Fatal(err)
	}
	// 8 条已回测：6 improved / 1 stable / 1 worsened
	seedOutcome(t, db, suggID, cid, "improved")
	seedOutcome(t, db, suggID, cid, "improved")
	seedOutcome(t, db, suggID, cid, "improved")
	seedOutcome(t, db, suggID, cid, "improved")
	seedOutcome(t, db, suggID, cid, "improved")
	seedOutcome(t, db, suggID, cid, "improved")
	seedOutcome(t, db, suggID, cid, "stable")
	seedOutcome(t, db, suggID, cid, "worsened")
	// 2 条 pending —— 必须被排除
	seedOutcome(t, db, suggID, cid, "pending")
	seedOutcome(t, db, suggID, cid, "pending")

	if err := ComputeInterventionLearning(db, now); err != nil {
		t.Fatal(err)
	}
	ins, _, err := GetCachedIntervention(db)
	if err != nil || ins == nil {
		t.Fatal(err)
	}
	if ins.TotalResolved != 8 {
		t.Errorf("pending 不该计入, 期望 8, got %d", ins.TotalResolved)
	}
	// 找到 cooling × 全部 分组
	var cell *InterventionLearning
	for i := range ins.ByKind {
		if ins.ByKind[i].Kind == "cooling" && ins.ByKind[i].Segment == "全部" {
			cell = &ins.ByKind[i]
		}
	}
	if cell == nil {
		t.Fatalf("应存在 cooling/全部 分组: %+v", ins.ByKind)
	}
	if cell.N != 8 || cell.Improved != 6 || cell.Stable != 1 || cell.Worsened != 1 {
		t.Errorf("分组计数不符: %+v", cell)
	}
	if !cell.LowSample {
		t.Errorf("8 例应标低样本(<10), got %+v", cell)
	}
	// 整体回暖率 = 6/8 = 75%
	if ins.BaseRate != 75 {
		t.Errorf("基线回暖率应为 75, got %v", ins.BaseRate)
	}
}

// seedOutcome 直接插一条回测结果。
func seedOutcome(t *testing.T, db *sql.DB, suggID, contactID int64, outcome string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO suggestion_outcomes (suggestion_id, contact_id, acted_at, outcome, checked_at)
		VALUES (?,?,?,?,?)`, suggID, contactID, "2026-09-20 10:00:00", outcome, "2026-10-04 10:00:00"); err != nil {
		t.Fatal(err)
	}
}
