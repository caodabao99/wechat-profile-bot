package main

// 对话质量历史趋势（增值功能，纯本地派生、零 LLM）。
//
// 把 ComputeContactQuality 每次算出的当周综合分，按 ISO 周幂等落一行进
// contact_quality_history，让「对话质量」长出时间维度：前端在雷达图旁画一条
// 综合分历史折线，直观量化关系变化的长期信号。
//
// 设计铁律（与 quality.go / trend.go 一致）：
//   - 全程本地 SQL + Go，不调模型、可离线单测、可复现。
//   - 单连接池：分层取 dbMu，绝不嵌套锁。upsert（写+裁剪）一趟、查询另一趟，各自释放。
//   - 惰性采集：仅在用户访问某联系人质量时追加当周快照，无需后台调度任务；
//     同周重复访问幂等覆盖（ON CONFLICT DO UPDATE），每联系人仅留近 qualityHistoryMaxWeeks 周。
//   - best-effort：写历史失败只记日志，绝不影响评分主响应（与 RecordContactEvent 同一容错口径）。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

const (
	qualityHistoryMaxWeeks    = 104 // 每联系人仅留近 104 周（约 2 年）
	qualityHistoryDefaultShow = 52  // 缺省返回近 52 周供画线
)

// QualityHistoryPoint 某周的综合分快照（映射 contact_quality_history 一行）。
type QualityHistoryPoint struct {
	WeekStart string       `json:"weekStart"`
	Score     int          `json:"score"`
	Dims      []QualityDim `json:"dims,omitempty"`
}

// ensureQualityHistory 懒建表（不 bump user_version，仿 ensurePromptTemplates 范式）；持单层 dbMu。
func ensureQualityHistory(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS contact_quality_history (
		contact_id INTEGER NOT NULL,
		week_start TEXT NOT NULL,
		score INTEGER NOT NULL,
		dims_json TEXT NOT NULL DEFAULT '',
		window_days INTEGER NOT NULL DEFAULT 0,
		generated_at TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (contact_id, week_start)
	)`); err != nil {
		return fmt.Errorf("建质量历史表失败: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_cqh_contact_week ON contact_quality_history(contact_id, week_start)`); err != nil {
		return fmt.Errorf("建质量历史索引失败: %w", err)
	}
	return nil
}

// upsertQualitySnapshot 幂等写入本周综合分快照，并裁剪仅留近 qualityHistoryMaxWeeks 周。
// 任何失败只记日志、不返回错误：历史是增值信息，不能因为它写不进去就让评分接口失败。
func upsertQualitySnapshot(db *sql.DB, q *ContactQuality) {
	if q == nil || q.ContactID <= 0 {
		return
	}
	dimsJSON, _ := json.Marshal(q.Dims)
	now := time.Now()
	ws := weekStartOf(now)
	dbMu.Lock()
	defer dbMu.Unlock()
	if _, err := db.Exec(
		`INSERT INTO contact_quality_history (contact_id, week_start, score, dims_json, window_days, generated_at)
		 VALUES (?,?,?,?,?,?)
		 ON CONFLICT(contact_id, week_start) DO UPDATE SET
		   score=excluded.score, dims_json=excluded.dims_json,
		   window_days=excluded.window_days, generated_at=excluded.generated_at`,
		q.ContactID, ws, q.Score, string(dimsJSON), q.WindowDays, q.GeneratedAt); err != nil {
		if !isNoSuchTable(err) {
			slog.Warn("写入对话质量历史失败", "contactId", q.ContactID, "err", err)
		}
		return
	}
	// 只留近两年（week_start 为 YYYY-MM-DD，字典序即时间序）。
	cutStr := weekStartOf(now.AddDate(0, 0, -7*qualityHistoryMaxWeeks))
	if _, err := db.Exec(`DELETE FROM contact_quality_history WHERE contact_id = ? AND week_start < ?`, q.ContactID, cutStr); err != nil {
		slog.Warn("裁剪对话质量历史失败", "contactId", q.ContactID, "err", err)
	}
}

// getQualityHistory 取某联系人近 weeks 周快照，返回按 week_start 升序（老→新）供画线。
func getQualityHistory(db *sql.DB, contactID int64, weeks int) ([]QualityHistoryPoint, error) {
	if weeks <= 0 {
		weeks = qualityHistoryDefaultShow
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "contact_quality_history") {
		return []QualityHistoryPoint{}, nil
	}
	rows, err := db.Query(`SELECT week_start, score, dims_json FROM contact_quality_history
		WHERE contact_id = ? ORDER BY week_start DESC LIMIT ?`, contactID, weeks)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pts := []QualityHistoryPoint{}
	for rows.Next() {
		var ws, dimsStr string
		var score int
		if err := rows.Scan(&ws, &score, &dimsStr); err != nil {
			continue
		}
		p := QualityHistoryPoint{WeekStart: ws, Score: score}
		if dimsStr != "" {
			var dims []QualityDim
			if json.Unmarshal([]byte(dimsStr), &dims) == nil {
				p.Dims = dims
			}
		}
		pts = append(pts, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// DESC 取最近 N 周后翻回升序，供前端画线。
	sort.Slice(pts, func(i, j int) bool { return pts[i].WeekStart < pts[j].WeekStart })
	return pts, nil
}

// isNoSuchTable 判断错误是否为「表不存在」（派生/懒建表未就绪时的良性情形）。
func isNoSuchTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}
