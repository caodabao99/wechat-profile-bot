package main

// 人生模拟器 · Phase B 回归：90 天推演引擎。
// 覆盖：projectBalance 纯外推（单调性/夹紧/边界）、horizon 夹紧、断联阈值判定、
// 三条自动 what-if 策略的单调性（保高风险 ≥ 维持现状）、确定性（同输入同输出）、
// 派生缓存读写 round-trip。

import (
	"testing"
	"time"
)

func TestProjectBalance(t *testing.T) {
	// 平稳（斜率 0）：余额不变
	if got := projectBalance(50, 0, 90, 0); got != 50 {
		t.Errorf("斜率 0 应保持 50, got %d", got)
	}
	// 负斜率应下滑
	down := projectBalance(50, -4, 90, 0)
	if down >= 50 {
		t.Errorf("负趋势应下滑, got %d", down)
	}
	// 正斜率应上升
	up := projectBalance(50, 4, 90, 0)
	if up <= 50 {
		t.Errorf("正趋势应上升, got %d", up)
	}
	// 夹紧下界 0：极端负
	if got := projectBalance(5, -100, 365, 0); got != 0 {
		t.Errorf("应夹紧到 0, got %d", got)
	}
	// 夹紧上界 100：极端正
	if got := projectBalance(95, 100, 365, 0); got != 100 {
		t.Errorf("应夹紧到 100, got %d", got)
	}
	// horizon 越长，负趋势跌得越多（单调）
	short := projectBalance(50, -4, 30, 0)
	long := projectBalance(50, -4, 120, 0)
	if long >= short {
		t.Errorf("更长窗口应跌更多: short=%d long=%d", short, long)
	}
	// boost 加成应抬高结果
	boosted := projectBalance(50, -4, 90, 3)
	if boosted <= down {
		t.Errorf("正加成应比无加成结果高: base=%d boosted=%d", down, boosted)
	}
}

func TestProjectionBreakAndStrategies(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()

	// 直接落一份人生状态缓存：三段关系
	//  cid1 高余额稳定 → 不会断
	//  cid2 中余额 + 陡峭负趋势 → 会断（现状）
	//  cid3 高风险 + 家人类，负趋势接近阈值 → 加 boost 后可保住
	st := &LifeState{
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
		Assets: []LifeAsset{
			{ContactID: 1, Name: "老王", Balance: 70, WeeklyRate: 0, RiskLevel: "low", AssetClass: "close", Category: "朋友", Freq30: 20, DecayDays: 5},
			{ContactID: 2, Name: "客户张", Balance: 40, WeeklyRate: -8, RiskLevel: "mid", AssetClass: "transactional", Category: "同事", Freq30: 10, DecayDays: 20},
			{ContactID: 3, Name: "妈妈", Balance: 25, WeeklyRate: -1, RiskLevel: "high", AssetClass: "friend", Category: "家人", Freq30: 8, DecayDays: 40},
		},
	}
	if err := saveLifeStateCache(db, now, st); err != nil {
		t.Fatal(err)
	}

	if err := ProjectLifeForward(db, now, 90); err != nil {
		t.Fatal(err)
	}
	proj, _, err := GetCachedLifeProjection(db)
	if err != nil {
		t.Fatal(err)
	}
	if proj == nil || proj.HorizonDays != 90 {
		t.Fatalf("推演缓存不符: %+v", proj)
	}

	byID := map[int64]ContactProjection{}
	for _, p := range proj.Projections {
		byID[p.ContactID] = p
	}

	// cid2：现状每周 -8，90 天后应从 40 跌破阈值 15 → 判为将断
	if p := byID[2]; !p.WillBreak {
		t.Errorf("cid2 应判为将断: now=%d future=%d rate=%v", p.NowBalance, p.FutureBalance, p.WeeklyRate)
	}
	// cid1：稳定，不应断，且预测值≈现状
	if p := byID[1]; p.WillBreak || p.FutureBalance != 70 {
		t.Errorf("cid1 不该断且应持平: %+v", p)
	}

	if proj.BreakCount < 1 {
		t.Errorf("至少应有 1 段将断, got %d", proj.BreakCount)
	}

	// 三条策略，且「保高风险」相对「维持现状」应非负（多保住的关系数 ≥ 0，财富不减少）
	if len(proj.Strategies) != 3 {
		t.Fatalf("应有 3 条策略, got %d", len(proj.Strategies))
	}
	strByKey := map[string]StrategyResult{}
	for _, s := range proj.Strategies {
		strByKey[s.Key] = s
	}
	hr, ok := strByKey["touch_high_risk"]
	if !ok {
		t.Fatal("缺 touch_high_risk 策略")
	}
	if hr.SavedCount < 0 || hr.WealthDelta < -0.001 {
		t.Errorf("保高风险策略不应比现状更差: saved=%d delta=%v", hr.SavedCount, hr.WealthDelta)
	}
	if _, ok := strByKey["status_quo"]; !ok {
		t.Error("缺 status_quo 策略")
	}
	if _, ok := strByKey["shift_family"]; !ok {
		t.Error("缺 shift_family 策略")
	}
}

func TestProjectionDeterminism(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()
	st := &LifeState{
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
		Assets: []LifeAsset{
			{ContactID: 1, Name: "A", Balance: 60, WeeklyRate: -5, RiskLevel: "mid", AssetClass: "friend", Category: "朋友", Freq30: 15, DecayDays: 10},
			{ContactID: 2, Name: "B", Balance: 30, WeeklyRate: -3, RiskLevel: "high", AssetClass: "acquaintance", Category: "家人", Freq30: 6, DecayDays: 30},
		},
	}
	if err := saveLifeStateCache(db, now, st); err != nil {
		t.Fatal(err)
	}

	if err := ProjectLifeForward(db, now, 90); err != nil {
		t.Fatal(err)
	}
	p1, _, _ := GetCachedLifeProjection(db)
	if err := ProjectLifeForward(db, now, 90); err != nil {
		t.Fatal(err)
	}
	p2, _, _ := GetCachedLifeProjection(db)

	if p1.NowWealth != p2.NowWealth || p1.FutureWealth != p2.FutureWealth || p1.BreakCount != p2.BreakCount {
		t.Fatalf("同输入应同输出: p1=%+v p2=%+v", p1, p2)
	}
	if len(p1.Projections) != len(p2.Projections) {
		t.Fatal("投影条数应一致")
	}
	for i := range p1.Projections {
		if p1.Projections[i].FutureBalance != p2.Projections[i].FutureBalance ||
			p1.Projections[i].WillBreak != p2.Projections[i].WillBreak {
			t.Fatalf("第 %d 条投影两次结果不一致", i)
		}
	}
}

func TestProjectionHorizonClamp(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()
	st := &LifeState{
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
		Assets:      []LifeAsset{{ContactID: 1, Name: "A", Balance: 50, AssetClass: "friend", Freq30: 5}},
	}
	if err := saveLifeStateCache(db, now, st); err != nil {
		t.Fatal(err)
	}
	// horizon<=0 → 回落到默认 90
	if err := ProjectLifeForward(db, now, 0); err != nil {
		t.Fatal(err)
	}
	p, _, _ := GetCachedLifeProjection(db)
	if p.HorizonDays != 90 {
		t.Errorf("horizon<=0 应回落 90, got %d", p.HorizonDays)
	}
	// horizon 超上限 → 夹到 horizonMax
	if err := ProjectLifeForward(db, now, 9999); err != nil {
		t.Fatal(err)
	}
	p, _, _ = GetCachedLifeProjection(db)
	if p.HorizonDays != horizonMax {
		t.Errorf("horizon 应夹到 %d, got %d", horizonMax, p.HorizonDays)
	}
}

func TestProjectionCacheRoundTrip(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()
	want := &LifeProjection{
		GeneratedAt:  now.Format("2006-01-02 15:04:05"),
		HorizonDays:  90,
		NowWealth:    100,
		FutureWealth: 80,
		BreakCount:   2,
		Projections:  []ContactProjection{{ContactID: 1, Name: "老王", NowBalance: 40, FutureBalance: 10, WillBreak: true}},
	}
	if err := saveLifeProjectionCache(db, now, want); err != nil {
		t.Fatal(err)
	}
	got, genAt, err := GetCachedLifeProjection(db)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.BreakCount != 2 || got.Projections[0].Name != "老王" {
		t.Fatalf("推演缓存回读不符: %+v", got)
	}
	if genAt.IsZero() {
		t.Fatal("generatedAt 不应为零值")
	}
}
