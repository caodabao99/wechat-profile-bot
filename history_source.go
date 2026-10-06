package main

// 历史消息统一访问层（V6.3 §P9）。
//
// 为什么必须收口，而不是「以后有空再重构」：
// RunArchive 会把老消息从 messages **移动**到 messages_archive。任何只查 messages 的
// 时间窗口统计，在用户点一次「立即归档」之后就少算一段历史——而且不报错、不告警，
// 属于静默失真（周报、周期报告、生命状态、画像摘要都可能基于残缺语料算）。
// 仓里已经有人处理对了（GetAllMessages / timeline / achievements / relationship / metrics），
// 但那是同一段 UNION 样板被手抄了五遍——抄漏一次就出一个静默 bug。
//
// 本层是「按窗口取历史」的唯一实现：一次决定并表、时间口径、排序与来源标记。
// 时间口径全仓唯一：老数据可能没有 msg_unix，必须回落 strftime；否则那些行会被窗口
// 条件直接过滤掉（等价于丢数据）。
// 并表结果必须带来源标记：两张表的 id 序列互相独立，跨表 id 不可当作同一个标识。

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// historyTimeExpr 是唯一认可的消息时间表达式（unix 秒）。
const historyTimeExpr = `COALESCE(msg_unix, CAST(strftime('%s', msg_time) AS INTEGER))`

// HistoryFilter 描述一次历史取数的范围。零值 = 不加限制（全量）。
type HistoryFilter struct {
	ContactID int64 // 0 = 不限联系人
	SinceUnix int64 // 窗口下界（含）；0 = 不限
	UntilUnix int64 // 窗口上界（不含）；0 = 不限
	// WithTimeOnly 为 true 时只取有 msg_time 的行。做日期分桶/窗口聚合必须开：
	// 空时间戳的行落在哪个桶都没有意义，留着只会污染统计。
	WithTimeOnly bool
}

// sinceDays 便捷构造：最近 n 天（含）到此刻。
func sinceDays(n int) HistoryFilter {
	return HistoryFilter{SinceUnix: time.Now().AddDate(0, 0, -n).Unix(), WithTimeOnly: true}
}

// historyBranchWhere 按过滤器拼单张表的 WHERE，并按相同顺序追加 args。
// 窗口/联系人/时间存在性的语义只在这里定义一次，两条分支与上层拼 SQL 均复用它
// （本层存在的理由就是不允许第二份拷贝）。
func historyBranchWhere(f HistoryFilter, args *[]interface{}) string {
	var where []string
	if f.ContactID > 0 {
		where = append(where, "contact_id = ?")
		*args = append(*args, f.ContactID)
	}
	if f.WithTimeOnly {
		where = append(where, "msg_time IS NOT NULL AND msg_time != ''")
	}
	if f.SinceUnix > 0 {
		where = append(where, historyTimeExpr+" >= ?")
		*args = append(*args, f.SinceUnix)
	}
	if f.UntilUnix > 0 {
		where = append(where, historyTimeExpr+" < ?")
		*args = append(*args, f.UntilUnix)
	}
	if len(where) == 0 {
		return "1 = 1"
	}
	return strings.Join(where, " AND ")
}

// historySelectLocked 拼出并表后的查询：
//
//	SELECT <outerCols> FROM (<messages 分支> UNION ALL <messages_archive 分支>) x <tail>
//
// innerCols 是每张表内部要投影的列（可用别名），outerCols 在并表之后聚合/挑选，
// tail 放 GROUP BY / ORDER BY / LIMIT（作用于并表结果）。args 顺序与 SQL 一致：
// 先 messages 分支、再 messages_archive 分支。
//
// 调用方必须已持有 dbMu（与 tableExistsLocked 同纪律）；不在锁内的调用请用下面的
// HistoryMessages / HistoryCount 包装。
func historySelectLocked(db *sql.DB, innerCols, outerCols string, f HistoryFilter, tail string) (string, []interface{}) {
	branch := func(table string, args *[]interface{}) string {
		return fmt.Sprintf("SELECT %s FROM %s WHERE %s", innerCols, table, historyBranchWhere(f, args))
	}

	var args []interface{}
	part := branch("messages", &args)
	// 归档表可能尚未建立（老库从未归档过），必须守门后再 UNION，否则整条 SQL 直接报错。
	if tableExistsLocked(db, "messages_archive") {
		part += " UNION ALL " + branch("messages_archive", &args)
	}
	q := "SELECT " + outerCols + " FROM (" + part + ") x"
	if tail != "" {
		q += " " + tail
	}
	return q, args
}

// HistoryMessage 是一条带来源标记的历史消息。
// Archived 为 true 时，ID 属于 messages_archive——与 messages 的 id 序列互相独立，
// 引用类调用方（点击跳转到某条消息）必须先判断再决定是否直跳。
type HistoryMessage struct {
	Message
	Archived bool
}

// HistoryMessagesLocked 取窗口内的完整消息，按时间正序返回（最早在前）。
// limit <= 0 表示不限条数；正数时取「最近的 limit 条」再翻成正序（与既有 AI 语料口径一致）。
//
// 与其它入口的唯一区别：需要给每条标出来自哪张表（两表 id 序列独立）。
// 投影与排序写在这里，但窗口条件仍走 historyBranchWhere——不存在第二份时间口径。
func HistoryMessagesLocked(db *sql.DB, f HistoryFilter, limit int) ([]HistoryMessage, error) {
	// 用纯拼接而不是 fmt.Sprintf：historyTimeExpr 包含 strftime('%s', …)，
	// 经过 Sprintf 会把 %s 当格式动词吐掉后续参数。
	fetch := func(table string, archivedFlag int, acc *[]HistoryMessage) error {
		var args []interface{}
		where := historyBranchWhere(f, &args)
		q := "SELECT id, sender, content, COALESCE(msg_time, '') AS msg_time, " +
			strconv.Itoa(archivedFlag) + " AS archived, " +
			historyTimeExpr + " AS su FROM " + table + " WHERE " + where
		if limit > 0 {
			q = "SELECT * FROM (" + q + " ORDER BY su DESC LIMIT " + strconv.Itoa(limit) + ") t"
		} else {
			q += " ORDER BY su DESC"
		}
		rows, err := db.Query(q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var sender, content, msgTime string
			var arch, su int64
			if err := rows.Scan(&id, &sender, &content, &msgTime, &arch, &su); err != nil {
				return err
			}
			// 统一用 SQL 算好的 su（已做 COALESCE）作时间戳，避免 parseMsgTime 对非 RFC3339 格式
			// 的 SQLite 常见 datetime 文本误报（回落 time.Now）导致排序错乱。
			var ts time.Time
			if su > 0 {
				ts = time.Unix(su, 0)
			} else if msgTime != "" {
				ts = parseMsgTime(msgTime) // 极端回落：su 为 NULL 且有时间文本
			}
			*acc = append(*acc, HistoryMessage{
				Message:  Message{ID: id, Sender: sender, Content: content, Timestamp: ts},
				Archived: arch == 1,
			})
		}
		return rows.Err()
	}

	var out []HistoryMessage
	if err := fetch("messages", 0, &out); err != nil {
		return nil, err
	}
	if tableExistsLocked(db, "messages_archive") {
		if err := fetch("messages_archive", 1, &out); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:] // 两表各取了 limit 条，合并后仍裁到最近的 limit 条
	}
	return out, nil
}

// HistoryMessages 是 HistoryMessagesLocked 的自持锁版本（不在 dbMu 内的调用方用这个）。
func HistoryMessages(db *sql.DB, f HistoryFilter, limit int) ([]HistoryMessage, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	return HistoryMessagesLocked(db, f, limit)
}

// HistoryCountLocked 数窗口内消息条数（并表口径）。
func HistoryCountLocked(db *sql.DB, f HistoryFilter) (int64, error) {
	q, args := historySelectLocked(db, "1", "COUNT(*)", f, "")
	var n int64
	err := db.QueryRow(q, args...).Scan(&n)
	return n, err
}

// HistoryCount 是 HistoryCountLocked 的自持锁版本。
func HistoryCount(db *sql.DB, f HistoryFilter) (int64, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	return HistoryCountLocked(db, f)
}

// HistoryMessagesPlain 只要正文与 sender/时间（不要 archived 标记）时的便捷出口。
func HistoryMessagesPlain(db *sql.DB, f HistoryFilter, limit int) ([]Message, error) {
	hs, err := HistoryMessages(db, f, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Message, len(hs))
	for i, h := range hs {
		out[i] = h.Message
	}
	return out, nil
}
