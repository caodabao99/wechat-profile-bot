package main

// 关系成就 / 里程碑系统（增值功能，纯确定性、离线可算、零 LLM）。
//
// 从现有聊天记录自动派生若干「关系里程碑」，游戏化驱动关系维护：
//   - 累计消息条数（无话不谈 → 挚友，档位 100 / 500 / 1000 / 5000）
//   - 连续互动天数（messages ∪ messages_archive 有任一发言的自然日，扫最长连续段，7 / 30 / 100）
//   - 相识周年（起点＝首条消息时间；无消息回退 contacts.created_at，满 1 / 3 / 5 年）
//
// 设计铁律（与 quality.go / timeline.go 一致）：
//   - 指标全部本地 SQL + Go 计算，结果确定、可复现、可离线单测（evaluateAchievements 是纯函数）。
//   - 检测即去重落库：某档位「首次观测到达成」才写 contact_achievements 一行 + 一条
//     kind="milestone" 的 contact_events（进时间线，自动可回溯解锁历史）；已记录档位不重复写。
//   - 单连接池：读指标一趟锁、读已记录一趟锁、写库一趟锁；RecordContactEvent 自持锁，
//     一律在「释放锁后」调用，绝不嵌套。
//   - 不新增 config.json 开关；contact_achievements 按持久化用户数据（进备份/恢复），
//     不入 derivedTables——否则恢复清空去重表会让时间线里程碑重复写入。

import (
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type achMetricKind string

const (
	metricMsgTotal   achMetricKind = "msg_total"
	metricStreakDays achMetricKind = "streak_days"
	metricYearsKnown achMetricKind = "years_known"
)

// achievementDef 一个成就度量及其档位阈值（升序）。注册表是展示 / 检测的单一事实源。
type achievementDef struct {
	Key    string        // 唯一键，同时是 contact_achievements.ach_key
	Metric achMetricKind // 度量类型
	Label  string        // 人类可读标题
	Tiers  []int         // 升序档位阈值
	Unit   string        // 展示单位：条 / 天 / 年
}

// achievementDefs 默认档位集（可按需增删）。顺序即前端展示顺序。
var achievementDefs = []achievementDef{
	{Key: "msg_total", Metric: metricMsgTotal, Label: "累计消息", Tiers: []int{100, 500, 1000, 5000}, Unit: "条"},
	{Key: "streak_days", Metric: metricStreakDays, Label: "连续互动", Tiers: []int{7, 30, 100}, Unit: "天"},
	{Key: "years_known", Metric: metricYearsKnown, Label: "相识周年", Tiers: []int{1, 3, 5}, Unit: "年"},
}

// achMetrics 某联系人的成就度量原语（不含时间相关派生）。
type achMetrics struct {
	msgTotal  int       // 累计消息条数（messages ∪ archive）
	maxStreak int       // 最长连续互动天数
	firstTime time.Time // 相识起点（首条消息时间，无则 contacts.created_at）
}

// recAchieve 已落库的达成记录（用于去重与回填解锁时间）。
type recAchieve struct {
	achievedAt string
}

// pendingUnlock 一次「新跨越」的成就，待落库 + 写时间线。
type pendingUnlock struct {
	Key        string
	Tier       int
	AchievedAt time.Time
	Title      string
	Detail     string
}

// achievementItem 单个档位的展示项（含未达成进度）。
type achievementItem struct {
	Key        string `json:"key"`
	Label      string `json:"label"`
	Tier       int    `json:"tier"`
	Target     int    `json:"target"`
	Current    int    `json:"current"`
	Pct        int    `json:"pct"`
	Met        bool   `json:"met"`
	Unit       string `json:"unit"`
	AchievedAt string `json:"achievedAt,omitempty"`
}

// achievementsResponse GET /api/contacts/{id}/achievements 的响应体。
type achievementsResponse struct {
	ContactID   int64             `json:"contactId"`
	Name        string            `json:"name"`
	Items       []achievementItem `json:"items"`
	Summary     string            `json:"summary"`
	GeneratedAt string            `json:"generatedAt"`
}

// longestStreak 纯函数：给定一组「YYYY-MM-DD」自然日（可乱序），返回最长连续天数。
// 用 UTC 日期解析后按 24h 步进判定相邻，规避本地时区 / 夏令时漂移（见 weekStartOf 教训）。
func longestStreak(days []string) int {
	if len(days) == 0 {
		return 0
	}
	ds := append([]string(nil), days...)
	sort.Strings(ds)
	prev, err := time.Parse("2006-01-02", ds[0])
	if err != nil {
		return 0
	}
	best, run := 1, 1
	for i := 1; i < len(ds); i++ {
		cur, err := time.Parse("2006-01-02", ds[i])
		if err != nil {
			continue
		}
		if cur.Equal(prev) {
			continue // 去重后不应出现，防御性跳过
		}
		if cur.Sub(prev) == 24*time.Hour {
			run++
		} else {
			run = 1
		}
		if run > best {
			best = run
		}
		prev = cur
	}
	return best
}

// wholeYearsSince 纯函数：from→now 的整年数（日历 AddDate 判定，不用时长，规避漂移）。
func wholeYearsSince(from, now time.Time) int {
	if from.IsZero() || now.Before(from) {
		return 0
	}
	y := now.Year() - from.Year()
	if y > 0 && now.AddDate(-y, 0, 0).Before(from) {
		y--
	}
	if y < 0 {
		y = 0
	}
	return y
}

// evaluateAchievements 纯函数（可脱离 DB 单测）：由度量 + 已记录集合 + now，产出全部档位展示项
// 与「新跨越」待落库清单。已记录档位沿用库内解锁时间；未记录且达成者视为新跨越。
// 相识周年的跨越时间取确定性日期 firstTime+N 年；其余取 now（应用首次观测时刻）。
func evaluateAchievements(metrics achMetrics, recorded map[string]recAchieve, now time.Time) ([]achievementItem, []pendingUnlock) {
	years := wholeYearsSince(metrics.firstTime, now)
	items := []achievementItem{}
	unlocks := []pendingUnlock{}
	for _, def := range achievementDefs {
		cur := 0
		switch def.Metric {
		case metricMsgTotal:
			cur = metrics.msgTotal
		case metricStreakDays:
			cur = metrics.maxStreak
		case metricYearsKnown:
			cur = years
		}
		for _, tier := range def.Tiers {
			pct := 0
			if tier > 0 {
				pct = cur * 100 / tier
				if pct > 100 {
					pct = 100
				}
				if pct < 0 {
					pct = 0
				}
			}
			met := cur >= tier
			item := achievementItem{
				Key: def.Key, Label: def.Label, Tier: tier, Target: tier,
				Current: cur, Pct: pct, Met: met, Unit: def.Unit,
			}
			id := def.Key + "|" + strconv.Itoa(tier)
			if rec, had := recorded[id]; had {
				item.AchievedAt = rec.achievedAt
			} else if met {
				at := now
				if def.Metric == metricYearsKnown && !metrics.firstTime.IsZero() {
					at = metrics.firstTime.AddDate(tier, 0, 0)
				}
				unlocks = append(unlocks, pendingUnlock{
					Key: def.Key, Tier: tier, AchievedAt: at,
					Title:  fmt.Sprintf("%s 满 %d %s", def.Label, tier, def.Unit),
					Detail: fmt.Sprintf("达成里程碑：%s %d %s", def.Label, tier, def.Unit),
				})
				item.AchievedAt = at.Format(time.RFC3339)
			}
			items = append(items, item)
		}
	}
	return items, unlocks
}

// ensureAchievements 懒建去重表（不 bump user_version）；持单层 dbMu。
func ensureAchievements(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS contact_achievements (
		contact_id INTEGER NOT NULL,
		ach_key TEXT NOT NULL,
		tier INTEGER NOT NULL,
		achieved_at TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (contact_id, ach_key, tier)
	)`)
	if err != nil {
		return fmt.Errorf("建成就表失败: %w", err)
	}
	return nil
}

// computeAchMetrics 单趟 dbMu 读：累计条数 / 首条时间 / 活跃自然日集合（→ 最长连续段）。
// 三个聚合查询都在同一把锁内顺序执行，绝不调用任何再加锁的函数。
func computeAchMetrics(db *sql.DB, id int64) (achMetrics, string, error) {
	var m achMetrics
	var name, remark, createdAt string
	dbMu.Lock()
	defer dbMu.Unlock()
	if err := db.QueryRow(`SELECT COALESCE(name,''), COALESCE(remark,''), COALESCE(created_at,'') FROM contacts WHERE id = ?`, id).Scan(&name, &remark, &createdAt); err != nil {
		return m, "", err
	}
	// 累计条数 / 首末 / 活跃日必须并档 messages_archive（归档把老消息移出 messages，只看 messages 会低估）。
	unit := `SELECT COALESCE(msg_unix, CAST(strftime('%s', msg_time) AS INTEGER)) AS su FROM messages WHERE contact_id = ? AND COALESCE(msg_time,'') != ''`
	args := []interface{}{id}
	if tableExistsLocked(db, "messages_archive") {
		unit += ` UNION ALL SELECT COALESCE(msg_unix, CAST(strftime('%s', msg_time) AS INTEGER)) FROM messages_archive WHERE contact_id = ? AND COALESCE(msg_time,'') != ''`
		args = append(args, id)
	}
	u := `(` + unit + `)`
	if err := db.QueryRow(`SELECT COUNT(*) FROM `+u, args...).Scan(&m.msgTotal); err != nil {
		return m, "", err
	}
	var minSu sql.NullInt64
	if err := db.QueryRow(`SELECT MIN(su) FROM `+u+` WHERE su > 0`, args...).Scan(&minSu); err != nil {
		return m, "", err
	}
	rows, err := db.Query(`SELECT DISTINCT strftime('%Y-%m-%d', su, 'unixepoch', 'localtime') AS d FROM `+u+` WHERE su > 0 ORDER BY d`, args...)
	if err != nil {
		return m, "", err
	}
	var days []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err == nil && d != "" {
			days = append(days, d)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return m, "", err
	}
	m.maxStreak = longestStreak(days)
	if minSu.Valid && minSu.Int64 > 0 {
		m.firstTime = time.Unix(minSu.Int64, 0)
	} else if t, ok := parseTimeLoose(createdAt); ok {
		m.firstTime = t
	}
	disp := name
	if strings.TrimSpace(remark) != "" {
		disp = remark + "（" + name + "）"
	}
	return m, disp, nil
}

// readRecordedAchievements 单趟 dbMu 读回该联系人已落库的达成档位集合。表未就绪时返回空。
func readRecordedAchievements(db *sql.DB, id int64) (map[string]recAchieve, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	out := map[string]recAchieve{}
	if !tableExistsLocked(db, "contact_achievements") {
		return out, nil
	}
	rows, err := db.Query(`SELECT ach_key, tier, achieved_at FROM contact_achievements WHERE contact_id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, at string
		var tier int
		if err := rows.Scan(&k, &tier, &at); err != nil {
			continue
		}
		out[k+"|"+strconv.Itoa(tier)] = recAchieve{achievedAt: at}
	}
	return out, nil
}

// persistAchievementUnlocks 一趟锁批量 INSERT OR IGNORE 落库；释放锁后再逐条写时间线事件。
// RecordContactEvent 自持 dbMu，必须在未持锁时调用，故放在写库锁之后。
func persistAchievementUnlocks(db *sql.DB, id int64, unlocks []pendingUnlock) error {
	if len(unlocks) == 0 {
		return nil
	}
	now := time.Now().Format(time.RFC3339)
	dbMu.Lock()
	var firstErr error
	for _, u := range unlocks {
		if _, err := db.Exec(`INSERT OR IGNORE INTO contact_achievements (contact_id, ach_key, tier, achieved_at, created_at) VALUES (?,?,?,?,?)`,
			id, u.Key, u.Tier, u.AchievedAt.Format(time.RFC3339), now); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	dbMu.Unlock()
	if firstErr != nil {
		return firstErr
	}
	for _, u := range unlocks {
		RecordContactEvent(db, id, "milestone", u.Title, u.Detail, u.AchievedAt)
	}
	return nil
}

// DetectAndRecordAchievements 计算指标 → 去重落库新跨越 → 返回全部档位（含进度）。
// 各步骤各自取放 dbMu，串行执行，不嵌套锁。now 由调用方注入以便测试确定性。
func DetectAndRecordAchievements(db *sql.DB, contactID int64, now time.Time) (*achievementsResponse, error) {
	metrics, name, err := computeAchMetrics(db, contactID)
	if err != nil {
		return nil, err
	}
	recorded, err := readRecordedAchievements(db, contactID)
	if err != nil {
		return nil, err
	}
	items, unlocks := evaluateAchievements(metrics, recorded, now)
	if len(unlocks) > 0 {
		if err := persistAchievementUnlocks(db, contactID, unlocks); err != nil {
			return nil, err
		}
	}
	metCount := 0
	for _, it := range items {
		if it.Met {
			metCount++
		}
	}
	return &achievementsResponse{
		ContactID:   contactID,
		Name:        name,
		Items:       items,
		Summary:     fmt.Sprintf("已达成 %d 个里程碑", metCount),
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
	}, nil
}

// routeContactAchievements GET /api/contacts/{id}/achievements
// 只读语义但含幂等副作用：首次检测到新跨越会写 contact_achievements + 时间线事件；重复调用无重复。
func (s *apiServer) routeContactAchievements(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	if _, err := GetContactByID(s.db, id); err != nil {
		writeErr(w, http.StatusNotFound, "联系人不存在")
		return
	}
	if err := ensureAchievements(s.db); err != nil {
		writeErr(w, http.StatusInternalServerError, "初始化成就表失败: "+err.Error())
		return
	}
	// 里程碑事件写进 contact_events（时间线）；正常由启动期 ensureTimelineTables 建好，
	// 这里再兜底一次（幂等、best-effort），确保即便调用顺序变化里程碑事件也不会静默丢失。
	_ = ensureTimelineTables(s.db)
	resp, err := DetectAndRecordAchievements(s.db, id, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "检测关系成就失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
