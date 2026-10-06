package main

// 蓝图 §15 P3：Network Opportunity Discovery（“谁可能帮我？”）。
//
// 铁律——不新建第二套数据真相，只做确定性召回的组合层：
//   - 15.1 Deterministic Recall：从已有的 facts / projects / topics 三处做词法召回（纯 SQL LIKE），
//     绝不依赖 LLM 决定谁被召回、绝不新建排序真相。
//   - 15.2 分层标注：每条证据必须显式标 FACT（事实/项目=已记录数据）或 INFERENCE（话题=LLM 派生
//     历史）。核心链路是纯确定性组装，LLM 即使不可用也完全工作（本层根本不调模型）。
//   - 15.3 输出：联系人 / 匹配原因 / 证据 / 关系强度 / 共同联系人 / 推荐切入点。
//
// 关系强度读 relationship_state（亲密度 + 基态/动态），共同联系人读 connections（既有图谱），
// 全部只读，绝不写库、绝不新增表、绝不 bump user_version。

import (
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
)

// OpportunityEvidence 一条召回证据（来源 + FACT/INFERENCE 分层 + 命中词）。
type OpportunityEvidence struct {
	Source  string `json:"source"` // fact | project | topic
	Layer   string `json:"layer"`  // FACT | INFERENCE
	Text    string `json:"text"`
	Matched string `json:"matched"`
}

// OpportunityMatch 一个「谁可能帮我」候选。
type OpportunityMatch struct {
	ContactID            int64                 `json:"contact_id"`
	Name                 string                `json:"name"`
	Score                int                   `json:"score"`
	Intimacy             int                   `json:"intimacy"`
	RelationshipStrength string                `json:"relationship_strength"`
	MatchReason          string                `json:"match_reason"`
	Evidence             []OpportunityEvidence `json:"evidence"`
	CommonContacts       []string              `json:"common_contacts"`
	EntrySuggestion      string                `json:"entry_suggestion"`
}

// OpportunityResult “谁可能帮我？”一次确定性机会发现。
type OpportunityResult struct {
	Query     string             `json:"query"`
	Keywords  []string           `json:"keywords"`
	Layer     string             `json:"layer"` // 恒为 DETERMINISTIC（不依赖 LLM）
	Matches   []OpportunityMatch `json:"matches"`
	Note      string             `json:"note"`
	Generated string             `json:"generated_at"`
}

// oppAcc 召回过程中的每联系人累加器（内部）。
type oppAcc struct {
	name  string
	ev    []OpportunityEvidence
	score int
}

type oppState struct {
	intimacy int
	base     string
	dynamic  string
}

// extractOpportunityKeywords 确定性词法切分：ASCII 词元 + CJK 连续串的 bigram，保序去重、上限 16。
// 纯规则、可复现、可单测——不用模型、不用分词库。
func extractOpportunityKeywords(query string) []string {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	var ascii []rune
	var cjk []rune
	flushASCII := func() {
		if len(ascii) > 0 {
			add(strings.ToLower(string(ascii)))
			ascii = ascii[:0]
		}
	}
	flushCJK := func() {
		if len(cjk) == 1 {
			add(string(cjk))
		}
		for i := 0; i+1 < len(cjk); i++ {
			add(string(cjk[i : i+2]))
		}
		cjk = cjk[:0]
	}
	for _, r := range q {
		switch {
		case unicode.Is(unicode.Han, r) || (r >= 0x3040 && r <= 0x30ff):
			flushASCII()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			ascii = append(ascii, r)
		default:
			flushASCII()
			flushCJK()
		}
	}
	flushASCII()
	flushCJK()
	if len(out) > 16 {
		out = out[:16]
	}
	return out
}

// likeArg 把关键词包成 LIKE 模式（转义交由包内已有 escapeLike）。
func likeArg(kw string) string { return "%" + escapeLike(kw) + "%" }

// buildORLike 为给定列生成 (col LIKE ? ESCAPE '\' OR ...) 片段与参数。
func buildORLike(cols, kws []string) (string, []interface{}) {
	var conds []string
	var args []interface{}
	for _, kw := range kws {
		for _, col := range cols {
			conds = append(conds, col+` LIKE ? ESCAPE '\'`)
			args = append(args, likeArg(kw))
		}
	}
	return strings.Join(conds, " OR "), args
}

func oppTouch(by map[int64]*oppAcc, id int64, name string) *oppAcc {
	a := by[id]
	if a == nil {
		a = &oppAcc{name: name, ev: []OpportunityEvidence{}}
		by[id] = a
	} else if a.name == "" && name != "" {
		a.name = name
	}
	return a
}

// matchedKeyword 返回文本里第一个命中的关键词（小写比较），无则 ""。
func matchedKeyword(text string, kws []string) string {
	lower := strings.ToLower(text)
	for _, k := range kws {
		if strings.Contains(lower, strings.ToLower(k)) {
			return k
		}
	}
	return ""
}

// DiscoverOpportunities 确定性召回「谁可能帮我」。全程只读，无 LLM。
func DiscoverOpportunities(db *sql.DB, query string, now time.Time, topN int) (*OpportunityResult, error) {
	if topN <= 0 || topN > 20 {
		topN = 5
	}
	res := &OpportunityResult{
		Query:     strings.TrimSpace(query),
		Layer:     "DETERMINISTIC",
		Matches:   []OpportunityMatch{},
		Generated: now.Format(time.RFC3339),
	}
	kws := extractOpportunityKeywords(query)
	res.Keywords = kws
	if len(kws) == 0 {
		res.Note = "请输入要查找的能力/话题关键词。"
		return res, nil
	}
	by := map[int64]*oppAcc{}
	if err := recallFacts(db, kws, by); err != nil {
		return nil, err
	}
	if err := recallProjects(db, kws, by); err != nil {
		return nil, err
	}
	if err := recallTopics(db, kws, by); err != nil {
		return nil, err
	}
	strength := loadRelationshipStrength(db)

	type ranked struct {
		id int64
		a  *oppAcc
	}
	list := make([]ranked, 0, len(by))
	for id, a := range by {
		list = append(list, ranked{id, a})
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].a.score != list[j].a.score {
			return list[i].a.score > list[j].a.score
		}
		li, lj := strength[list[i].id], strength[list[j].id]
		if li.intimacy != lj.intimacy {
			return li.intimacy > lj.intimacy
		}
		return list[i].id < list[j].id
	})

	for i := 0; i < len(list) && i < topN; i++ {
		id, a := list[i].id, list[i].a
		st := strength[id]
		trimEvidence(a, 6)
		res.Matches = append(res.Matches, OpportunityMatch{
			ContactID:            id,
			Name:                 a.name,
			Score:                a.score,
			Intimacy:             st.intimacy,
			RelationshipStrength: relationshipStrengthLabel(st),
			MatchReason:          opportunityMatchReason(a.ev),
			Evidence:             a.ev,
			CommonContacts:       commonContactNames(db, id, 5),
			EntrySuggestion:      opportunityEntrySuggestion(a.ev),
		})
	}
	if len(res.Matches) == 0 {
		res.Note = "网络里暂无与该关键词匹配的人（可换词，或先补充相关联系人的事实/话题）。"
	} else {
		res.Note = fmt.Sprintf("基于关键词 %v 的确定性召回；证据按 FACT/INFERENCE 分层。相关≠因果，请自行判断。", kws)
	}
	return res, nil
}

// recallFacts 从 profile_facts 召回（FACT，权重 3）。
func recallFacts(db *sql.DB, kws []string, by map[int64]*oppAcc) error {
	orClause, args := buildORLike([]string{"pf.fact_value", "pf.fact_key", "pf.fact_type"}, kws)
	q := `SELECT pf.contact_id, COALESCE(c.name,''), pf.fact_type, pf.fact_key, pf.fact_value, pf.source_type
		FROM profile_facts pf LEFT JOIN contacts c ON c.id = pf.contact_id
		WHERE pf.status NOT IN ('retired','superseded','rejected') AND (` + orClause + `)
		ORDER BY pf.confidence DESC LIMIT 500`
	return eachRow(db, q, args, func(row *sql.Rows) {
		var cid int64
		var name, ftype, fkey, fval, src string
		if row.Scan(&cid, &name, &ftype, &fkey, &fval, &src) != nil {
			return
		}
		text := fval
		if fkey != "" {
			text = fkey + "：" + fval
		}
		a := oppTouch(by, cid, name)
		a.score += 3
		a.ev = append(a.ev, OpportunityEvidence{Source: "fact", Layer: "FACT", Text: clipRunes(text, 80), Matched: matchedKeyword(text, kws)})
	})
}

// recallProjects 从 relationship_projects 召回（FACT，权重 2）。
func recallProjects(db *sql.DB, kws []string, by map[int64]*oppAcc) error {
	if err := ensureProjects(db); err != nil {
		return nil // 表未就绪：诚实跳过该来源，不阻断
	}
	orClause, args := buildORLike([]string{"p.title", "p.description", "p.next_action"}, kws)
	q := `SELECT p.contact_id, COALESCE(c.name,''), p.title, p.description
		FROM relationship_projects p LEFT JOIN contacts c ON c.id = p.contact_id
		WHERE p.status IN ('active','paused') AND (` + orClause + `)
		ORDER BY p.priority DESC LIMIT 300`
	return eachRow(db, q, args, func(row *sql.Rows) {
		var cid int64
		var name, title, desc string
		if row.Scan(&cid, &name, &title, &desc) != nil {
			return
		}
		a := oppTouch(by, cid, name)
		a.score += 2
		a.ev = append(a.ev, OpportunityEvidence{Source: "project", Layer: "FACT", Text: clipRunes(title, 80), Matched: matchedKeyword(title+" "+desc, kws)})
	})
}

// recallTopics 从 contact_topic_history 召回（INFERENCE，权重 1；话题是 LLM 派生的历史）。
func recallTopics(db *sql.DB, kws []string, by map[int64]*oppAcc) error {
	q := `SELECT h.contact_id, COALESCE(c.name,''), h.topics_json
		FROM contact_topic_history h LEFT JOIN contacts c ON c.id = h.contact_id
		ORDER BY h.week_start DESC LIMIT 800`
	return eachRow(db, q, nil, func(row *sql.Rows) {
		var cid int64
		var name, topicsJSON string
		if row.Scan(&cid, &name, &topicsJSON) != nil {
			return
		}
		for _, t := range decodeTopics(topicsJSON) {
			kw := matchedKeyword(t.Name, kws)
			if kw == "" {
				continue
			}
			a := oppTouch(by, cid, name)
			a.score += 1
			a.ev = append(a.ev, OpportunityEvidence{Source: "topic", Layer: "INFERENCE", Text: clipRunes(t.Name, 60), Matched: kw})
		}
	})
}

// eachRow 单层加锁把一次查询全部读完再回调（绝不边遍历边再取 dbMu，避免自锁）。
func eachRow(db *sql.DB, query string, args []interface{}, handle func(*sql.Rows)) error {
	dbMu.Lock()
	rows, err := db.Query(query, args...)
	if err != nil {
		dbMu.Unlock()
		return nil // 某来源表缺失/出错：吞掉、诚实降级，绝不让整页 500
	}
	defer func() {
		rows.Close()
		dbMu.Unlock()
	}()
	for rows.Next() {
		handle(rows)
	}
	return nil
}

// loadRelationshipStrength 一次性读全部 relationship_state 的亲密度/基态/动态。
func loadRelationshipStrength(db *sql.DB) map[int64]oppState {
	out := map[int64]oppState{}
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "relationship_state") {
		return out
	}
	rows, err := db.Query(`SELECT contact_id, intimacy, base_state, dynamic_state FROM relationship_state`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var intimacy int
		var base, dynamic string
		if rows.Scan(&id, &intimacy, &base, &dynamic) == nil {
			out[id] = oppState{intimacy: intimacy, base: base, dynamic: dynamic}
		}
	}
	return out
}

func relationshipStrengthLabel(st oppState) string {
	if st.base == "" && st.dynamic == "" && st.intimacy == 0 {
		return "未知（暂无关系状态）"
	}
	s := fmt.Sprintf("亲密度 %d", st.intimacy)
	if st.base != "" {
		s += " · " + st.base
	}
	if st.dynamic != "" {
		s += "/" + st.dynamic
	}
	return s
}

// commonContactNames 读该联系人图谱邻居名字（共同联系人/可借道的关系），复用既有 connections。
func commonContactNames(db *sql.DB, contactID int64, limit int) []string {
	out := []string{}
	conns, err := ListConnections(db, contactID, limit*2)
	if err != nil {
		return out
	}
	seen := map[string]bool{}
	for _, c := range conns {
		other := c.NameB
		if c.ContactB == contactID {
			other = c.NameA
		}
		if other == "" || seen[other] {
			continue
		}
		seen[other] = true
		out = append(out, other)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func trimEvidence(a *oppAcc, max int) {
	if len(a.ev) > max {
		a.ev = a.ev[:max]
	}
}

// opportunityMatchReason 确定性概要：命中了哪些来源各几条。
func opportunityMatchReason(ev []OpportunityEvidence) string {
	var f, p, t int
	for _, e := range ev {
		switch e.Source {
		case "fact":
			f++
		case "project":
			p++
		case "topic":
			t++
		}
	}
	var parts []string
	if f > 0 {
		parts = append(parts, fmt.Sprintf("%d 条事实", f))
	}
	if p > 0 {
		parts = append(parts, fmt.Sprintf("%d 个项目", p))
	}
	if t > 0 {
		parts = append(parts, fmt.Sprintf("%d 个话题", t))
	}
	if len(parts) == 0 {
		return ""
	}
	return "命中 " + strings.Join(parts, "、")
}

// opportunityEntrySuggestion 由最高价值证据确定性给出推荐切入点。
func opportunityEntrySuggestion(ev []OpportunityEvidence) string {
	if len(ev) == 0 {
		return ""
	}
	// 优先级：事实 > 项目 > 话题（越确定的越适合做由头）。
	for _, want := range []string{"fact", "project", "topic"} {
		for _, e := range ev {
			if e.Source != want {
				continue
			}
			switch want {
			case "fact":
				return fmt.Sprintf("以你记录的「%s」为由头自然联系", e.Text)
			case "project":
				return fmt.Sprintf("借「%s」项目推进一次互动", e.Text)
			default:
				return fmt.Sprintf("从你们聊过的「%s」自然延续", e.Text)
			}
		}
	}
	return ""
}

// routeOpportunity GET /api/opportunity?q=<关键词>&top=<n> → “谁可能帮我”确定性发现（无 LLM 也 200）。
func (s *apiServer) routeOpportunity(w http.ResponseWriter, r *http.Request, sub []string) {
	if len(sub) != 0 || r.Method != http.MethodGet {
		writeErr(w, http.StatusNotFound, "未知接口: /api/opportunity")
		return
	}
	q := r.URL.Query().Get("q")
	top := 5
	if v := r.URL.Query().Get("top"); v != "" {
		fmt.Sscanf(v, "%d", &top)
	}
	res, err := DiscoverOpportunities(s.db, q, time.Now(), top)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "机会发现失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "data": res})
}
