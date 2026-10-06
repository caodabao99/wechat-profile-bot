package main

// v5.3.0 #10：消息主题演化追踪（LLM，唯一涉模型的新功能，双重降级门控）。
//
// 定期（周一调度 + 手动触发）让模型基于某联系人近段时间的真实聊天原文，聚出主题清单，
//   并与上周清单对照标出 emergent（新涌现）/ persistent（持续）/ fading（消退）。按 ISO 周
//   幂等落一行进 contact_topic_history，让「聊过什么」长出时间维度。
//
// 设计铁律：
//   - 门控降级：仅当 llm.configured() 且 AdvancedInsightsEnabled 才在周一跑；未配模型 → 读路径
//     返回空历史 + note（诚实、不 500、绝不编造），手动 POST 明确 503（与 summary 一致口径）。
//   - 不塞进 ComputeAdvancedInsights（后者保持 LLM-free）；独立函数、独立周锁。
//   - 单连接池分层锁：取原文（loadSummaryInputs）、读上周（latestTopicsBefore）、渲染提示词
//     （RenderPrompt 内部自锁、必须锁外调用）、LLM 调用（无锁）、落库（upsert 自锁）各趟顺序取放，
//     绝不嵌套；LLM 调用一律在未持锁时发起。
//   - contact_topic_history 是 LLM 产物、持久表（不加入 derivedTables）→ 依 listRestoreTables 自动
//     纳入备份/恢复、恢复后不重跑模型；每联系人裁剪近 topicsHistoryMaxWeeks 周（仿 quality_history）。
//   - 坏 JSON 回退上周快照、不新增信息；有效但空数组如实落空。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	topicsWindowDays         = 90  // 缺省主题统计窗口
	topicsMaxContacts        = 30  // 周一批量刷新一次最多覆盖的活跃联系人数
	topicsMinMsgs            = 12  // 少于此数不为该联系人跑模型（原文不足）
	topicsMaxPerContact      = 8   // 每联系人每周最多主题数
	topicsHistoryMaxWeeks    = 104 // 每联系人仅留近 104 周
	topicsHistoryDefaultShow = 26  // GET 缺省返回近 26 周
)

// TopicEntry 单个主题（LLM 产物解析后的规范化结构）。
type TopicEntry struct {
	Name   string `json:"name"`
	Weight int    `json:"weight"` // 0-100 占比
	Status string `json:"status"` // emergent / persistent / fading
}

// TopicWeek 某联系人某周的主题快照（映射 contact_topic_history 一行）。
type TopicWeek struct {
	WeekStart   string       `json:"weekStart"`
	GeneratedAt string       `json:"generatedAt"`
	Topics      []TopicEntry `json:"topics"`
}

// ContactTopics 某联系人的主题演化历史（GET 响应体）。
type ContactTopics struct {
	ContactID int64       `json:"contactId"`
	Name      string      `json:"name"`
	Window    int         `json:"windowDays"`
	Weeks     []TopicWeek `json:"weeks"` // 按 week_start 升序（老→新）
	Note      string      `json:"note"`
}

// ensureTopicHistory 懒建主题历史表 + 周锁状态行（幂等 DDL，不 bump user_version）。
// 两张均持久表、不入 derivedTables → 依 listRestoreTables 自动纳入备份/恢复。
func ensureTopicHistory(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS contact_topic_history (
		contact_id INTEGER NOT NULL,
		week_start TEXT NOT NULL,
		topics_json TEXT NOT NULL DEFAULT '[]',
		generated_at TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (contact_id, week_start))`); err != nil {
		return fmt.Errorf("建主题演化历史表失败: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS topic_evolution_state (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		last_week TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL DEFAULT '')`); err != nil {
		return fmt.Errorf("建主题演化周锁表失败: %w", err)
	}
	return nil
}

// topicWeekFresh 本周是否已跑过主题演化（用状态行的 last_week 作天然周锁，避免重跑与并发）。
func topicWeekFresh(db *sql.DB, ws string) bool {
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "topic_evolution_state") {
		return false
	}
	var lw string
	if db.QueryRow(`SELECT last_week FROM topic_evolution_state WHERE id=1`).Scan(&lw) != nil {
		return false
	}
	return lw == ws
}

// markTopicWeekRun 把「本周已触发」落进状态行（在派生异步任务前同步置位，天然防并发重入）。
func markTopicWeekRun(db *sql.DB, ws string, now time.Time) {
	if err := ensureTopicHistory(db); err != nil {
		slog.Warn("主题演化：建表失败", "err", err)
		return
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	if _, err := db.Exec(`INSERT INTO topic_evolution_state (id, last_week, updated_at)
		VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET last_week=excluded.last_week, updated_at=excluded.updated_at`,
		ws, now.Format(time.RFC3339)); err != nil {
		slog.Warn("主题演化：写周锁状态失败", "err", err)
	}
}

// decodeTopics 解析 topics_json（坏/空一律回退空切片）。
func decodeTopics(s string) []TopicEntry {
	out := []TopicEntry{}
	s = strings.TrimSpace(s)
	if s == "" {
		return out
	}
	var ts []TopicEntry
	if json.Unmarshal([]byte(s), &ts) != nil {
		return out
	}
	if ts != nil {
		out = ts
	}
	return out
}

// normalizeTopicEntries 规范化模型产出的主题：去空名/去重、名称截断、weight 夹取、status 枚举兜底，
// 按 weight 降序、同分按名称升序（确定性），截到 topicsMaxPerContact。
func normalizeTopicEntries(in []TopicEntry) []TopicEntry {
	seen := map[string]bool{}
	out := []TopicEntry{}
	for _, t := range in {
		name := strings.TrimSpace(t.Name)
		if name == "" || seen[name] {
			continue
		}
		if r := []rune(name); len(r) > 12 {
			name = string(r[:12])
		}
		w := t.Weight
		if w < 0 {
			w = 0
		}
		if w > 100 {
			w = 100
		}
		st := strings.TrimSpace(t.Status)
		switch st {
		case "emergent", "persistent", "fading":
		default:
			st = "persistent" // 状态非法/缺失时保守归为持续
		}
		seen[name] = true
		out = append(out, TopicEntry{Name: name, Weight: w, Status: st})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight > out[j].Weight
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > topicsMaxPerContact {
		out = out[:topicsMaxPerContact]
	}
	return out
}

// parseTopicJSON 解析模型返回的 {"topics":[{"name","weight","status"}]}。
// 返回 ok=false 表示不是合法 JSON（触发调用方回退上周快照）；合法但空数组仍 ok=true。
func parseTopicJSON(raw string) ([]TopicEntry, bool) {
	s := strings.TrimSpace(ExtractJSON(raw))
	var wrapper struct {
		Topics []struct {
			Name   string `json:"name"`
			Weight int    `json:"weight"`
			Status string `json:"status"`
		} `json:"topics"`
	}
	if json.Unmarshal([]byte(s), &wrapper) != nil {
		return nil, false
	}
	in := make([]TopicEntry, 0, len(wrapper.Topics))
	for _, t := range wrapper.Topics {
		in = append(in, TopicEntry{Name: t.Name, Weight: t.Weight, Status: t.Status})
	}
	return normalizeTopicEntries(in), true
}

// latestTopicsBefore 读该联系人早于本周的最近一条主题快照（供提示词 prevTopics 与坏 JSON 回退）。
func latestTopicsBefore(db *sql.DB, contactID int64, beforeWeek string) []TopicEntry {
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "contact_topic_history") {
		return []TopicEntry{}
	}
	var tj string
	if db.QueryRow(`SELECT topics_json FROM contact_topic_history
		WHERE contact_id=? AND week_start<? ORDER BY week_start DESC LIMIT 1`, contactID, beforeWeek).Scan(&tj) != nil {
		return []TopicEntry{}
	}
	return decodeTopics(tj)
}

// upsertTopicHistory 幂等写入本周主题快照，并裁剪仅留近 topicsHistoryMaxWeeks 周。best-effort：
// 写失败只记日志，不返回错误（主题历史是增值信息，不能因此让分析接口失败）。
func upsertTopicHistory(db *sql.DB, contactID int64, ws string, topics []TopicEntry, now time.Time) {
	tj, _ := json.Marshal(topics)
	genAt := now.Format("2006-01-02 15:04:05")
	dbMu.Lock()
	defer dbMu.Unlock()
	if _, err := db.Exec(`INSERT INTO contact_topic_history (contact_id, week_start, topics_json, generated_at)
		VALUES (?,?,?,?)
		ON CONFLICT(contact_id, week_start) DO UPDATE SET topics_json=excluded.topics_json, generated_at=excluded.generated_at`,
		contactID, ws, string(tj), genAt); err != nil {
		if !isNoSuchTable(err) {
			slog.Warn("写入主题演化历史失败", "contactId", contactID, "err", err)
		}
		return
	}
	cut := weekStartOf(now.AddDate(0, 0, -7*topicsHistoryMaxWeeks))
	if _, err := db.Exec(`DELETE FROM contact_topic_history WHERE contact_id=? AND week_start<?`, contactID, cut); err != nil {
		slog.Warn("裁剪主题演化历史失败", "contactId", contactID, "err", err)
	}
}

// analyzeContactTopics 对单个联系人跑一次主题演化聚类并落库当周快照。未配模型返回 ErrLLMNotConfigured。
// 各步分层取锁、LLM 调用在锁外，绝不嵌套。
func analyzeContactTopics(ctx context.Context, db *sql.DB, llm *LLMClient, contactID int64, windowDays int, now time.Time) (*TopicWeek, error) {
	if !llm.configured() {
		return nil, ErrLLMNotConfigured
	}
	if windowDays <= 0 {
		windowDays = topicsWindowDays
	}
	// 取原文（loadSummaryInputs 内部自锁并释放，返回时间正序消息）。
	c, _, msgs, err := loadSummaryInputs(db, contactID, windowDays)
	if err != nil {
		return nil, err
	}
	ws := weekStartOf(now)
	if len(msgs) < topicsMinMsgs {
		return nil, fmt.Errorf("近 %d 天原文不足 %d 条，暂不聚类主题", windowDays, topicsMinMsgs)
	}
	_, captioned := numberMessagesForSummary(msgs)

	// 上周主题（供对照 + 坏 JSON 回退）。
	prev := latestTopicsBefore(db, contactID, ws)
	prevJSON, _ := json.Marshal(prev)

	// 渲染提示词（RenderPrompt 内部取 dbMu，必须锁外调用）。
	prompt, err := RenderPrompt(db, "topic_evolution", map[string]string{
		"name":       displayName(c),
		"days":       strconv.Itoa(windowDays),
		"captioned":  strings.Join(captioned, "\n"),
		"prevTopics": string(prevJSON),
	})
	if err != nil {
		return nil, err
	}

	// 调模型（无锁）。
	raw, err := callLLMCached(ctx, db, llm, contactID, TaskTopic, "", prompt)
	if err != nil {
		return nil, err
	}
	topics, ok := parseTopicJSON(raw)
	if !ok {
		topics = prev // 坏 JSON：回退上周快照，不新增信息
	}
	tw := &TopicWeek{WeekStart: ws, GeneratedAt: now.Format("2006-01-02 15:04:05"), Topics: topics}
	upsertTopicHistory(db, contactID, ws, topics, now)
	return tw, nil
}

// activeContactsForTopics 取近 windowDays 互动最多、且原文不少于 topicsMinMsgs 的活跃联系人（上限护栏）。
func activeContactsForTopics(db *sql.DB, now time.Time, windowDays int) []int64 {
	from := now.AddDate(0, 0, -windowDays).Format("2006-01-02")
	out := []int64{}
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "relationship_daily_metrics") {
		return out
	}
	// 自愈：指标表为空但有消息（活跃或归档）时全量重建一次（与 ComputeHealth 同语义）。
	var mc int
	if db.QueryRow(`SELECT COUNT(*) FROM relationship_daily_metrics`).Scan(&mc) == nil && mc == 0 {
		if hasMessagesForRebuildLocked(db, 0) {
			_, _ = rebuildDailyMetricsLocked(db, 0)
		}
	}
	if rows, err := db.Query(
		`SELECT m.contact_id FROM relationship_daily_metrics m JOIN contacts c ON c.id = m.contact_id
		 WHERE c.merged_into IS NULL AND m.day >= ?
		 GROUP BY m.contact_id HAVING SUM(m.me_count + m.other_count) >= ?
		 ORDER BY SUM(m.me_count + m.other_count) DESC, m.contact_id ASC LIMIT ?`,
		from, topicsMinMsgs, topicsMaxContacts); err == nil {
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				out = append(out, id)
			}
		}
		rows.Close()
	}
	return out
}

// ComputeTopicEvolution 对活跃联系人逐个跑主题演化并落库（供周一调度与手动批量复用）。
// 未配模型返回空 + note（不 500，交调用方按 note 呈现）。返回成功聚类的联系人数。
func ComputeTopicEvolution(ctx context.Context, db *sql.DB, llm *LLMClient, now time.Time, windowDays int) (int, string, error) {
	if !llm.configured() {
		return 0, "未配置对话模型，主题演化不可用。", ErrLLMNotConfigured
	}
	if err := ensureTopicHistory(db); err != nil {
		return 0, "", err
	}
	actives := activeContactsForTopics(db, now, windowDays)
	computed := 0
	for _, id := range actives {
		if _, err := analyzeContactTopics(ctx, db, llm, id, windowDays, now); err != nil {
			slog.Warn("主题演化：单联系人分析失败", "contactId", id, "err", err)
			continue
		}
		computed++
	}
	note := fmt.Sprintf("本周已对 %d 位活跃联系人更新主题演化。", computed)
	return computed, note, nil
}

// GetContactTopics 读某联系人主题演化历史（只读、不调模型）。
func GetContactTopics(db *sql.DB, contactID int64, weeks int) (*ContactTopics, error) {
	c, err := GetContactByID(db, contactID)
	if err != nil {
		return nil, err
	}
	if weeks <= 0 {
		weeks = topicsHistoryDefaultShow
	}
	dbMu.Lock()
	weeksOut := []TopicWeek{}
	if tableExistsLocked(db, "contact_topic_history") {
		if rows, err := db.Query(`SELECT week_start, topics_json, generated_at FROM contact_topic_history
			WHERE contact_id=? ORDER BY week_start DESC LIMIT ?`, contactID, weeks); err == nil {
			for rows.Next() {
				var tw TopicWeek
				var tj string
				if rows.Scan(&tw.WeekStart, &tj, &tw.GeneratedAt) == nil {
					tw.Topics = decodeTopics(tj)
					weeksOut = append(weeksOut, tw)
				}
			}
			rows.Close()
		}
	}
	dbMu.Unlock()
	// DESC 取最近 N 周后翻回升序（老→新），供前端画演化。
	sort.SliceStable(weeksOut, func(i, j int) bool { return weeksOut[i].WeekStart < weeksOut[j].WeekStart })

	note := ""
	if len(weeksOut) == 0 {
		note = "暂无主题演化记录——周一自动分析或在此联系人页手动触发后生成。"
	}
	return &ContactTopics{
		ContactID: contactID,
		Name:      displayName(c),
		Window:    topicsWindowDays,
		Weeks:     weeksOut,
		Note:      note,
	}, nil
}

// routeContactTopics /api/contacts/{id}/topics：
//
//	GET  → 读历史（不调模型）；POST topics/analyze → 手动跑一次 LLM 聚类。
func (s *apiServer) routeContactTopics(w http.ResponseWriter, r *http.Request, id int64, sub []string) {
	if len(sub) == 0 {
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		if _, err := GetContactByID(s.db, id); err != nil {
			writeErr(w, http.StatusNotFound, "联系人不存在")
			return
		}
		res, err := GetContactTopics(s.db, id, 0)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取主题演化失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}

	if sub[0] == "analyze" && len(sub) == 1 {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		if _, err := GetContactByID(s.db, id); err != nil {
			writeErr(w, http.StatusNotFound, "联系人不存在")
			return
		}
		if !s.llm.configured() {
			writeErr(w, http.StatusServiceUnavailable, ErrLLMNotConfigured.Error())
			return
		}
		if err := ensureTopicHistory(s.db); err != nil {
			writeErr(w, http.StatusInternalServerError, "初始化主题历史失败: "+err.Error())
			return
		}
		tw, err := analyzeContactTopics(r.Context(), s.db, s.llm, id, topicsWindowDays, time.Now())
		if err != nil {
			if err == ErrLLMNotConfigured {
				writeErr(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, tw)
		return
	}

	writeErr(w, http.StatusNotFound, "未知接口")
}
