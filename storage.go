package main

import (
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	_ "github.com/glebarez/go-sqlite" // 纯 Go SQLite 驱动，无需 CGO
)

// dbMu 串行化所有数据库操作（配合 WAL，避免 database is locked）
var dbMu sync.Mutex

// profileEpoch is guarded by dbMu; it is never restored from a backup.
var profileEpoch uint64

func currentProfileEpoch() uint64 {
	dbMu.Lock()
	defer dbMu.Unlock()
	return profileEpoch
}

// ProfileHistory 画像变更历史
type ProfileHistory struct {
	ID            int64
	ContactID     int64
	ProfileJSON   string
	ChangeSummary string
	CreatedAt     string
}

// ContactStats 联系人统计信息
type ContactStats struct {
	Total     int64
	Mine      int64
	Other     int64
	FirstTime string
	LastTime  string
}

// InitDB 打开（不存在则创建）SQLite 数据库并建表
func InitDB(path string) (*sql.DB, error) {
	// WAL 下 synchronous=NORMAL 既安全又显著降低写盘放大；temp_store=MEMORY、
	// cache_size(-8000≈8MB) 属保守调优，不会在低内存机器上造成问题。
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)&_pragma=cache_size(-8000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// 单连接 + WAL，手机/桌面混用场景最稳
	db.SetMaxOpenConns(1)

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS contacts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			remark TEXT,
			profile_json TEXT DEFAULT '{}',
			profile_summary TEXT DEFAULT '',
			other_msg_count INTEGER DEFAULT 0,
			last_updated DATETIME,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			contact_id INTEGER NOT NULL,
			sender TEXT NOT NULL CHECK(sender IN ('me', 'other')),
			content TEXT NOT NULL,
			msg_hash TEXT NOT NULL,
			msg_time DATETIME,
			captured_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(contact_id, msg_hash),
			FOREIGN KEY (contact_id) REFERENCES contacts(id)
		)`,
		`CREATE TABLE IF NOT EXISTS profile_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			contact_id INTEGER NOT NULL,
			profile_json TEXT NOT NULL,
			change_summary TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (contact_id) REFERENCES contacts(id)
		)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// migrate 执行数据库版本迁移（幂等）
func migrate(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}

	if version < 1 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()

		// 1. contacts 增加 merged_into 列
		var colCount int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('contacts') WHERE name='merged_into'`).
			Scan(&colCount); err != nil {
			return err
		}
		if colCount == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE contacts ADD COLUMN merged_into INTEGER DEFAULT NULL`); err != nil {
				return err
			}
		}

		// 2. 别名表
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS contact_aliases (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		contact_id INTEGER NOT NULL REFERENCES contacts(id),
		alias TEXT NOT NULL UNIQUE,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`CREATE INDEX IF NOT EXISTS idx_aliases_contact ON contact_aliases(contact_id)`); err != nil {
			return err
		}

		// 3. 合并日志表
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS merge_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_id INTEGER NOT NULL,
		target_id INTEGER NOT NULL,
		source_name TEXT NOT NULL,
		target_name TEXT NOT NULL,
		moved_message_ids TEXT DEFAULT '[]',
		moved_history_ids TEXT DEFAULT '[]',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		undone_at DATETIME DEFAULT NULL
	)`); err != nil {
			return err
		}

		if err := tx.Commit(); err != nil {
			return err
		}
		// PRAGMA user_version 必须在事务外执行
		if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
			return err
		}
	}

	if version < 2 {
		// v2: contacts 增加 profile_msg_count（上次生成画像时的对方消息数），
		// 用于「距离上次画像新增 N 条」的更新判断，替代原来的整除判断
		var colCount int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('contacts') WHERE name='profile_msg_count'`).
			Scan(&colCount); err != nil {
			return err
		}
		if colCount == 0 {
			if _, err := db.Exec(
				`ALTER TABLE contacts ADD COLUMN profile_msg_count INTEGER DEFAULT 0`); err != nil {
				return err
			}
			// 已有画像的联系人：以当前消息数为基线，避免升级后立即触发一次多余更新
			if _, err := db.Exec(
				`UPDATE contacts SET profile_msg_count = other_msg_count
				 WHERE profile_json IS NOT NULL AND profile_json != '' AND profile_json != '{}'`); err != nil {
				return err
			}
		}
		if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
			return err
		}
	}

	if version < 3 {
		// v3: 一次性自清洗。测试期曾用 v3 迁移把 last_updated 错误 +8 小时，
		// 导致出现"未来时间"（如 22:45 比当前还晚）。所有 last_updated 超过当前
		// 时间的值都是这种脏数据，统一重置为当前时间。
		// 正常写入的时间永远不会是未来时间，所以这个条件只命中脏数据。
		//
		// 必须挂在版本号下：早先它无条件跑在每次 InitDB 里，库变大后每次启动
		// 都要全表扫描一次 contacts，且迁移语义被冲淡。
		now := time.Now().Format(time.RFC3339)
		if _, err := db.Exec(`UPDATE contacts SET last_updated = ? WHERE last_updated > ?`, now, now); err != nil {
			return err
		}
		if _, err := db.Exec(`PRAGMA user_version = 3`); err != nil {
			return err
		}
	}

	if version < 4 {
		// v4: merge_log 增加 profile_copied，记录本次合并是否把 source 的画像
		// 拷贝给了 target。撤销合并时必须据此把 target 的画像清空，
		// 否则 target 会永久留着一份基于「source+target 合并消息集」生成的画像，
		// 而它的消息集在撤销后已经变回去了。
		var colCount int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('merge_log') WHERE name='profile_copied'`).
			Scan(&colCount); err != nil {
			return err
		}
		if colCount == 0 {
			if _, err := db.Exec(
				`ALTER TABLE merge_log ADD COLUMN profile_copied INTEGER DEFAULT 0`); err != nil {
				return err
			}
		}
		if _, err := db.Exec(`PRAGMA user_version = 4`); err != nil {
			return err
		}
	}

	if version < 5 {
		// v5: 备份/恢复操作日志。只追加不修改，恢复备份时本表不参与整体替换，
		// 保证「这台机器上发生过什么备份操作」的历史不会因恢复旧备份而丢失。
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS backup_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		action TEXT NOT NULL CHECK(action IN ('export', 'import')),
		source TEXT NOT NULL DEFAULT '',
		filename TEXT DEFAULT '',
		size_bytes INTEGER DEFAULT 0,
		detail TEXT DEFAULT '',
		success INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
			return err
		}
		if _, err := db.Exec(`PRAGMA user_version = 5`); err != nil {
			return err
		}
	}
	if version < 6 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, stmt := range []string{
			`ALTER TABLE merge_log ADD COLUMN deleted_messages TEXT DEFAULT '[]'`,
			`ALTER TABLE merge_log ADD COLUMN target_profile TEXT DEFAULT ''`,
			`PRAGMA user_version = 6`,
		} {
			if _, err := tx.Exec(stmt); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if version < 7 {
		// v7: merge_log 增加 undo_extra，存撤销合并所需的额外快照
		// （标签集合、被搬走的手动事件与待跟进、被搬走的归档消息）。
		// 老日志读出来是 '{}'，撤销时按「什么都没记」处理，行为与升级前一致。
		var colCount int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('merge_log') WHERE name='undo_extra'`).
			Scan(&colCount); err != nil {
			return err
		}
		if colCount == 0 {
			if _, err := db.Exec(`ALTER TABLE merge_log ADD COLUMN undo_extra TEXT DEFAULT '{}'`); err != nil {
				return err
			}
		}
		if _, err := db.Exec(`PRAGMA user_version = 7`); err != nil {
			return err
		}
	}
	if version < 8 {
		// v8: 数据库性能升级。为 messages 新增 msg_unix（Unix 秒），并建立覆盖
		// “按联系人 + 时间/分页” 常见查询的复合索引；旧数据分批回填，避免一个超长事务。
		//
		// 归档表 messages_archive 的同类列在 ensureArchiveTables 里维护：migrate() 早于
		// 它执行，此刻归档表可能尚不存在，不能在这里建归档索引/列。
		//
		// 幂等：列存在则跳过 ALTER；回填只处理 msg_unix IS NULL 的行，重复启动不会重复全表扫描。
		// 任一步失败都在 PRAGMA user_version=8 之前返回，下次启动整体重跑，不留半升级状态。
		var colCount int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name='msg_unix'`).
			Scan(&colCount); err != nil {
			return err
		}
		if colCount == 0 {
			if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN msg_unix INTEGER`); err != nil {
				return err
			}
		}
		if err := backfillMsgUnix(db, "messages"); err != nil {
			return err
		}
		for _, idx := range []string{
			`CREATE INDEX IF NOT EXISTS idx_messages_contact_id ON messages(contact_id, id DESC)`,
			`CREATE INDEX IF NOT EXISTS idx_messages_contact_unix ON messages(contact_id, msg_unix DESC, id DESC)`,
		} {
			if _, err := db.Exec(idx); err != nil {
				return err
			}
		}
		if _, err := db.Exec(`PRAGMA user_version = 8`); err != nil {
			return err
		}
	}
	if version < 9 {
		// v9: 全文搜索升级。为 messages 建立 external-content FTS5(trigram) 索引 + 同步触发器，
		// 并一次性 rebuild 回填历史数据。归档表 messages_archive 的同类索引在 ensureArchiveTables
		// 里维护（migrate() 早于它执行，此刻归档表可能尚不存在）。
		//
		// FTS 建立失败（极端情况：驱动不支持/虚表损坏）绝不让启动整体失败：只告警、把可用性
		// 开关置为降级，搜索自动退回 LIKE。无论成功与否都推进 user_version，避免每次启动重复
		// 尝试 rebuild 造成全表扫描（可用管理端「重建索引」接口手动重试）。
		ensureFTSFor(db, "messages", &ftsMessagesEnabled)
		if _, err := db.Exec(`PRAGMA user_version = 9`); err != nil {
			return err
		}
	}
	if version < 10 {
		// v10: 可信画像——把画像 JSON 里的离散断言（职业/城市/兴趣/重要日子/性格/重要事实/口头禅/亲密度）
		// 拆成结构化事实，供证据链与人工校对。status 取 active/retired：画像更新后不再出现的旧事实
		// 不删除而是标 retired，保留其历史与证据（可信度溯源不能断层）。幂等。
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS profile_facts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			contact_id INTEGER NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
			fact_type TEXT NOT NULL,
			fact_key TEXT NOT NULL DEFAULT '',
			fact_value TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT 'profile',
			confidence REAL NOT NULL DEFAULT 0.6,
			status TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active','retired')),
			first_seen TEXT NOT NULL DEFAULT '',
			last_seen TEXT NOT NULL DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(contact_id, fact_type, fact_key, fact_value)
		)`); err != nil {
			return err
		}
		for _, idx := range []string{
			`CREATE INDEX IF NOT EXISTS idx_profile_facts_contact ON profile_facts(contact_id, status)`,
		} {
			if _, err := db.Exec(idx); err != nil {
				return err
			}
		}
		if _, err := db.Exec(`PRAGMA user_version = 10`); err != nil {
			return err
		}
	}
	if version < 11 {
		// v11: 证据链——把每条事实链到支撑它的消息（含归档，archived 标记来源表）。
		// 外键 ON DELETE CASCADE：事实被硬删时证据跟着没；日常事实只标 retired，证据保留。
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS profile_fact_evidence (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			fact_id INTEGER NOT NULL REFERENCES profile_facts(id) ON DELETE CASCADE,
			contact_id INTEGER NOT NULL,
			message_id INTEGER NOT NULL,
			archived INTEGER NOT NULL DEFAULT 0,
			snippet TEXT NOT NULL DEFAULT '',
			msg_time TEXT NOT NULL DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(fact_id, message_id, archived)
		)`); err != nil {
			return err
		}
		for _, idx := range []string{
			`CREATE INDEX IF NOT EXISTS idx_fact_evidence_fact ON profile_fact_evidence(fact_id)`,
			`CREATE INDEX IF NOT EXISTS idx_fact_evidence_contact ON profile_fact_evidence(contact_id)`,
		} {
			if _, err := db.Exec(idx); err != nil {
				return err
			}
		}
		if _, err := db.Exec(`PRAGMA user_version = 11`); err != nil {
			return err
		}
	}
	if version < 12 {
		// v12: 关系变化检测——按联系人×自然日聚合互动量，为升温/降温预警提供不依赖 LLM 的硬数据。
		// 可随时从 messages 全量重建（RebuildDailyMetrics），故不需外键约束、不必入备份（派生数据）。
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS relationship_daily_metrics (
			contact_id INTEGER NOT NULL,
			day TEXT NOT NULL,
			me_count INTEGER NOT NULL DEFAULT 0,
			other_count INTEGER NOT NULL DEFAULT 0,
			first_unix INTEGER NOT NULL DEFAULT 0,
			last_unix INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(contact_id, day)
		)`); err != nil {
			return err
		}
		if _, err := db.Exec(`PRAGMA user_version = 12`); err != nil {
			return err
		}
	}
	if version < 13 {
		// v13: 下一步行动建议——由规则引擎（冷却/生日/未回/待跟进）产出，draft 可选地由 LLM 填充。
		// window_key 做幂等锁：同一联系同类同窗口不重复生成（例 cooling:2026-10）。
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS relationship_action_suggestions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			contact_id INTEGER NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
			kind TEXT NOT NULL,
			reason TEXT NOT NULL DEFAULT '',
			draft TEXT NOT NULL DEFAULT '',
			priority INTEGER NOT NULL DEFAULT 5,
			status TEXT NOT NULL DEFAULT 'open' CHECK(status IN ('open','done','dismissed')),
			window_key TEXT NOT NULL DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(contact_id, kind, window_key)
		)`); err != nil {
			return err
		}
		if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_action_sugg_contact ON relationship_action_suggestions(contact_id, status)`); err != nil {
			return err
		}
		if _, err := db.Exec(`PRAGMA user_version = 13`); err != nil {
			return err
		}
	}
	if version < 14 {
		// v14: 每周维护计划缓存 + 隐式反馈跟踪（建议被执行与回测结果）
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS weekly_plan_cache (
			id INTEGER PRIMARY KEY CHECK(id = 1),
			generated_at TEXT NOT NULL,
			items_json TEXT NOT NULL
		)`); err != nil {
			return err
		}
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS suggestion_outcomes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			suggestion_id INTEGER NOT NULL REFERENCES relationship_action_suggestions(id) ON DELETE CASCADE,
			contact_id INTEGER NOT NULL,
			acted_at TEXT NOT NULL,
			trend_before INTEGER NOT NULL DEFAULT 0,
			outcome TEXT NOT NULL DEFAULT 'pending'
				CHECK(outcome IN ('pending','improved','stable','worsened')),
			trend_after INTEGER NOT NULL DEFAULT 0,
			checked_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
			return err
		}
		if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_suggestion_outcome_pending ON suggestion_outcomes(outcome, acted_at)`); err != nil {
			return err
		}
		if _, err := db.Exec(`PRAGMA user_version = 14`); err != nil {
			return err
		}
	}
	if version < 15 {
		// v15: 跨联系人关系图谱（从 profile_facts 派生的人际关联）
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS contact_connections (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			contact_a INTEGER NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
			contact_b INTEGER NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
			connection_type TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT '',
			confidence REAL NOT NULL DEFAULT 0.5,
			created_at TEXT NOT NULL,
			UNIQUE(contact_a, contact_b, connection_type)
		)`); err != nil {
			return err
		}
		if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_conn_a ON contact_connections(contact_a)`); err != nil {
			return err
		}
		if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_conn_b ON contact_connections(contact_b)`); err != nil {
			return err
		}
		if _, err := db.Exec(`PRAGMA user_version = 15`); err != nil {
			return err
		}
	}
	return nil
}

// backfillMsgUnix 分批把 msg_time 换算成 Unix 秒写入 msg_unix（仅补 NULL 行）。
// 按 id 窗口分批自动提交，避免百万级数据落入一个超长事务；首尾打印进度日志。
// table 只接受内部白名单常量，不接受任何外部输入，故拼接表名是安全的。
func backfillMsgUnix(db *sql.DB, table string) error {
	if table != "messages" && table != "messages_archive" {
		return fmt.Errorf("backfillMsgUnix: 未知表 %q", table)
	}
	var minID, maxID sql.NullInt64
	if err := db.QueryRow(`SELECT MIN(id), MAX(id) FROM `+table).Scan(&minID, &maxID); err != nil {
		return err
	}
	if !maxID.Valid {
		return nil // 空表，无需回填
	}
	const batch = int64(20000)
	start, end := minID.Int64, maxID.Int64
	var total int64
	slog.Info("msg_unix 回填开始", "table", table, "minID", start, "maxID", end)
	for lo := start; lo <= end; lo += batch {
		hi := lo + batch - 1
		res, err := db.Exec(
			`UPDATE `+table+` SET msg_unix = CAST(strftime('%s', msg_time) AS INTEGER)
			 WHERE id >= ? AND id <= ? AND msg_unix IS NULL
			   AND msg_time IS NOT NULL AND msg_time != ''`, lo, hi)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			total += n
		}
	}
	slog.Info("msg_unix 回填完成", "table", table, "updated", total)
	return nil
}

// messageHash 计算消息去重哈希：发送方 + 时间 + 正文。
//
// ts 必须由调用方传入（即最终写入 msg_time 的那个值）。早先这里在 ts 为零值时
// 自己调 time.Now()，与 SaveMessages 里的 time.Now() 是两个不同时刻，导致
// hash 每次都不一样、UNIQUE(contact_id, msg_hash) 去重彻底失效，且 hash 里的
// 时间与库里的 msg_time 对不上。
//
// hasTime=false 表示这条消息没解析到真实时间（聊天记录里没带时间，或格式没认出来）。
// 此时去重键**不含时间**——否则用 time.Now() 兜底参与 hash，同一段无时间的记录
// 每次粘贴都会算出不同 hash，INSERT OR IGNORE 去不了重、other_msg_count 虚增。
// 有真实时间时仍纳入时间，避免同一内容在不同时刻发的两条被误判为重复。
func messageHash(m Message, ts time.Time, hasTime bool) string {
	key := m.Sender + ":" + m.Content
	if hasTime {
		key = m.Sender + ":" + ts.Format(time.RFC3339) + ":" + m.Content
	}
	sum := md5.Sum([]byte(key))
	return hex.EncodeToString(sum[:])
}

// parseMsgTime 解析库中存储的 msg_time（统一为 RFC3339）。
// 解析失败时回落当前时间，但会打印告警：静默改写时间会让数据错乱无从排查。
func parseMsgTime(raw string) time.Time {
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts
	}
	slog.Warn("msg_time 不是合法的 RFC3339，已回落为当前时间", "raw", raw)
	return time.Now()
}

// SaveMessages 保存一批消息（INSERT OR IGNORE 去重）。
// 返回实际新增条数；新增的对方消息同时累加 contacts.other_msg_count。
//
// 性能：整批在一个事务内提交（过去每条消息一次隐式提交，粘贴一大片时写盘放大严
// 重）；other_msg_count 不按条 UPDATE，而是累加后在末尾写一次。整批原子：
// 中途出错一律回滚，返回 0 与错误，不会留下“半批已入库”。
func SaveMessages(db *sql.DB, contactID int64, messages []Message) (int, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	newCount := 0
	newOther := 0
	for _, m := range messages {
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		ts := m.Timestamp
		hasTime := !ts.IsZero()
		if !hasTime {
			ts = time.Now()
		}
		res, err := tx.Exec(
			`INSERT OR IGNORE INTO messages (contact_id, sender, content, msg_hash, msg_time, msg_unix)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			contactID, m.Sender, content, messageHash(m, ts, hasTime), ts.Format(time.RFC3339), ts.Unix())
		if err != nil {
			return 0, err
		}
		if affected, _ := res.RowsAffected(); affected > 0 {
			newCount++
			if m.Sender == "other" {
				newOther++
			}
		}
	}
	if newOther > 0 {
		if _, err := tx.Exec(
			`UPDATE contacts SET other_msg_count = other_msg_count + ? WHERE id = ?`,
			newOther, contactID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newCount, nil
}

// scanMessages 从 rows 扫描消息并按时间正序（ASC）返回。
//
// 契约：传入的查询**必须**按 id DESC 排序，本函数负责把它反转成正序。
// 所有调用方都要遵守这一点，否则净结果会变成倒序（GetAllMessages 曾经就是这样，
// 用 ASC 查询再被这里反转，等于把最新在前的对话喂给了 LLM）。
func scanMessages(rows *sql.Rows) ([]Message, error) {
	defer rows.Close()
	var reversed []Message
	for rows.Next() {
		var sender, content, msgTime string
		if err := rows.Scan(&sender, &content, &msgTime); err != nil {
			return nil, err
		}
		reversed = append(reversed, Message{
			Sender:          sender,
			Content:         content,
			Timestamp:       parseMsgTime(msgTime),
			profileEpoch:    profileEpoch,
			profileSnapshot: true,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 反转为时间正序
	out := make([]Message, len(reversed))
	for i, m := range reversed {
		out[len(reversed)-1-i] = m
	}
	return out, nil
}

// GetRecentMessages 取最近 limit 条消息，返回时按时间正序排列
func GetRecentMessages(db *sql.DB, contactID int64, limit int) ([]Message, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	rows, err := db.Query(
		`SELECT sender, content, msg_time FROM messages
		 WHERE contact_id = ? ORDER BY id DESC LIMIT ?`,
		contactID, limit)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

// GetAllMessages 取该联系人全部消息（按时间正序 ASC），供画像生成 / 合并后重生成使用。
//
// 注意查询必须是 id DESC：scanMessages 会无条件反转，ASC 查询 + 反转 = 倒序，
// 这正是修复前的 bug（LLM 拿到的是最新在前的对话，画像质量受损，且与桌面端
// 远程模式 doGetAllMessages 的正序结果方向相反）。
func GetAllMessages(db *sql.DB, contactID int64) ([]Message, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	q := `SELECT sender, content, msg_time FROM messages WHERE contact_id = ? ORDER BY id DESC`
	args := []interface{}{contactID}
	if tableExistsLocked(db, "messages_archive") {
		// 归档会把老消息移出 messages，画像语料必须并入 messages_archive，否则用户点一次
		// 「立即归档」，下次画像就基于被截断的语料重生成。两表 id 序列各自独立，统一按消息时间
		// 倒序（最新在前，scanMessages 会再翻成正序）；无 msg_unix 的回落 strftime。
		q = `SELECT sender, content, msg_time FROM (
			SELECT sender, content, msg_time, COALESCE(msg_unix, CAST(strftime('%s', msg_time) AS INTEGER)) AS ord
				FROM messages WHERE contact_id = ?
			UNION ALL
			SELECT sender, content, msg_time, COALESCE(msg_unix, CAST(strftime('%s', msg_time) AS INTEGER))
				FROM messages_archive WHERE contact_id = ?
		) ORDER BY ord DESC, msg_time DESC`
		args = []interface{}{contactID, contactID}
	}

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

// GetMessagesPage 分页取消息（offset 从 0 开始，按时间倒序），用于画像窗口翻页
func GetMessagesPage(db *sql.DB, contactID int64, offset, limit int) ([]Message, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	rows, err := db.Query(
		`SELECT id, sender, content, msg_time FROM messages
		 WHERE contact_id = ? ORDER BY id DESC LIMIT ? OFFSET ?`,
		contactID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{} // 空结果也返回 [] 而非 nil（JSON null），前端才不会白屏
	for rows.Next() {
		var (
			id                       int64
			sender, content, msgTime string
		)
		if err := rows.Scan(&id, &sender, &content, &msgTime); err != nil {
			return nil, err
		}
		// 必须回填 ID：前端 keyset 翻页（“加载更多”）以末条 id 作游标，
		// id=0 会被当成假值导致永不带 beforeId、反复拉第一页。
		out = append(out, Message{ID: id, Sender: sender, Content: content, Timestamp: parseMsgTime(msgTime)})
	}
	return out, rows.Err()
}

// GetMessagesPageBefore 以 keyset（游标）分页取消息：返回严格早于 beforeID 的最近 limit 条，
// 按时间倒序（最新在前）。beforeID<=0 表示取最新一页。
//
// 相比 OFFSET：深翻页时 OFFSET N 要先扫描并丢弃 N 行，越翻越慢；keyset 用
// (contact_id, id) 复合索引直接定位到 id < beforeID，翻页成本恒定。走的是 Phase1 建的
// idx_messages_contact_id。结果携带 ID，供前端回传作为下一页游标。
func GetMessagesPageBefore(db *sql.DB, contactID, beforeID int64, limit int) ([]Message, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	q := `SELECT id, sender, content, msg_time FROM messages
		  WHERE contact_id = ?`
	args := []interface{}{contactID}
	if beforeID > 0 {
		q += ` AND id < ?`
		args = append(args, beforeID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{} // 空结果返回 [] 而非 nil（避免 JSON null 前端白屏）
	for rows.Next() {
		var (
			id                       int64
			sender, content, msgTime string
		)
		if err := rows.Scan(&id, &sender, &content, &msgTime); err != nil {
			return nil, err
		}
		out = append(out, Message{ID: id, Sender: sender, Content: content, Timestamp: parseMsgTime(msgTime)})
	}
	return out, rows.Err()
}

// GetContactByID 按 id 查询联系人
func GetContactByID(db *sql.DB, id int64) (*Contact, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	var c Contact
	var remark, profileJSON, profileSummary, lastUpdated sql.NullString
	err := db.QueryRow(
		`SELECT id, name, remark, profile_json, profile_summary,
		        other_msg_count, last_updated, created_at
		 FROM contacts WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &remark, &profileJSON, &profileSummary,
			&c.OtherMsgCount, &lastUpdated, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	c.Remark = remark.String
	c.ProfileJSON = profileJSON.String
	c.ProfileSummary = profileSummary.String
	c.LastUpdated = lastUpdated.String
	return &c, nil
}

// GetAllContacts 返回联系人列表，按最近更新时间倒序。
// includeMerged=true 时包含已合并的联系人。
func GetAllContacts(db *sql.DB, includeMerged bool) ([]Contact, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	query := `SELECT c.id, c.name, COALESCE(c.remark,''), COALESCE(c.profile_json,'{}'),
		        COALESCE(c.profile_summary,''), c.other_msg_count,
		        COALESCE(c.last_updated,''), c.created_at,
		        COALESCE(c.merged_into, 0),
		        (SELECT COUNT(*) FROM merge_log ml WHERE ml.target_id = c.id AND ml.undone_at IS NULL)
		 FROM contacts c`
	if !includeMerged {
		query += ` WHERE c.merged_into IS NULL`
	}
	// 用 strftime('%s', ...) 转成 epoch 再排序：last_updated 是 RFC3339 字符串，
	// 直接按字典序比较只在所有记录时区偏移相同时才成立。一旦混入 +08:00 与 Z
	// （例如库在容器里以 UTC 写过一段），字典序就会给出错误的先后关系。
	// 无法解析/NULL 的值 strftime 返回 NULL，在 DESC 下排最后，与原 '' 行为一致。
	query += ` ORDER BY strftime('%s', c.last_updated) DESC, c.id DESC`

	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Contact{} // 空结果也返回 [] 而非 nil（JSON null），前端才不会白屏
	for rows.Next() {
		var c Contact
		if err := rows.Scan(&c.ID, &c.Name, &c.Remark, &c.ProfileJSON,
			&c.ProfileSummary, &c.OtherMsgCount, &c.LastUpdated, &c.CreatedAt,
			&c.MergedInto, &c.MergeCount); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetContactsPage 分页查询联系人，返回当前页数据与符合条件的总数。
// q 非空时按昵称/备注/别名模糊匹配（LIKE 忽略 ASCII 大小写，中文天然精确）。
// GetAllContacts 保持全量语义不动（桌面端远程模式在用），网页端改用本函数避免全量渲染。
func GetContactsPage(db *sql.DB, includeMerged bool, q string, offset, limit int) ([]Contact, int, error) {
	return GetContactsPageFiltered(db, includeMerged, q, nil, offset, limit)
}

// GetContactsPageFiltered 与 GetContactsPage 相同，额外支持按标签筛选。
// tagIDs 非空时要求联系人**同时**带有全部这些标签（"且"关系）；为 nil 时行为与原来完全一致。
func GetContactsPageFiltered(db *sql.DB, includeMerged bool, q string, tagIDs []int64, offset, limit int) ([]Contact, int, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	if limit <= 0 {
		limit = 30
	}
	if limit > 200 {
		limit = 200 // 硬上限，避免一个请求把整库拉走
	}
	if offset < 0 {
		offset = 0
	}

	where := ""
	var args []interface{}
	if !includeMerged {
		where = ` WHERE c.merged_into IS NULL`
	}
	if q = strings.TrimSpace(q); q != "" {
		like := "%" + q + "%"
		cond := `(c.name LIKE ? OR COALESCE(c.remark,'') LIKE ?
			OR EXISTS (SELECT 1 FROM contact_aliases ca WHERE ca.contact_id = c.id AND ca.alias LIKE ?))`
		if where == "" {
			where = ` WHERE ` + cond
		} else {
			where += ` AND ` + cond
		}
		args = append(args, like, like, like)
	}
	if len(tagIDs) > 0 && !tagTableReadyLocked(db) {
		// 标签表还没建好（ensureTagTables 失败的老库）：没表就等于没人有标签，
		// 返回空列表比整页 500 更符合语义
		return []Contact{}, 0, nil
	}
	for _, tid := range tagIDs {
		cond := `EXISTS (SELECT 1 FROM contact_tag_links tl WHERE tl.contact_id = c.id AND tl.tag_id = ?)`
		if where == "" {
			where = ` WHERE ` + cond
		} else {
			where += ` AND ` + cond
		}
		args = append(args, tid)
	}

	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM contacts c`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `SELECT c.id, c.name, COALESCE(c.remark,''), COALESCE(c.profile_json,'{}'),
		        COALESCE(c.profile_summary,''), c.other_msg_count,
		        COALESCE(c.last_updated,''), c.created_at,
		        COALESCE(c.merged_into, 0),
		        (SELECT COUNT(*) FROM merge_log ml WHERE ml.target_id = c.id AND ml.undone_at IS NULL)
		 FROM contacts c` + where +
		// 排序依据同 GetAllContacts：strftime 转 epoch 再比，避免混入不同时区偏移时字典序出错
		` ORDER BY strftime('%s', c.last_updated) DESC, c.id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []Contact{} // 空结果也返回 [] 而非 nil（JSON null），前端才不会白屏
	for rows.Next() {
		var c Contact
		if err := rows.Scan(&c.ID, &c.Name, &c.Remark, &c.ProfileJSON,
			&c.ProfileSummary, &c.OtherMsgCount, &c.LastUpdated, &c.CreatedAt,
			&c.MergedInto, &c.MergeCount); err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// GetProfileHistory 取画像变更历史（最新在前）
func GetProfileHistory(db *sql.DB, contactID int64, limit int) ([]ProfileHistory, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Query(
		`SELECT id, contact_id, profile_json, COALESCE(change_summary,''), created_at
		 FROM profile_history WHERE contact_id = ?
		 ORDER BY id DESC LIMIT ?`, contactID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ProfileHistory{} // 空结果也返回 [] 而非 nil（JSON null），前端才不会白屏
	for rows.Next() {
		var h ProfileHistory
		if err := rows.Scan(&h.ID, &h.ContactID, &h.ProfileJSON,
			&h.ChangeSummary, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// SaveProfile 持久化最新画像并写入一条历史记录
func SaveProfile(db *sql.DB, contactID int64, profileJSON, summary, changeSummary string) error {
	if err := saveProfileAtEpoch(db, contactID, profileJSON, summary, changeSummary, currentProfileEpoch()); err != nil {
		return err
	}
	// 画像更新后同步刷新可信画像（事实 + 证据）。best-effort：失败只记日志，
	// 绝不让“画像保存成功”因派生表刷新失败而回滚为报错。
	if _, _, err := RebuildFactsAndEvidence(db, contactID); err != nil {
		slog.Warn("画像更新后刷新事实/证据失败（不影响画像保存）", "contactId", contactID, "err", err)
	}
	return nil
}

func saveProfileAtEpoch(db *sql.DB, contactID int64, profileJSON, summary, changeSummary string, epoch uint64) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	if epoch != profileEpoch {
		return ErrProfileStale
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var merged sql.NullInt64
	if err := tx.QueryRow(`SELECT merged_into FROM contacts WHERE id = ?`, contactID).Scan(&merged); err != nil {
		return err
	}
	if merged.Valid {
		return ErrProfileStale
	}

	// 统一用本地时间（不用 SQLite CURRENT_TIMESTAMP，避免存成 UTC 慢 8 小时）
	// 同时记录本次生成画像时的对方消息数，作为下次周期更新的基线
	now := time.Now().Format(time.RFC3339)
	if _, err := tx.Exec(`UPDATE contacts
		 SET profile_json = ?, profile_summary = ?, last_updated = ?,
		     profile_msg_count = other_msg_count
		 WHERE id = ?`, profileJSON, summary, now, contactID); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO profile_history (contact_id, profile_json, change_summary, created_at)
		 VALUES (?, ?, ?, ?)`, contactID, profileJSON, changeSummary, now); err != nil {
		return err
	}
	return tx.Commit()
}

// saveProfileHistory 只写一条历史记录（不改 contacts 当前画像），用于记录生成失败等事件
func saveProfileHistory(db *sql.DB, contactID int64, profileJSON, changeSummary string) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	if strings.TrimSpace(profileJSON) == "" {
		profileJSON = "{}"
	}
	now := time.Now().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO profile_history (contact_id, profile_json, change_summary, created_at)
		 VALUES (?, ?, ?, ?)`, contactID, profileJSON, changeSummary, now); err != nil {
		return err
	}
	return nil
}

// GetContactStats 返回统计页需要的计数与时间跨度
func GetContactStats(db *sql.DB, contactID int64) (ContactStats, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	var s ContactStats
	var first, last sql.NullString
	// 首/末消息时间不能用 MIN/MAX(msg_time)：msg_time 是 RFC3339 字符串，
	// MIN/MAX 走的是字典序，只在所有记录时区偏移一致时才等价于时间序。
	// 改为按 strftime('%s', msg_time) 排序取端点，返回的仍是原始字符串。
	// 必须并档 messages_archive：否则归档一次会使总数骤减、首聊日期突然跳到归档后的新日期。
	unit := `SELECT sender, msg_time FROM messages WHERE contact_id = ?`
	per := 1
	if tableExistsLocked(db, "messages_archive") {
		unit = `SELECT sender, msg_time FROM messages WHERE contact_id = ?
		        UNION ALL SELECT sender, msg_time FROM messages_archive WHERE contact_id = ?`
		per = 2
	}
	unitW := `(` + unit + `)`
	q := `SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN sender='me' THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN sender='other' THEN 1 ELSE 0 END), 0),
		        COALESCE((SELECT msg_time FROM ` + unitW + ` WHERE COALESCE(msg_time,'')!=''
		                  ORDER BY strftime('%s', msg_time) ASC, msg_time ASC LIMIT 1), ''),
		        COALESCE((SELECT msg_time FROM ` + unitW + ` WHERE COALESCE(msg_time,'')!=''
		                  ORDER BY strftime('%s', msg_time) DESC, msg_time DESC LIMIT 1), '')
		 FROM ` + unitW
	args := make([]interface{}, 0, 3*per)
	for i := 0; i < 3*per; i++ {
		args = append(args, contactID)
	}
	err := db.QueryRow(q, args...).Scan(&s.Total, &s.Mine, &s.Other, &first, &last)
	if err != nil {
		return s, err
	}
	s.FirstTime = first.String
	s.LastTime = last.String
	return s, nil
}
