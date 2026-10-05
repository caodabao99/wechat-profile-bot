package main

// ═══════════════════════════════════════════════════════════════════════════
// Relationship Projects（PERSONAL RELATIONSHIP OS 2.0 · Phase 5，规格「八、P4」）
//
// 定位：relationship_goals 保留（细粒度、可自动达标的量化目标），其上再加一层
// 更高层语义——Relationship Project：一段有主题、有阶段、有下一步行动、可长期
// 推进/暂停/收尾的关系经营单元（例：与「王总」的「深圳项目」，阶段=需求确认，
// 下一行动=跟进报价，截止=2026-10-08）。
//
// 治理红线（对齐既有数据层规则）：
//   - 这是**用户创建的 core 业务表，非派生**：随备份恢复（不入 backup.derivedTables），
//     删联系人时级联清理（入 contactCleanupTables），登记 registry 且 HasContactID:true。
//   - 沿用 goals 的**懒建表范式**（ensure* 自锁 + CREATE IF NOT EXISTS），不 bump user_version，
//     避免既有迁移测试版本抖动，且表在任何入口首次使用前必被建出。
//   - 只保存引用 ID（contact_id），**不复制消息内容**（规格 8.2）。
//   - 单连接池分层锁：所有函数自锁、绝不嵌套；时间戳统一 RFC3339 字符串。
// ═══════════════════════════════════════════════════════════════════════════

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// 项目状态机（规格 8.1）：active / paused / completed / cancelled。
const (
	projectStatusActive    = "active"
	projectStatusPaused    = "paused"
	projectStatusCompleted = "completed"
	projectStatusCancelled = "cancelled"
)

// 项目阶段（规格 8.1）：discovery / building / maintaining / negotiating / closing / completed。
const (
	projectStageDiscovery   = "discovery"
	projectStageBuilding    = "building"
	projectStageMaintaining = "maintaining"
	projectStageNegotiating = "negotiating"
	projectStageClosing     = "closing"
	projectStageCompleted   = "completed"
)

var errProjectBadInput = errors.New("项目参数不合法")

const projectMaxTitleRunes = 80

func validProjectStatus(s string) bool {
	switch s {
	case projectStatusActive, projectStatusPaused, projectStatusCompleted, projectStatusCancelled:
		return true
	}
	return false
}

func validProjectStage(s string) bool {
	switch s {
	case projectStageDiscovery, projectStageBuilding, projectStageMaintaining,
		projectStageNegotiating, projectStageClosing, projectStageCompleted:
		return true
	}
	return false
}

// normalizeProjectStatus 空→active；非法→errProjectBadInput。
func normalizeProjectStatus(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return projectStatusActive, nil
	}
	if !validProjectStatus(s) {
		return "", errProjectBadInput
	}
	return s, nil
}

// normalizeProjectStage 空→discovery；非法→errProjectBadInput。
func normalizeProjectStage(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return projectStageDiscovery, nil
	}
	if !validProjectStage(s) {
		return "", errProjectBadInput
	}
	return s, nil
}

// ensureProjects 懒建 relationship_projects 表 + 索引（幂等、自锁）。
func ensureProjects(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS relationship_projects (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			contact_id      INTEGER NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
			title           TEXT NOT NULL,
			description     TEXT NOT NULL DEFAULT '',
			status          TEXT NOT NULL DEFAULT 'active'
				CHECK(status IN ('active','paused','completed','cancelled')),
			stage           TEXT NOT NULL DEFAULT 'discovery'
				CHECK(stage IN ('discovery','building','maintaining','negotiating','closing','completed')),
			priority        INTEGER NOT NULL DEFAULT 0,
			start_date      TEXT NOT NULL DEFAULT '',
			target_date     TEXT NOT NULL DEFAULT '',
			next_action     TEXT NOT NULL DEFAULT '',
			next_action_due TEXT NOT NULL DEFAULT '',
			blocked_reason  TEXT NOT NULL DEFAULT '',
			created_at      TEXT NOT NULL,
			updated_at      TEXT NOT NULL,
			completed_at    TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_projects_contact ON relationship_projects(contact_id, status);
	`)
	return err
}

// ProjectView 项目读视图（带联系人名，供前端时间线渲染）。
type ProjectView struct {
	ID            int64  `json:"id"`
	ContactID     int64  `json:"contact_id"`
	Name          string `json:"name"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	Status        string `json:"status"`
	Stage         string `json:"stage"`
	Priority      int    `json:"priority"`
	StartDate     string `json:"start_date"`
	TargetDate    string `json:"target_date"`
	NextAction    string `json:"next_action"`
	NextActionDue string `json:"next_action_due"`
	BlockedReason string `json:"blocked_reason"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	CompletedAt   string `json:"completed_at"`
}

const projectSelectSQL = `SELECT p.id, p.contact_id, COALESCE(c.name,''), p.title, p.description,
		p.status, p.stage, p.priority, p.start_date, p.target_date, p.next_action, p.next_action_due,
		p.blocked_reason, p.created_at, p.updated_at, p.completed_at
		FROM relationship_projects p LEFT JOIN contacts c ON c.id = p.contact_id `

func scanProject(rows *sql.Rows) (ProjectView, error) {
	var v ProjectView
	err := rows.Scan(&v.ID, &v.ContactID, &v.Name, &v.Title, &v.Description,
		&v.Status, &v.Stage, &v.Priority, &v.StartDate, &v.TargetDate, &v.NextAction, &v.NextActionDue,
		&v.BlockedReason, &v.CreatedAt, &v.UpdatedAt, &v.CompletedAt)
	return v, err
}

// CreateProjectInput 新建项目入参。
type CreateProjectInput struct {
	ContactID     int64
	Title         string
	Description   string
	Status        string
	Stage         string
	Priority      int
	StartDate     string
	TargetDate    string
	NextAction    string
	NextActionDue string
	BlockedReason string
}

// CreateProject 校验 + 落库一条项目，返回其 ID。联系人不存在或参数非法返回错误。
func CreateProject(db *sql.DB, in CreateProjectInput, now time.Time) (int64, error) {
	if in.ContactID <= 0 {
		return 0, errProjectBadInput
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return 0, errProjectBadInput
	}
	if r := []rune(title); len(r) > projectMaxTitleRunes {
		title = string(r[:projectMaxTitleRunes])
	}
	status, err := normalizeProjectStatus(in.Status)
	if err != nil {
		return 0, err
	}
	stage, err := normalizeProjectStage(in.Stage)
	if err != nil {
		return 0, err
	}
	if err := ensureProjects(db); err != nil {
		return 0, err
	}
	if _, err := GetContactByID(db, in.ContactID); err != nil {
		return 0, err
	}
	nowStr := now.Format(time.RFC3339)
	completedAt := ""
	if status == projectStatusCompleted {
		completedAt = nowStr
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	res, err := db.Exec(`INSERT INTO relationship_projects
		(contact_id, title, description, status, stage, priority, start_date, target_date,
		 next_action, next_action_due, blocked_reason, created_at, updated_at, completed_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		in.ContactID, title, strings.TrimSpace(in.Description), status, stage, clampProjectPriority(in.Priority),
		first8Date(in.StartDate), first8Date(in.TargetDate), strings.TrimSpace(in.NextAction),
		first8Date(in.NextActionDue), strings.TrimSpace(in.BlockedReason), nowStr, nowStr, completedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func clampProjectPriority(p int) int {
	if p < 0 {
		return 0
	}
	if p > 9 {
		return 9
	}
	return p
}

// ListProjects 按联系人（contactID<=0 视为全部）与状态（空=全部，"open"=active+paused）过滤，
// 优先级降序 → 更新时间降序 → id 升序返回。表不存在则懒建后返回空。
func ListProjects(db *sql.DB, contactID int64, status string) ([]ProjectView, error) {
	if err := ensureProjects(db); err != nil {
		return nil, err
	}
	where := []string{"1=1"}
	args := []any{}
	if contactID > 0 {
		where = append(where, "p.contact_id=?")
		args = append(args, contactID)
	}
	switch strings.TrimSpace(status) {
	case "":
		// 全部
	case "open":
		where = append(where, "p.status IN ('active','paused')")
	default:
		if !validProjectStatus(status) {
			return nil, errProjectBadInput
		}
		where = append(where, "p.status=?")
		args = append(args, status)
	}
	q := projectSelectSQL + "WHERE " + strings.Join(where, " AND ") +
		" ORDER BY p.priority DESC, p.updated_at DESC, p.id ASC"
	dbMu.Lock()
	defer dbMu.Unlock()
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectView{}
	for rows.Next() {
		v, err := scanProject(rows)
		if err != nil {
			continue
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetProject 读单条项目；不存在返回 sql.ErrNoRows。
func GetProject(db *sql.DB, id int64) (ProjectView, error) {
	if err := ensureProjects(db); err != nil {
		return ProjectView{}, err
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	rows, err := db.Query(projectSelectSQL+"WHERE p.id=?", id)
	if err != nil {
		return ProjectView{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return ProjectView{}, sql.ErrNoRows
	}
	return scanProject(rows)
}

// UpdateProjectInput 更新入参：仅非空字段被覆写（PATCH 语义）；Status/Stage 走校验。
type UpdateProjectInput struct {
	Title         *string
	Description   *string
	Status        *string
	Stage         *string
	Priority      *int
	StartDate     *string
	TargetDate    *string
	NextAction    *string
	NextActionDue *string
	BlockedReason *string
}

// UpdateProject 局部更新一条项目；status 迁入 completed 时补 completed_at，迁出时清空。
// 不存在返回 sql.ErrNoRows。
func UpdateProject(db *sql.DB, id int64, in UpdateProjectInput, now time.Time) (ProjectView, error) {
	if err := ensureProjects(db); err != nil {
		return ProjectView{}, err
	}
	sets := []string{}
	args := []any{}
	put := func(col string, val string) {
		sets = append(sets, col+"=?")
		args = append(args, val)
	}
	if in.Title != nil {
		t := strings.TrimSpace(*in.Title)
		if t == "" {
			return ProjectView{}, errProjectBadInput
		}
		if r := []rune(t); len(r) > projectMaxTitleRunes {
			t = string(r[:projectMaxTitleRunes])
		}
		put("title", t)
	}
	if in.Description != nil {
		put("description", strings.TrimSpace(*in.Description))
	}
	if in.NextAction != nil {
		put("next_action", strings.TrimSpace(*in.NextAction))
	}
	if in.NextActionDue != nil {
		put("next_action_due", first8Date(*in.NextActionDue))
	}
	if in.BlockedReason != nil {
		put("blocked_reason", strings.TrimSpace(*in.BlockedReason))
	}
	if in.StartDate != nil {
		put("start_date", first8Date(*in.StartDate))
	}
	if in.TargetDate != nil {
		put("target_date", first8Date(*in.TargetDate))
	}
	if in.Priority != nil {
		sets = append(sets, "priority=?")
		args = append(args, clampProjectPriority(*in.Priority))
	}
	if in.Stage != nil {
		st, err := normalizeProjectStage(*in.Stage)
		if err != nil {
			return ProjectView{}, err
		}
		put("stage", st)
	}
	completedAt := (*string)(nil)
	if in.Status != nil {
		st, err := normalizeProjectStatus(*in.Status)
		if err != nil {
			return ProjectView{}, err
		}
		put("status", st)
		ca := ""
		if st == projectStatusCompleted {
			ca = now.Format(time.RFC3339)
		}
		completedAt = &ca
	}
	if len(sets) == 0 {
		return GetProject(db, id) // 无字段可改：幂等回读
	}
	sets = append(sets, "updated_at=?")
	args = append(args, now.Format(time.RFC3339))
	if completedAt != nil {
		sets = append(sets, "completed_at=?")
		args = append(args, *completedAt)
	}
	args = append(args, id)
	dbMu.Lock()
	res, err := db.Exec("UPDATE relationship_projects SET "+strings.Join(sets, ", ")+" WHERE id=?", args...)
	dbMu.Unlock()
	if err != nil {
		return ProjectView{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ProjectView{}, sql.ErrNoRows
	}
	return GetProject(db, id)
}

// DeleteProject 删除一条项目，返回是否确有删除。
func DeleteProject(db *sql.DB, id int64) (bool, error) {
	if err := ensureProjects(db); err != nil {
		return false, err
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	res, err := db.Exec(`DELETE FROM relationship_projects WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
