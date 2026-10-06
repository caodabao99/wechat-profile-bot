package main

// 蓝图 §13 P2：Relationship Command Center 2.0（首页指挥中心）。
//
// 铁律——当前 Action Center 2.0 已存在，不重做：本文件只做「只读编排」，把已有子系统各自的
// 真相来源收成首页一屏、每栏 Top 3（"不要首页出现大量卡片"）。绝不新建第二套 today/风险/
// 记忆/预算真相：
//   - TODAY      ← SurfaceDecisions（决策引擎，已确定性排序）
//   - MEMORY     ← BuildMemoryReviewQueue（待确认/待合并记忆，已按优先级排序）
//   - RISK       ← BuildRisks（风险中心，已确定性排序）
//   - PROJECT    ← relationship_projects（只读扫描：即将到期/受阻/停滞）
//   - PORTFOLIO  ← ComputePortfolio（本周关系时间预算：预算/已用/建议分配）
//   - AI         ← ComputeLLMUsage + llmBudgetStatus（今日 token：已用/预算/Top task/Top model）
//
// 每个子系统取数失败都单独吞掉、只置空该栏，绝不拖垮整页（核心页绝不 500）。

import (
	"database/sql"
	"net/http"
	"sort"
	"time"
)

const commandSectionTop = 3 // §13 每栏 Top 3

// ---- 各栏紧凑视图（只保留首页要点，避免大卡片）----

type commandTodo struct {
	ContactID int64  `json:"contact_id"`
	Name      string `json:"name"`
	Action    string `json:"action"`
	Why       string `json:"why,omitempty"`
	Priority  int    `json:"priority"`
}

type commandMemory struct {
	ContactID   int64  `json:"contact_id"`
	Name        string `json:"name"`
	Fact        string `json:"fact"`
	Reason      string `json:"reason,omitempty"`
	HasConflict bool   `json:"has_conflict"`
}

type commandRisk struct {
	ContactID int64  `json:"contact_id"`
	Name      string `json:"name"`
	Title     string `json:"title"`
	Severity  string `json:"severity"`
}

type commandProject struct {
	ProjectID int64  `json:"project_id"`
	ContactID int64  `json:"contact_id"`
	Name      string `json:"name"`
	Title     string `json:"title"`
	Signal    string `json:"signal"` // due_soon / blocked / stalled
}

type commandPortfolio struct {
	WeeklyBudgetMinutes int  `json:"weekly_budget_minutes"`
	UsedMinutes         int  `json:"used_minutes"`
	UsedIsEstimate      bool `json:"used_is_estimate"`
	AllocatedMinutes    int  `json:"allocated_minutes"`
	OverBudget          bool `json:"over_budget"`
}

type commandAI struct {
	UsedToday     int64  `json:"used_today"`
	DailyBudget   int64  `json:"daily_budget"` // 0=不限
	Unlimited     bool   `json:"unlimited"`
	Over          bool   `json:"over"`
	TopTask       string `json:"top_task,omitempty"`
	TopTaskTokens int64  `json:"top_task_tokens,omitempty"`
	TopModel      string `json:"top_model,omitempty"`
	TopModelCalls int    `json:"top_model_calls,omitempty"`
}

// CommandCenter 首页一屏：六栏，各 Top 3（§13.1~13.6）。
type CommandCenter struct {
	GeneratedAt string           `json:"generated_at"`
	Today       []commandTodo    `json:"today"`
	Memory      []commandMemory  `json:"memory"`
	Risk        []commandRisk    `json:"risk"`
	Project     []commandProject `json:"project"`
	Portfolio   commandPortfolio `json:"portfolio"`
	AI          commandAI        `json:"ai"`
}

// BuildCommandCenter 只读聚合首页六栏。任一子系统失败仅置空该栏，整体绝不为 nil。
func BuildCommandCenter(db *sql.DB, now time.Time) *CommandCenter {
	c := &CommandCenter{
		GeneratedAt: now.Format(time.RFC3339),
		Today:       []commandTodo{},
		Memory:      []commandMemory{},
		Risk:        []commandRisk{},
		Project:     []commandProject{},
	}
	c.Today = commandSectionToday(db, now)
	c.Memory = commandSectionMemory(db, now)
	c.Risk = commandSectionRisk(db, now)
	c.Project = commandSectionProject(db, now)
	c.Portfolio = commandSectionPortfolio(db, now)
	c.AI = commandSectionAI(db)
	return c
}

func commandSectionToday(db *sql.DB, now time.Time) []commandTodo {
	cands, err := SurfaceDecisions(db, now, commandSectionTop)
	if err != nil {
		return []commandTodo{}
	}
	out := []commandTodo{}
	for _, x := range cands {
		why := ""
		if len(x.WhyNow) > 0 {
			why = x.WhyNow[0]
		}
		out = append(out, commandTodo{ContactID: x.ContactID, Name: x.Name, Action: x.Action, Why: why, Priority: x.Priority})
	}
	return out
}

func commandSectionMemory(db *sql.DB, now time.Time) []commandMemory {
	items, err := BuildMemoryReviewQueue(db, now, commandSectionTop)
	if err != nil {
		return []commandMemory{}
	}
	out := []commandMemory{}
	for i, it := range items {
		if i >= commandSectionTop {
			break
		}
		fact := it.FactValue
		if it.FactKey != "" {
			fact = it.FactKey + "：" + fact
		}
		out = append(out, commandMemory{ContactID: it.ContactID, Name: it.ContactName, Fact: fact, Reason: it.Reason, HasConflict: it.HasConflict})
	}
	return out
}

func commandSectionRisk(db *sql.DB, now time.Time) []commandRisk {
	risks, err := BuildRisks(db, now)
	if err != nil {
		return []commandRisk{}
	}
	// 防御性再排序（高→低），不依赖上游恰好有序；tie-break 保稳定。
	sort.SliceStable(risks, func(i, j int) bool {
		return riskSeverityRank(risks[i].Severity) > riskSeverityRank(risks[j].Severity)
	})
	out := []commandRisk{}
	for i, rk := range risks {
		if i >= commandSectionTop {
			break
		}
		out = append(out, commandRisk{ContactID: rk.ContactID, Name: rk.ContactName, Title: rk.Title, Severity: rk.Severity})
	}
	return out
}

// commandSectionProject 只读扫描 relationship_projects：即将到期（next_action_due 近 14 天）、
// 受阻（blocked_reason 非空）、停滞（active 但久未更新）。绝不写库。
func commandSectionProject(db *sql.DB, now time.Time) []commandProject {
	if err := ensureProjects(db); err != nil {
		return []commandProject{}
	}
	soon := now.AddDate(0, 0, 14).Format("2006-01-02")
	today := now.Format("2006-01-02")
	stallBefore := now.AddDate(0, 0, -21).Format(time.RFC3339)
	out := []commandProject{}
	err := func() error {
		dbMu.Lock()
		defer dbMu.Unlock()
		rows, err := db.Query(projectSelectSQL+`
			WHERE p.status IN ('active','paused')
			  AND (
					(next_action_due != '' AND next_action_due <= ?)
				 OR blocked_reason != ''
				 OR (status = 'active' AND updated_at != '' AND updated_at < ?)
			  )
			ORDER BY (CASE WHEN blocked_reason != '' THEN 0 WHEN next_action_due != '' THEN 1 ELSE 2 END),
			         next_action_due ASC, p.priority DESC, p.id ASC
			LIMIT ?`, soon, stallBefore, commandSectionTop)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			v, err := scanProject(rows)
			if err != nil {
				continue
			}
			out = append(out, commandProject{
				ProjectID: v.ID, ContactID: v.ContactID, Name: v.Name, Title: v.Title,
				Signal: projectSignal(v, today),
			})
		}
		return rows.Err()
	}()
	if err != nil {
		return []commandProject{}
	}
	return out
}

// projectSignal 确定性判定项目信号（受阻 > 到期 > 停滞）。
func projectSignal(v ProjectView, today string) string {
	if v.BlockedReason != "" {
		return "blocked"
	}
	if v.NextActionDue != "" && v.NextActionDue <= today {
		return "due_soon"
	}
	return "stalled"
}

func commandSectionPortfolio(db *sql.DB, now time.Time) commandPortfolio {
	view, err := ComputePortfolio(db, now, commandSectionTop)
	if err != nil || view == nil {
		return commandPortfolio{}
	}
	return commandPortfolio{
		WeeklyBudgetMinutes: view.WeeklyBudgetMinutes,
		UsedMinutes:         view.UsedMinutes,
		UsedIsEstimate:      view.UsedIsEstimate,
		AllocatedMinutes:    view.AllocatedMinutes,
		OverBudget:          view.OverBudget,
	}
}

// commandSectionAI 今日 token：已用/预算/Top task/Top model（无数据全 0，绝不报错）。
func commandSectionAI(db *sql.DB) commandAI {
	st := llmBudgetStatus(db)
	ai := commandAI{UsedToday: st.UsedToday, DailyBudget: st.Limit, Unlimited: st.Unlimited, Over: st.Over}
	usage, err := ComputeLLMUsage(db)
	if err != nil {
		return ai
	}
	if len(usage.ByTask) > 0 {
		ai.TopTask, ai.TopTaskTokens = usage.ByTask[0].Task, usage.ByTask[0].TotalTokens
	}
	if len(usage.ByModel) > 0 {
		ai.TopModel, ai.TopModelCalls = usage.ByModel[0].Model, usage.ByModel[0].Calls
	}
	return ai
}

// routeCommandCenter GET /api/command-center → 首页指挥中心（只读，无 LLM 也完全工作）。
func (s *apiServer) routeCommandCenter(w http.ResponseWriter, r *http.Request, sub []string) {
	if len(sub) != 0 || r.Method != http.MethodGet {
		writeErr(w, http.StatusNotFound, "未知接口: /api/command-center")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "data": BuildCommandCenter(s.db, time.Now())})
}
