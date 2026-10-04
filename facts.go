package main

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"strings"
	"time"
)

// 可信画像 / 证据链（Phase 4 + 5）。
//
// 核心思路：画像 profile_json 里已经有很多**离散、可核对**的断言（职业、城市、兴趣、
// 重要日子、性格、重要事实、口头禅、亲密度）。把它们拆成 profile_facts 结构化事实，
// 再用事实值去该联系人的真实聊天里检索支撑消息，落成 profile_fact_evidence。
// 于是 UI 能回答「系统凭什么认为 TA 在深圳」——每条结论都能展开看原话。
//
// 不引入任何新 LLM 调用：事实全部从既有画像派生，可解释、可复现。
// 置信度由证据数量支撑：命中证据越多，可信度越高（封顶 0.95），无证据保持基线 0.6。

// evidencePerFact 每条事实最多保留的支撑证据条数（取最近的，避免一次画像拉出上千行证据）
const evidencePerFact = 5

// profileFact 一条待写入的结构化事实
type profileFact struct {
	Type  string
	Key   string
	Value string
}

// deriveFacts 从画像 JSON 派生出事实集合。解析失败（{}、旧格式）返回空集，不报错。
func deriveFacts(profileJSON string) []profileFact {
	var p Profile
	if strings.TrimSpace(profileJSON) == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(profileJSON), &p); err != nil {
		return nil
	}
	var out []profileFact
	add := func(typ, key, val string) {
		val = strings.TrimSpace(val)
		if val == "" {
			return
		}
		out = append(out, profileFact{Type: typ, Key: key, Value: val})
	}
	add("occupation", "", p.BasicInfo.Occupation)
	add("location", "", p.BasicInfo.Location)
	for _, d := range p.BasicInfo.ImportantDates {
		// 形如 "生日: 5月20日"；拆 key/value，拆不出就把整串当 value
		if i := strings.IndexByte(d, ':'); i >= 0 {
			add("important_date", strings.TrimSpace(d[:i]), strings.TrimSpace(d[i+1:]))
		} else {
			add("important_date", "", d)
		}
	}
	for _, s := range p.Personality {
		add("personality", "", s)
	}
	for _, s := range p.Interests {
		add("interest", "", s)
	}
	for _, s := range p.ImportantFacts {
		add("important_fact", "", s)
	}
	for _, s := range p.CommunicationStyle.FrequentPhrases {
		add("phrase", "", s)
	}
	add("closeness", "", p.Relationship.Closeness)
	return out
}

// ExtractProfileFacts 重建某联系人的事实集合：
// 先把当前 active 事实全部标 retired，再按最新画像 UPSERT 回 active（本次画像不再包含的自然留在 retired）。
// 幂等、可反复调用；retired 事实连同其证据一并保留，作为可信度溯源历史。返回 (active, retired, err)。
func ExtractProfileFacts(db *sql.DB, contactID int64) (int, int, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	return extractProfileFactsLocked(db, contactID)
}

// extractProfileFactsLocked 是 ExtractProfileFacts 的实现体，要求调用方已持有 dbMu。
func extractProfileFactsLocked(db *sql.DB, contactID int64) (int, int, error) {
	var profileJSON sql.NullString
	if err := db.QueryRow(`SELECT profile_json FROM contacts WHERE id=?`, contactID).Scan(&profileJSON); err != nil {
		return 0, 0, err
	}
	facts := deriveFacts(profileJSON.String)
	now := time.Now().Format(time.RFC3339)

	// 1) 现有一律先标 retired
	if _, err := db.Exec(`UPDATE profile_facts SET status='retired', updated_at=? WHERE contact_id=? AND status='active'`, now, contactID); err != nil {
		return 0, 0, err
	}
	// 2) 逐条 UPSERT 回 active；已存在则只刷新状态与时间，保留其 confidence（可能已被证据抬高）
	for _, f := range facts {
		if _, err := db.Exec(
			`INSERT INTO profile_facts (contact_id, fact_type, fact_key, fact_value, source, confidence, status, first_seen, last_seen, created_at, updated_at)
			 VALUES (?, ?, ?, ?, 'profile', 0.6, 'active', ?, ?, ?, ?)
			 ON CONFLICT(contact_id, fact_type, fact_key, fact_value) DO UPDATE SET
			   status='active', last_seen=excluded.last_seen, updated_at=excluded.updated_at`,
			contactID, f.Type, f.Key, f.Value, now, now, now, now); err != nil {
			return 0, 0, err
		}
	}
	var active, retired int
	if err := db.QueryRow(`SELECT COUNT(*) FROM profile_facts WHERE contact_id=? AND status='active'`, contactID).Scan(&active); err != nil {
		return 0, 0, err
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM profile_facts WHERE contact_id=? AND status='retired'`, contactID).Scan(&retired); err != nil {
		return 0, 0, err
	}
	return active, retired, nil
}

// AttachFactEvidence 为某联系人当前 active 的事实重新计算证据与置信度。
// 采用「先清空该联系人证据、再全量重算」策略，天然幂等；证据来自真实聊天（活跃 + 归档），
// 用参数化 LIKE（转义通配符）在联系人范围内检索包含该事实值的消息，最多 evidencePerFact 条。
// 置信度 = min(0.95, 0.6 + 0.08 * 命中数)。返回写入的证据总条数。
func AttachFactEvidence(db *sql.DB, contactID int64) (int, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	return attachFactEvidenceLocked(db, contactID)
}

func attachFactEvidenceLocked(db *sql.DB, contactID int64) (int, error) {
	// 先取 active 事实清单（id + value）
	rows, err := db.Query(`SELECT id, fact_value FROM profile_facts WHERE contact_id=? AND status='active'`, contactID)
	if err != nil {
		return 0, err
	}
	type factRow struct {
		id    int64
		value string
	}
	var facts []factRow
	for rows.Next() {
		var f factRow
		if err := rows.Scan(&f.id, &f.value); err != nil {
			rows.Close()
			return 0, err
		}
		facts = append(facts, f)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	if _, err := db.Exec(`DELETE FROM profile_fact_evidence WHERE contact_id=?`, contactID); err != nil {
		return 0, err
	}

	archived := ftsArchiveEnabled.Load() || tableExistsLocked(db, "messages_archive")
	total := 0
	now := time.Now().Format(time.RFC3339)
	for _, f := range facts {
		like := "%" + escapeLike(f.value) + "%"
		// 联系人范围内检索包含事实值的消息，按时间倒序取最近若干条
		findFor := func(table string, archFlag int) []SearchHit {
			q := `SELECT id, COALESCE(msg_time,''), content FROM ` + table +
				` WHERE contact_id=? AND content LIKE ? ESCAPE '\' ORDER BY id DESC LIMIT ?`
			rs, err := db.Query(q, contactID, like, evidencePerFact)
			if err != nil {
				return nil
			}
			defer rs.Close()
			var hits []SearchHit
			for rs.Next() {
				var h SearchHit
				if err := rs.Scan(&h.ID, &h.MsgTime, &h.Content); err != nil {
					continue
				}
				h.Archived = archFlag == 1
				h.Snippet = buildSnippet(h.Content, []string{f.value})
				hits = append(hits, h)
			}
			return hits
		}
		hits := findFor("messages", 0)
		if archived {
			hits = append(hits, findFor("messages_archive", 1)...)
		}
		n := 0
		for _, h := range hits {
			if n >= evidencePerFact {
				break
			}
			if _, err := db.Exec(
				`INSERT OR IGNORE INTO profile_fact_evidence (fact_id, contact_id, message_id, archived, snippet, msg_time, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				f.id, contactID, h.ID, boolToInt(h.Archived), h.Snippet, h.MsgTime, now); err != nil {
				return total, err
			}
			n++
			total++
		}
		conf := 0.6 + 0.08*float64(n)
		if conf > 0.95 {
			conf = 0.95
		}
		if _, err := db.Exec(`UPDATE profile_facts SET confidence=?, updated_at=? WHERE id=?`, conf, now, f.id); err != nil {
			return total, err
		}
	}
	return total, nil
}

// RebuildFactsAndEvidence 一次性重建某联系人的事实 + 证据（自愈入口：视图发现事实缺失时调用）。
func RebuildFactsAndEvidence(db *sql.DB, contactID int64) (active, evidence int, err error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	if _, _, err = extractProfileFactsLocked(db, contactID); err != nil {
		return 0, 0, err
	}
	ev, err := attachFactEvidenceLocked(db, contactID)
	if err != nil {
		return 0, 0, err
	}
	var a int
	if err := db.QueryRow(`SELECT COUNT(*) FROM profile_facts WHERE contact_id=? AND status='active'`, contactID).Scan(&a); err != nil {
		return 0, 0, err
	}
	return a, ev, nil
}

// refreshFactsQuietly 在画像保存成功后刷新事实/证据（best-effort，失败只记日志不影响主流程）。
// 调用方必须未持有 dbMu（RebuildFactsAndEvidence 内部会加锁）。
func refreshFactsQuietly(db *sql.DB, contactID int64) {
	if _, _, err := RebuildFactsAndEvidence(db, contactID); err != nil {
		slog.Warn("刷新可信画像事实/证据失败（不影响画像保存）", "contactId", contactID, "err", err)
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// tableExistsLocked 检查表是否存在，要求调用方已持 dbMu。
func tableExistsLocked(db *sql.DB, name string) bool {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type IN ('table','view') AND name=?`, name).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// FactView 事实 + 其证据（供 API/前端展示）
type FactView struct {
	ID         int64              `json:"id"`
	Type       string             `json:"type"`
	Key        string             `json:"key"`
	Value      string             `json:"value"`
	Status     string             `json:"status"`
	Confidence float64            `json:"confidence"`
	FirstSeen  string             `json:"firstSeen"`
	LastSeen   string             `json:"lastSeen"`
	Evidence   []FactEvidenceView `json:"evidence"`
}

// FactEvidenceView 一条支撑证据
type FactEvidenceView struct {
	MessageID int64  `json:"messageId"`
	Archived  bool   `json:"archived"`
	Snippet   string `json:"snippet"`
	MsgTime   string `json:"msgTime"`
}

// GetFacts 读取某联系人的事实（含证据）。includeRetired=false 时只返回 active。
func GetFacts(db *sql.DB, contactID int64, includeRetired bool) ([]FactView, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	where := `contact_id=?`
	if !includeRetired {
		where += ` AND status='active'`
	}
	rows, err := db.Query(
		`SELECT id, fact_type, fact_key, fact_value, status, confidence, first_seen, last_seen
		 FROM profile_facts WHERE `+where+`
		 ORDER BY status ASC, confidence DESC, fact_type ASC, id ASC`, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FactView{}
	ids := []int64{}
	idx := map[int64]int{}
	for rows.Next() {
		var v FactView
		if err := rows.Scan(&v.ID, &v.Type, &v.Key, &v.Value, &v.Status, &v.Confidence, &v.FirstSeen, &v.LastSeen); err != nil {
			return nil, err
		}
		v.Evidence = []FactEvidenceView{}
		idx[v.ID] = len(out)
		ids = append(ids, v.ID)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 批量取证据，避免 N+1
	if len(ids) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		args := make([]interface{}, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		er, err := db.Query(
			`SELECT fact_id, message_id, archived, snippet, msg_time
			 FROM profile_fact_evidence WHERE fact_id IN (`+placeholders+`)
			 ORDER BY id DESC`, args...)
		if err != nil {
			return nil, err
		}
		defer er.Close()
		for er.Next() {
			var factID int64
			var ev FactEvidenceView
			var arch int
			if err := er.Scan(&factID, &ev.MessageID, &arch, &ev.Snippet, &ev.MsgTime); err != nil {
				return nil, err
			}
			ev.Archived = arch == 1
			if pos, ok := idx[factID]; ok {
				out[pos].Evidence = append(out[pos].Evidence, ev)
			}
		}
		if err := er.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
