package main

// Action Ledger 自动结果观察（蓝图 §5.4）。
//
// 用户执行行动（status=acted/completed）后，系统在 7/14/30 天窗口据「真实关系数据」估算一个
// possible outcome：主信号是不依赖 LLM 的硬数据——执行前后互动量日均变化（relationship_daily_metrics），
// 与既有 suggestion_outcomes 回测同源、同阈值，保持确定性一致。
//
// 铁律（§5.3 / §5.4「不能混淆 / 不要声称因果」）：
//   - 估算一律 provenance=estimated，绝不写 confirmed（那是用户手工确认的专属）；
//   - note 明确「观察性估算、非因果证明」，措辞只描述相关变化，绝不宣称「因为这次行动所以回暖」；
//   - 数据稀疏、判断不了时诚实给 unknown，不硬编故事；
//   - 幂等：只处理 outcome_provenance 仍为空的记录，故已确认/已估算的不会被覆盖，用户确认永不被估算污染。
//
// 锁纪律：先在一把 dbMu 锁内一次读尽候选 + 前后互动量（单连接池下边迭代边写会自锁），Close 后释放，
// 再逐个调 SetActionOutcome（其内部自锁）落库——两处锁严格顺序、绝不嵌套。

import (
	"database/sql"
	"fmt"
	"time"
)

const (
	actionObserveMinDays   = 7   // 执行满 7 天才开始观察
	actionObserveMaxDays   = 30  // 前后对照窗口最大天数（日均归一）
	actionObserveRatioHigh = 1.3 // 日均互动比 > 此 → 偏正
	actionObserveRatioLow  = 0.7 // 日均互动比 < 此 → 偏负
)

// ObserveActionOutcomes 扫描到期未观察的已执行行动，估算并落库 outcome（provenance=estimated）。
// 返回本轮估算写入的条数。任何单条读取失败只跳过该条，绝不中断整体观察。
func ObserveActionOutcomes(db *sql.DB, now time.Time) (int, error) {
	cutoff := now.AddDate(0, 0, -actionObserveMinDays).Format(time.RFC3339)

	type cand struct {
		id, cid         int64
		preAvg, postAvg float64
		postDays        int
	}
	var cands []cand

	dbMu.Lock()
	if !tableExistsLocked(db, "relationship_action_log") {
		dbMu.Unlock()
		return 0, nil
	}
	rows, err := db.Query(
		`SELECT id, contact_id, acted_at FROM relationship_action_log
		 WHERE status IN ('acted','completed') AND outcome_provenance=''
		   AND acted_at != '' AND acted_at <= ?`, cutoff)
	if err != nil {
		dbMu.Unlock()
		return 0, fmt.Errorf("读取待观察行动失败: %w", err)
	}
	type raw struct {
		id, cid int64
		actedAt string
	}
	var todo []raw
	for rows.Next() {
		var r raw
		if err := rows.Scan(&r.id, &r.cid, &r.actedAt); err != nil {
			continue
		}
		todo = append(todo, r)
	}
	rows.Close()

	today := now.Format("2006-01-02")
	for _, r := range todo {
		actedTime, err := time.Parse(time.RFC3339, r.actedAt)
		if err != nil {
			continue
		}
		actedDay := actedTime.Format("2006-01-02")
		preFrom := actedTime.AddDate(0, 0, -actionObserveMaxDays).Format("2006-01-02")
		// 执行前窗口：[actedDay-30, actedDay)；执行后窗口：[actedDay, today]。
		var preTotal, postTotal int
		if err := db.QueryRow(
			`SELECT COALESCE(SUM(me_count+other_count),0) FROM relationship_daily_metrics
			 WHERE contact_id=? AND day>=? AND day<?`, r.cid, preFrom, actedDay).Scan(&preTotal); err != nil {
			continue
		}
		if err := db.QueryRow(
			`SELECT COALESCE(SUM(me_count+other_count),0) FROM relationship_daily_metrics
			 WHERE contact_id=? AND day>=? AND day<=?`, r.cid, actedDay, today).Scan(&postTotal); err != nil {
			continue
		}
		// 归一到日均：前窗口固定 30 天，后窗口取实际经过天数（夹到 [1,30]）。
		elapsed := int(now.Sub(actedTime).Hours() / 24)
		if elapsed < 1 {
			elapsed = 1
		}
		if elapsed > actionObserveMaxDays {
			elapsed = actionObserveMaxDays
		}
		cands = append(cands, cand{
			id: r.id, cid: r.cid,
			preAvg:   float64(preTotal) / float64(actionObserveMaxDays),
			postAvg:  float64(postTotal) / float64(elapsed),
			postDays: elapsed,
		})
	}
	dbMu.Unlock()

	observed := 0
	for _, c := range cands {
		outcome, note := estimateOutcome(c.preAvg, c.postAvg, c.postDays)
		// SetActionOutcome 内部自锁；此处必须处于锁外。estimated 永不覆盖 confirmed（内部亦拒）。
		if err := SetActionOutcome(db, c.id, outcome, ActionProvenanceEstimated, note, now); err != nil {
			continue // 并发下已被用户确认 → 覆盖被拒，跳过即可
		}
		observed++
	}
	return observed, nil
}

// estimateOutcome 由前后日均互动量估出极性与诚实 note。措辞只陈述观察到的变化，不宣称因果。
func estimateOutcome(preAvg, postAvg float64, postDays int) (string, string) {
	base := fmt.Sprintf("执行后 %d 天窗口内的观察性估算（互动量变化与行动相关，非因果证明）", postDays)
	switch {
	case preAvg <= 0 && postAvg <= 0:
		return ActionOutcomeUnknown, "前后窗口几乎无互动数据，无法判断，保持 unknown。" + base
	case preAvg <= 0 && postAvg > 0:
		return ActionOutcomePositive, "执行前无互动、执行后出现互动，关系呈回暖迹象。" + base
	case preAvg > 0 && postAvg <= 0:
		return ActionOutcomeNegative, "执行前有互动、执行后归零，互动趋于沉寂。" + base
	default:
		ratio := postAvg / preAvg
		switch {
		case ratio > actionObserveRatioHigh:
			return ActionOutcomePositive, fmt.Sprintf("执行后日均互动约为执行前的 %.1f 倍，互动上升。", ratio) + base
		case ratio < actionObserveRatioLow:
			return ActionOutcomeNegative, fmt.Sprintf("执行后日均互动降至执行前的 %.1f 倍，互动回落。", ratio) + base
		default:
			return ActionOutcomeNeutral, fmt.Sprintf("执行前后日均互动基本持平（比值 %.1f）。", ratio) + base
		}
	}
}
