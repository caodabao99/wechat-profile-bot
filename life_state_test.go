package main

// 人生模拟器 · Phase A 回归：人生状态模型。
// 覆盖：资产账本象限分类、现金流基线比值、HHI 集中度、风险分级、类别推断、
// 每周斜率、派生缓存读写 round-trip 与过期判定、lifeSimEnabled 默认开。
//
// buildLifeState 是纯函数（不碰 DB），用它做确定性的账本/组合聚合断言；
// 再用 ComputeLifeState / saveLifeStateCache 验证缓存落库与自愈读回。

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLifeCategorizeByTags(t *testing.T) {
	cases := map[string]string{
		"妈妈":    "家人",
		"家人 亲戚": "家人",
		"同事":    "同事",
		"客户 A":  "同事",
		"大学同学":  "朋友",
		"闺蜜":    "朋友",
		"":      "其他",
		"快递员":   "其他",
		"同事 老婆": "家人", // 多标签：家人关键词优先
	}
	for tags, want := range cases {
		if got := categorizeByTags(strings.Fields(tags)); got != want {
			t.Errorf("categorizeByTags(%q)=%q, want %q", tags, got, want)
		}
	}
}

func TestLifeClassifyAsset(t *testing.T) {
	// 高频、浅、单向 → 事务型
	if got := classifyAsset(40, 12, 0.9); got != "transactional" {
		t.Errorf("事务型判定错: %q", got)
	}
	// 又深又频 → 挚友
	if got := classifyAsset(75, 30, 0.2); got != "close" {
		t.Errorf("挚友判定错: %q", got)
	}
	// 中深 → 好友
	if got := classifyAsset(55, 5, 0.2); got != "friend" {
		t.Errorf("好友判定错: %q", got)
	}
	// 浅 → 熟人 / 弱关系
	if got := classifyAsset(30, 2, 0.1); got != "acquaintance" {
		t.Errorf("熟人判定错: %q", got)
	}
	if got := classifyAsset(10, 1, 0.1); got != "weak" {
		t.Errorf("弱关系判定错: %q", got)
	}
}

func TestLifeWeeklySlope(t *testing.T) {
	// 空序列
	if got := weeklySlope(nil); got != 0 {
		t.Errorf("空序列斜率应为 0, got %v", got)
	}
	// 常数序列 → 0
	flat := make([]float64, 90)
	for i := range flat {
		flat[i] = 5
	}
	if got := weeklySlope(flat); got != 0 {
		t.Errorf("常数序列斜率应为 0, got %v", got)
	}
	// 每天 +1 的线性增长：perDay=1 → 每周 7
	rising := make([]float64, 90)
	for i := range rising {
		rising[i] = float64(i)
	}
	if got := weeklySlope(rising); got < 6.9 || got > 7.1 {
		t.Errorf("线性增长斜率应≈7, got %v", got)
	}
	// 递减序列斜率应为负
	falling := make([]float64, 90)
	for i := range falling {
		falling[i] = float64(90 - i)
	}
	if got := weeklySlope(falling); got >= 0 {
		t.Errorf("递减序列斜率应为负, got %v", got)
	}
}

// TestLifeBuildStateLedger 用纯函数直接校验账本聚合：现金流基线比值、
// 象限分类、风险分级、HHI 集中度、组合总财富、高风险清单。
func TestLifeBuildStateLedger(t *testing.T) {
	now := time.Now()
	raw := &lifeRaw{
		balance:   map[int64]int{1: 80, 2: 40},
		freq30:    map[int64]int{1: 30, 2: 12},
		mine30:    map[int64]int{1: 15, 2: 11},
		other30:   map[int64]int{1: 15, 2: 1},
		name:      map[int64]string{1: "妈妈", 2: "客户小张大"},
		lastTS:    map[int64]int64{1: now.Unix(), 2: now.AddDate(0, 0, -100).Unix()},
		lifeCount: map[int64]int{1: 300, 2: 40},
		lifeDays:  map[int64]int{1: 300, 2: 120},
		slope:     map[int64]float64{1: 0, 2: -6},
		category:  map[int64]string{1: "家人", 2: "同事"},
		catRecent: map[string]int{"家人": 30, "同事": 12},
		catLife:   map[string]int{"家人": 300, "同事": 40},
		active:    map[int64]bool{1: true, 2: true},
	}

	st := buildLifeState(now, raw)

	if st.Portfolio.ContactCount != 2 {
		t.Fatalf("账本联系人应为 2, got %d", st.Portfolio.ContactCount)
	}

	byID := map[int64]LifeAsset{}
	for _, a := range st.Assets {
		byID[a.ContactID] = a
	}

	// 现金流 = (freq30/30)/(lifeCount/lifeDays)：cid1 = 1.0/1.0 = 1.0
	if c := byID[1].Cashflow; c < 0.99 || c > 1.01 {
		t.Errorf("cid1 现金流应≈1.0, got %v", c)
	}
	// cid2 = (12/30)/(40/120) = 0.4/0.333 ≈ 1.2
	if c := byID[2].Cashflow; c < 1.15 || c > 1.25 {
		t.Errorf("cid2 现金流应≈1.2, got %v", c)
	}

	// 象限分类：cid1 挚友，cid2 事务型（浅+单向+高频）
	if byID[1].AssetClass != "close" {
		t.Errorf("cid1 应为 close, got %q", byID[1].AssetClass)
	}
	if byID[2].AssetClass != "transactional" {
		t.Errorf("cid2 应为 transactional, got %q", byID[2].AssetClass)
	}

	// 风险：cid2 陈旧(100天)+负趋势+单向 → high；cid1 → low
	if byID[2].RiskLevel != "high" {
		t.Errorf("cid2 应为 high 风险, got %q score=%v", byID[2].RiskLevel, byID[2].RiskScore)
	}
	if byID[1].RiskLevel != "low" {
		t.Errorf("cid1 应为 low 风险, got %q", byID[1].RiskLevel)
	}

	// 高风险清单只含 cid2
	if len(st.HighRisk) != 1 || st.HighRisk[0].ContactID != 2 {
		t.Fatalf("高风险清单应只有 cid2, got %+v", st.HighRisk)
	}
	if st.Portfolio.HighRiskCount != 1 {
		t.Errorf("HighRiskCount 应为 1, got %d", st.Portfolio.HighRiskCount)
	}

	// 组合总财富 = 80*1.5 + 40*0.5 = 140
	if w := st.Portfolio.TotalWealth; w < 139.9 || w > 140.1 {
		t.Errorf("TotalWealth 应≈140, got %v", w)
	}

	// HHI = (80/120)^2 + (40/120)^2 = 0.444+0.111 = 0.56
	if c := st.Portfolio.Concentration; c < 0.55 || c > 0.57 {
		t.Errorf("集中度 HHI 应≈0.56, got %v", c)
	}

	// 类别数量
	if st.Portfolio.ClassCounts["close"] != 1 || st.Portfolio.ClassCounts["transactional"] != 1 {
		t.Errorf("ClassCounts 不符: %+v", st.Portfolio.ClassCounts)
	}

	// 时间回流偏差洞察：同事近期占比 12/42=28.6% vs 历史 40/340=11.8%（升），
	// 家人近期 71.4% vs 历史 88.2%（降超 8 个百分点）→ 至少一条洞察
	if len(st.Time.Insights) == 0 {
		t.Error("类别偏差应生成洞察句")
	}
}

func TestLifeStateCacheRoundTrip(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()

	// 空库读缓存应返回 nil（无行），不报错
	st, _, err := GetCachedLifeState(db)
	if err != nil {
		t.Fatal(err)
	}
	if st != nil {
		t.Fatal("空库不应有缓存")
	}

	want := &LifeState{
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
		Portfolio:   LifePortfolio{TotalWealth: 123.4, ContactCount: 7},
		Assets:      []LifeAsset{{ContactID: 1, Name: "老王", Balance: 66}},
	}
	if err := saveLifeStateCache(db, now, want); err != nil {
		t.Fatal(err)
	}
	got, genAt, err := GetCachedLifeState(db)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Portfolio.ContactCount != 7 || got.Assets[0].Name != "老王" {
		t.Fatalf("缓存回读不符: %+v", got)
	}
	if genAt.IsZero() {
		t.Fatal("generatedAt 不应为零值")
	}
	// 再存一次覆盖 id=1 单行
	want.Portfolio.ContactCount = 9
	if err := saveLifeStateCache(db, now, want); err != nil {
		t.Fatal(err)
	}
	got, _, _ = GetCachedLifeState(db)
	if got.Portfolio.ContactCount != 9 {
		t.Fatalf("UPSERT 应覆盖同一行, got %d", got.Portfolio.ContactCount)
	}
}

func TestLifeStateStale(t *testing.T) {
	now := time.Now()
	if !IsLifeStateStale(time.Time{}, now) {
		t.Error("零值时间应判为过期")
	}
	if IsLifeStateStale(now.Add(-24*time.Hour), now) {
		t.Error("1 天前不应过期")
	}
	if !IsLifeStateStale(now.Add(-8*24*time.Hour), now) {
		t.Error("8 天前应过期")
	}
	if IsLifeStateStale(now.Add(-6*24*time.Hour), now) {
		t.Error("6 天前不应过期")
	}
}

func TestComputeLifeStateOnRealDB(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()
	cid := regressionContact(t, db, "小美")
	// 近期高频双向互动，让 cid 进入账本
	for d := 0; d < 10; d++ {
		day := now.AddDate(0, 0, -d)
		seedMessage(t, db, cid, "me", "在吗", hashFor(d, 0), day)
		seedMessage(t, db, cid, "other", "在的呀", hashFor(d, 1), day)
	}
	if err := ComputeLifeState(db, now); err != nil {
		t.Fatal(err)
	}
	st, _, err := GetCachedLifeState(db)
	if err != nil {
		t.Fatal(err)
	}
	if st == nil || st.Portfolio.ContactCount < 1 {
		t.Fatalf("有互动的联系人应进账本: %+v", st)
	}
	var found bool
	for _, a := range st.Assets {
		if a.ContactID == cid {
			found = true
			if a.Balance <= 0 {
				t.Errorf("亲密度余额应>0, got %d", a.Balance)
			}
		}
	}
	if !found {
		t.Fatal("账本里应包含刚互动的联系人")
	}
}

func TestLifeSimEnabledDefaultsTrue(t *testing.T) {
	if !defaultAssistantSettings().LifeSimEnabled {
		t.Fatal("LifeSimEnabled 默认应为 true（opt-out 思路）")
	}
	db := regressionAssistantDB(t)
	if _, err := db.Exec(`INSERT INTO assistant_settings (id, settings_json) VALUES (1, '{}')
		ON CONFLICT(id) DO UPDATE SET settings_json=excluded.settings_json`); err != nil {
		t.Fatal(err)
	}
	st, err := loadAssistantSettings(db)
	if err != nil {
		t.Fatal(err)
	}
	if !st.LifeSimEnabled {
		t.Fatal("老存量配置 JSON 里没这个字段时，加载后应保持默认 true")
	}
}

func hashFor(d, k int) string {
	return fmt.Sprintf("life-%d-%d", d, k)
}
