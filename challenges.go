package main

// v5.3.0 #8：关系维护挑战 / 游戏化（零 LLM、确定性、扩展 v5.2.1 成就体系）。
//
// 每周自动生成最多 3 条「跳一跳够得着」的维护挑战（联系久未联系的人 / 回复未回的消息 /
//   问候核心圈），完成即得 XP、累积升级；徽章直接复用 v5.2.1 的 contact_achievements。
//   首次达成某条挑战会给该联系人写一条 kind="milestone" 的 contact_events，时间线自动留痕。
//
// 设计铁律：
//   - 幂等：周按 ISO 周（weekStartOf）键定，同一 (week,kind,contact) 只一行；重复访问不重复生成、
//     不重复加 XP（achieved 一旦置位不再回退）。
//   - 单连接池分层锁：读候选/读待完成/写库分趟顺序取放，绝不嵌套；RecordContactEvent 一律在未持锁时调用。
//   - 两张表按持久化用户数据（不加入 derivedTables），依 listRestoreTables 自动纳入备份/恢复。

import (
	"database/sql"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

const (
	challengeXPPerDone  = 20  // 每完成一条挑战的 XP
	xpPerLevel          = 100 // 每级所需 XP
	challengeMaxPerWeek = 3   // 每周最多挑战数
)

// 挑战类型键。
const (
	chalReachDormant = "reach_dormant"
	chalReplyPending = "reply_pending"
	chalGreetCore    = "greet_core"
)

// ensureGamification 懒建两张持久表 + 保证状态行存在（幂等 DDL）。
func ensureGamification(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS weekly_challenges (
		week_start TEXT NOT NULL,
		contact_id INTEGER NOT NULL,
		kind TEXT NOT NULL,
		target INTEGER NOT NULL DEFAULT 1,
		achieved INTEGER NOT NULL DEFAULT 0,
		xp INTEGER NOT NULL DEFAULT 0,
		done_at TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (week_start, kind, contact_id))`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS gamification_state (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		xp INTEGER NOT NULL DEFAULT 0,
		level INTEGER NOT NULL DEFAULT 1,
		updated_at TEXT NOT NULL DEFAULT '')`); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT OR IGNORE INTO gamification_state (id, xp, level, updated_at) VALUES (1, 0, 1, ?)`,
		time.Now().Format(time.RFC3339))
	return err
}

// levelForXP 纯函数：XP → 等级（1 + floor(xp/xpPerLevel)）。
func levelForXP(xp int) int {
	if xp < 0 {
		xp = 0
	}
	return 1 + xp/xpPerLevel
}

type chalTarget struct {
	kind      string
	contactID int64
}

// selectChallengeTargets 确定性选出本周最多 challengeMaxPerWeek 条挑战靶点。
func selectChallengeTargets(db *sql.DB, now time.Time) []chalTarget {
	// 亲密度榜（computeIntimacy 内部自锁，先在外面拿好整张表，锁内只复用其 ID）。
	intimList, _ := computeIntimacy(db, now, healthWindowDays)
	var coreID int64
	if len(intimList) > 0 {
		coreID = intimList[0].ContactID
	}

	// 一趟锁内取「最久未联系」与「未回复」候选。
	var dormantID int64
	var dormantName string
	var replyID int64
	var replyName string
	dbMu.Lock()
	// 最久未联系：最后活跃日最早、且距今超过 7 天。
	cutoff := now.AddDate(0, 0, -7).Format("2006-01-02")
	if r := db.QueryRow(
		`SELECT m.contact_id, COALESCE(c.remark, c.name, '')
		 FROM relationship_daily_metrics m JOIN contacts c ON c.id = m.contact_id
		 WHERE c.merged_into IS NULL
		 GROUP BY m.contact_id HAVING MAX(m.day) < ?
		 ORDER BY MAX(m.day) ASC, m.contact_id ASC LIMIT 1`, cutoff); r.Scan(&dormantID, &dormantName) == nil {
		// ok
	} else {
		dormantID = 0
	}

	// 未回复：在若干高亲密候选里找「对方最后发言且我方未回」的一个。
	// lastUnrepliedLocked 需调用方持 dbMu，符合本锁；候选 ID 复用锁外拿好的 intimList，绝不在此再取锁。
	ids := make([]int64, 0, 10)
	for _, it := range intimList {
		ids = append(ids, it.ContactID)
		if len(ids) >= 10 {
			break
		}
	}
	for _, id := range ids {
		if unrep, _ := lastUnrepliedLocked(db, id); unrep {
			replyID = id
			db.QueryRow(`SELECT COALESCE(remark, name, '') FROM contacts WHERE id=?`, id).Scan(&replyName)
			break
		}
	}
	dbMu.Unlock()

	// 组靶点：去重（同人只留一条），按固定顺序，最多 challengeMaxPerWeek 条。
	seen := map[int64]bool{}
	add := func(out *[]chalTarget, kind string, id int64) {
		if id <= 0 || seen[id] || len(*out) >= challengeMaxPerWeek {
			return
		}
		seen[id] = true
		*out = append(*out, chalTarget{kind: kind, contactID: id})
	}
	var out []chalTarget
	add(&out, chalReachDormant, dormantID)
	add(&out, chalReplyPending, replyID)
	add(&out, chalGreetCore, coreID)
	return out
}

// generateWeeklyChallenges 为本周把选定的靶点幂等落库（INSERT OR IGNORE，已存在不覆盖 achieved）。
func generateWeeklyChallenges(db *sql.DB, now time.Time, targets []chalTarget) error {
	if len(targets) == 0 {
		return nil
	}
	ws := weekStartOf(now)
	nowStr := now.Format(time.RFC3339)
	dbMu.Lock()
	defer dbMu.Unlock()
	for _, t := range targets {
		if _, err := db.Exec(
			`INSERT OR IGNORE INTO weekly_challenges (week_start, contact_id, kind, target, achieved, xp, done_at, created_at)
			 VALUES (?, ?, ?, 1, 0, 0, '', ?)`, ws, t.contactID, t.kind, nowStr); err != nil {
			return err
		}
	}
	return nil
}

type doneEvent struct {
	contactID int64
	title     string
	when      time.Time
}

// detectCompletions 检测本周待完成挑战是否已达成（该联系人本周我方发过言），首次达成即加 XP、升级、
// 返回需在锁外补写时间线的里程碑事件。幂等：只处理 achieved=0 的行。
func detectCompletions(db *sql.DB, now time.Time) ([]doneEvent, error) {
	ws := weekStartOf(now)
	today := now.Format("2006-01-02")

	// pass1 读：待完成行 + 本周我方发言量。
	type pend struct {
		kind      string
		contactID int64
	}
	var pending []pend
	weeklyMe := map[int64]int{}
	dbMu.Lock()
	if rows, err := db.Query(`SELECT kind, contact_id FROM weekly_challenges WHERE week_start=? AND achieved=0`, ws); err == nil {
		for rows.Next() {
			var p pend
			if rows.Scan(&p.kind, &p.contactID) == nil {
				pending = append(pending, p)
			}
		}
		rows.Close()
	}
	for _, p := range pending {
		var me int
		db.QueryRow(`SELECT COALESCE(SUM(me_count),0) FROM relationship_daily_metrics
			WHERE contact_id=? AND day>=? AND day<=?`, p.contactID, ws, today).Scan(&me)
		weeklyMe[p.contactID] = me
	}
	dbMu.Unlock()

	var newly []pend
	for _, p := range pending {
		if weeklyMe[p.contactID] > 0 {
			newly = append(newly, p)
		}
	}
	if len(newly) == 0 {
		return nil, nil
	}

	// pass2 写：置 achieved、加 XP、重算等级。
	nowStr := now.Format(time.RFC3339)
	addXP := len(newly) * challengeXPPerDone
	dbMu.Lock()
	for _, p := range newly {
		if _, err := db.Exec(`UPDATE weekly_challenges SET achieved=1, xp=?, done_at=?
			WHERE week_start=? AND kind=? AND contact_id=? AND achieved=0`,
			challengeXPPerDone, nowStr, ws, p.kind, p.contactID); err != nil {
			dbMu.Unlock()
			return nil, err
		}
	}
	if _, err := db.Exec(`UPDATE gamification_state SET xp = xp + ?, level = 1 + (xp + ?) / ?, updated_at = ? WHERE id = 1`,
		addXP, addXP, xpPerLevel, nowStr); err != nil {
		dbMu.Unlock()
		return nil, err
	}
	dbMu.Unlock()

	// pass3 锁外补写时间线里程碑。
	events := make([]doneEvent, 0, len(newly))
	for _, p := range newly {
		title := challengeTitle(p.kind)
		RecordContactEvent(db, p.contactID, "milestone", "完成本周挑战："+title, "关系维护挑战达成，+"+strconv.Itoa(challengeXPPerDone)+" XP", now)
		events = append(events, doneEvent{p.contactID, title, now})
	}
	return events, nil
}

// challengeTitle 挑战类型 → 中文标题。
func challengeTitle(kind string) string {
	switch kind {
	case chalReachDormant:
		return "联系久未联系的朋友"
	case chalReplyPending:
		return "回复对方还没回的消息"
	case chalGreetCore:
		return "问候核心圈的人"
	default:
		return "维护一次关系"
	}
}

// ChallengeRow 展示用一条挑战。
type ChallengeRow struct {
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	ContactID int64  `json:"contactId"`
	Name      string `json:"name"`
	Achieved  bool   `json:"achieved"`
	DoneAt    string `json:"doneAt"`
	XP        int    `json:"xp"`
}

// ChallengeResponse /api/assistant/challenges 响应体。
type ChallengeResponse struct {
	WeekStart      string         `json:"weekStart"`
	GeneratedAt    string         `json:"generatedAt"`
	Challenges     []ChallengeRow `json:"challenges"`
	Done           int            `json:"done"`
	Total          int            `json:"total"`
	XP             int            `json:"xp"`
	Level          int            `json:"level"`
	NextLevelAt    int            `json:"nextLevelAt"`
	BadgesUnlocked int            `json:"badgesUnlocked"`
}

// readChallengesState 读本周挑战清单 + 全局 XP/等级 + 徽章总数，组装响应。
func readChallengesState(db *sql.DB, now time.Time) (*ChallengeResponse, error) {
	ws := weekStartOf(now)
	resp := &ChallengeResponse{
		WeekStart:   ws,
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
		Challenges:  []ChallengeRow{},
	}
	dbMu.Lock()
	if rows, err := db.Query(
		`SELECT wc.kind, wc.contact_id, COALESCE(c.remark, c.name, ''), wc.achieved, wc.done_at, wc.xp
		 FROM weekly_challenges wc LEFT JOIN contacts c ON c.id = wc.contact_id
		 WHERE wc.week_start = ? ORDER BY wc.kind ASC, wc.contact_id ASC`, ws); err == nil {
		for rows.Next() {
			var r ChallengeRow
			var ach int
			if rows.Scan(&r.Kind, &r.ContactID, &r.Name, &ach, &r.DoneAt, &r.XP) == nil {
				r.Title = challengeTitle(r.Kind)
				r.Achieved = ach == 1
				if r.Achieved {
					resp.Done++
				}
				resp.Challenges = append(resp.Challenges, r)
			}
		}
		rows.Close()
	}
	resp.Total = len(resp.Challenges)

	var xp int
	db.QueryRow(`SELECT xp FROM gamification_state WHERE id=1`).Scan(&xp)
	resp.XP = xp
	resp.Level = levelForXP(xp)
	resp.NextLevelAt = resp.Level * xpPerLevel

	// 徽章总数：已解锁成就档位数（表缺失则 0）。
	if tableExistsLocked(db, "contact_achievements") {
		var badges int
		db.QueryRow(`SELECT COUNT(*) FROM contact_achievements`).Scan(&badges)
		resp.BadgesUnlocked = badges
	}
	dbMu.Unlock()
	return resp, nil
}

// BuildChallenges 编排：建表 → 选靶 → 幂等生成 → 完成检测（含锁外事件）→ 读回。
func BuildChallenges(db *sql.DB, now time.Time) (*ChallengeResponse, error) {
	if err := ensureGamification(db); err != nil {
		return nil, err
	}
	if err := generateWeeklyChallenges(db, now, selectChallengeTargets(db, now)); err != nil {
		slog.Warn("挑战：生成本周清单失败", "err", err)
	}
	if _, err := detectCompletions(db, now); err != nil {
		slog.Warn("挑战：完成检测失败", "err", err)
	}
	return readChallengesState(db, now)
}

// hChallenges GET /api/assistant/challenges：本周挑战 + XP/等级 + 徽章。
func (s *apiServer) hChallenges(w http.ResponseWriter, r *http.Request) {
	resp, err := BuildChallenges(s.db, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取维护挑战失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
