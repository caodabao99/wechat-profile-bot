package main

// v6.3 §P12 Smart Paste 增量去重统计可视化。
//
// 背景：用户经常重复粘贴同一段聊天记录（微信桌面端选中范围不好控制），
// SaveMessages 用 INSERT OR IGNORE 静默去重——用户体感是"粘了两次，数据没翻倍"，
// 但不知道到底去了多少、这个机制到底在帮多少忙。
//
// P12 做三件事：
//   1. 每次 ingest 后把 (parsed_count, new_count, dup_count) 落入 ingest_stats 轻量表。
//   2. GET /api/stats/ingest 返回聚合统计：总粘贴次数、总行数、去重率、按联系人 Top 5。
//   3. 前端在"系统"面板加一个小卡片展示。
//
// 全零迁移——ingest_stats 为 derived/cache 级（不入备份、可重建），表小（一次粘贴一行），
// 90 天自动清理。

import (
	"database/sql"
	"fmt"
	"net/http"
	"time"
)

// ensureIngestStatsTable 幂等建表。
func ensureIngestStatsTable(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS ingest_stats (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		contact_id INTEGER NOT NULL,
		parsed_count INTEGER NOT NULL,
		new_count INTEGER NOT NULL,
		dup_count INTEGER NOT NULL,
		recorded_at TEXT NOT NULL,
		UNIQUE(contact_id, recorded_at)
	)`)
	return err
}

// RecordIngestStat 在每次成功 ingest 后记一行。
func RecordIngestStat(db *sql.DB, contactID int64, parsed, newCnt int, now time.Time) {
	dup := parsed - newCnt
	if dup < 0 {
		dup = 0
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	db.Exec(`INSERT OR IGNORE INTO ingest_stats (contact_id, parsed_count, new_count, dup_count, recorded_at)
		VALUES (?, ?, ?, ?, ?)`, contactID, parsed, newCnt, dup, now.Format(time.RFC3339))
}

// IngestStatSummary 聚合统计结果。
type IngestStatSummary struct {
	TotalPastes   int                `json:"total_pastes"`
	TotalParsed   int                `json:"total_parsed"`
	TotalNew      int                `json:"total_new"`
	TotalDup      int                `json:"total_dup"`
	DedupRate     float64            `json:"dedup_rate"` // dup / parsed，0~1
	TopDuplicates []IngestTopContact `json:"top_duplicates"`
	WindowDays    int                `json:"window_days"`
}

type IngestTopContact struct {
	ContactID   int64  `json:"contact_id"`
	ContactName string `json:"contact_name"`
	DupCount    int    `json:"dup_count"`
	PasteCount  int    `json:"paste_count"`
}

// GetIngestStats 查询近 windowDays 天的聚合统计。
func GetIngestStats(db *sql.DB, now time.Time, windowDays int) (*IngestStatSummary, error) {
	if windowDays <= 0 {
		windowDays = 90
	}
	since := now.AddDate(0, 0, -windowDays).Format(time.RFC3339)

	dbMu.Lock()
	defer dbMu.Unlock()

	// 确保表存在
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS ingest_stats (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		contact_id INTEGER NOT NULL,
		parsed_count INTEGER NOT NULL,
		new_count INTEGER NOT NULL,
		dup_count INTEGER NOT NULL,
		recorded_at TEXT NOT NULL,
		UNIQUE(contact_id, recorded_at)
	)`); err != nil {
		return nil, err
	}

	s := &IngestStatSummary{WindowDays: windowDays}
	err := db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(parsed_count),0), COALESCE(SUM(new_count),0), COALESCE(SUM(dup_count),0)
		FROM ingest_stats WHERE recorded_at >= ?`, since).
		Scan(&s.TotalPastes, &s.TotalParsed, &s.TotalNew, &s.TotalDup)
	if err != nil {
		return nil, err
	}
	if s.TotalParsed > 0 {
		s.DedupRate = float64(s.TotalDup) / float64(s.TotalParsed)
	}

	// Top 5 去重最多的联系人
	rows, err := db.Query(`
		SELECT s.contact_id, c.name, SUM(s.dup_count), COUNT(*)
		FROM ingest_stats s JOIN contacts c ON c.id = s.contact_id
		WHERE s.recorded_at >= ? AND s.dup_count > 0
		GROUP BY s.contact_id ORDER BY SUM(s.dup_count) DESC LIMIT 5`, since)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var tc IngestTopContact
			if rows.Scan(&tc.ContactID, &tc.ContactName, &tc.DupCount, &tc.PasteCount) == nil {
				s.TopDuplicates = append(s.TopDuplicates, tc)
			}
		}
	}
	return s, nil
}

// PurgeOldIngestStats 清理超出保留期的行。
func PurgeOldIngestStats(db *sql.DB, now time.Time, keepDays int) (int64, error) {
	cutoff := now.AddDate(0, 0, -keepDays).Format(time.RFC3339)
	dbMu.Lock()
	defer dbMu.Unlock()
	res, err := db.Exec(`DELETE FROM ingest_stats WHERE recorded_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// routeStats 处理 /api/stats/... 路由。
func (s *apiServer) routeStats(w http.ResponseWriter, r *http.Request, sub []string) {
	if len(sub) == 1 && sub[0] == "ingest" && r.Method == http.MethodGet {
		window := 90
		if v := r.URL.Query().Get("window"); v != "" {
			fmt.Sscanf(v, "%d", &window)
		}
		st, err := GetIngestStats(s.db, time.Now(), window)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "统计查询失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "data": st})
		return
	}
	writeErr(w, http.StatusNotFound, "未知接口")
}
