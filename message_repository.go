package main

// 历史消息统一仓储层（V7 §16）。
//
// 这一层是「新模块取消息数据」的唯一命名入口。SQL、并表、时间口径、关键词匹配等
// 数据访问语义全部留在 history_source.go 单一实现里；本门面只负责表达意图
// （Recent / Historical / Search / Aggregate），不重复拼任何 SQL。
//
// 纪律：本文件必须保持 0 处直接查 messages 表的裸 SQL——TestNoGrowthOfRawMessagesQueries
// 会把任何新增文件里的直查表语句判为架构违规。想加取数能力，请扩 history_source.go
// 的原语，再在这里命名包装，绝不绕过仓储层直查表。
//
// §16.1 语义：
//   Recent     —— 只需近窗口（messages 即够）。这里复用与 Historical 相同的并表原语：
//                 不是妥协，而是归档出去的都是老消息，本就落不进近窗口，结果一致，
//                 且不再引入第二份 SQL。
//   Historical —— 必须并 messages 与归档表（走 history_source.go 的统一并表）。
//
// §16 统一 API 里的 SearchMessages 已由 search.go 的既有入口承担（FTS5 优先、失败降级 LIKE、
// 并覆盖 messages + messages_archive），它本身就是唯一检索真相。按单一来源纪律，本门面不再
// 重复定义同名函数、也不再造第二条关键词通路——要检索请直用 search.go:SearchMessages。

import (
	"database/sql"
	"time"
)

// recentDefaultDays 是 RecentMessages 未给窗口时的保守默认（与周报/近语料口径同量级）。
const recentDefaultDays = 30

// RecentMessages 返回某联系人最近 days 天内的消息，时间正序（最早在前）。
// contactID<=0 表示不限联系人；limit<=0 表示不限条数。
// 语义是「近期窗口」——底层走统一并表，归档的老消息自然落在窗口之外。
func RecentMessages(db *sql.DB, contactID int64, days int, limit int, now time.Time) ([]Message, error) {
	if days <= 0 {
		days = recentDefaultDays
	}
	f := HistoryFilter{
		ContactID:    contactID,
		SinceUnix:    now.AddDate(0, 0, -days).Unix(),
		WithTimeOnly: true, // 近窗口取数按时间口径过滤，空时间戳的行对「最近 N 天」无意义
	}
	return HistoryMessagesPlain(db, f, limit)
}

// HistoricalMessages 返回窗口内的完整历史（含归档）。§16 强制并表口径的唯一入口。
// 由调用方给出既有 HistoryFilter——门面不重新定义窗口语义，避免第二份时间口径。
func HistoricalMessages(db *sql.DB, f HistoryFilter, limit int) ([]HistoryMessage, error) {
	return HistoryMessages(db, f, limit)
}

// AggregateMessages 按本地自然日聚合窗口内消息条数（含归档）。
// 复用与明细同一套并表/时间口径，保证「列表看到的」与「聚合算出来的」永远一致。
func AggregateMessages(db *sql.DB, f HistoryFilter) ([]DayStat, error) {
	return HistoryAggregateByDay(db, f)
}
