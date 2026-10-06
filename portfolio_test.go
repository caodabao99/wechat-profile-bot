package main

// Phase 10 Relationship Portfolio（蓝图 §12 · P8）回归。
// 覆盖：纯函数（portfolioNeedWeight/largestRemainder）、normalize + save/load round-trip、
// ComputePortfolio（手指定优先占预算 / 余量比例分配 / 类别汇总 / Top 排序 / 超预算 / UsedEstimate / INFERENCE）、
// 以及 GET /api/portfolio 与 /api/portfolio/settings 端到端读写。

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// ── 纯函数：需求权重（分配系数，非新分数）──

func TestPortfolioNeedWeightPure(t *testing.T) {
	// 类别权重越大、亲密度越高 → 权重越大
	base := portfolioNeedWeight(40, 80, 20, dynStable, 70)
	low := portfolioNeedWeight(4, 10, 1, dynStable, 70)
	if !(base > low) {
		t.Fatalf("核心高亲密应比弱联系权重大：base=%d low=%d", base, low)
	}
	// 值得挽回（at_risk 且仍有相当亲密度）应加码于同参数 stable
	risky := portfolioNeedWeight(40, 80, 20, dynAtRisk, 70)
	if risky <= base {
		t.Fatalf("at_risk+亲密度应加码：%d 应大于 %d", risky, base)
	}
	// 弱联系流失（at_risk 但亲密度<30）不硬塞：与同档 stable 权重相等（不触发挽回加码）
	weakRisk := portfolioNeedWeight(4, 10, 1, dynAtRisk, 70)
	if weakRisk != low {
		t.Fatalf("低亲密 at_risk 不应触发挽回加码（应与 stable 同档）：weakRisk=%d low=%d", weakRisk, low)
	}
	// 健康度偏低（0<health<50）应高于同参数健康良好
	unhealthy := portfolioNeedWeight(25, 50, 10, dynStable, 40)
	healthy := portfolioNeedWeight(25, 50, 10, dynStable, 70)
	if unhealthy <= healthy {
		t.Fatalf("健康偏低应加码：%d 应大于 %d", unhealthy, healthy)
	}
	// 确定性：同输入同输出，且非负
	if a, b := portfolioNeedWeight(12, 30, 5, dynCooling, 55), portfolioNeedWeight(12, 30, 5, dynCooling, 55); a != b {
		t.Fatalf("纯函数应确定性：%d != %d", a, b)
	}
	if w := portfolioNeedWeight(0, 0, 0, "", 0); w < 0 {
		t.Fatalf("权重不应为负，得 %d", w)
	}
}

// ── 纯函数：最大余数法整数分配，和恰为 total ──

func TestLargestRemainderSumsExact(t *testing.T) {
	cases := []struct {
		total   int
		weights []int
	}{
		{300, []int{200, 100, 60, 4}},
		{100, []int{1, 1, 1}},        // 无法整除：100/3 → 34,33,33
		{0, []int{5, 5}},             // total<=0 → 全 0
		{50, []int{0, 0, 0}},         // 权重全 0 → 全 0（多余额退回）
		{7, []int{100}},              // 单项 → 全给
		{1000, []int{3, 3, 3, 3, 3}}, // 五等分
	}
	for _, c := range cases {
		got := largestRemainder(c.total, c.weights)
		if len(got) != len(c.weights) {
			t.Fatalf("长度应等于权重数：%+v", got)
		}
		sum := 0
		weightSum := 0
		for _, w := range c.weights {
			weightSum += w
		}
		for _, v := range got {
			if v < 0 {
				t.Fatalf("分配不应为负：%+v", got)
			}
			sum += v
		}
		wantTotal := c.total
		if c.total <= 0 || weightSum <= 0 {
			wantTotal = 0
		}
		if sum != wantTotal {
			t.Fatalf("total=%d weights=%v 分配和应 %d，得 %d（%v）", c.total, c.weights, wantTotal, sum, got)
		}
	}
	// 具体校验 100/3 → 和恰 100，最大余数法平票小索引优先 → [34,33,33]
	got := largestRemainder(100, []int{1, 1, 1})
	if got[0] != 34 || got[1] != 33 || got[2] != 33 {
		t.Fatalf("100 按 [1,1,1] 应 [34,33,33]，得 %v", got)
	}
}

// ── normalize + save/load round-trip ──

func TestPortfolioSettingsRoundTrip(t *testing.T) {
	db := regressionDB(t)
	// 越界预算 + 缺类别权重 + 负手指定 → normalize 兜底
	in := PortfolioSettings{
		WeeklyBudgetMinutes: 99999999,
		TierWeights:         map[string]int{circleCore: 55}, // 只给 core，其余回落默认
		ManualMinutes:       map[string]int{"7": -50},       // 负 → 归零
	}
	if err := savePortfolioSettings(db, in); err != nil {
		t.Fatal(err)
	}
	got, err := loadPortfolioSettings(db)
	if err != nil {
		t.Fatal(err)
	}
	if got.WeeklyBudgetMinutes != portfolioDefaultBudgetMinutes {
		t.Fatalf("越界预算应回默认 %d，得 %d", portfolioDefaultBudgetMinutes, got.WeeklyBudgetMinutes)
	}
	if got.TierWeights[circleCore] != 55 {
		t.Fatalf("core 权重应保留 55，得 %d", got.TierWeights[circleCore])
	}
	if got.TierWeights[circleWeak] != portfolioDefaultTierWeights[circleWeak] {
		t.Fatalf("缺失类别应回默认 weak=%d，得 %d", portfolioDefaultTierWeights[circleWeak], got.TierWeights[circleWeak])
	}
	if got.ManualMinutes["7"] != 0 {
		t.Fatalf("负手指定应归零，得 %d", got.ManualMinutes["7"])
	}
	// 空库无行 → 默认
	db2 := regressionDB(t)
	d, err := loadPortfolioSettings(db2)
	if err != nil {
		t.Fatal(err)
	}
	if d.WeeklyBudgetMinutes != portfolioDefaultBudgetMinutes || len(d.TierWeights) != len(portfolioDefaultTierWeights) {
		t.Fatalf("无设置应返回默认，得 %#v", d)
	}
}

// ── ComputePortfolio：手指定优先占预算 + 余量比例分配 ──

func TestComputePortfolioManualAndAllocation(t *testing.T) {
	db := regressionDB(t)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.Local)
	a := regressionContact(t, db, "手指定人")
	b := regressionContact(t, db, "系统建议人")

	// 预算 300，a 手指定 100（优先占用），剩余 200 应全部落到唯一非手指定的 b
	if err := savePortfolioSettings(db, PortfolioSettings{
		WeeklyBudgetMinutes: 300,
		TierWeights:         copyDefaultTierWeights(),
		ManualMinutes:       map[string]int{strconv.FormatInt(a, 10): 100},
	}); err != nil {
		t.Fatal(err)
	}

	view, err := ComputePortfolio(db, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if view.Layer != "INFERENCE" {
		t.Fatalf("组合建议分层应恒 INFERENCE，得 %q", view.Layer)
	}
	if !view.UsedIsEstimate {
		t.Fatal("已使用必须是估算")
	}
	if view.OverBudget {
		t.Fatal("手指定 100 ≤ 预算 300，不应判超预算")
	}
	if view.AllocatedMinutes != 300 {
		t.Fatalf("不超预算且有余量接收者时分配和应恰为预算 300，得 %d", view.AllocatedMinutes)
	}
	ma := findEntry(view.Top, a)
	mb := findEntry(view.Top, b)
	if ma == nil || mb == nil {
		t.Fatalf("a/b 都应进入 Top：%#v %#v", ma, mb)
	}
	if ma.Minutes != 100 || !ma.Overridden {
		t.Fatalf("手指定人应 100 且 Overridden，得 %d/%v", ma.Minutes, ma.Overridden)
	}
	if mb.Minutes != 200 || mb.Overridden {
		t.Fatalf("唯一系统建议人应吃满余量 200 且非手指定，得 %d/%v", mb.Minutes, mb.Overridden)
	}
	// 类别汇总人数应含两位联系人
	totalContacts := 0
	for _, ta := range view.Tiers {
		totalContacts += ta.Contacts
	}
	if totalContacts < 2 {
		t.Fatalf("类别汇总人数应 ≥2，得 %d", totalContacts)
	}
}

// ── ComputePortfolio：手指定之和超预算 → OverBudget，余量清零 ──

func TestComputePortfolioOverBudget(t *testing.T) {
	db := regressionDB(t)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.Local)
	a := regressionContact(t, db, "超支甲")
	b := regressionContact(t, db, "超支乙")
	if err := savePortfolioSettings(db, PortfolioSettings{
		WeeklyBudgetMinutes: 300,
		TierWeights:         copyDefaultTierWeights(),
		ManualMinutes:       map[string]int{strconv.FormatInt(a, 10): 200, strconv.FormatInt(b, 10): 200},
	}); err != nil {
		t.Fatal(err)
	}
	view, err := ComputePortfolio(db, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !view.OverBudget {
		t.Fatal("手指定之和 400 > 预算 300 应判超预算")
	}
	// 无剩余可分，两位手指定者各保 200，系统建议部分为 0
	if ma := findEntry(view.Top, a); ma == nil || ma.Minutes != 200 {
		t.Fatalf("超预算下手指定甲仍应 200，得 %#v", ma)
	}
	if view.AllocatedMinutes != 400 {
		t.Fatalf("分配和应为手指定之和 400，得 %d", view.AllocatedMinutes)
	}
}

// ── Top 排序与 topN 截断 ──

func TestComputePortfolioTopOrderAndLimit(t *testing.T) {
	db := regressionDB(t)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.Local)
	ids := make([]int64, 0, 6)
	for i := 0; i < 6; i++ {
		ids = append(ids, regressionContact(t, db, "联系人"+strconv.Itoa(i)))
	}
	// 给每人手指定不同分钟，令排序可断言（降序）
	manual := map[string]int{}
	want := []int{10, 20, 30, 40, 50, 60}
	for i, id := range ids {
		manual[strconv.FormatInt(id, 10)] = want[i]
	}
	if err := savePortfolioSettings(db, PortfolioSettings{
		WeeklyBudgetMinutes: portfolioMaxBudgetMinutes, // 足够大不触顶
		TierWeights:         copyDefaultTierWeights(),
		ManualMinutes:       manual,
	}); err != nil {
		t.Fatal(err)
	}
	// topN=3 → 只返回分钟最高的 3 人（60,50,40）
	view, err := ComputePortfolio(db, now, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Top) != 3 {
		t.Fatalf("topN=3 应返回 3 条，得 %d", len(view.Top))
	}
	if view.Top[0].Minutes != 60 || view.Top[1].Minutes != 50 || view.Top[2].Minutes != 40 {
		t.Fatalf("Top 应按分钟降序 60/50/40，得 %d/%d/%d", view.Top[0].Minutes, view.Top[1].Minutes, view.Top[2].Minutes)
	}
	// 可解释：每条建议都应有非空 reason
	for _, e := range view.Top {
		if e.Reason == "" {
			t.Fatalf("建议 %#v 缺少可解释依据", e)
		}
	}
}

// ── API 端到端 ──

func TestPortfolioAPIEndToEnd(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}
	regressionContact(t, db, "API人")

	// 读默认仪表盘
	w := callAPI(s, http.MethodGet, "/api/portfolio", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/portfolio: %d %s", w.Code, w.Body.String())
	}
	var view PortfolioView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Layer != "INFERENCE" || !view.UsedIsEstimate {
		t.Fatalf("仪表盘应 INFERENCE + 估算已使用，得 %#v", view)
	}

	// 改设置：预算 240
	w = callAPI(s, http.MethodPut, "/api/portfolio/settings", `{"weeklyBudgetMinutes":240}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT settings: %d %s", w.Code, w.Body.String())
	}

	// 回读设置
	w = callAPI(s, http.MethodGet, "/api/portfolio/settings", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET settings: %d %s", w.Code, w.Body.String())
	}
	var set PortfolioSettings
	if err := json.Unmarshal(w.Body.Bytes(), &set); err != nil {
		t.Fatal(err)
	}
	if set.WeeklyBudgetMinutes != 240 {
		t.Fatalf("设置应持久化为 240，得 %d", set.WeeklyBudgetMinutes)
	}

	// 非法方法 405
	w = callAPI(s, http.MethodDelete, "/api/portfolio", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /api/portfolio 应 405，得 %d", w.Code)
	}
	// 未知子路径 404
	w = callAPI(s, http.MethodGet, "/api/portfolio/unknown", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("未知子路径应 404，得 %d", w.Code)
	}
}

// ── 测试辅助 ──

func copyDefaultTierWeights() map[string]int {
	m := map[string]int{}
	for k, v := range portfolioDefaultTierWeights {
		m[k] = v
	}
	return m
}

func findEntry(entries []PortfolioEntry, cid int64) *PortfolioEntry {
	for i := range entries {
		if entries[i].ContactID == cid {
			return &entries[i]
		}
	}
	return nil
}
