package main

// 人生模拟器 · Phase B：90 天推演引擎。
//
// 设计原则：系统主动做，人只看结果 / 零操作减负。
//   - ProjectLifeForward 读人生状态，按当前互动趋势把每段关系「快进」到 horizon 天后
//   - 纯确定性外推、可解释（每条附 reason），零模型开销、同输入同输出
//   - what-if 由系统自动生成 3 条策略结论（维持现状 / 每周联系高风险清单一分钟 /
//     把时间移给家人），人只需看不需拨旋钮
//   - 缓存到 life_projection_cache，复用 weekly_plan / life_state 的派生缓存模式

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// ContactProjection 单段关系的 90 天走势。
type ContactProjection struct {
	ContactID      int64   `json:"contactId"`
	Name           string  `json:"name"`
	NowBalance     int     `json:"nowBalance"`
	FutureBalance  int     `json:"futureBalance"`
	WeeklyRate     float64 `json:"weeklyRate"`
	WillBreak      bool    `json:"willBreak"`
	SufficientData bool    `json:"sufficientData"`
	Reason         string  `json:"reason"`
}

// StrategyResult 一条自动 what-if 的结论。
type StrategyResult struct {
	Key         string  `json:"key"`
	Name        string  `json:"name"`
	SavedCount  int     `json:"savedCount"`  // 相比维持现状，多保住几段
	WealthDelta float64 `json:"wealthDelta"` // 相比现状，总财富变化
	Summary     string  `json:"summary"`
}

// LifeProjection 一次完整推演快照。
type LifeProjection struct {
	GeneratedAt  string              `json:"generatedAt"`
	HorizonDays  int                 `json:"horizonDays"`
	NowWealth    float64             `json:"nowWealth"`
	FutureWealth float64             `json:"futureWealth"` // 维持现状下的 horizon 天后
	BreakCount   int                 `json:"breakCount"`
	Projections  []ContactProjection `json:"projections"` // 有足够数据、按走势降序（最先断的在前）
	Strategies   []StrategyResult    `json:"strategies"`
}

const (
	breakThreshold    = 15  // 亲密度低于此判定「将断」
	projectDamping    = 0.5 // 每周互动变化 → 亲密度变化的阻尼系数
	horizonMax        = 365 // 推演窗口上限
	minHistoryForPred = 30  // 少于该历史天数不预测
)

// projectBalance 把当前亲密度按每周互动斜率（含策略加成）外推到 horizon 天后。
// 纯函数、确定性，供单测直接验证。
func projectBalance(balance int, weeklyRate float64, horizonDays int, boost float64) int {
	r := weeklyRate + boost
	delta := r / 7.0 * float64(horizonDays) * projectDamping
	f := float64(balance) + delta
	if f < 0 {
		f = 0
	}
	if f > 100 {
		f = 100
	}
	return int(f + 0.5)
}

// projRow 推演内部逐段关系的中间态。
type projRow struct {
	a       LifeAsset
	fb      int
	breaks  bool
	hasData bool
}

// ProjectLifeForward 重算 90 天推演并写入缓存。
func ProjectLifeForward(db *sql.DB, now time.Time, horizonDays int) error {
	if horizonDays <= 0 {
		horizonDays = 90
	}
	if horizonDays > horizonMax {
		horizonDays = horizonMax
	}

	assets, err := loadLifeAssets(db, now)
	if err != nil {
		return err
	}

	proj := &LifeProjection{
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
		HorizonDays: horizonDays,
	}

	// 逐段现状外推，并累计「维持现状」的组合财富
	nowWeighted, futureWeighted := 0.0, 0.0
	var rows []projRow
	for _, a := range assets {
		// 有往来历史才纳入资产账本；无近期活动且无深度分的直接跳过
		if a.Balance == 0 && a.Freq30 == 0 {
			continue
		}
		w := assetClassWeight(a.AssetClass)
		nowWeighted += float64(a.Balance) * w

		hasData := a.DecayDays <= horizonMax && (a.Freq30 > 0 || a.Balance > 0)
		fb := projectBalance(a.Balance, a.WeeklyRate, horizonDays, 0)
		breaks := hasData && a.Balance >= breakThreshold && fb < breakThreshold
		if hasData {
			futureWeighted += float64(fb) * w
		} else {
			futureWeighted += float64(a.Balance) * w
		}
		if breaks {
			proj.BreakCount++
		}

		reason := projectionReason(a, fb, horizonDays, hasData)
		proj.Projections = append(proj.Projections, ContactProjection{
			ContactID: a.ContactID, Name: a.Name, NowBalance: a.Balance, FutureBalance: fb,
			WeeklyRate: a.WeeklyRate, WillBreak: breaks, SufficientData: hasData, Reason: reason,
		})
		rows = append(rows, projRow{a: a, fb: fb, breaks: breaks, hasData: hasData})
	}

	proj.NowWealth = round1(nowWeighted)
	proj.FutureWealth = round1(futureWeighted)
	// 最先断/跌幅最大的排前
	sort.Slice(proj.Projections, func(i, j int) bool {
		di := proj.Projections[i].NowBalance - proj.Projections[i].FutureBalance
		dj := proj.Projections[j].NowBalance - proj.Projections[j].FutureBalance
		if di != dj {
			return di > dj
		}
		return proj.Projections[i].NowBalance > proj.Projections[j].NowBalance
	})
	if len(proj.Projections) > 20 {
		proj.Projections = proj.Projections[:20]
	}

	proj.Strategies = buildStrategies(rows, nowWeighted, horizonDays)
	return saveLifeProjectionCache(db, now, proj)
}

// projectionReason 生成可解释的中文理由。
func projectionReason(a LifeAsset, fb, horizon int, hasData bool) string {
	if !hasData {
		return "数据不足，暂不预测"
	}
	if a.WeeklyRate < -0.5 {
		return fmt.Sprintf("当前每周约 %.1f 条互动变化，%d 天后亲密度约 %d", a.WeeklyRate, horizon, fb)
	}
	if a.WeeklyRate > 0.5 {
		return fmt.Sprintf("互动在升温（每周约 +%.1f 条），%d 天后亲密度约 %d", a.WeeklyRate, horizon, fb)
	}
	return fmt.Sprintf("互动平稳，%d 天后亲密度约 %d", horizon, fb)
}

// buildStrategies 生成三条自动 what-if：维持现状 / 保高风险 / 移给家人。
func buildStrategies(rows []projRow, nowWeighted float64, horizon int) []StrategyResult {
	baseFuture := 0.0
	for _, rr := range rows {
		w := assetClassWeight(rr.a.AssetClass)
		if rr.hasData {
			baseFuture += float64(rr.fb) * w
		} else {
			baseFuture += float64(rr.a.Balance) * w
		}
	}

	// 策略二：每周联系高风险清单一分钟 → 高风险项每周 +3
	famFuture, famSaved := 0.0, 0
	for _, rr := range rows {
		w := assetClassWeight(rr.a.AssetClass)
		boost := 0.0
		if rr.a.RiskLevel == "high" {
			boost = 3.0
		}
		nfb := projectBalance(rr.a.Balance, rr.a.WeeklyRate, horizon, boost)
		if rr.hasData {
			famFuture += float64(nfb) * w
		} else {
			famFuture += float64(rr.a.Balance) * w
		}
		wasBreak := rr.hasData && rr.a.Balance >= breakThreshold && rr.fb < breakThreshold
		nowBreak := rr.hasData && rr.a.Balance >= breakThreshold && nfb < breakThreshold
		if wasBreak && !nowBreak {
			famSaved++
		}
	}

	// 策略三：把时间移给家人 → 家人类每周 +2
	shiftFuture, shiftSaved := 0.0, 0
	for _, rr := range rows {
		w := assetClassWeight(rr.a.AssetClass)
		boost := 0.0
		if rr.a.Category == "家人" {
			boost = 2.0
		}
		nfb := projectBalance(rr.a.Balance, rr.a.WeeklyRate, horizon, boost)
		if rr.hasData {
			shiftFuture += float64(nfb) * w
		} else {
			shiftFuture += float64(rr.a.Balance) * w
		}
		wasBreak := rr.hasData && rr.a.Balance >= breakThreshold && rr.fb < breakThreshold
		nowBreak := rr.hasData && rr.a.Balance >= breakThreshold && nfb < breakThreshold
		if wasBreak && !nowBreak {
			shiftSaved++
		}
	}

	out := []StrategyResult{
		{
			Key: "status_quo", Name: "什么都不做",
			SavedCount: 0, WealthDelta: 0,
			Summary: fmt.Sprintf("%d 天后人际总财富约 %.0f，预计 %d 段关系将自然断掉",
				horizon, round1(baseFuture), baseSavedCount(rows)),
		},
		{
			Key: "touch_high_risk", Name: "每周联系高风险清单一分钟",
			SavedCount: famSaved, WealthDelta: round1(famFuture - baseFuture),
			Summary: fmt.Sprintf("可多保住 %d 段关系，总财富 +%.0f", famSaved, famFuture-baseFuture),
		},
		{
			Key: "shift_family", Name: "把时间往家人身上移",
			SavedCount: shiftSaved, WealthDelta: round1(shiftFuture - baseFuture),
			Summary: fmt.Sprintf("可多保住 %d 段关系，总财富 +%.0f", shiftSaved, shiftFuture-baseFuture),
		},
	}
	return out
}

func baseSavedCount(rows []projRow) int {
	n := 0
	for _, rr := range rows {
		if rr.breaks {
			n++
		}
	}
	return n
}

// loadLifeAssets 取人生状态里的资产清单；缓存缺失/过期则先现算一次。
func loadLifeAssets(db *sql.DB, now time.Time) ([]LifeAsset, error) {
	st, genAt, err := GetCachedLifeState(db)
	if err != nil {
		return nil, err
	}
	if st == nil || IsLifeStateStale(genAt, now) {
		if err := ComputeLifeState(db, now); err != nil {
			return nil, err
		}
		st, _, err = GetCachedLifeState(db)
		if err != nil {
			return nil, err
		}
	}
	if st == nil {
		return []LifeAsset{}, nil
	}
	return st.Assets, nil
}

// ---------- 缓存读写 ----------

func saveLifeProjectionCache(db *sql.DB, now time.Time, proj *LifeProjection) error {
	data, err := json.Marshal(proj)
	if err != nil {
		return fmt.Errorf("序列化人生推演失败: %w", err)
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err = db.Exec(
		`INSERT INTO life_projection_cache (id, generated_at, proj_json) VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET generated_at=excluded.generated_at, proj_json=excluded.proj_json`,
		now.Format(time.RFC3339), string(data))
	return err
}

// GetCachedLifeProjection 读取推演缓存；无行返回 nil。
func GetCachedLifeProjection(db *sql.DB) (*LifeProjection, time.Time, error) {
	dbMu.Lock()
	var genAt, raw string
	err := db.QueryRow(`SELECT generated_at, proj_json FROM life_projection_cache WHERE id = 1`).Scan(&genAt, &raw)
	dbMu.Unlock()

	if err == sql.ErrNoRows {
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	var proj LifeProjection
	if err := json.Unmarshal([]byte(raw), &proj); err != nil {
		return nil, time.Time{}, fmt.Errorf("人生推演缓存解析失败: %w", err)
	}
	t, _ := time.Parse(time.RFC3339, genAt)
	return &proj, t, nil
}
