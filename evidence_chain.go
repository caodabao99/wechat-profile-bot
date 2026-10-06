package main

// ═══════════════════════════════════════════════════════════════════════════
// §11 P2：Evidence Chain 3.0（PERSONAL RELATIONSHIP OS 3.0）
//
// 蓝图：当前 Evidence Provenance 2.0（六档 evidence_type + 加权置信度）已存在且不回退。
// 3.0 进一步做到——每个重要 Fact 都能**确定性地**回答八个问题：
//   1 谁说的  2 什么时候  3 原文  4 为什么支持  5 证据等级
//   6 是否存在反向证据  7 是否为第一人称  8 当前是否仍有效
// 并给出 §11.2 的**综合证据评分**（directness / source quality / consistency /
// recency / conflict / user confirmation 六因子）。
//
// 单一事实来源纪律：本文件是「证据评级」的唯一实现，不改写 §7.4 已测试的写路径置信度
// 公式（profile_facts.confidence 继续由 facts.go 维护）；这里只从**已存的证据行**做
// 只读、可解释、可复现的**解释层**，绝不引入第二套可信度真相，也绝不调用 LLM。
// 铁律：纯关键词命中（topic_related/weak_context）不得把评级抬到 high。
// ═══════════════════════════════════════════════════════════════════════════

import (
	"database/sql"
	"strconv"
	"strings"
	"time"
)

// EvidenceGrade 综合评级（§11.2 结论档，人类可读、可核对）。
type EvidenceGrade string

const (
	GradeHigh         EvidenceGrade = "high"         // 充分直接支撑
	GradeMedium       EvidenceGrade = "medium"       // 有支撑但不足/偏旧
	GradeLow          EvidenceGrade = "low"          // 弱相关或纯关键词命中
	GradeInsufficient EvidenceGrade = "insufficient" // 无任何支撑证据
	GradeConflicted   EvidenceGrade = "conflicted"   // 存在未失效的反向证据
)

// EvidenceAssessment §11.2 六因子综合评分（全部 0..1，确定性地从既有证据行推导）。
type EvidenceAssessment struct {
	Directness      float64       `json:"directness"`      // 直接证据占支撑证据的比例
	SourceQuality   float64       `json:"sourceQuality"`   // 第一人称 + 精确匹配的质量均值
	Consistency     float64       `json:"consistency"`     // 独立直接支撑条数（3 条封顶归一）
	Recency         float64       `json:"recency"`         // 最近证据新鲜度（180 天半衰）
	ConflictPenalty float64       `json:"conflictPenalty"` // 反向证据惩罚（有第一人称冲突=0.5，仅关键词冲突=0.25）
	UserConfirmed   bool          `json:"userConfirmed"`   // 用户确认（§8.2）直接封顶
	Score           float64       `json:"score"`           // 综合 0..1
	Grade           EvidenceGrade `json:"grade"`           // 结论档
}

// EvidenceChainAnswer §11 八字段的可核对回答（一条重要事实的证据链视图）。
type EvidenceChainAnswer struct {
	FactID          int64              `json:"fact_id"`
	ContactID       int64              `json:"contact_id"`
	FactType        string             `json:"fact_type"`
	FactKey         string             `json:"fact_key"`
	FactValue       string             `json:"fact_value"`
	Status          string             `json:"status"`
	WhoSaid         string             `json:"who_said"`         // 本人 / 用户确认 / 他人 / 未知（据第一人称线索）
	When            string             `json:"when"`             // 最近一次证据/见证实时间
	OriginalTexts   []EvidenceQuote    `json:"original_texts"`   // 支撑原文（含证据类型）
	CounterEvidence []EvidenceQuote    `json:"counter_evidence"` // 反向证据（evidence_type=conflict）
	FirstPerson     bool               `json:"first_person"`     // 是否存在第一人称直接证据
	StillValid      bool               `json:"still_valid"`      // 当前是否仍有效
	WhySupports     string             `json:"why_supports"`     // 依据哪类证据支撑（确定性描述）
	Assessment      EvidenceAssessment `json:"assessment"`       // §11.2 综合评分
}

// EvidenceQuote 一条证据的原文引用 + 分类。
type EvidenceQuote struct {
	MessageID       int64   `json:"message_id"`
	Archived        bool    `json:"archived"`
	Quote           string  `json:"quote"`
	MsgTime         string  `json:"msg_time"`
	EvidenceType    string  `json:"evidence_type"`
	SupportStrength float64 `json:"support_strength"`
	FirstPerson     bool    `json:"first_person"`
}

// AssessEvidence 从一条事实的既有证据行确定性地算出 §11.2 六因子综合评分。
// evidences 为该事实的全部证据（含 conflict 行）；sourceType/status 提供用户确认信号。
// 无证据 → insufficient、score=0。纯关键词命中（无 direct/strong）→ 上限 low。
func AssessEvidence(factType, sourceType, status string, evidences []FactEvidenceView, validUntil string, conflictEvidence []FactEvidenceView, now time.Time) EvidenceAssessment {
	a := EvidenceAssessment{}
	// 用户确认是最高权威（§8.2）：直接封顶，不再受关键词/时效影响。
	a.UserConfirmed = sourceType == "user" || status == "confirmed" || status == "verified"

	var support []FactEvidenceView // 计正向支撑的证据（排除 conflict）
	for _, e := range evidences {
		if e.EvidenceType != EvConflict {
			support = append(support, e)
		}
	}

	if len(support) == 0 {
		if a.UserConfirmed {
			// 用户确认是最高权威（§8.2）：即便无可检索证据也封顶高可信。
			a.Score = 0.98
			a.Grade = GradeHigh
			return a
		}
		a.Grade = GradeInsufficient
		if len(conflictEvidence) > 0 {
			a.Grade = GradeConflicted
		}
		return a
	}

	// Directness：direct 占支撑证据的比例。
	direct := 0
	var qsum float64
	for _, e := range support {
		if e.EvidenceType == EvDirect {
			direct++
		}
		// Source quality 单项：第一人称(+0.5) + 精确匹配(+0.5) ∈ [0,1]。
		sq := 0.0
		if hasFirstPerson(e.Quote) {
			sq += 0.5
		}
		if e.MatchType == "exact" {
			sq += 0.5
		}
		if e.EvidenceType == EvDirect {
			sq += 0.5 // 直接断言再加权；单条封顶 1
		}
		if sq > 1 {
			sq = 1
		}
		qsum += sq
	}
	a.Directness = float64(direct) / float64(len(support))
	a.SourceQuality = qsum / float64(len(support))
	// Consistency：独立直接支撑条数，3 条封顶归一（再多不线性膨胀）。
	a.Consistency = minF(1, float64(direct)/3.0)
	// Recency：最近一条支撑证据 msg_time 的 180 天半衰。
	a.Recency = recencyScore(support, now)

	// Conflict 惩罚（§11 第 6 问）：有第一人称反转 → 0.5；仅关键词反转 → 0.25。
	if len(conflictEvidence) > 0 {
		a.ConflictPenalty = 0.25
		for _, c := range conflictEvidence {
			if hasFirstPerson(c.Quote) {
				a.ConflictPenalty = 0.5
				break
			}
		}
	}

	base := 0.5*a.Directness + 0.2*a.SourceQuality + 0.15*a.Consistency + 0.15*a.Recency
	score := base * (1 - a.ConflictPenalty)
	if a.UserConfirmed {
		score = maxF(score, 0.98)
	}
	if score > 1 {
		score = 1
	}
	a.Score = score

	// 结论档：先判冲突，再判纯关键词上限，最后按分数分档。
	a.Grade = gradeFromScore(score)
	if a.ConflictPenalty >= 0.5 && !a.UserConfirmed {
		a.Grade = GradeConflicted
	}
	// 铁律：无直接证据（纯 topic_related/weak/strong_context）不得评 high/medium。
	if direct == 0 && !a.UserConfirmed && (a.Grade == GradeHigh || a.Grade == GradeMedium) {
		a.Grade = GradeLow
	}
	_ = validUntil
	return a
}

// gradeFromScore 分数 → 档位（阈值确定性）。
func gradeFromScore(s float64) EvidenceGrade {
	switch {
	case s >= 0.7:
		return GradeHigh
	case s >= 0.45:
		return GradeMedium
	default:
		return GradeLow
	}
}

// recencyScore 最近证据的新鲜度：以 180 天为半衰期，越近越接近 1，无法解析给中性 0.5。
func recencyScore(support []FactEvidenceView, now time.Time) float64 {
	var latest time.Time
	var found bool
	for _, e := range support {
		if t, ok := parseFactTime(e.MsgTime); ok && (!found || t.After(latest)) {
			latest, found = t, true
		}
	}
	if !found {
		return 0.5
	}
	days := now.Sub(latest).Hours() / 24
	if days < 0 {
		days = 0
	}
	// 半衰期 180 天：score = 0.5^(days/180)。
	const half = 180.0
	return exp2neg(days / half)
}

// exp2neg 计算 0.5^x = exp(-x*ln2)，用无 math 依赖的泰勒展开（8 阶）确定性地近似。
// x>=0；x 通常 <3 时精度足够。仅用于评分单调性，结果夹到 [0,1]。
func exp2neg(x float64) float64 {
	if x <= 0 {
		return 1
	}
	// 0.5^x = exp(-x*ln2)；用泰勒到 5 阶足够（x 通常 <3）。
	const ln2 = 0.6931471805599453
	t := -ln2 * x
	sum := 1.0
	term := 1.0
	for n := 1; n <= 8; n++ {
		term *= t / float64(n)
		sum += term
	}
	if sum < 0 {
		return 0
	}
	if sum > 1 {
		return 1
	}
	return sum
}

// hasFirstPerson 判断文本里是否含第一人称线索（复用 §7 的 evSelf 词表）。
func hasFirstPerson(s string) bool {
	return containsAny(s, evSelf...)
}

// BuildEvidenceChain 组装某联系人一条事实的完整证据链（§11 八问 + §11.2 评分）。
// 只读、无副作用、无 LLM。事实不存在返回 sql.ErrNoRows（调用方按 404 处理，不 500）。
// profile_facts / profile_fact_evidence 表由迁移保证存在（同 GetFacts）；表缺失时据实返回 sql.ErrNoRows。
func BuildEvidenceChain(db *sql.DB, contactID, factID int64, now time.Time) (*EvidenceChainAnswer, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "profile_facts") || !tableExistsLocked(db, "profile_fact_evidence") {
		return nil, sql.ErrNoRows
	}

	var (
		factType, factKey, factValue   string
		status, sourceType, validUntil string
	)
	err := db.QueryRow(
		`SELECT fact_type, fact_key, fact_value, status, source_type, valid_until
		 FROM profile_facts WHERE id=? AND contact_id=?`, factID, contactID).
		Scan(&factType, &factKey, &factValue, &status, &sourceType, &validUntil)
	if err != nil {
		return nil, err
	}
	// 取全部证据行（含 conflict），按时间倒序。
	evRows, err := db.Query(
		`SELECT message_id, archived, snippet, msg_time, match_type, support_strength, quote, is_direct_support, evidence_type
		 FROM profile_fact_evidence WHERE fact_id=? ORDER BY id DESC`, factID)
	if err != nil {
		return nil, err
	}
	defer evRows.Close()
	var all []FactEvidenceView
	for evRows.Next() {
		var e FactEvidenceView
		var arch, isDirect int
		if err := evRows.Scan(&e.MessageID, &arch, &e.Snippet, &e.MsgTime, &e.MatchType, &e.SupportStrength, &e.Quote, &isDirect, &e.EvidenceType); err != nil {
			continue
		}
		e.Archived = arch == 1
		e.IsDirectSupport = isDirect == 1
		all = append(all, e)
	}

	return assembleChain(contactID, factID, factType, factKey, factValue, status, sourceType, validUntil, all, now), nil
}

// assembleChain 纯函数：把事实字段 + 证据行装配成 §11 八问回答（便于确定性单测，不触库）。
func assembleChain(contactID, factID int64, factType, factKey, factValue, status, sourceType, validUntil string, all []FactEvidenceView, now time.Time) *EvidenceChainAnswer {
	var support, counter []EvidenceQuote
	var latestWhen string
	for _, e := range all {
		q := EvidenceQuote{
			MessageID:       e.MessageID,
			Archived:        e.Archived,
			Quote:           firstNonEmpty(e.Quote, e.Snippet),
			MsgTime:         e.MsgTime,
			EvidenceType:    e.EvidenceType,
			SupportStrength: e.SupportStrength,
			FirstPerson:     hasFirstPerson(e.Quote) || hasFirstPerson(e.Snippet),
		}
		if e.EvidenceType == EvConflict {
			counter = append(counter, q)
			continue
		}
		support = append(support, q)
		if e.MsgTime > latestWhen {
			latestWhen = e.MsgTime
		}
	}

	// 把原始 FactEvidenceView 传进评分器（含 conflict 全集 + 单独的 conflict 子集）。
	var rawViews, conflictViews []FactEvidenceView
	rawViews = all
	for _, e := range all {
		if e.EvidenceType == EvConflict {
			conflictViews = append(conflictViews, e)
		}
	}
	assessment := AssessEvidence(factType, sourceType, status, rawViews, validUntil, conflictViews, now)

	// 谁说的：用户确认 > 第一人称本人 > 他人 > 未知。
	who := "未知"
	firstPerson := false
	for _, s := range support {
		if s.FirstPerson {
			firstPerson = true
			break
		}
	}
	switch {
	case assessment.UserConfirmed:
		who = "用户确认"
	case firstPerson:
		who = "本人（第一人称）"
	case len(support) > 0:
		who = "他人/语境推断"
	}

	// 当前是否仍有效：非历史态、未失效（valid_until 空）。
	stillValid := statusActiveLike(status) && strings.TrimSpace(validUntil) == ""

	why := describeSupport(support)

	return &EvidenceChainAnswer{
		FactID:          factID,
		ContactID:       contactID,
		FactType:        factType,
		FactKey:         factKey,
		FactValue:       factValue,
		Status:          status,
		WhoSaid:         who,
		When:            latestWhen,
		OriginalTexts:   support,
		CounterEvidence: counter,
		FirstPerson:     firstPerson,
		StillValid:      stillValid,
		WhySupports:     why,
		Assessment:      assessment,
	}
}

// describeSupport 确定性描述「为什么支持」（取支撑里最高档证据类型）。
func describeSupport(support []EvidenceQuote) string {
	if len(support) == 0 {
		return "无支撑证据（仅来自画像断言）"
	}
	var direct, strong, weak, topic int
	for _, s := range support {
		switch s.EvidenceType {
		case EvDirect:
			direct++
		case EvStrongContext:
			strong++
		case EvWeakContext:
			weak++
		case EvTopicRelated:
			topic++
		}
	}
	switch {
	case direct > 0:
		return "第一人称直接陈述命中 " + strconv.Itoa(direct) + " 条"
	case strong > 0:
		return "强语境（计划/传闻/过去）线索 " + strconv.Itoa(strong) + " 条，不足以断言为事实本身"
	case topic > 0 || weak > 0:
		return "仅关键词/弱相关命中 " + strconv.Itoa(topic+weak) + " 条（不据此提高可信度，§7.4）"
	default:
		return "语境证据 " + strconv.Itoa(len(support)) + " 条"
	}
}

// statusActiveLike 判断事实是否处于「当前态」（非 retired/superseded/rejected）。
func statusActiveLike(status string) bool {
	switch status {
	case "retired", "superseded", "rejected":
		return false
	default:
		return true
	}
}

// parseFactTime 容错解析证据时间戳（RFC3339 / "2006-01-02 15:04:05" / "2006-01-02"）。
func parseFactTime(ts string) (time.Time, bool) {
	ts = strings.TrimSpace(ts)
	if ts == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// firstNonEmpty / minF 已由既有文件提供（risk.go / life_state.go），此处不重复定义。

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
