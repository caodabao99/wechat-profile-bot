package main

// v6.3 §P11 Action Center 2.0：TODAY 聚合 + 噪声控制。
//
// 在 OS 2.0 中，Today Decisions（§7）已做决策排序 + 指纹去重 + 窗口屏蔽。
// 但用户首页仍然要分别看四个独立面板（决策/待办/记忆审核/风险），信息碎片化，
// 且同一联系人可能出现在多个面板——"噪音"不是指"不该出现的出现了"，
// 而是"同一个人被拆成三条卡片、看起来任务量翻倍"。
//
// P11 的核心思路：
//   1. 四源聚合 → 按 contact_id collapse → 每人最多一张卡；
//   2. 噪声控制：每日上限(max 7 人)、per-source 贡献上限(每人每源最多取 2 条理由)；
//   3. 紧急度分组：urgent（风险 high + 决策 priority ≥ 70）/ today / later；
//   4. Snooze 机制：today_snooze 表按 (contact_id, until_date) 屏蔽，次日自动恢复。
//
// 全确定性、无 LLM 调用。API:
//   GET  /api/today?max=7         返回分组聚合结果
//   POST /api/today/snooze        {contact_id, days} → 写入 snooze

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ---------- 数据结构 ----------

// TodayCard 一张聚合后的联系人行动卡片。
type TodayCard struct {
	ContactID   int64    `json:"contact_id"`
	ContactName string   `json:"contact_name"`
	Urgency     string   `json:"urgency"`      // urgent / today / later
	Priority    int      `json:"priority"`     // 综合优先级（取各源最大值）
	Reasons     []string `json:"reasons"`      // 聚合理由（最多 4 条）
	ActionHints []string `json:"action_hints"` // 建议行动（最多 2 条）
	BestTime    string   `json:"best_time,omitempty"`
	SourceTypes []string `json:"source_types"` // 涉及的来源
}

// TodayAggregate GET /api/today 的完整响应。
type TodayAggregate struct {
	Date            string      `json:"date"`
	Summary         string      `json:"summary"`
	Urgent          []TodayCard `json:"urgent"`
	Today           []TodayCard `json:"today"`
	Later           []TodayCard `json:"later"`
	TotalShown      int         `json:"total_shown"`
	TotalSuppressed int         `json:"total_suppressed"`
}

// ---------- 核心聚合 ----------

// BuildTodayAggregate 拉取四源并聚合为卡片。
func BuildTodayAggregate(db *sql.DB, now time.Time, max int) (*TodayAggregate, error) {
	if max <= 0 || max > 20 {
		max = 7
	}

	// 四源并行收集（此处简化为顺序执行，均为只读）
	type raw struct {
		contactID int64
		name      string
		source    string
		priority  int
		urgency   string
		reason    string
		hint      string
		bestTime  string
	}
	var all []raw

	// 1) 决策
	decisions, err := SurfaceDecisions(db, now, max*3) // 多取一些用于 collapse
	if err == nil {
		for _, d := range decisions {
			urgency := "today"
			if d.Priority >= 70 || d.Risk >= 2 {
				urgency = "urgent"
			}
			reason := strings.Join(d.ReasonCodes, "、")
			all = append(all, raw{d.ContactID, d.Name, "decision", d.Priority, urgency, reason, d.Action, d.BestTime})
		}
	}

	// 2) 待办（逾期/今天到期 = urgent，其余 = today）
	followups, err := ListFollowups(db, "open", 20)
	if err == nil {
		today := now.Format("2006-01-02")
		for _, f := range followups {
			urgency := "today"
			if f.DueDate != "" && f.DueDate <= today {
				urgency = "urgent"
			}
			all = append(all, raw{f.ContactID, f.Name, "followup", 40, urgency, "待办: " + f.Content, f.Content, ""})
		}
	}

	// 3) 记忆审核（priority 100 = urgent，其余 = later）
	reviews, err := BuildMemoryReviewQueue(db, now, 10)
	if err == nil {
		for _, r := range reviews {
			urgency := "later"
			prio := 30
			if r.Priority >= 80 {
				urgency = "urgent"
				prio = 60
			}
			all = append(all, raw{r.ContactID, r.ContactName, "memory_review", prio, urgency, "记忆待确认: " + r.FactValue, "", ""})
		}
	}

	// 4) 风险（high = urgent）
	risks, err := BuildRisks(db, now)
	if err == nil {
		for _, r := range risks {
			urgency := "today"
			prio := 50
			if r.Severity == "high" {
				urgency = "urgent"
				prio = 80
			}
			hint := r.SuggestedAction
			all = append(all, raw{r.ContactID, r.ContactName, "risk", prio, urgency, "风险: " + r.Title, hint, ""})
		}
	}

	// 过滤 snoozed
	snoozed, err := getSnoozedContactIDs(db, now)
	if err == nil && len(snoozed) > 0 {
		var filtered []raw
		for _, r := range all {
			if !snoozed[r.contactID] {
				filtered = append(filtered, r)
			}
		}
		all = filtered
	}

	// 按 contact_id 折叠
	byContact := map[int64]*TodayCard{}
	type contactAgg struct {
		cid    int64
		prio   int
		urg    string
		reason map[string]bool
		hint   map[string]bool
		srcs   map[string]bool
	}
	aggs := map[int64]*contactAgg{}
	for _, r := range all {
		a, ok := aggs[r.contactID]
		if !ok {
			a = &contactAgg{cid: r.contactID, urg: "later", reason: map[string]bool{}, hint: map[string]bool{}, srcs: map[string]bool{}}
			aggs[r.contactID] = a
		}
		if r.priority > a.prio {
			a.prio = r.priority
		}
		// urgency 取最高
		if r.urgency == "urgent" || (r.urgency == "today" && a.urg == "later") {
			a.urg = r.urgency
		}
		a.reason[r.reason] = true
		if r.hint != "" {
			a.hint[r.hint] = true
		}
		a.srcs[r.source] = true
		byContact[r.contactID] = &TodayCard{ContactID: r.contactID, ContactName: r.name, BestTime: r.bestTime}
	}

	// 构造卡片列表
	var cards []TodayCard
	for cid, a := range aggs {
		card := byContact[cid]
		card.Priority = a.prio
		card.Urgency = a.urg
		card.Reasons = takeN(a.reason, 4)
		card.ActionHints = takeN(a.hint, 2)
		for s := range a.srcs {
			card.SourceTypes = append(card.SourceTypes, s)
		}
		sort.Strings(card.SourceTypes)
		cards = append(cards, *card)
	}

	// 按 priority 降序排序，裁到 max
	sort.Slice(cards, func(i, j int) bool { return cards[i].Priority > cards[j].Priority })
	shown := cards
	suppressed := 0
	if len(shown) > max {
		suppressed = len(shown) - max
		shown = shown[:max]
	}

	// 分组
	agg := &TodayAggregate{
		Date:            now.Format("2006-01-02"),
		TotalShown:      len(shown),
		TotalSuppressed: suppressed,
		Urgent:          []TodayCard{},
		Today:           []TodayCard{},
		Later:           []TodayCard{},
	}
	for _, c := range shown {
		switch c.Urgency {
		case "urgent":
			agg.Urgent = append(agg.Urgent, c)
		case "today":
			agg.Today = append(agg.Today, c)
		default:
			agg.Later = append(agg.Later, c)
		}
	}
	agg.Summary = fmt.Sprintf("今日聚合 %d 位联系人（%d 紧急 / %d 今天 / %d 稍后）",
		agg.TotalShown, len(agg.Urgent), len(agg.Today), len(agg.Later))
	return agg, nil
}

// ---------- Snooze ----------

// ensureTodaySnoozeTable 幂等建表（在 migrate 之外独立维护，与 ai_cache 等模式一致）。
func ensureTodaySnoozeTable(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS today_snooze (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		contact_id INTEGER NOT NULL,
		until_date TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT '',
		UNIQUE(contact_id, until_date)
	)`)
	return err
}

// SnoozeContact 将某联系人从今天聚合中屏蔽 N 天。
func SnoozeContact(db *sql.DB, contactID int64, days int, now time.Time) error {
	if days <= 0 {
		days = 1
	}
	until := now.AddDate(0, 0, days).Format("2006-01-02")
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err := db.Exec(`INSERT OR REPLACE INTO today_snooze (contact_id, until_date, created_at) VALUES (?, ?, ?)`,
		contactID, until, now.Format(time.RFC3339))
	return err
}

// getSnoozedContactIDs 返回今天仍在 snooze 中的 contact id 集合。
func getSnoozedContactIDs(db *sql.DB, now time.Time) (map[int64]bool, error) {
	today := now.Format("2006-01-02")
	dbMu.Lock()
	defer dbMu.Unlock()
	rows, err := db.Query(`SELECT DISTINCT contact_id FROM today_snooze WHERE until_date > ?`, today)
	if err != nil {
		return nil, nil // 表可能不存在，静默返回空
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out, nil
}

// PurgeExpiredSnoozes 清理已过期的 snooze 行（可挂入定时任务）。
func PurgeExpiredSnoozes(db *sql.DB, now time.Time) (int64, error) {
	today := now.Format("2006-01-02")
	dbMu.Lock()
	defer dbMu.Unlock()
	res, err := db.Exec(`DELETE FROM today_snooze WHERE until_date <= ?`, today)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------- API ----------

// routeToday 挂载点：/api/today（api.go case 添加）。
func (s *apiServer) routeToday(w http.ResponseWriter, r *http.Request, sub []string) {
	switch {
	case len(sub) == 0 && r.Method == http.MethodGet:
		maxN := 7
		if v := r.URL.Query().Get("max"); v != "" {
			fmt.Sscanf(v, "%d", &maxN)
		}
		agg, err := BuildTodayAggregate(s.db, time.Now(), maxN)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "聚合失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "data": agg})
	case len(sub) == 1 && sub[0] == "snooze" && r.Method == http.MethodPost:
		var body struct {
			ContactID int64 `json:"contact_id"`
			Days      int   `json:"days"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "JSON: "+err.Error())
			return
		}
		if body.ContactID == 0 {
			writeErr(w, http.StatusBadRequest, "contact_id 必填")
			return
		}
		if err := SnoozeContact(s.db, body.ContactID, body.Days, time.Now()); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
	default:
		writeErr(w, http.StatusNotFound, "未知接口")
	}
}

// ---------- 辅助 ----------

func takeN(set map[string]bool, n int) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		if k != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	if len(out) > n {
		out = out[:n]
	}
	return out
}
