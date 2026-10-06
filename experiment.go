package main

import (
	"database/sql"
	"time"
)

// Personal Relationship Experiment 数据层（蓝图 §9 P5）。
//
// 把现有 intervention learning 升级为用户可自定义的「观察性关系实验」：定义目标/策略/避免项/
// 周期，系统据真实历史数据做前/后对照测量。§9 铁律：这是观察性实验、不是因果实验——结论只描述
// 相关性，绝不宣称「X 导致关系变好」。测量与结论逻辑见 experiment_measure.go。
//
// 锁纪律与本仓一致：顶层函数各自取 dbMu、单层、绝不嵌套；app 函数自锁、调用点在锁外。

// 实验状态（§9.1 status）。
const (
	ExpStatusDraft     = "draft"
	ExpStatusRunning   = "running"
	ExpStatusCompleted = "completed"
	ExpStatusAbandoned = "abandoned"
	ExpStatusCancelled = "cancelled"
)

// 实验结论（§9.3，四选一，绝不输出因果）。
const (
	ExpConclusionImproved     = "improved"
	ExpConclusionNoChange     = "no_change"
	ExpConclusionNegative     = "negative"
	ExpConclusionInsufficient = "insufficient"
)

var validExpStatuses = map[string]bool{
	ExpStatusDraft: true, ExpStatusRunning: true, ExpStatusCompleted: true,
	ExpStatusAbandoned: true, ExpStatusCancelled: true,
}

var validExpConclusions = map[string]bool{
	"":                        true,
	ExpConclusionImproved:     true,
	ExpConclusionNoChange:     true,
	ExpConclusionNegative:     true,
	ExpConclusionInsufficient: true,
}

// expTextMax 目标/策略等文本字段的最大字符数（按 rune 截断，保中文不产生 U+FFFD）。
const expTextMax = 200

// Experiment 一条关系实验记录（含前后测量快照与结论）。
type Experiment struct {
	ID             int64  `json:"id"`
	ContactID      int64  `json:"contact_id"`
	Goal           string `json:"goal"`
	Strategy       string `json:"strategy"`
	AvoidStrategy  string `json:"avoid_strategy"`
	Metrics        string `json:"metrics"` // JSON 数组字符串，列出关注指标
	DurationDays   int    `json:"duration_days"`
	StartDate      string `json:"start_date"`
	EndDate        string `json:"end_date"`
	Status         string `json:"status"`
	Conclusion     string `json:"conclusion"`
	ConclusionNote string `json:"conclusion_note"`
	BaselineJSON   string `json:"baseline_json"`
	ObservedJSON   string `json:"observed_json"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

// CreateExperimentInput 新建实验的入参。StartDate 为空则取今天；EndDate 由 start+duration 推出。
type CreateExperimentInput struct {
	ContactID     int64
	Goal          string
	Strategy      string
	AvoidStrategy string
	Metrics       string // 空则用默认指标集
	DurationDays  int
	StartDate     string
}

const expSelectCols = `id, contact_id, goal, strategy, avoid_strategy, metrics, duration_days,
	start_date, end_date, status, conclusion, conclusion_note, baseline_json, observed_json, created_at, updated_at`

func scanExperiment(s interface{ Scan(...interface{}) error }) (Experiment, error) {
	var e Experiment
	err := s.Scan(&e.ID, &e.ContactID, &e.Goal, &e.Strategy, &e.AvoidStrategy, &e.Metrics,
		&e.DurationDays, &e.StartDate, &e.EndDate, &e.Status, &e.Conclusion, &e.ConclusionNote,
		&e.BaselineJSON, &e.ObservedJSON, &e.CreatedAt, &e.UpdatedAt)
	return e, err
}

const defaultExpMetrics = `["interaction_volume","other_initiated","reply_latency","intimacy","health","state"]`

// CreateExperiment 校验后新建一条 draft 实验，返回其 id。
// 校验顺序：联系人存在 → duration>0 → 起止日期可解析（空 start 取今天，end=start+duration）。
func CreateExperiment(db *sql.DB, in CreateExperimentInput, now time.Time) (int64, error) {
	if in.DurationDays <= 0 {
		return 0, sql.ErrNoRows // 周期必须为正
	}
	start := in.StartDate
	if start == "" {
		start = now.Format("2006-01-02")
	}
	startT, err := time.Parse("2006-01-02", start)
	if err != nil {
		return 0, err
	}
	end := startT.AddDate(0, 0, in.DurationDays).Format("2006-01-02")
	metrics := in.Metrics
	if metrics == "" {
		metrics = defaultExpMetrics
	}
	nowStr := now.Format(time.RFC3339)

	dbMu.Lock()
	defer dbMu.Unlock()
	var one int
	if err := db.QueryRow(`SELECT 1 FROM contacts WHERE id=?`, in.ContactID).Scan(&one); err != nil {
		return 0, err
	}
	res, err := db.Exec(
		`INSERT INTO relationship_experiment (contact_id, goal, strategy, avoid_strategy, metrics, duration_days, start_date, end_date, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'draft', ?, ?)`,
		in.ContactID, clipRunes(in.Goal, expTextMax), clipRunes(in.Strategy, expTextMax),
		clipRunes(in.AvoidStrategy, expTextMax), metrics, in.DurationDays, start, end, nowStr, nowStr)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetExperiment 读取单条实验（自锁）；不存在返回 (nil, nil)。
func GetExperiment(db *sql.DB, id int64) (*Experiment, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "relationship_experiment") {
		return nil, nil
	}
	row := db.QueryRow(`SELECT `+expSelectCols+` FROM relationship_experiment WHERE id=?`, id)
	e, err := scanExperiment(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// ListExperiments 列出某联系人的实验（status 为空表示全部），按 id 倒序。
func ListExperiments(db *sql.DB, contactID int64, status string, limit int) ([]Experiment, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	out := []Experiment{}
	if !tableExistsLocked(db, "relationship_experiment") {
		return out, nil
	}
	q := `SELECT ` + expSelectCols + ` FROM relationship_experiment WHERE contact_id=?`
	args := []interface{}{contactID}
	if status != "" {
		q += ` AND status=?`
		args = append(args, status)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanExperiment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetExperimentStatus 迁移实验状态（§9.1）。running 之外均可自由切换；仅校验目标态合法。
func SetExperimentStatus(db *sql.DB, id int64, to string, now time.Time) error {
	if !validExpStatuses[to] {
		return sql.ErrNoRows
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	res, err := db.Exec(`UPDATE relationship_experiment SET status=?, updated_at=? WHERE id=?`, to, now.Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdateExperimentResult 落库一次测量结论（供 MeasureExperiment 调用）。结论枚举受 CHECK 约束，
// 由调用方保证只写相关性结论。未命中返回 sql.ErrNoRows。
func UpdateExperimentResult(db *sql.DB, id int64, conclusion, note, baselineJSON, observedJSON string, now time.Time) error {
	if !validExpConclusions[conclusion] {
		return sql.ErrNoRows
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	res, err := db.Exec(
		`UPDATE relationship_experiment SET conclusion=?, conclusion_note=?, baseline_json=?, observed_json=?, status='completed', updated_at=? WHERE id=?`,
		conclusion, clipRunes(note, 500), baselineJSON, observedJSON, now.Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
