package main

// 统一指标层（Unified Metrics Layer）—— 把「按联系人、归档感知、窗口内」的互动统计
// 收敛到一处，供 coach / rhythm / timing 等洞察模块复用，避免每个模块各写各的 messages SQL。
//
// 设计铁律（与 relationship.go 同源）：
//   - 分层锁：GetAggregatedMetrics 自取 dbMu、读完即放；调用方（coach）绝不持 dbMu 再调它，
//     因此不构成嵌套加锁。缺表 messages_archive 时按 tableExistsLocked 降级只查主表。
//   - 一趟取数：一次按时间升序拉出 (sender, msg_unix) 流，在 Go 侧同时聚出小时直方与回复延迟，
//     不重复扫表。
//   - 纯读无副作用：不改写 relationship_daily_metrics（那是 RebuildDailyMetrics 的职责）。
//
// 口径：msg_unix 存的是秒级 epoch；小时一律按本地时区取（datetime(...,'localtime') 的等价 Go 写法），
// 与日聚合、时段建议保持同一口径，避免 UTC 折小时导致作息错位。

import (
	"database/sql"
	"sort"
	"time"
)

// Metrics 某联系人窗口内的聚合指标。coach 时机/节奏与作息签名都读这份。
type Metrics struct {
	ContactID       int         `json:"contactId"`
	WindowDay       int         `json:"windowDay"`
	OtherCount      int         `json:"otherCount"`      // 窗口内对方发言条数
	MeCount         int         `json:"meCount"`         // 窗口内我方发言条数
	OtherHourHist   map[int]int `json:"otherHourHist"`   // 对方发言小时直方（0-23，本地时区）
	MeHourHist      map[int]int `json:"meHourHist"`      // 我方发言小时直方
	ReplyLatencyP50 int         `json:"replyLatencyP50"` // 我方回复对方的间隔中位数（秒），0 表示样本不足
}

// GetAggregatedMetrics 计算某联系人近 windowDay 天的聚合指标（归档感知、窗口内、本地时区）。
// 无数据时返回一个空直方的 *Metrics（非 nil）与 nil error，调用方据此诚实降级。
func GetAggregatedMetrics(db *sql.DB, contactID int, windowDay int) (*Metrics, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	return getAggregatedMetricsLocked(db, contactID, windowDay)
}

func getAggregatedMetricsLocked(db *sql.DB, contactID int, windowDay int) (*Metrics, error) {
	if windowDay <= 0 {
		windowDay = 1
	}
	m := &Metrics{
		ContactID:     contactID,
		WindowDay:     windowDay,
		OtherHourHist: map[int]int{},
		MeHourHist:    map[int]int{},
	}

	since := time.Now().AddDate(0, 0, -windowDay).Unix()

	// 一趟取数：活跃表 (+ 归档表，若存在)，按时间升序拉 (sender, msg_unix)。
	src := `SELECT sender, msg_unix FROM messages
	        WHERE contact_id=? AND msg_unix IS NOT NULL AND msg_unix > 0 AND msg_unix >= ?`
	args := []interface{}{contactID, since}
	if tableExistsLocked(db, "messages_archive") {
		src += ` UNION ALL SELECT sender, msg_unix FROM messages_archive
		         WHERE contact_id=? AND msg_unix IS NOT NULL AND msg_unix > 0 AND msg_unix >= ?`
		args = append(args, contactID, since)
	}
	// UNION ALL 的派生表列名取自第一个 SELECT（sender, msg_unix），外层须用同名。
	q := `SELECT sender, msg_unix FROM (` + src + `) ORDER BY msg_unix ASC`

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var latencies []int64
	var lastOther int64 // 最近一条「尚未被我方回复」的对方发言时刻
	for rows.Next() {
		var sender string
		var ts int64
		if err := rows.Scan(&sender, &ts); err != nil {
			return nil, err
		}
		h := int(time.Unix(ts, 0).Local().Hour())
		switch sender {
		case "other":
			m.OtherCount++
			m.OtherHourHist[h]++
			// 记录最近一条待回复的对方发言（后到的会覆盖前一条，回复只算对最新一条的响应）。
			lastOther = ts
		case "me":
			m.MeCount++
			m.MeHourHist[h]++
			if lastOther > 0 {
				d := ts - lastOther
				if d >= 0 {
					latencies = append(latencies, d)
				}
				lastOther = 0
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	m.ReplyLatencyP50 = medianSeconds(latencies)
	return m, nil
}

// hasMessagesForRebuildLocked 判断某作用域（contactID>0 单人，<=0 全局）在活跃表或归档表里是否
// 仍有可归入自然日的消息，供「指标表为空则重建」的自愈门槛使用（蓝图 §10.2 历史统计归档感知）。
// 必须计入 messages_archive：长期沉默的人消息可能已全部归档，只看活跃表会误判为「无数据」而跳过
// 重建，令依赖 relationship_daily_metrics 的历史统计（累计互动/热力图/趋势/年度报告/主题）读 0。
// 门槛口径与各调用点原状一致：全局限带 msg_unix 的行（无时间戳无法归日），单人不限时间戳。
// 仅在持 dbMu 时调用（单连接池，内部只读不写、绝不嵌套）。
func hasMessagesForRebuildLocked(db *sql.DB, contactID int64) bool {
	anyPositive := func(q string, args ...interface{}) bool {
		var c int
		return db.QueryRow(q, args...).Scan(&c) == nil && c > 0
	}
	if contactID > 0 {
		if anyPositive(`SELECT COUNT(*) FROM messages WHERE contact_id=?`, contactID) {
			return true
		}
	} else if anyPositive(`SELECT COUNT(*) FROM messages WHERE msg_unix IS NOT NULL AND msg_unix > 0`) {
		return true
	}
	if !tableExistsLocked(db, "messages_archive") {
		return false
	}
	if contactID > 0 {
		return anyPositive(`SELECT COUNT(*) FROM messages_archive WHERE contact_id=?`, contactID)
	}
	return anyPositive(`SELECT COUNT(*) FROM messages_archive WHERE msg_unix IS NOT NULL AND msg_unix > 0`)
}

// ensureDailyMetricsSeededLocked 在持 dbMu 时确保日聚合指标非空：若 relationship_daily_metrics
// 为空但消息表（活跃或归档）里有带时间戳的消息，则全量重建一次（与 health/heatmap/GetRelationshipTrend 同语义）。
// 目的：让 status / data-report 等「从 metrics 聚合计数」的读路径，永不因刚升级、指标尚未
// 建立而报 0 条消息（同一人跨面板口径一致的护栏）。仅在持 dbMu 时调用。
func ensureDailyMetricsSeededLocked(db *sql.DB) {
	var mc int
	if db.QueryRow(`SELECT COUNT(*) FROM relationship_daily_metrics`).Scan(&mc) == nil && mc == 0 {
		if hasMessagesForRebuildLocked(db, 0) {
			_, _ = rebuildDailyMetricsLocked(db, 0)
		}
	}
}

// medianSeconds 纯函数：返回升序化后的中位数（秒，四舍五入取整）；空样本返回 0。确定性、可脱库单测。
func medianSeconds(vals []int64) int {
	if len(vals) == 0 {
		return 0
	}
	s := append([]int64{}, vals...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return int(s[mid])
	}
	return int((s[mid-1] + s[mid]) / 2)
}
