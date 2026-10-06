package main

// v5.4.0 #2：消息密度热力图（零 LLM、纯 SQL 按天聚合、可离线单测）。
//
// 联系人详情页展示某联系人全年消息密度（类似 GitHub 贡献图）：按自然日聚合互动量，
//   一眼看穿关系的「季节变化」。
//
// 设计铁律：
//   - 增量复用：数据直接来自 relationship_daily_metrics（派生、可随时重建）；沿用
//     GetRelationshipTrend 的自愈重建语义——该联系人指标为空但有消息时先就地重建。
//   - 单连接池分层锁：自愈判定 + 全年聚合在「一趟锁」内顺序读完即释放，绝不嵌套。
//   - 计算即读、不落库、不新增表。

import (
	"database/sql"
	"net/http"
	"strconv"
	"time"
)

// HeatmapDay 某天的互动量。
type HeatmapDay struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// HeatmapResponse 热力图响应体。
type HeatmapResponse struct {
	ContactID   int64        `json:"contactId"`
	Year        int          `json:"year"`
	Days        []HeatmapDay `json:"days"` // 仅含有互动的日子（前端按 53×7 网格着色，缺日视为 0）
	Max         int          `json:"max"`
	Total       int          `json:"total"`
	GeneratedAt string       `json:"generatedAt"`
}

// buildHeatmap 从日指标表聚合某联系人指定年份的每日互动量（me+other），day 升序。
func buildHeatmap(db *sql.DB, contactID int64, year int, now time.Time) *HeatmapResponse {
	resp := &HeatmapResponse{
		ContactID:   contactID,
		Year:        year,
		Days:        []HeatmapDay{},
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
	}
	from := strconv.Itoa(year) + "-01-01"
	to := strconv.Itoa(year) + "-12-31"

	dbMu.Lock()
	// 自愈：该联系人指标表为空但有消息（活跃或归档）时全量重建其日聚合（与 GetRelationshipTrend 同语义）。
	var mc int
	if db.QueryRow(`SELECT COUNT(*) FROM relationship_daily_metrics WHERE contact_id=?`, contactID).Scan(&mc) == nil && mc == 0 {
		if hasMessagesForRebuildLocked(db, contactID) {
			_, _ = rebuildDailyMetricsLocked(db, contactID)
		}
	}
	if rows, err := db.Query(
		`SELECT day, SUM(me_count+other_count) FROM relationship_daily_metrics
		 WHERE contact_id=? AND day>=? AND day<=? GROUP BY day ORDER BY day ASC`,
		contactID, from, to); err == nil {
		for rows.Next() {
			var d string
			var c int
			if rows.Scan(&d, &c) == nil {
				resp.Days = append(resp.Days, HeatmapDay{Date: d, Count: c})
				resp.Total += c
				if c > resp.Max {
					resp.Max = c
				}
			}
		}
		rows.Close()
	}
	dbMu.Unlock()
	return resp
}

// hContactHeatmap GET /api/contacts/{id}/heatmap?year=YYYY：某联系人全年消息密度热力图。
func (s *apiServer) hContactHeatmap(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	if _, err := GetContactByID(s.db, id); err != nil {
		writeErr(w, http.StatusNotFound, "联系人不存在")
		return
	}
	year := time.Now().Year()
	if yq := r.URL.Query().Get("year"); yq != "" {
		if y, err := strconv.Atoi(yq); err == nil && y >= 1970 && y <= 9999 {
			year = y
		}
	}
	writeJSON(w, http.StatusOK, buildHeatmap(s.db, id, year, time.Now()))
}
