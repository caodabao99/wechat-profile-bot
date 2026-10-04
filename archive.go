package main

// 消息归档：把超过保留期（默认 730 天 ≈ 2 年）的旧消息从 messages 表
// 搬到同库的 messages_archive 表，缩小活动表体积、加快日常查询。
//
// 设计要点：
//   - 归档表与主库同一个 SQLite 文件，备份（VACUUM INTO）天然覆盖，恢复
//     时随 backupTables 一起搬回，无需额外处理
//   - 归档/恢复都在单事务里完成，先 INSERT OR IGNORE 再按「确认已入档」
//     的条件 DELETE，即使 msg_hash 撞车也不会丢消息
//   - 恢复时联系人已不存在的消息留在归档表（孤儿），单独计数返回，
//     绝不写回 messages 造成外键悬空
//   - 不改动任何现有消息查询逻辑：归档后旧消息自然不再出现在常规查询里

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const (
	// defaultArchiveRetentionDays 活动消息默认保留天数（约 2 年）
	defaultArchiveRetentionDays = 730
	// archiveAutoInterval 自动归档调度周期
	archiveAutoInterval = 24 * time.Hour
)

// ---------- 配置（存 archive_settings 表，单行 JSON） ----------

// ArchiveSettings 归档配置
type ArchiveSettings struct {
	Enabled       bool `json:"enabled"`       // 是否每日自动归档
	RetentionDays int  `json:"retentionDays"` // 活动消息保留天数
}

func normalizeArchiveSettings(s ArchiveSettings) ArchiveSettings {
	if s.RetentionDays <= 0 {
		s.RetentionDays = defaultArchiveRetentionDays
	}
	if s.RetentionDays < 30 {
		s.RetentionDays = 30 // 下限 30 天，防止误配把近期消息也卷走
	}
	return s
}

func ensureArchiveTables(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	stmts := []string{
		// 归档表：与 messages 同构（无外键约束，恢复时才校验联系人存在性）
		`CREATE TABLE IF NOT EXISTS messages_archive (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			contact_id INTEGER NOT NULL,
			sender TEXT NOT NULL CHECK(sender IN ('me', 'other')),
			content TEXT NOT NULL,
			msg_hash TEXT NOT NULL,
			msg_time DATETIME,
			msg_unix INTEGER,
			captured_at DATETIME,
			archived_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(contact_id, msg_hash)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_archive_contact ON messages_archive(contact_id)`,
		`CREATE TABLE IF NOT EXISTS archive_settings (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			settings_json TEXT NOT NULL,
			last_run_at TEXT,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	// v8 兼容：旧归档表可能没有 msg_unix。列不存在时补列并分批回填（仅本次真正
	// 加列时回填，避免每次启动全表扫描），并补齐与 messages 对齐的复合索引。
	var colCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('messages_archive') WHERE name='msg_unix'`).
		Scan(&colCount); err != nil {
		return err
	}
	if colCount == 0 {
		if _, err := db.Exec(`ALTER TABLE messages_archive ADD COLUMN msg_unix INTEGER`); err != nil {
			return err
		}
		if err := backfillMsgUnix(db, "messages_archive"); err != nil {
			return err
		}
	}
	for _, idx := range []string{
		`CREATE INDEX IF NOT EXISTS idx_messages_archive_contact_id ON messages_archive(contact_id, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_archive_contact_unix ON messages_archive(contact_id, msg_unix DESC, id DESC)`,
	} {
		if _, err := db.Exec(idx); err != nil {
			return err
		}
	}
	// v9 兼容：为归档表建立 external-content FTS5 索引（与 messages 对等）。
	// 幂等、失败只降级不报错；建好后归档/恢复经触发器自动同步，搜索可选覆盖归档表。
	ensureFTSFor(db, "messages_archive", &ftsArchiveEnabled)
	return nil
}

func loadArchiveSettings(db *sql.DB) (ArchiveSettings, error) {
	s := ArchiveSettings{Enabled: false, RetentionDays: defaultArchiveRetentionDays}
	dbMu.Lock()
	defer dbMu.Unlock()
	var raw string
	err := db.QueryRow(`SELECT settings_json FROM archive_settings WHERE id = 1`).Scan(&raw)
	if err == sql.ErrNoRows {
		return normalizeArchiveSettings(s), nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return s, err
	}
	return normalizeArchiveSettings(s), nil
}

// writeArchiveSettings 只做 UPSERT 本身，不加锁、不开事务；调用方负责锁或事务。
func writeArchiveSettings(ex sqlExec, s ArchiveSettings) error {
	s = normalizeArchiveSettings(s)
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = ex.Exec(
		`INSERT INTO archive_settings (id, settings_json, updated_at) VALUES (1, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(id) DO UPDATE SET settings_json = excluded.settings_json, updated_at = CURRENT_TIMESTAMP`,
		string(raw))
	return err
}

func saveArchiveSettings(db *sql.DB, s ArchiveSettings) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	return writeArchiveSettings(db, s)
}

func archiveLastRunAt(db *sql.DB) string {
	dbMu.Lock()
	defer dbMu.Unlock()
	var v sql.NullString
	if err := db.QueryRow(`SELECT last_run_at FROM archive_settings WHERE id = 1`).Scan(&v); err != nil {
		return ""
	}
	return v.String
}

// touchArchiveLastRun 记录最近一次归档时间（upsert，配置行不存在也能写）。
// 注意：调用方必须已持有 dbMu（RunArchive 在事务提交后同锁内调用）。
func touchArchiveLastRun(db *sql.DB, now string) {
	db.Exec(`INSERT INTO archive_settings (id, settings_json, last_run_at) VALUES (1, '{}', ?)
		ON CONFLICT(id) DO UPDATE SET last_run_at = excluded.last_run_at`, now)
}

// ---------- 归档 / 恢复 ----------

// archiveCutoff 返回归档截止时刻（早于它的消息可归档）
func archiveCutoff(days int) string {
	return time.Now().AddDate(0, 0, -days).Format(time.RFC3339)
}

// archiveEligibleCond msg_time 可解析且早于 cutoff 的行才允许归档；
// msg_time 为空/非法的历史数据原地保留，宁可少归不可错归
const archiveEligibleCond = `msg_time IS NOT NULL AND msg_time != '' AND strftime('%s', msg_time) < strftime('%s', ?)`

// RunArchive 把超过 days 天的消息移入归档表，返回实际归档条数。
// days <= 0 时使用配置值。
func RunArchive(db *sql.DB, days int) (int64, error) {
	if days <= 0 {
		s, err := loadArchiveSettings(db)
		if err != nil {
			return 0, err
		}
		days = s.RetentionDays
	}
	cutoff := archiveCutoff(days)
	now := time.Now().Format(time.RFC3339)

	dbMu.Lock()
	defer dbMu.Unlock()
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// 1. 复制入档（撞 hash 的忽略，随后 DELETE 的 EXISTS 条件保证它们也会被清掉，不丢不重）
	// 连 id 一起搬：两张表都是 AUTOINCREMENT，rowid 不会被复用，
	// 保留原 id 才能让 merge_log 里按 id 记录的消息搬迁/回滚在一轮归档之后依然有效
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO messages_archive
		   (id, contact_id, sender, content, msg_hash, msg_time, msg_unix, captured_at, archived_at)
		 SELECT id, contact_id, sender, content, msg_hash, msg_time, msg_unix, captured_at, ?
		 FROM messages WHERE `+archiveEligibleCond, now, cutoff); err != nil {
		return 0, err
	}
	// 2. 只删除「确认已在归档表」的行
	res, err := tx.Exec(
		`DELETE FROM messages WHERE `+archiveEligibleCond+`
		   AND EXISTS (SELECT 1 FROM messages_archive a
		             WHERE a.contact_id = messages.contact_id AND a.msg_hash = messages.msg_hash)`,
		cutoff)
	if err != nil {
		return 0, err
	}
	moved, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	touchArchiveLastRun(db, time.Now().Format(time.RFC3339))
	if moved > 0 {
		slog.Info("消息归档完成", "moved", moved, "cutoff", cutoff)
	}
	return moved, nil
}

// ArchiveRestoreResult 一次恢复的结果
type ArchiveRestoreResult struct {
	Restored int64 `json:"restored"` // 已回到活跃表的条数（含主表本来就有同一条、直接出档的）
	Skipped  int64 `json:"skipped"`  // 联系人已不存在、留在归档表的孤儿条数
}

// RestoreArchive 把归档消息写回 messages。contactID 为 0 表示恢复全部。
// 联系人已不存在的消息不写回（避免外键悬空），留在归档表并计入 Skipped。
func RestoreArchive(db *sql.DB, contactID int64) (*ArchiveRestoreResult, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 1. 写回：仅联系人仍存在的行；撞 UNIQUE(contact_id,msg_hash) 的忽略
	// id 原样带回（入档时保留的就是 messages 里的原 id，AUTOINCREMENT 不会复用 rowid）
	res, err := tx.Exec(
		`INSERT OR IGNORE INTO messages (id, contact_id, sender, content, msg_hash, msg_time, msg_unix, captured_at)
		 SELECT a.id, a.contact_id, a.sender, a.content, a.msg_hash, a.msg_time, a.msg_unix, a.captured_at
		 FROM messages_archive a JOIN contacts c ON c.id = a.contact_id
		 WHERE (? = 0 OR a.contact_id = ?)`, contactID, contactID)
	if err != nil {
		return nil, err
	}
	inserted, _ := res.RowsAffected()

	// 2. 清档：联系人存在的行全部出档（含因重复被 IGNORE 的——主表已有同消息即视为恢复完成）
	res, err = tx.Exec(
		`DELETE FROM messages_archive WHERE (? = 0 OR contact_id = ?)
		   AND contact_id IN (SELECT id FROM contacts)`, contactID, contactID)
	if err != nil {
		return nil, err
	}
	deleted, _ := res.RowsAffected()

	// 3. 统计留下的孤儿
	var skipped int64
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM messages_archive WHERE (? = 0 OR contact_id = ?)
		   AND contact_id NOT IN (SELECT id FROM contacts)`, contactID, contactID).Scan(&skipped); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	slog.Info("归档消息已恢复", "restored", deleted, "writtenBack", inserted, "skippedOrphan", skipped, "contactId", contactID)
	return &ArchiveRestoreResult{Restored: deleted, Skipped: skipped}, nil
}

// ---------- 统计 ----------

// ArchiveStats 归档概览
type ArchiveStats struct {
	Enabled          bool   `json:"enabled"`
	RetentionDays    int    `json:"retentionDays"`
	ActiveMessages   int64  `json:"activeMessages"`
	ArchivedMessages int64  `json:"archivedMessages"`
	Eligible         int64  `json:"eligible"` // 当前满足归档条件的条数
	Oldest           string `json:"oldest"`   // 归档中最早消息时间
	Newest           string `json:"newest"`   // 归档中最新消息时间
	LastRunAt        string `json:"lastRunAt"`
}

func getArchiveStats(db *sql.DB) (*ArchiveStats, error) {
	s, err := loadArchiveSettings(db)
	if err != nil {
		return nil, err
	}
	st := &ArchiveStats{Enabled: s.Enabled, RetentionDays: s.RetentionDays, LastRunAt: archiveLastRunAt(db)}
	cutoff := archiveCutoff(s.RetentionDays)

	dbMu.Lock()
	defer dbMu.Unlock()
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&st.ActiveMessages); err != nil {
		return nil, err
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_archive`).Scan(&st.ArchivedMessages); err != nil {
		return nil, err
	}
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE `+archiveEligibleCond, cutoff).Scan(&st.Eligible); err != nil {
		return nil, err
	}
	var oldest, newest sql.NullString
	if err := db.QueryRow(
		`SELECT MIN(msg_time), MAX(msg_time) FROM messages_archive`).Scan(&oldest, &newest); err != nil {
		return nil, err
	}
	st.Oldest = oldest.String
	st.Newest = newest.String
	return st, nil
}

// ArchiveContactRow 归档按联系人聚合的一行
type ArchiveContactRow struct {
	ContactID int64  `json:"contactId"`
	Name      string `json:"name"`    // 联系人已删除时为空
	Deleted   bool   `json:"deleted"` // 联系人已不存在（孤儿归档）
	Count     int64  `json:"count"`   // 归档条数
	Oldest    string `json:"oldest"`
	Newest    string `json:"newest"`
}

func listArchiveByContact(db *sql.DB) ([]ArchiveContactRow, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	rows, err := db.Query(
		`SELECT a.contact_id, COALESCE(c.name, ''), c.id IS NULL, COUNT(*),
		        COALESCE((SELECT a2.msg_time FROM messages_archive a2
		                  WHERE a2.contact_id = a.contact_id AND COALESCE(a2.msg_time,'') != ''
		                  ORDER BY strftime('%s', a2.msg_time) ASC, a2.id ASC LIMIT 1), ''),
		        COALESCE((SELECT a3.msg_time FROM messages_archive a3
		                  WHERE a3.contact_id = a.contact_id AND COALESCE(a3.msg_time,'') != ''
		                  ORDER BY strftime('%s', a3.msg_time) DESC, a3.id DESC LIMIT 1), '')
		 FROM messages_archive a LEFT JOIN contacts c ON c.id = a.contact_id
		 GROUP BY a.contact_id ORDER BY COUNT(*) DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ArchiveContactRow{}
	for rows.Next() {
		var r ArchiveContactRow
		if err := rows.Scan(&r.ContactID, &r.Name, &r.Deleted, &r.Count, &r.Oldest, &r.Newest); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------- 自动归档调度 ----------

// startArchiveScheduler 每日自动归档（配置开启时才真正执行）。
// 启动 1 分钟后先跑一次，之后每 24 小时一次；单次归档是一个短事务，
// 对轮询主流程无影响。
// stopCh 关闭后调度器退出，返回的 channel 在 goroutine 真正结束时关闭，
// 供 main 在关库前等待，避免留下「库已关还在写事务」的 goroutine。
func startArchiveScheduler(db *sql.DB, stopCh <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait := func(d time.Duration) bool {
			select {
			case <-stopCh:
				return false
			case <-time.After(d):
				return true
			}
		}
		if !wait(time.Minute) {
			return
		}
		for {
			archiveTick(db)
			if !wait(archiveAutoInterval) {
				return
			}
		}
	}()
	return done
}

// archiveTick 跑一轮自动归档。panic 只废掉这一轮，绝不能把整个进程带走。
func archiveTick(db *sql.DB) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("自动归档任务异常，本轮跳过", "recover", r)
		}
	}()
	if s, err := loadArchiveSettings(db); err != nil {
		slog.Warn("自动归档：读取配置失败", "err", err)
	} else if s.Enabled {
		moved, err := RunArchive(db, s.RetentionDays)
		if err != nil {
			slog.Warn("自动归档失败", "err", err)
		} else if moved > 0 {
			slog.Info("自动归档完成", "moved", moved)
		}
	}
}

// ---------- HTTP 接口（/api/archive/*，需登录） ----------

// routeArchive /api/archive/{sub} 子路由
func (s *apiServer) routeArchive(w http.ResponseWriter, r *http.Request, sub []string) {
	switch {
	case len(sub) == 1 && sub[0] == "status" && r.Method == http.MethodGet:
		s.hArchiveStatus(w, r)
	case len(sub) == 1 && sub[0] == "run" && r.Method == http.MethodPost:
		s.hArchiveRun(w, r)
	case len(sub) == 1 && sub[0] == "restore" && r.Method == http.MethodPost:
		s.hArchiveRestore(w, r)
	case len(sub) == 1 && sub[0] == "settings":
		s.hArchiveSettings(w, r)
	default:
		writeErr(w, http.StatusNotFound, "未知归档接口")
	}
}

func (s *apiServer) hArchiveStatus(w http.ResponseWriter, r *http.Request) {
	stats, err := getArchiveStats(s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取归档状态失败: "+err.Error())
		return
	}
	byContact, err := listArchiveByContact(s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取归档明细失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"stats": stats, "byContact": byContact})
}

func (s *apiServer) hArchiveRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Days int `json:"days"` // 可选覆盖；0 表示用配置值
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil && err != io.EOF {
		// 空 body 允许（用配置值），但坏 body 不能装作没看见
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	moved, err := RunArchive(s.db, req.Days)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "归档失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "moved": moved})
}

func (s *apiServer) hArchiveRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ContactID int64 `json:"contactId"` // 0 = 恢复全部
	}
	// 解析失败必须报错：ContactID 的零值语义是「恢复全部」，
	// 一个坏请求静默变成全量恢复是不可接受的
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	result, err := RestoreArchive(s.db, req.ContactID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "恢复失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "result": result})
}

func (s *apiServer) hArchiveSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s2, err := loadArchiveSettings(s.db)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取配置失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s2)
	case http.MethodPost:
		var req struct {
			Enabled       *bool `json:"enabled"`
			RetentionDays *int  `json:"retentionDays"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体解析失败")
			return
		}
		cur, err := loadArchiveSettings(s.db)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取配置失败: "+err.Error())
			return
		}
		if req.Enabled != nil {
			cur.Enabled = *req.Enabled
		}
		if req.RetentionDays != nil {
			if *req.RetentionDays < 30 || *req.RetentionDays > 36500 {
				writeErr(w, http.StatusBadRequest, "保留天数需在 30 ~ 36500 之间")
				return
			}
			cur.RetentionDays = *req.RetentionDays
		}
		if err := saveArchiveSettings(s.db, cur); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存配置失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "settings": normalizeArchiveSettings(cur)})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}
