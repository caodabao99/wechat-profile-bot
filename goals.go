package main

// v5.5.0 #7：关系目标追踪（零 LLM、确定性、复用 XP/等级体系与成就徽章口径）。
//
// 用户为某联系人自设一个可量化目标（如「本周主动发 5 条」），系统按周读互动日指标
//   relationship_daily_metrics 的 me_count 自动判定达标：达成即 +XP、升级、写一条
//   kind="milestone" 的 contact_events（与「维护挑战」划清语义：挑战=系统建议、目标=用户自设）。
//
// 设计铁律：
//   - 持久新表 relationship_goals：不加入 backup.go 的 derivedTables，依动态 listRestoreTables
//     自动纳入备份/恢复；懒 ensureGoals（CREATE TABLE IF NOT EXISTS）建表，不 bump user_version。
//   - 单连接池分层锁：读候选/写库/补事件顺序取放，绝不嵌套；RecordContactEvent 一律在未持锁时调用。
//   - 幂等：只处理 status='active' 的行，达成置 done 后不再回退、不重复加 XP。
//   - XP 复用：沿用 challengeXPPerDone / levelForXP / gamification_state（ensureGamification 幂等建）。

import (
	"database/sql"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

const (
	goalMaxTitleRunes  = 40  // 目标标题长度上限
	goalMaxTargetCount = 999 // 达标计数上限护栏
)

// 目标度量口径（当前支持按我方主动发言计数）。
const (
	goalMetricWeeklyMe = "weekly_me_messages" // 本 ISO 周我方发言条数达标
	goalMetricPeriodMe = "period_me_messages" // 指定周期内我方发言条数达标
)

// goalStatus* 目标状态。
const (
	goalStatusActive = "active"
	goalStatusDone   = "done"
)

// ensureGoals 懒建持久目标表（不 bump user_version、不入 derivedTables）。持单层 dbMu、幂等 DDL。
func ensureGoals(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS relationship_goals (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		contact_id INTEGER NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		metric TEXT NOT NULL DEFAULT 'weekly_me_messages',
		target_count INTEGER NOT NULL DEFAULT 1,
		period_start TEXT NOT NULL DEFAULT '',
		period_end TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'active',
		created_at TEXT NOT NULL DEFAULT '',
		done_at TEXT NOT NULL DEFAULT '')`)
	return err
}

// normalizeGoalMetric 纯函数：把外部 metric 归一到受支持口径，未知/空回落周口径。
func normalizeGoalMetric(m string) string {
	switch strings.TrimSpace(m) {
	case goalMetricPeriodMe:
		return goalMetricPeriodMe
	case goalMetricWeeklyMe, "":
		return goalMetricWeeklyMe
	default:
		return goalMetricWeeklyMe
	}
}

// goalProgressPct 纯函数（可脱库单测）：count/target → 0-100 完成百分比（截断、确定性）。
func goalProgressPct(count, target int) int {
	if target <= 0 {
		if count > 0 {
			return 100
		}
		return 0
	}
	pct := count * 100 / target
	if pct > 100 {
		pct = 100
	}
	if pct < 0 {
		pct = 0
	}
	return pct
}

// goalDateWindow 纯函数：按口径与周期返回日指标比较的 [from,to]（YYYY-MM-DD，含端点）。
//   - 周口径：本 ISO 周（weekStartOf(now）→ 今天）；
//   - 周期口径：优先 period_start/period_end，缺界回落到周界，保证始终有合法区间。
func goalDateWindow(metric, periodStart, periodEnd string, now time.Time) (from, to string) {
	ws := weekStartOf(now)
	today := now.Format("2006-01-02")
	if metric == goalMetricPeriodMe {
		from, to = ws, today
		if periodStart != "" {
			from = periodStart
		}
		if periodEnd != "" {
			to = periodEnd
		}
		if from > to { // 区间反了或过期：收敛为界本身，保证查询不误伤
			from, to = to, from
		}
		return from, to
	}
	return ws, today
}

// GoalRow 展示用一条目标。
type GoalRow struct {
	ID          int64  `json:"id"`
	ContactID   int64  `json:"contactId"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	Metric      string `json:"metric"`
	TargetCount int    `json:"targetCount"`
	PeriodStart string `json:"periodStart"`
	PeriodEnd   string `json:"periodEnd"`
	Status      string `json:"status"`
	Current     int    `json:"current"` // 窗口内我方发言条数（达成判定同口径）
	ProgressPct int    `json:"progressPct"`
	CreatedAt   string `json:"createdAt"`
	DoneAt      string `json:"doneAt"`
}

// GoalsResponse /api/assistant/goals 响应体。
type GoalsResponse struct {
	GeneratedAt string    `json:"generatedAt"`
	WeekStart   string    `json:"weekStart"`
	Goals       []GoalRow `json:"goals"`
	Done        int       `json:"done"`
	Active      int       `json:"active"`
	Total       int       `json:"total"`
	XP          int       `json:"xp"`
	Level       int       `json:"level"`
	NextLevelAt int       `json:"nextLevelAt"`
	Note        string    `json:"note"`
}

// meCountForWindowLocked 在调用方持 dbMu 时取某联系人窗口内我方发言条数（me_count 求和）。
// 指标表为空/缺失时安全返回 0（派生表可重建，但不在此触发重建以免写库；由只读端点自愈路径另行处理）。
func meCountForWindowLocked(db *sql.DB, contactID int64, from, to string) int {
	var n int
	db.QueryRow(`SELECT COALESCE(SUM(me_count),0) FROM relationship_daily_metrics
		WHERE contact_id=? AND day>=? AND day<=?`, contactID, from, to).Scan(&n)
	return n
}

// autoDetectGoalCompletions 检测 active 目标是否达标（窗口内我方发言 ≥ target），
// 首次达成即置 done、+XP、升级，并返回需在锁外补写时间线的里程碑事件。分层三趟锁、绝不嵌套。
func autoDetectGoalCompletions(db *sql.DB, now time.Time) ([]doneEvent, error) {
	type pend struct {
		goalID    int64
		contactID int64
		title     string
		metric    string
		target    int
		pStart    string
		pEnd      string
	}
	var pending []pend
	metIDs := map[int64]int64{} // goalID -> contactID
	metTitle := map[int64]string{}
	nowStr := now.Format(time.RFC3339)

	// pass1 读：active 目标 + 各自窗口达标计数。
	dbMu.Lock()
	if tableExistsLocked(db, "relationship_goals") {
		if rows, err := db.Query(`SELECT id, contact_id, title, metric, target_count, period_start, period_end
			FROM relationship_goals WHERE status=? ORDER BY id ASC`, goalStatusActive); err == nil {
			for rows.Next() {
				var p pend
				if rows.Scan(&p.goalID, &p.contactID, &p.title, &p.metric, &p.target, &p.pStart, &p.pEnd) == nil {
					pending = append(pending, p)
				}
			}
			rows.Close()
		}
	}
	var met []pend
	for _, p := range pending {
		from, to := goalDateWindow(p.metric, p.pStart, p.pEnd, now)
		if meCountForWindowLocked(db, p.contactID, from, to) >= p.target && p.target > 0 {
			met = append(met, p)
		}
	}
	if len(met) > 0 {
		// pass2 写：置 done、加 XP、重算等级（复用 gamification_state）。
		if err := ensureGamificationLocked(db, nowStr); err != nil {
			dbMu.Unlock()
			return nil, err
		}
		for _, p := range met {
			if _, err := db.Exec(`UPDATE relationship_goals SET status=?, done_at=? WHERE id=? AND status=?`,
				goalStatusDone, nowStr, p.goalID, goalStatusActive); err != nil {
				dbMu.Unlock()
				return nil, err
			}
			metIDs[p.goalID] = p.contactID
			metTitle[p.goalID] = p.title
		}
		addXP := len(met) * challengeXPPerDone
		if _, err := db.Exec(`UPDATE gamification_state SET xp = xp + ?, level = 1 + (xp + ?) / ?, updated_at = ? WHERE id = 1`,
			addXP, addXP, xpPerLevel, nowStr); err != nil {
			dbMu.Unlock()
			return nil, err
		}
	}
	dbMu.Unlock()

	// pass3 锁外补写时间线里程碑。
	events := make([]doneEvent, 0, len(met))
	for _, p := range met {
		title := strings.TrimSpace(p.title)
		if title == "" {
			title = "关系目标"
		}
		RecordContactEvent(db, p.contactID, "milestone", "达成关系目标："+title, "目标打卡完成，+"+strconv.Itoa(challengeXPPerDone)+" XP", now)
		events = append(events, doneEvent{p.contactID, title, now})
	}
	return events, nil
}

// ensureGamificationLocked 是 ensureGamification 的「已持锁」版本：调用方须已持 dbMu。
// （挑战表/状态行是目标加 XP 的前置，二者共用 gamification_state。）
func ensureGamificationLocked(db *sql.DB, nowStr string) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS gamification_state (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		xp INTEGER NOT NULL DEFAULT 0,
		level INTEGER NOT NULL DEFAULT 1,
		updated_at TEXT NOT NULL DEFAULT '')`); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT OR IGNORE INTO gamification_state (id, xp, level, updated_at) VALUES (1, 0, 1, ?)`, nowStr)
	return err
}

// readGoals 读全部目标（带展示名 + 当前窗口进度）+ XP/等级，组装响应。单趟锁读完即释放。
func readGoals(db *sql.DB, now time.Time) (*GoalsResponse, error) {
	resp := &GoalsResponse{
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
		WeekStart:   weekStartOf(now),
		Goals:       []GoalRow{},
	}
	dbMu.Lock()
	if tableExistsLocked(db, "relationship_goals") {
		// 一趟锁内：先把目标行全部读完并释放游标，再逐条补算窗口进度。
		// 单连接池（MaxOpenConns=1）下绝不在 rows 迭代未结束时再发内层查询，否则死锁。
		rows, err := db.Query(`SELECT g.id, g.contact_id, COALESCE(c.remark, c.name, ''), g.title, g.metric,
				g.target_count, g.period_start, g.period_end, g.status, g.created_at, g.done_at
			FROM relationship_goals g LEFT JOIN contacts c ON c.id = g.contact_id
			ORDER BY (g.status='active') DESC, g.id DESC`)
		if err == nil {
			for rows.Next() {
				var gr GoalRow
				if rows.Scan(&gr.ID, &gr.ContactID, &gr.Name, &gr.Title, &gr.Metric,
					&gr.TargetCount, &gr.PeriodStart, &gr.PeriodEnd, &gr.Status, &gr.CreatedAt, &gr.DoneAt) == nil {
					resp.Goals = append(resp.Goals, gr)
				}
			}
			rows.Close()
		}
		// 游标已释放，此处可安全复用唯一连接做内层进度查询。
		for i := range resp.Goals {
			gr := &resp.Goals[i]
			from, to := goalDateWindow(gr.Metric, gr.PeriodStart, gr.PeriodEnd, now)
			gr.Current = meCountForWindowLocked(db, gr.ContactID, from, to)
			gr.ProgressPct = goalProgressPct(gr.Current, gr.TargetCount)
			if gr.Status == goalStatusDone {
				resp.Done++
			} else {
				resp.Active++
			}
		}
	}
	resp.Total = len(resp.Goals)
	if err := ensureGamificationLocked(db, now.Format(time.RFC3339)); err == nil {
		var xp int
		db.QueryRow(`SELECT xp FROM gamification_state WHERE id=1`).Scan(&xp)
		resp.XP = xp
		resp.Level = levelForXP(xp)
		resp.NextLevelAt = resp.Level * xpPerLevel
	}
	dbMu.Unlock()
	if resp.Total == 0 {
		resp.Note = "还没有关系目标，给自己定一个可量化的小目标吧（例如「本周主动发 5 条」）。"
	}
	return resp, nil
}

// BuildGoals 编排：建表 → 达标检测（含锁外事件）→ 读回。
func BuildGoals(db *sql.DB, now time.Time) (*GoalsResponse, error) {
	if err := ensureGoals(db); err != nil {
		return nil, err
	}
	if _, err := autoDetectGoalCompletions(db, now); err != nil {
		slog.Warn("目标：达标检测失败", "err", err)
	}
	return readGoals(db, now)
}

// CreateGoalInput 新建目标入参。
type CreateGoalInput struct {
	ContactID   int64
	Title       string
	Metric      string
	TargetCount int
	PeriodStart string
	PeriodEnd   string
}

var errGoalBadInput = errors.New("目标参数不合法")

// CreateGoal 校验 + 落库一条 active 目标，返回其 ID。联系人不存在或参数非法返回错误。
func CreateGoal(db *sql.DB, in CreateGoalInput, now time.Time) (int64, error) {
	if in.ContactID <= 0 {
		return 0, errGoalBadInput
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return 0, errGoalBadInput
	}
	if r := []rune(title); len(r) > goalMaxTitleRunes {
		title = string(r[:goalMaxTitleRunes])
	}
	if in.TargetCount <= 0 {
		in.TargetCount = 1
	}
	if in.TargetCount > goalMaxTargetCount {
		in.TargetCount = goalMaxTargetCount
	}
	metric := normalizeGoalMetric(in.Metric)
	var ps, pe = strings.TrimSpace(in.PeriodStart), strings.TrimSpace(in.PeriodEnd)
	if metric == goalMetricPeriodMe && (len(ps) != 10 || len(pe) != 10) {
		return 0, errGoalBadInput // 周期口径必须给全起止
	}
	if err := ensureGoals(db); err != nil {
		return 0, err
	}
	if _, err := GetContactByID(db, in.ContactID); err != nil {
		return 0, err
	}
	nowStr := now.Format(time.RFC3339)
	dbMu.Lock()
	defer dbMu.Unlock()
	res, err := db.Exec(`INSERT INTO relationship_goals
		(contact_id, title, metric, target_count, period_start, period_end, status, created_at, done_at)
		VALUES (?,?,?,?,?,?,?,?, '')`,
		in.ContactID, title, metric, in.TargetCount, ps, pe, goalStatusActive, nowStr)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// CompleteGoal 手动把某条 active 目标置为达成：+XP、升级、返回需在锁外补写的里程碑事件。
// 已达成/不存在 → 幂等返回 (false, nil)。分层锁、绝不嵌套。
func CompleteGoal(db *sql.DB, goalID int64, now time.Time) (bool, error) {
	if err := ensureGoals(db); err != nil {
		return false, err
	}
	nowStr := now.Format(time.RFC3339)
	var contactID int64
	var title string
	dbMu.Lock()
	err := db.QueryRow(`SELECT contact_id, title FROM relationship_goals WHERE id=? AND status=?`,
		goalID, goalStatusActive).Scan(&contactID, &title)
	if errors.Is(err, sql.ErrNoRows) {
		dbMu.Unlock()
		return false, nil // 不存在或已达成：幂等
	}
	if err != nil {
		dbMu.Unlock()
		return false, err
	}
	if err := ensureGamificationLocked(db, nowStr); err != nil {
		dbMu.Unlock()
		return false, err
	}
	if _, err := db.Exec(`UPDATE relationship_goals SET status=?, done_at=? WHERE id=? AND status=?`,
		goalStatusDone, nowStr, goalID, goalStatusActive); err != nil {
		dbMu.Unlock()
		return false, err
	}
	if _, err := db.Exec(`UPDATE gamification_state SET xp = xp + ?, level = 1 + (xp + ?) / ?, updated_at = ? WHERE id = 1`,
		challengeXPPerDone, challengeXPPerDone, xpPerLevel, nowStr); err != nil {
		dbMu.Unlock()
		return false, err
	}
	dbMu.Unlock()

	t := strings.TrimSpace(title)
	if t == "" {
		t = "关系目标"
	}
	RecordContactEvent(db, contactID, "milestone", "达成关系目标："+t, "手动完成目标打卡，+"+strconv.Itoa(challengeXPPerDone)+" XP", now)
	return true, nil
}

// DeleteGoal 删除一条目标（任意状态）。返回是否删到。
func DeleteGoal(db *sql.DB, goalID int64) (bool, error) {
	if err := ensureGoals(db); err != nil {
		return false, err
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	res, err := db.Exec(`DELETE FROM relationship_goals WHERE id=?`, goalID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
