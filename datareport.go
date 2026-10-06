package main

// v5.4.0 #9：数据健康自检报告（零 LLM、纯 SQL 聚合、确定性、可离线单测）。
//
// 给运维一张「系统运行状态」体检单：DB 体积、消息总量与近30天增速、画像覆盖率、
//   分组/时间线规模、各派生缓存表状态、最近备份结果。提升对系统运行状态的信任感。
//
// 设计铁律：
//   - 增量复用：派生缓存表清单直接读 backup.go 的 derivedTables，绝不另列一份（避免漂移）。
//   - 单连接池分层锁：所有计数在「一趟锁」内顺序读完即释放；表缺失静默标记 present=false。
//   - 只读、不落库、不新增表；DB 体积走 os.Stat(dbPath())，与 /api/status 磁盘口径互补。

import (
	"database/sql"
	"net/http"
	"os"
	"time"
)

// CacheTableStatus 一张派生缓存表的状态。
type CacheTableStatus struct {
	Table   string `json:"table"`
	Rows    int    `json:"rows"`
	Present bool   `json:"present"`
}

// DataReport 数据健康自检响应体。
type DataReport struct {
	GeneratedAt          string             `json:"generatedAt"`
	Version              string             `json:"version"`
	DBSizeBytes          int64              `json:"dbSizeBytes"`
	DBSizeMB             float64            `json:"dbSizeMb"`
	MessagesTotal        int                `json:"messagesTotal"`
	MessagesLast30d      int                `json:"messagesLast30d"`
	MessagesGrowthPerDay float64            `json:"messagesGrowthPerDay"`
	ContactsTotal        int                `json:"contactsTotal"`
	ProfiledContacts     int                `json:"profiledContacts"`
	ProfileCoverage      float64            `json:"profileCoverage"` // 0-100
	TaggedContacts       int                `json:"taggedContacts"`
	TimelineEvents       int                `json:"timelineEvents"`
	CacheTables          []CacheTableStatus `json:"cacheTables"`
	LastBackupAt         string             `json:"lastBackupAt"`
	LastBackupOK         bool               `json:"lastBackupOk"`
}

// buildDataReport 一趟锁内聚合各项数据健康指标。
func buildDataReport(db *sql.DB, now time.Time) *DataReport {
	r := &DataReport{
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
		Version:     appVersion,
		CacheTables: []CacheTableStatus{},
	}
	// DB 文件体积（不依赖锁）。
	if fi, err := os.Stat(dbPath()); err == nil {
		r.DBSizeBytes = fi.Size()
		r.DBSizeMB = round2(float64(fi.Size()) / 1024 / 1024)
	}

	since30 := now.AddDate(0, 0, -30).Format("2006-01-02")
	dbMu.Lock()
	// 自愈：刚升级/指标尚未建立时先全量重建，避免下面从 metrics 聚合计数的口径报 0。
	ensureDailyMetricsSeededLocked(db)
	// 消息总量 + 近30天：从 relationship_daily_metrics 聚合（归档感知，口径统一）。
	db.QueryRow(`SELECT COALESCE(SUM(me_count+other_count),0) FROM relationship_daily_metrics`).Scan(&r.MessagesTotal)
	db.QueryRow(`SELECT COALESCE(SUM(me_count+other_count),0) FROM relationship_daily_metrics WHERE day>=?`, since30).Scan(&r.MessagesLast30d)
	r.MessagesGrowthPerDay = round2(float64(r.MessagesLast30d) / 30)

	// 联系人与画像覆盖。
	db.QueryRow(`SELECT COUNT(*) FROM contacts WHERE merged_into IS NULL`).Scan(&r.ContactsTotal)
	db.QueryRow(`SELECT COUNT(*) FROM contacts WHERE merged_into IS NULL AND COALESCE(profile_json,'') != ''`).Scan(&r.ProfiledContacts)
	if r.ContactsTotal > 0 {
		r.ProfileCoverage = round2(float64(r.ProfiledContacts) / float64(r.ContactsTotal) * 100)
	}

	// 分组覆盖（已打标签的联系人去重计数）。
	if tagTableReadyLocked(db) {
		db.QueryRow(`SELECT COUNT(DISTINCT contact_id) FROM contact_tag_links`).Scan(&r.TaggedContacts)
	}
	// 时间线事件总量。
	if tableExistsLocked(db, "contact_events") {
		db.QueryRow(`SELECT COUNT(*) FROM contact_events`).Scan(&r.TimelineEvents)
	}

	// 派生缓存表状态：直接复用 derivedTables 清单，缺表 present=false、行数 0。
	for _, t := range derivedTables {
		st := CacheTableStatus{Table: t, Present: tableExistsLocked(db, t)}
		if st.Present {
			db.QueryRow(`SELECT COUNT(*) FROM "` + t + `"`).Scan(&st.Rows)
		}
		r.CacheTables = append(r.CacheTables, st)
	}

	// 最近一次备份结果。
	if tableExistsLocked(db, "backup_log") {
		var ok int
		db.QueryRow(`SELECT created_at, COALESCE(success,0) FROM backup_log ORDER BY id DESC LIMIT 1`).Scan(&r.LastBackupAt, &ok)
		r.LastBackupOK = ok == 1
	}
	dbMu.Unlock()
	return r
}

// hStatusDataReport GET /api/system/data-report：数据健康自检报告。
func (s *apiServer) hStatusDataReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	writeJSON(w, http.StatusOK, buildDataReport(s.db, time.Now()))
}

// routeSystem /api/system/* 子路由。
func (s *apiServer) routeSystem(w http.ResponseWriter, r *http.Request, sub []string) {
	if len(sub) == 0 {
		writeErr(w, http.StatusNotFound, "未知接口")
		return
	}
	switch sub[0] {
	case "data-report":
		s.hStatusDataReport(w, r)
	case "data-health": // V7 §21：数据健康总览 + 仅重建派生/缓存
		if len(sub) > 1 && sub[1] == "rebuild" {
			s.hDataHealthRebuild(w, r)
			return
		}
		s.hDataHealth(w, r)
	case "table-registry":
		s.hTableRegistry(w, r)
	default:
		writeErr(w, http.StatusNotFound, "未知接口: /api/system/"+sub[0])
	}
}

// hTableRegistry GET /api/system/table-registry：返回所有表的元数据 JSON 数组，供运维调试。
func (s *apiServer) hTableRegistry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	writeJSON(w, http.StatusOK, TableRegistry())
}
