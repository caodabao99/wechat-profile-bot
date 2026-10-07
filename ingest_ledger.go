package main

// ingest_ledger：消息收取的持久幂等账本（migration v28）。
//
// 为什么需要：修复「先去重后处理导致丢消息」时，把游标提交点后移到「整批处理成功之后」，
// 代价是服务端会**重放**已处理过的消息（这是 at-least-once 的定义，不是 bug）。
// 内存里 5 分钟滑窗挡不住重启后的重放，所以必须有持久账本：
//   - status='done'   → 已成功处理，重放时直接跳过；
//   - status='failed' → 处理失败（panic 等），attempts 累计；达到上限即视为毒丸，
//                       由调用方照常提交游标以免整批永久卡死，同时留下告警。
//
// 并发：本表只被消息主循环读写（单 goroutine），且 sql.DB 已限成单连接，
// 故不取 dbMu——避免与 migrate()/业务写入的锁顺序纠缠。

import (
	"database/sql"
	"fmt"
	"time"
)

// ingestMaxAttempts 是同一条消息允许的处理尝试次数；超过即放弃（防坏消息无限重放）。
const ingestMaxAttempts = 3

// ingestLedgerTTL 是账本行的保留期。
// 为什么是 30 而不是 7 天（投产前审计 N3）：重绑（-14 后重新扫码）会把游标归零，
// 而协议规定空/旧游标会重放其后的历史；账本只要比这个重放窗口短，超出部分就会被
// 重新跑一遍 LLM（内容靠 msg_hash 兜底不会重复入库，但额度白花）。30 天足以覆盖常见重放窗口，
// 且行数量级很小（每条消息一行），不致表胀。
const ingestLedgerTTL = 30 * 24 * time.Hour

// ensureIngestLedger 建表（幂等）。迁移里已建，这里供测试与老库热升级兜底。
func ensureIngestLedger(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS ingest_ledger(
		msg_key    TEXT PRIMARY KEY,
		from_user  TEXT NOT NULL DEFAULT '',
		status     TEXT NOT NULL DEFAULT 'done' CHECK(status IN ('done','failed')),
		attempts   INTEGER NOT NULL DEFAULT 1,
		last_error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_ingest_ledger_updated ON ingest_ledger(updated_at)`)
	return err
}

// ingestShouldProcess 判断这条消息是否还需要处理。
// 返回 process=false 的情况：已 done（重放，跳过即可）、或已达毒丸上限。
// attempts 回传既有尝试次数，供日志与告警措辞。
func ingestShouldProcess(db *sql.DB, key string) (process bool, attempts int) {
	if key == "" {
		return true, 0 // 无稳定键的消息不记账，照常处理（宁可重复也不漏）
	}
	var status string
	err := db.QueryRow(`SELECT status, attempts FROM ingest_ledger WHERE msg_key=?`, key).Scan(&status, &attempts)
	if err == sql.ErrNoRows {
		return true, 0
	}
	if err != nil {
		// 账本读不动（缺表等）时选择「照常处理」：重复入库由 msg_hash 去重兜底，
		// 而漏处理是真丢消息，两者代价不对称。
		return true, 0
	}
	if status == "done" {
		return false, attempts
	}
	return attempts < ingestMaxAttempts, attempts
}

// ingestMarkDone 记录一条消息已成功处理。
func ingestMarkDone(db *sql.DB, key, fromUser string) error {
	if key == "" {
		return nil
	}
	_, err := db.Exec(`INSERT INTO ingest_ledger(msg_key, from_user, status, attempts, last_error, updated_at)
		VALUES(?, ?, 'done', 1, '', datetime('now'))
		ON CONFLICT(msg_key) DO UPDATE SET status='done', attempts=ingest_ledger.attempts, last_error='', updated_at=datetime('now')`,
		key, fromUser)
	return err
}

// ingestMarkFailed 累加失败次数并记录原因，返回累计次数。
func ingestMarkFailed(db *sql.DB, key, fromUser, reason string) (int, error) {
	if key == "" {
		return 1, nil
	}
	var attempts int
	_, err := db.Exec(`INSERT INTO ingest_ledger(msg_key, from_user, status, attempts, last_error, updated_at)
		VALUES(?, ?, 'failed', 1, ?, datetime('now'))
		ON CONFLICT(msg_key) DO UPDATE SET status='failed', attempts=attempts+1, last_error=excluded.last_error, updated_at=datetime('now')`,
		key, fromUser, truncateReason(reason))
	if err != nil {
		return 0, err
	}
	if qerr := db.QueryRow(`SELECT attempts FROM ingest_ledger WHERE msg_key=?`, key).Scan(&attempts); qerr != nil {
		return 0, qerr
	}
	return attempts, nil
}

// ingestPoisoned 判断该键是否已达毒丸上限（调用方据此决定是否提交游标并告警）。
func ingestPoisoned(db *sql.DB, key string) bool {
	if key == "" {
		return false
	}
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM ingest_ledger WHERE msg_key=? AND status='failed' AND attempts>=?`,
		key, ingestMaxAttempts).Scan(&n)
	return err == nil && n > 0
}

// ingestPrune 删除超过保留期的行，返回删除数。定期调用防表膨胀。
// 用 datetime('now', '-N days') 而不是把时间当参数拼修饰符：后者对带时区偏移的
// RFC3339 字符串依赖 SQLite 版本解析，不如这种写法确定。
func ingestPrune(db *sql.DB) (int64, error) {
	res, err := db.Exec(`DELETE FROM ingest_ledger WHERE updated_at < datetime('now', ?)`,
		fmt.Sprintf("-%d days", int(ingestLedgerTTL.Hours()/24)))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// truncateReason 把错误原因压到可入库长度，避免超长 panic 栈塞满账本行。
func truncateReason(s string) string {
	r := []rune(s)
	if len(r) <= 300 {
		return s
	}
	return string(r[:300])
}
