package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 关系变化检测 + 下一步行动（Phase 6 + 7）。
//
// relationship_daily_metrics：把每个联系人的聊天按「自然日 × 发送方」预聚合，作为一切
//   趋势判断的硬数据底座（不依赖 LLM，可反复全量重建）。
// 升温/降温：读取近 30 天 vs 前 30 天的互动量、距今沉默天数、主动方占比，实时算出趋势。
// relationship_action_suggestions：规则引擎产出可执行建议（冷却预警、未回复、长期沉默、
//   临近重要日子），draft 字段可选地交给 LLM 起草；没配模型时 draft 为空、建议依然真实可用。

// RebuildDailyMetrics 从 messages(+messages_archive) 全量重建日聚合。contactID=0 表示全部。
// 幂等：先删后插，按 (contact_id, day) upsert。返回处理的聚合行数。
func RebuildDailyMetrics(db *sql.DB, contactID int64) (int, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	return rebuildDailyMetricsLocked(db, contactID)
}

func rebuildDailyMetricsLocked(db *sql.DB, contactID int64) (int, error) {
	delArgs := []interface{}{}
	del := `DELETE FROM relationship_daily_metrics`
	if contactID > 0 {
		del += ` WHERE contact_id=?`
		delArgs = append(delArgs, contactID)
	}
	if _, err := db.Exec(del, delArgs...); err != nil {
		return 0, err
	}

	// 源：活跃表 (+ 归档表，若存在)。msg_unix 为空/0（无时间戳）的行无法归入自然日，跳过。
	src := `SELECT contact_id, sender, msg_unix FROM messages
	        WHERE msg_unix IS NOT NULL AND msg_unix > 0 AND (? = 0 OR contact_id = ?)`
	args := []interface{}{contactID, contactID}
	if tableExistsLocked(db, "messages_archive") {
		src += ` UNION ALL SELECT contact_id, sender, msg_unix FROM messages_archive
		        WHERE msg_unix IS NOT NULL AND msg_unix > 0 AND (? = 0 OR contact_id = ?)`
		args = append(args, contactID, contactID)
	}

	q := `INSERT INTO relationship_daily_metrics (contact_id, day, me_count, other_count, first_unix, last_unix)
		SELECT contact_id,
		       strftime('%Y-%m-%d', msg_unix, 'unixepoch', 'localtime') AS day,
		       SUM(CASE WHEN sender='me' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN sender='other' THEN 1 ELSE 0 END),
		       MIN(msg_unix), MAX(msg_unix)
		FROM (` + src + `)
		GROUP BY contact_id, day
		ON CONFLICT(contact_id, day) DO UPDATE SET
		  me_count=excluded.me_count, other_count=excluded.other_count,
		  first_unix=excluded.first_unix, last_unix=excluded.last_unix`
	res, err := db.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RelationshipTrend 关系趋势（实时算，不落库）
type RelationshipTrend struct {
	ContactID       int64   `json:"contactId"`
	Recent30        int     `json:"recent30"`      // 近 30 天互动总条数
	Prior30         int     `json:"prior30"`       // 前 30~60 天互动总条数
	DaysSinceLast   int     `json:"daysSinceLast"` // 距最近一次互动
	LastActivityDay string  `json:"lastActivityDay"`
	MeRatioRecent   float64 `json:"meRatioRecent"` // 近 30 天我方发送占比
	State           string  `json:"state"`         // warming|cooling|stable|dormant|new
	Summary         string  `json:"summary"`
}

func sumWindow(db *sql.DB, contactID int64, from, to string) (total, me int) {
	err := db.QueryRow(
		`SELECT COALESCE(SUM(me_count+other_count),0), COALESCE(SUM(me_count),0)
		 FROM relationship_daily_metrics
		 WHERE contact_id=? AND day>=? AND day<=?`, contactID, from, to).Scan(&total, &me)
	if err != nil {
		return 0, 0
	}
	return total, me
}

// GetRelationshipTrend 读取某联系人趋势。若该联系人有消息但指标表为空（例如刚恢复/老库），
// 先就地重建其日聚合，保证读到的永远是新鲜数据（自愈）。
func GetRelationshipTrend(db *sql.DB, contactID int64) (*RelationshipTrend, error) {
	// 趋势阈值来自运行模式设置，必须在 dbMu 加锁前读取（loadAssistantSettings 内部会取 dbMu，不可重入）。
	// 读失败/表不存在时 loadAssistantSettings 仍返回默认值，保持旧的硬编码行为。
	thr, _ := loadAssistantSettings(db)

	dbMu.Lock()
	defer dbMu.Unlock()

	var metricRows, msgRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM relationship_daily_metrics WHERE contact_id=?`, contactID).Scan(&metricRows); err != nil {
		return nil, err
	}
	// 自愈门槛同样计入归档：长期沉默的人消息可能已全部被归档，只看 messages 会算出 0 而不触发自愈。
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE contact_id=?`, contactID).Scan(&msgRows); err != nil {
		return nil, err
	}
	if tableExistsLocked(db, "messages_archive") {
		var an int
		if err := db.QueryRow(`SELECT COUNT(*) FROM messages_archive WHERE contact_id=?`, contactID).Scan(&an); err == nil {
			msgRows += an
		}
	}
	if metricRows == 0 && msgRows > 0 {
		if _, err := rebuildDailyMetricsLocked(db, contactID); err != nil {
			return nil, err
		}
	}

	now := time.Now()
	today := now.Format("2006-01-02")
	recentFrom := now.AddDate(0, 0, -29).Format("2006-01-02")
	priorFrom := now.AddDate(0, 0, -59).Format("2006-01-02")
	priorTo := now.AddDate(0, 0, -30).Format("2006-01-02")

	t := &RelationshipTrend{ContactID: contactID}
	recentTotal, recentMe := sumWindow(db, contactID, recentFrom, today)
	priorTotal, _ := sumWindow(db, contactID, priorFrom, priorTo)
	t.Recent30, t.Prior30 = recentTotal, priorTotal
	if recentTotal > 0 {
		t.MeRatioRecent = float64(recentMe) / float64(recentTotal)
	}
	var lastDay sql.NullString
	if err := db.QueryRow(
		`SELECT MAX(day) FROM relationship_daily_metrics WHERE contact_id=? AND (me_count+other_count)>0`, contactID).Scan(&lastDay); err != nil {
		return nil, err
	}
	t.LastActivityDay = lastDay.String
	if lastDay.Valid && lastDay.String != "" {
		if lt, err := time.ParseInLocation("2006-01-02", lastDay.String, time.Local); err == nil {
			t.DaysSinceLast = int(now.Sub(lt).Hours() / 24)
		}
	}
	t.State, t.Summary = classifyTrendWith(t, thr)
	return t, nil
}

// classifyTrend 依据默认阈值给出趋势结论（保留旧行为，供直接调用/测试）。
func classifyTrend(t *RelationshipTrend) (string, string) {
	return classifyTrendWith(t, defaultAssistantSettings())
}

// classifyTrendWith 依据互动量变化与沉默时长给出趋势结论（纯规则，可解释）。
// 沉寂天数门槛与降温/升温的“前期最低互动”门槛来自助手设置（可配）；
// 传入默认助手设置时即高灵敏档（沉寂 14 / 降温前期 4 / 升温前期 3）。
func classifyTrendWith(t *RelationshipTrend, thr AssistantSettings) (string, string) {
	if t.Recent30 == 0 && t.Prior30 == 0 {
		if t.DaysSinceLast > 0 {
			return "dormant", fmt.Sprintf("已 %d 天无互动，关系趋于沉寂", t.DaysSinceLast)
		}
		return "new", "尚无互动数据"
	}
	// 长期沉默优先级最高
	if t.DaysSinceLast >= thr.SilenceDays {
		return "dormant", fmt.Sprintf("已 %d 天未联系，建议主动问候", t.DaysSinceLast)
	}
	switch {
	case t.Prior30 >= thr.CoolingMinPrior && t.Recent30 <= t.Prior30/2:
		return "cooling", fmt.Sprintf("近 30 天互动 %d 条，较前期 %d 条明显减少，关系降温", t.Recent30, t.Prior30)
	case t.Prior30 >= thr.WarmingMinPrior && t.Recent30 >= t.Prior30*3/2:
		return "warming", fmt.Sprintf("近 30 天互动 %d 条，较前期 %d 条增长，关系升温", t.Recent30, t.Prior30)
	default:
		return "stable", fmt.Sprintf("近 30 天互动 %d 条，与前期基本持平", t.Recent30)
	}
}

// SuggestionView 一条行动建议
type SuggestionView struct {
	ID          int64  `json:"id"`
	ContactID   int64  `json:"contactId"`
	ContactName string `json:"contactName"`
	Kind        string `json:"kind"`
	Reason      string `json:"reason"`
	Draft       string `json:"draft"`
	Priority    int    `json:"priority"`
	Status      string `json:"status"`
	WindowKey   string `json:"windowKey"`
}

// GenerateActionSuggestions 为 contactID(0=全部活跃联系人) 生成/刷新行动建议（规则部分）。
// llm 可为 nil——此时只产出建议与理由，draft 留空；有 llm 时为冷却/沉默类补一段开场白草稿。
// 幂等：按 (contact_id, kind, window_key) upsert，且**不覆盖**用户已置的 done/dismissed 状态。
func GenerateActionSuggestions(db *sql.DB, llm *LLMClient, contactID int64) (int, error) {
	// 趋势阈值须在 dbMu 加锁前读取（loadAssistantSettings 内部会取 dbMu，不可重入）。
	thr, _ := loadAssistantSettings(db)

	dbMu.Lock()
	ids := []int64{}
	if contactID > 0 {
		ids = append(ids, contactID)
	} else {
		rows, err := db.Query(`SELECT DISTINCT contact_id FROM relationship_daily_metrics`)
		if err != nil {
			dbMu.Unlock()
			return 0, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
	}
	// 建议生成依赖最新的日指标：批量前先全量重建一次
	if _, err := rebuildDailyMetricsLocked(db, 0); err != nil {
		dbMu.Unlock()
		return 0, err
	}
	produced := 0
	for _, id := range ids {
		t, err := getTrendLocked(db, id, thr)
		if err != nil {
			continue
		}
		ym := time.Now().Format("2006-01")
		tryUpsert := func(kind, reason, windowKey string, prio int) {
			res, err := db.Exec(
				`INSERT INTO relationship_action_suggestions (contact_id, kind, reason, priority, status, window_key, created_at, updated_at)
				 VALUES (?, ?, ?, ?, 'open', ?, ?, ?)
				 ON CONFLICT(contact_id, kind, window_key) DO UPDATE SET
				   reason=excluded.reason, priority=excluded.priority, updated_at=excluded.updated_at`,
				id, kind, reason, prio, windowKey, time.Now().Format(time.RFC3339), time.Now().Format(time.RFC3339))
			if err == nil {
				if n, _ := res.RowsAffected(); n > 0 {
					produced++
				}
			}
		}
		switch t.State {
		case "cooling":
			tryUpsert("cooling", t.Summary, "cooling:"+ym, 7)
		case "dormant":
			tryUpsert("silence", t.Summary, "silence:"+ym, 8)
		}
		// 未回复：最近一条来自对方、且我方在其之后没发言
		if unreplied, days := lastUnrepliedLocked(db, id); unreplied {
			tryUpsert("no_reply",
				fmt.Sprintf("对方 %d 天前的消息你还没回复", days),
				"noreply:"+t.LastActivityDay, 9)
		}
	}
	dbMu.Unlock()

	// LLM 起草放在锁外（网络调用慢，绝不能长时间持锁）。仅对冷却/沉默且 draft 为空者补一段。
	if llm != nil {
		fillDrafts(db, llm, ids)
	}
	return produced, nil
}

// getTrendLocked 是 GetRelationshipTrend 的取数核心（调用方持 dbMu）；此处仅聚合已有指标，不触发重建。
func getTrendLocked(db *sql.DB, contactID int64, thr AssistantSettings) (*RelationshipTrend, error) {
	now := time.Now()
	today := now.Format("2006-01-02")
	t := &RelationshipTrend{ContactID: contactID}
	recentTotal, recentMe := sumWindow(db, contactID, now.AddDate(0, 0, -29).Format("2006-01-02"), today)
	priorTotal, _ := sumWindow(db, contactID, now.AddDate(0, 0, -59).Format("2006-01-02"), now.AddDate(0, 0, -30).Format("2006-01-02"))
	t.Recent30, t.Prior30 = recentTotal, priorTotal
	if recentTotal > 0 {
		t.MeRatioRecent = float64(recentMe) / float64(recentTotal)
	}
	var lastDay sql.NullString
	db.QueryRow(`SELECT MAX(day) FROM relationship_daily_metrics WHERE contact_id=? AND (me_count+other_count)>0`, contactID).Scan(&lastDay)
	t.LastActivityDay = lastDay.String
	if lastDay.Valid && lastDay.String != "" {
		if lt, err := time.ParseInLocation("2006-01-02", lastDay.String, time.Local); err == nil {
			t.DaysSinceLast = int(now.Sub(lt).Hours() / 24)
		}
	}
	t.State, t.Summary = classifyTrendWith(t, thr)
	return t, nil
}

// lastUnrepliedLocked 判断某联系人最近一条消息是否来自对方且我方未在其后回复。
// 并档 messages_archive：否则最近一条往来被归档后，会从 messages 误判“无最近 inbound”而漏掉未回提醒。
func lastUnrepliedLocked(db *sql.DB, contactID int64) (bool, int) {
	hasArchive := tableExistsLocked(db, "messages_archive")
	var sender string
	var msgUnix int64
	recentQ := `SELECT sender, COALESCE(msg_unix,0) FROM messages WHERE contact_id=? AND msg_unix>0 ORDER BY id DESC LIMIT 1`
	recentArgs := []interface{}{contactID}
	if hasArchive {
		recentQ = `SELECT sender, su FROM (
			SELECT sender, COALESCE(msg_unix,0) AS su, id FROM messages WHERE contact_id=? AND msg_unix>0
			UNION ALL
			SELECT sender, COALESCE(msg_unix,0) AS su, id FROM messages_archive WHERE contact_id=? AND msg_unix>0
		) ORDER BY su DESC, id DESC LIMIT 1`
		recentArgs = []interface{}{contactID, contactID}
	}
	err := db.QueryRow(recentQ, recentArgs...).Scan(&sender, &msgUnix)
	if err != nil || sender != "other" {
		return false, 0
	}
	// 该消息之后我方是否发过言（含归档）
	mineQ := `SELECT COUNT(*) FROM messages WHERE contact_id=? AND sender='me' AND msg_unix>?`
	mineArgs := []interface{}{contactID, msgUnix}
	if hasArchive {
		mineQ = `SELECT (SELECT COUNT(*) FROM messages WHERE contact_id=? AND sender='me' AND msg_unix>?)
		         + (SELECT COUNT(*) FROM messages_archive WHERE contact_id=? AND sender='me' AND msg_unix>?)`
		mineArgs = []interface{}{contactID, msgUnix, contactID, msgUnix}
	}
	var mine int
	db.QueryRow(mineQ, mineArgs...).Scan(&mine)
	if mine > 0 {
		return false, 0
	}
	days := int(time.Since(time.Unix(msgUnix, 0)).Hours() / 24)
	if days < 1 {
		return false, 0 // 当天未回很正常，不催
	}
	return true, days
}

// fillDrafts 为冷却/沉默类且尚无 draft 的建议，用 LLM 生成一句自然的开场白。逐个联系人生成，失败静默跳过。
func fillDrafts(db *sql.DB, llm *LLMClient, ids []int64) {
	ctx := context.Background()
	if len(ids) == 0 {
		return
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]interface{}, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.Query(
		`SELECT s.id, s.contact_id, s.kind, COALESCE(c.name,''), COALESCE(c.profile_summary,'')
		 FROM relationship_action_suggestions s
		 LEFT JOIN contacts c ON c.id = s.contact_id
		 WHERE s.draft='' AND s.status='open' AND s.kind IN ('cooling','silence') AND s.contact_id IN (`+placeholders+`)`,
		args...)
	if err != nil {
		return
	}
	type row struct {
		sid, cid            int64
		kind, name, summary string
	}
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.sid, &r.cid, &r.kind, &r.name, &r.summary); err != nil {
			continue
		}
		pending = append(pending, r)
	}
	rows.Close()
	for _, r := range pending {
		prompt := fmt.Sprintf(
			`你在帮用户维护微信关系。对方昵称：%s。对方画像概要：%s。当前状态：%s。`+
				`请生成 1 句自然的、不油腻、不像群发的中文开场白，用于重新开启聊天。`+
				`只输出 JSON：{"draft":"开场白内容"}`, r.name, r.summary, r.kind)
		raw, err := llm.CallContext(ctx, prompt)
		if err != nil {
			continue
		}
		if draft := parseDraft(ExtractJSON(raw)); draft != "" {
			db.Exec(`UPDATE relationship_action_suggestions SET draft=?, updated_at=? WHERE id=?`,
				draft, time.Now().Format(time.RFC3339), r.sid)
		}
	}
}

// parseDraft 从模型返回的 {"draft":"..."} 里取开场白，解析不了返回空。
func parseDraft(s string) string {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return ""
	}
	if v, ok := m["draft"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// ListSuggestions 返回行动建议（默认仅 open），按优先级降序。带联系人名。
func ListSuggestions(db *sql.DB, includeHandled bool) ([]SuggestionView, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	where := `1=1`
	if !includeHandled {
		where = `s.status='open'`
	}
	rows, err := db.Query(
		`SELECT s.id, s.contact_id, s.kind, s.reason, s.draft, s.priority, s.status, s.window_key,
		        COALESCE(c.remark, c.name, '')
		 FROM relationship_action_suggestions s
		 LEFT JOIN contacts c ON c.id = s.contact_id
		 WHERE ` + where + ` ORDER BY s.priority DESC, s.updated_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SuggestionView{}
	for rows.Next() {
		var v SuggestionView
		if err := rows.Scan(&v.ID, &v.ContactID, &v.Kind, &v.Reason, &v.Draft, &v.Priority, &v.Status, &v.WindowKey, &v.ContactName); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SetSuggestionStatus 用户处理建议：done / dismissed / open。
func SetSuggestionStatus(db *sql.DB, id int64, status string) error {
	if status != "open" && status != "done" && status != "dismissed" {
		return fmt.Errorf("非法状态: %s", status)
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	res, err := db.Exec(`UPDATE relationship_action_suggestions SET status=?, updated_at=? WHERE id=?`,
		status, time.Now().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("建议不存在: %d", id)
	}
	return nil
}
