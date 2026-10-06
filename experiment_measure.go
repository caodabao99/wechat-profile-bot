package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Relationship Experiment 测量与结论（蓝图 §9.2 / §9.3，Phase6b）。
//
// 观察性、非因果：据 relationship_daily_metrics 的真实历史互动数据，对「实验前基线窗口」与
// 「实验观察窗口」做日均对照（与行动账本 §5.4 同源、同阈值 1.3/0.7，保持确定性一致），再叠加
// 当前 relationship_state 快照（intimacy/health/state）。结论只描述相关性，绝不宣称因果。
//
// 锁纪律：本函数自锁一次读尽（实验行 + 前后互动量 + 状态快照，单连接池下先读尽再写），Close 释放
// 后，再调 UpdateExperimentResult（其内部自锁）落库——两处锁严格顺序、绝不嵌套。

// expMinObserveDays 观察窗口至少经过这么多天才给出极性结论，否则 insufficient。
const expMinObserveDays = 3

// expSnapshot 一次前/后测量快照。
type expSnapshot struct {
	From               string  `json:"from"`
	To                 string  `json:"to"`
	Days               int     `json:"days"`
	InteractionsPerDay float64 `json:"interactions_per_day"`
	OtherPerDay        float64 `json:"other_per_day"`
	Intimacy           int     `json:"intimacy,omitempty"`
	Health             int     `json:"health,omitempty"`
	State              string  `json:"state,omitempty"`
}

// windowInteractions 在持有 dbMu 时聚合 [fromDay, toDay) 半开区间内的日均互动（前窗口用）。
func windowInteractionsAvg(db *sql.DB, contactID int64, fromDay, toDay string, includeEnd bool) (interSum, otherSum, days int, err error) {
	op := "day<?"
	if includeEnd {
		op = "day<=?"
	}
	q := `SELECT COALESCE(SUM(me_count+other_count),0), COALESCE(SUM(other_count),0) FROM relationship_daily_metrics WHERE contact_id=? AND day>=? AND ` + op
	if err = db.QueryRow(q, contactID, fromDay, toDay).Scan(&interSum, &otherSum); err != nil {
		return
	}
	f, e1 := time.Parse("2006-01-02", fromDay)
	t, e2 := time.Parse("2006-01-02", toDay)
	if e1 != nil || e2 != nil {
		days = 1
		return
	}
	days = int(t.Sub(f).Hours() / 24)
	if includeEnd {
		days++
	}
	if days < 1 {
		days = 1
	}
	return
}

// concludeExperiment 由前后快照给出 §9.3 四类结论之一与相关性措辞（绝不因果）。
func concludeExperiment(base, obs expSnapshot) (string, string) {
	correlation := "（观察性对照，仅表示在你的历史数据中「该策略」与互动变化存在相关性，非因果证明）"
	if obs.Days < expMinObserveDays {
		return ExpConclusionInsufficient, fmt.Sprintf("观察窗口仅 %d 天，不足 %d 天，暂不下结论。", obs.Days, expMinObserveDays) + correlation
	}
	if base.InteractionsPerDay <= 0 && obs.InteractionsPerDay <= 0 {
		return ExpConclusionInsufficient, "实验前后几乎都无互动数据，无法判断，保持 insufficient。" + correlation
	}
	switch {
	case base.InteractionsPerDay <= 0 && obs.InteractionsPerDay > 0:
		return ExpConclusionImproved, "实验前无互动、实验后出现互动，关系呈回暖迹象。" + correlation
	case base.InteractionsPerDay > 0 && obs.InteractionsPerDay <= 0:
		return ExpConclusionNegative, "实验前有互动、实验后归零，互动趋于沉寂。" + correlation
	default:
		ratio := obs.InteractionsPerDay / base.InteractionsPerDay
		switch {
		case ratio > actionObserveRatioHigh:
			return ExpConclusionImproved, fmt.Sprintf("实验后日均互动约为实验前的 %.1f 倍，互动上升。", ratio) + correlation
		case ratio < actionObserveRatioLow:
			return ExpConclusionNegative, fmt.Sprintf("实验后日均互动降至实验前的 %.1f 倍，互动回落。", ratio) + correlation
		default:
			return ExpConclusionNoChange, fmt.Sprintf("实验前后日均互动基本持平（比值 %.1f）。", ratio) + correlation
		}
	}
}

// MeasureExperiment 对一条实验做前/后对照测量并落库结论；成功测量返回 (true,nil)，
// 实验不存在/处于 draft（尚未开始）返回 (false,nil)。可重复调用（每次据最新数据重算并覆盖结论）。
func MeasureExperiment(db *sql.DB, id int64, now time.Time) (bool, error) {
	var (
		e          Experiment
		found      bool
		base, obs  expSnapshot
		conclusion string
		note       string
		baselineJS string
		observedJS string
	)
	dbMu.Lock()
	if tableExistsLocked(db, "relationship_experiment") {
		row := db.QueryRow(`SELECT `+expSelectCols+` FROM relationship_experiment WHERE id=?`, id)
		if sc, err := scanExperiment(row); err == sql.ErrNoRows {
			// 不存在
		} else if err != nil {
			dbMu.Unlock()
			return false, err
		} else {
			e = sc
			found = true
		}
	}
	if !found {
		dbMu.Unlock()
		return false, nil
	}
	if e.Status == ExpStatusDraft { // 尚未开始，不测量
		dbMu.Unlock()
		return false, nil
	}
	startT, err1 := time.Parse("2006-01-02", e.StartDate)
	if _, err2 := time.Parse("2006-01-02", e.EndDate); err1 != nil || err2 != nil {
		dbMu.Unlock()
		return false, fmt.Errorf("实验起止日期无法解析: %s~%s", e.StartDate, e.EndDate)
	}
	today := now.Format("2006-01-02")
	if today < e.StartDate {
		// 还没到开始日：诚实给 insufficient
		dbMu.Unlock()
		if err := UpdateExperimentResult(db, id, ExpConclusionInsufficient, "实验尚未开始（未到 start_date），暂不测量。", "", "", now); err != nil {
			return false, err
		}
		return true, nil
	}
	winTo := e.EndDate
	if winTo > today {
		winTo = today // 观察窗口截至今天
	}
	baseFrom := startT.AddDate(0, 0, -e.DurationDays).Format("2006-01-02")

	// 前窗口 [baseFrom, start) 半开；观察窗口 [start, winTo] 闭。
	bi, bo, bd, err := windowInteractionsAvg(db, e.ContactID, baseFrom, e.StartDate, false)
	if err != nil {
		dbMu.Unlock()
		return false, err
	}
	oi, oo, od, err := windowInteractionsAvg(db, e.ContactID, e.StartDate, winTo, true)
	if err != nil {
		dbMu.Unlock()
		return false, err
	}
	base = expSnapshot{From: baseFrom, To: e.StartDate, Days: bd,
		InteractionsPerDay: float64(bi) / float64(bd), OtherPerDay: float64(bo) / float64(bd)}
	obs = expSnapshot{From: e.StartDate, To: winTo, Days: od,
		InteractionsPerDay: float64(oi) / float64(od), OtherPerDay: float64(oo) / float64(od)}
	// 当前状态快照（intimacy/health/state）附到观察侧，作为补充上下文（点时值，非前后差）。
	if st, _ := readStateLocked(db, e.ContactID); st != nil {
		obs.Intimacy, obs.Health, obs.State = st.Intimacy, st.Health, st.DynamicState
	}
	dbMu.Unlock()

	conclusion, note = concludeExperiment(base, obs)
	if b, err := json.Marshal(base); err == nil {
		baselineJS = string(b)
	}
	if o, err := json.Marshal(obs); err == nil {
		observedJS = string(o)
	}
	// UpdateExperimentResult 内部自锁；此处已在锁外。
	if err := UpdateExperimentResult(db, id, conclusion, note, baselineJS, observedJS, now); err != nil {
		return false, err
	}
	return true, nil
}
