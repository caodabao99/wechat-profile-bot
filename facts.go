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

	// 1) 现有一律先标 retired（用户确认的事实 source_type='user' 优先级最高，绝不自动降级）
	if _, err := db.Exec(`UPDATE profile_facts SET status='retired', updated_at=? WHERE contact_id=? AND status='active' AND source_type!='user'`, now, contactID); err != nil {
		return 0, 0, err
	}
	// 2) 逐条 UPSERT 回 active；已存在则只刷新状态与时间，保留其 confidence（可能已被证据抬高）。
	// 用户确认事实（source_type='user'）：ON CONFLICT 不改其 status/失效标记，保持权威。
	// 重新出现的旧值：复位 valid_until/superseded_by（事实重新生效）。
	for _, f := range facts {
		if _, err := db.Exec(
			`INSERT INTO profile_facts (contact_id, fact_type, fact_key, fact_value, source, confidence, status, first_seen, last_seen, created_at, updated_at, source_type, confidence_type, valid_from)
			 VALUES (?, ?, ?, ?, 'profile', 0.6, 'active', ?, ?, ?, ?, 'ai', 'inferred', ?)
			 ON CONFLICT(contact_id, fact_type, fact_key, fact_value) DO UPDATE SET
			   status=CASE WHEN source_type='user' THEN status ELSE 'active' END,
			   last_seen=excluded.last_seen,
			   updated_at=excluded.updated_at,
			   valid_until=CASE WHEN source_type='user' THEN valid_until ELSE '' END,
			   superseded_by=CASE WHEN source_type='user' THEN superseded_by ELSE NULL END`,
			contactID, f.Type, f.Key, f.Value, now, now, now, now, now); err != nil {
			return 0, 0, err
		}
	}
	// 2b) 时效取代（OS 2.0 5.4）：仅对单值型事实（职业/城市/亲密度）生效——本轮刚被降级、
	// 且同 (类型,键) 已出现不同值的 active 事实的旧记录，语义上是被「取代」而非「消失」，
	// 标 superseded、写 valid_until 失效时间、回填 superseded_by 指向新事实。多值集合型（兴趣/性格/
	// 口头禅…）的移除属「消失」，保持 retired，不在此列。
	if _, err := db.Exec(`UPDATE profile_facts
		SET status='superseded', valid_until=?, superseded_by=(
			SELECT q.id FROM profile_facts q
			WHERE q.contact_id=profile_facts.contact_id AND q.fact_type=profile_facts.fact_type
			  AND q.fact_key=profile_facts.fact_key AND q.status='active'
			ORDER BY q.confidence DESC, q.id DESC LIMIT 1)
		WHERE contact_id=? AND status='retired' AND updated_at=? AND source_type!='user'
		  AND fact_type IN ('occupation','location','closeness')
		  AND EXISTS(SELECT 1 FROM profile_facts q
			WHERE q.contact_id=profile_facts.contact_id AND q.fact_type=profile_facts.fact_type
			  AND q.fact_key=profile_facts.fact_key AND q.status='active' AND q.fact_value!=profile_facts.fact_value)`,
		now, contactID, now); err != nil {
		return 0, 0, err
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
	// 先取 active 事实清单（id + type + value）
	rows, err := db.Query(`SELECT id, fact_type, fact_value FROM profile_facts WHERE contact_id=? AND status='active'`, contactID)
	if err != nil {
		return 0, err
	}
	type factRow struct {
		id       int64
		factType string
		value    string
	}
	var facts []factRow
	for rows.Next() {
		var f factRow
		if err := rows.Scan(&f.id, &f.factType, &f.value); err != nil {
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
		direct := 0
		conf := 0.6 // Evidence Provenance 2.0（§7.4）：置信度按证据类型加权，纯关键词不再一票抬高
		for _, h := range hits {
			if n >= evidencePerFact {
				break
			}
			// §7.1：确定性分类每条命中消息的证据类型（direct/strong_context/weak_context/topic_related/conflict）
			etype := classifyEvidenceType(f.factType, f.value, h.Content)
			// 字面命中仍沿用旧 match_type(exact/contextual)，与 evidence_type 正交，不破坏既有读者
			literal := strings.Contains(h.Content, f.value) || strings.Contains(h.Snippet, f.value)
			mt, ss := "contextual", 0.4
			if literal {
				mt, ss = "exact", 0.8
			}
			isDirectSupport := etype == EvDirect
			if isDirectSupport {
				direct++
			}
			conf += evidenceConfContrib(etype) // topic_related/weak_context/conflict → +0（§7.4）
			if _, err := db.Exec(
				`INSERT OR IGNORE INTO profile_fact_evidence (fact_id, contact_id, message_id, archived, snippet, msg_time, created_at, match_type, support_strength, quote, is_direct_support, evidence_type)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				f.id, contactID, h.ID, boolToInt(h.Archived), h.Snippet, h.MsgTime, now, mt, ss, h.Snippet, boolToInt(isDirectSupport), etype); err != nil {
				return total, err
			}
			n++
			total++
		}
		if conf > 0.95 {
			conf = 0.95
		}
		// 证据强度（0~1）= 直接支撑占比；置信类型随直接证据数升级（描述性元数据，不改数值置信公式）
		strength := 0.0
		if n > 0 {
			strength = float64(direct) / float64(n)
		}
		confType := "inferred"
		switch {
		case direct >= 2:
			confType = "multi_evidence"
		case direct == 1:
			confType = "direct"
		}
		if _, err := db.Exec(`UPDATE profile_facts SET confidence=?, evidence_strength=?, confidence_type=?, updated_at=? WHERE id=? AND source_type!='user'`, conf, strength, confType, now, f.id); err != nil {
			return total, err
		}
		// §7.5 Evidence Provenance 2.0：对身份型事实做一次「不含事实值、但表明旧值已不成立」的
		// 冲突召回（如事实职业=律师 vs 消息「我辞职创业了」），落成 evidence_type=conflict 证据行。
		// 只新增证据、不改写事实 status/生命周期（取代仍由画像换值路径处理）；幂等：证据每轮全清重建。
		if len(conflictMarkersForType(f.factType)) > 0 {
			added, err := attachConflictEvidenceForFact(db, contactID, f.id, f.factType, f.value, now)
			if err != nil {
				return total, err
			}
			total += added
		}
	}
	return total, nil
}

// attachConflictEvidenceForFact 为一条身份型事实召回「不含事实值、却表明旧值已不成立」的第
// 一人称反转消息，落成 evidence_type=conflict 的证据行（§7.5）。去重靠 UNIQUE(fact_id,message_id,
// archived)+INSERT OR IGNORE；只新增证据、不改写事实 status/生命周期。返回实际新增行数。
func attachConflictEvidenceForFact(db *sql.DB, contactID, factID int64, factType, factValue, now string) (int, error) {
	markers := conflictMarkersForType(factType)
	if len(markers) == 0 {
		return 0, nil
	}
	conds := make([]string, 0, len(markers))
	likeArgs := make([]interface{}, 0, len(markers))
	for _, m := range markers {
		conds = append(conds, `content LIKE ? ESCAPE '\'`)
		likeArgs = append(likeArgs, "%"+escapeLike(m)+"%")
	}
	where := `contact_id=? AND (` + strings.Join(conds, " OR ") + `)`
	added := 0
	type cand struct {
		mid     int64
		msgTime string
		content string
	}
	query := func(table string, archFlag int) error {
		q := `SELECT id, COALESCE(msg_time,''), content FROM ` + table + ` WHERE ` + where + ` ORDER BY id DESC LIMIT ?`
		full := append([]interface{}{contactID}, likeArgs...)
		full = append(full, evidencePerFact)
		rs, err := db.Query(q, full...)
		if err != nil {
			return err
		}
		// 先把候选一次性读尽并关闭游标，再逐条写入——单连接池(MaxOpenConns(1))下，
		// 边遍历 open rows 边 Exec 会等第二个连接而死锁。
		var cands []cand
		for rs.Next() {
			var c cand
			if err := rs.Scan(&c.mid, &c.msgTime, &c.content); err != nil {
				continue
			}
			cands = append(cands, c)
		}
		err = rs.Err()
		rs.Close()
		if err != nil {
			return err
		}
		for _, c := range cands {
			// 含事实值者交由主分类处理（含否定会被判 conflict）；此处只收「不含值」的反转
			if factValue != "" && strings.Contains(c.content, factValue) {
				continue
			}
			// 需第一人称，避免「他辞职了」这类他人事件误判为本人冲突
			if !containsAny(c.content, evSelf...) {
				continue
			}
			snippet := buildSnippet(c.content, markers)
			res, err := db.Exec(
				`INSERT OR IGNORE INTO profile_fact_evidence (fact_id, contact_id, message_id, archived, snippet, msg_time, created_at, match_type, support_strength, quote, is_direct_support, evidence_type)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				factID, contactID, c.mid, archFlag, snippet, c.msgTime, now, "contextual", 0.0, snippet, 0, EvConflict)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				added++
			}
		}
		return nil
	}
	if err := query("messages", 0); err != nil {
		return added, err
	}
	// 归档表只在确实存在时查（ftsArchiveEnabled 为进程级全局标志，可能被其他测试置 true
	// 而本库并无 messages_archive，故以 pragma 存在性为权威判据）。
	if tableExistsLocked(db, "messages_archive") {
		if err := query("messages_archive", 1); err != nil {
			return added, err
		}
	}
	return added, nil
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

// ConfirmFact 将一条既有事实提升为「用户确认」的最高可信来源（OS 2.0 5.5）。
// 之后派生流程不会自动降级或改写它的 status/confidence，只有再次显式操作才会变动。
// 未命中任何行返回 sql.ErrNoRows。
func ConfirmFact(db *sql.DB, factID int64) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	now := time.Now().Format(time.RFC3339)
	res, err := db.Exec(`UPDATE profile_facts
		SET source_type='user', status='confirmed', confidence=1.0, confidence_type='user_confirmed',
		    last_confirmed_at=?, valid_from=CASE WHEN valid_from='' THEN ? ELSE valid_from END, updated_at=?
		WHERE id=?`, now, now, now, factID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
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
	ID               int64              `json:"id"`
	Type             string             `json:"type"`
	Key              string             `json:"key"`
	Value            string             `json:"value"`
	Status           string             `json:"status"`
	Confidence       float64            `json:"confidence"`
	FirstSeen        string             `json:"firstSeen"`
	LastSeen         string             `json:"lastSeen"`
	SourceType       string             `json:"sourceType"`
	ConfidenceType   string             `json:"confidenceType"`
	EvidenceStrength float64            `json:"evidenceStrength"`
	ValidFrom        string             `json:"validFrom"`
	ValidUntil       string             `json:"validUntil"`
	LastConfirmedAt  string             `json:"lastConfirmedAt"`
	SupersededBy     *int64             `json:"supersededBy,omitempty"`
	HasConflict      bool               `json:"hasConflict"` // §7.5：是否挂了矛盾证据（视图级派生，不改 status）
	Evidence         []FactEvidenceView `json:"evidence"`
}

// FactEvidenceView 一条支撑证据
type FactEvidenceView struct {
	MessageID       int64   `json:"messageId"`
	Archived        bool    `json:"archived"`
	Snippet         string  `json:"snippet"`
	MsgTime         string  `json:"msgTime"`
	MatchType       string  `json:"matchType"`
	SupportStrength float64 `json:"supportStrength"`
	Quote           string  `json:"quote"`
	IsDirectSupport bool    `json:"isDirectSupport"`
	EvidenceType    string  `json:"evidenceType"` // §7.1 六档证据类型（direct/strong_context/…）
}

// GetFacts 读取某联系人的事实（含证据）。includeRetired=false 时只返回 active。
func GetFacts(db *sql.DB, contactID int64, includeRetired bool) ([]FactView, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	where := `contact_id=?`
	if !includeRetired {
		// 「当前态」= 非历史（排除 retired/superseded/rejected）；包含 active/confirmed/verified/inferred/stale/conflict。
		// 既有仅有 active/retired 的测试中，与旧 `status='active'` 完全等价。
		where += ` AND status NOT IN ('retired','superseded','rejected')`
	}
	rows, err := db.Query(
		`SELECT id, fact_type, fact_key, fact_value, status, confidence, first_seen, last_seen,
		        source_type, confidence_type, evidence_strength, valid_from, valid_until, last_confirmed_at, superseded_by
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
		var sup sql.NullInt64
		if err := rows.Scan(&v.ID, &v.Type, &v.Key, &v.Value, &v.Status, &v.Confidence, &v.FirstSeen, &v.LastSeen,
			&v.SourceType, &v.ConfidenceType, &v.EvidenceStrength, &v.ValidFrom, &v.ValidUntil, &v.LastConfirmedAt, &sup); err != nil {
			return nil, err
		}
		if sup.Valid {
			s := sup.Int64
			v.SupersededBy = &s
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
			`SELECT fact_id, message_id, archived, snippet, msg_time, match_type, support_strength, quote, is_direct_support, evidence_type
			 FROM profile_fact_evidence WHERE fact_id IN (`+placeholders+`)
			 ORDER BY id DESC`, args...)
		if err != nil {
			return nil, err
		}
		defer er.Close()
		for er.Next() {
			var factID int64
			var ev FactEvidenceView
			var arch, isDirect int
			if err := er.Scan(&factID, &ev.MessageID, &arch, &ev.Snippet, &ev.MsgTime, &ev.MatchType, &ev.SupportStrength, &ev.Quote, &isDirect, &ev.EvidenceType); err != nil {
				return nil, err
			}
			ev.Archived = arch == 1
			ev.IsDirectSupport = isDirect == 1
			if ev.EvidenceType == EvConflict {
				if pos, ok := idx[factID]; ok {
					out[pos].HasConflict = true
				}
			}
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
