package main

// v5.0.0 智能联系人自动分组（确定性规则 + 一键采纳）。
//
// 只读地把「画像 profile_json + 亲密度统计 + 情绪 alert + 待跟进 open」组合成
// 一份标签建议清单：不写库、不调模型、可离线复现（同数据同参数跑两次逐字段相等）。
// 用户在前端勾选/一键采纳时，通过 apply 端点走 CreateTag（幂等） + INSERT OR IGNORE，
// 零新增写路径。
//
// 规则（全部确定性、无外部依赖）：
//   - 关系分层：复用 assistant.go 的 computeIntimacy 综合分（近 90 天，一次 SQL 全量算完）
//     ≥70 → 「核心关系」（conf 90）；40-69 → 「常联系」（conf 75）；1-39 → 「待激活」（conf 60）
//   - 情绪关注：assistant_emotions 近 30 天 alert=1 → 「近期需关心」（conf 85）
//   - 往来待跟进：followup_items 有 status='open' 且 kind ∈ {money, promise} → 「有往来待跟进」（conf 90）
//   - 地域：profile.BasicInfo.Location 非空 → 该值做标签（conf 80）
//   - 职业/行业：profile.BasicInfo.Occupation 关键词映射 → 分组标签（conf 70）
//   - 兴趣：profile.Interests 前 2 条（顺序按画像原样，确定性）→ 各打一个标签（conf 60）
//
// 铁律：
//   - 单连接池分层锁：一次读 contacts、一次读 emotions、一次读 followups、一次读已挂标签，
//     每段各自 dbMu.Lock/Unlock，绝不嵌套。computeIntimacy 内部已按此方式取锁，不重复加锁。
//   - 增值表缺失（assistant_emotions / followup_items / contact_tag_links）→ 静默跳过对应规则，
//     不 500；已挂标签缺失也当作空集继续。
//   - 已挂同名标签跳过（去重）、每联系人限 perContactMax 条、按 (contactId asc, confidence desc, tagName asc) 稳定排序。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	suggestMaxContacts        = 200 // ids 缺省时一次最多给多少联系人出建议
	suggestDefaultPerMax      = 4   // 每联系人默认建议上限
	suggestHardPerMax         = 8   // 每联系人硬上限
	suggestIntimacyWindowDays = 90  // 关系分层窗口
	suggestEmotionWindowDays  = 30  // 情绪告警窗口
	suggestInterestsMax       = 2   // 每联系人最多从 interests 抽几条
	suggestApplyMaxItems      = 500 // apply 一次最多落多少条
)

// TagSuggestion 一条标签建议
type TagSuggestion struct {
	ContactID  int64  `json:"contactId"`
	Name       string `json:"name"`
	TagName    string `json:"tagName"`
	Reason     string `json:"reason"`
	Confidence int    `json:"confidence"`
}

// occupationKeywordMap 职业关键词 → 分组标签；命中即停，按声明顺序优先。
var occupationKeywordMap = []struct {
	Keywords []string
	Tag      string
}{
	{[]string{"程序员", "软件开发", "开发工程师", "码农", "前端", "后端", "全栈", "算法工程师", "技术总监", "架构师"}, "技术"},
	{[]string{"产品经理", "产品总监", "产品专员", "产品运营"}, "产品"},
	{[]string{"设计师", "UI设计", "UE设计", "平面设计", "工业设计", "视觉设计"}, "设计"},
	{[]string{"销售", "市场营销", "商务拓展", "BD", "增长运营", "市场经理"}, "市场销售"},
	{[]string{"老师", "教师", "教授", "讲师", "教育工作者", "培训讲师"}, "教育"},
	{[]string{"医生", "护士", "医师", "药剂师", "医疗"}, "医疗"},
	{[]string{"律师", "法务", "法律顾问"}, "法律"},
	{[]string{"财务", "会计", "审计", "税务"}, "财务"},
	{[]string{"公务员", "事业单位", "机关", "编制", "老师编制"}, "体制内"},
	{[]string{"创始人", "CEO", "CTO", "COO", "法人", "创业"}, "创业者"},
	{[]string{"记者", "编辑", "传媒", "自媒体", "主播"}, "传媒"},
	{[]string{"银行", "投资", "基金", "证券", "保险", "金融"}, "金融"},
}

// SuggestTags 对给定联系人（ids 空 = 全部未合并活跃联系人，截断到 suggestMaxContacts）
// 按确定性规则产标签建议。只读、不写库。
func SuggestTags(db *sql.DB, ids []int64, perContactMax int) ([]TagSuggestion, error) {
	if perContactMax <= 0 {
		perContactMax = suggestDefaultPerMax
	}
	if perContactMax > suggestHardPerMax {
		perContactMax = suggestHardPerMax
	}

	// —— 1) 一次锁读联系人（id/name/remark/profile_json），过滤合并、截断上限 ——
	type row struct {
		id       int64
		name     string
		remark   string
		profJSON string
	}
	var contacts []row
	dbMu.Lock()
	idSet := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id > 0 {
			idSet[id] = true
		}
	}
	if len(idSet) > suggestMaxContacts {
		// 显式传入超限也截断，保证护栏有效
		tmp := make([]int64, 0, suggestMaxContacts)
		for id := range idSet {
			tmp = append(tmp, id)
			if len(tmp) >= suggestMaxContacts {
				break
			}
		}
		idSet = map[int64]bool{}
		for _, id := range tmp {
			idSet[id] = true
		}
	}
	// 用 IN 参数化，避免 SQLite 绑定上限；规模有限（<=200）
	var qRows *sql.Rows
	var qErr error
	if len(idSet) == 0 {
		qRows, qErr = db.Query(
			`SELECT id, name, COALESCE(remark, ''), COALESCE(profile_json, '')
			 FROM contacts WHERE merged_into IS NULL ORDER BY id ASC LIMIT ?`,
			suggestMaxContacts)
	} else {
		placeholders := strings.Repeat("?,", len(idSet))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]interface{}, 0, len(idSet))
		for id := range idSet {
			args = append(args, id)
		}
		q := fmt.Sprintf(
			`SELECT id, name, COALESCE(remark, ''), COALESCE(profile_json, '')
			 FROM contacts WHERE merged_into IS NULL AND id IN (%s) ORDER BY id ASC`, placeholders)
		qRows, qErr = db.Query(q, args...)
	}
	if qErr != nil {
		dbMu.Unlock()
		return nil, qErr
	}
	for qRows.Next() {
		var r row
		if err := qRows.Scan(&r.id, &r.name, &r.remark, &r.profJSON); err != nil {
			continue
		}
		contacts = append(contacts, r)
	}
	qRows.Close()
	dbMu.Unlock()

	if len(contacts) == 0 {
		return []TagSuggestion{}, nil
	}

	// —— 2) 亲密度全量算一次（computeIntimacy 内已按分层锁方式取 dbMu）——
	intimacy := map[int64]int{}
	if list, err := computeIntimacy(db, time.Now(), suggestIntimacyWindowDays); err == nil {
		for _, it := range list {
			intimacy[it.ContactID] = it.Score
		}
	}

	allIDs := make([]int64, 0, len(contacts))
	for _, c := range contacts {
		allIDs = append(allIDs, c.id)
	}

	// —— 3) 一次锁读已挂标签（若标签表缺失，TagsForContacts 返回错，静默视空）——
	existingTags := map[int64]map[string]bool{}
	if tagMap, err := TagsForContacts(db, allIDs); err == nil {
		for cid, list := range tagMap {
			s := map[string]bool{}
			for _, t := range list {
				s[t.Name] = true
			}
			existingTags[cid] = s
		}
	}

	// —— 4) 一次锁读情绪告警（表缺失/查询失败 → 该规则静默跳过）——
	emotionAlert := map[int64]bool{}
	dbMu.Lock()
	if tableExistsLocked(db, "assistant_emotions") {
		since := time.Now().AddDate(0, 0, -suggestEmotionWindowDays)
		rows, err := db.Query(
			`SELECT DISTINCT contact_id FROM assistant_emotions
			 WHERE alert = 1 AND strftime('%s', created_at, 'localtime') >= strftime('%s', ?)`,
			since.Format("2006-01-02 15:04:05"))
		if err == nil {
			for rows.Next() {
				var id int64
				if rows.Scan(&id) == nil {
					emotionAlert[id] = true
				}
			}
			rows.Close()
		}
	}
	dbMu.Unlock()

	// —— 5) 一次锁读 open 的 money/promise 待跟进（表缺失 → 静默跳过）——
	openFollowup := map[int64]bool{}
	dbMu.Lock()
	if tableExistsLocked(db, "followup_items") {
		rows, err := db.Query(
			`SELECT DISTINCT contact_id FROM followup_items
			 WHERE status = 'open' AND kind IN ('money','promise')`)
		if err == nil {
			for rows.Next() {
				var id int64
				if rows.Scan(&id) == nil {
					openFollowup[id] = true
				}
			}
			rows.Close()
		}
	}
	dbMu.Unlock()

	// —— 6) 逐联系人跑规则、去重、截断 ——
	out := []TagSuggestion{}
	for _, c := range contacts {
		// 复用 displayName 的呈现：有备注显示「备注（昵称）」
		cc := &Contact{ID: c.id, Name: c.name, Remark: c.remark}
		label := displayName(cc)
		seen := existingTags[c.id]
		if seen == nil {
			seen = map[string]bool{}
		}
		// 同一批建议内也去重（比如 interests 有同名项）
		batch := map[string]bool{}
		push := func(tag, reason string, conf int) {
			tag = normalizeSuggestTag(tag)
			if tag == "" || seen[tag] || batch[tag] {
				return
			}
			batch[tag] = true
			out = append(out, TagSuggestion{ContactID: c.id, Name: label, TagName: tag, Reason: reason, Confidence: conf})
		}

		// 6.1 关系分层
		if s, ok := intimacy[c.id]; ok && s > 0 {
			switch {
			case s >= 70:
				push("核心关系", fmt.Sprintf("近 %d 天亲密度 %d（≥70）", suggestIntimacyWindowDays, s), 90)
			case s >= 40:
				push("常联系", fmt.Sprintf("近 %d 天亲密度 %d（40-69）", suggestIntimacyWindowDays, s), 75)
			default:
				push("待激活", fmt.Sprintf("近 %d 天亲密度 %d（<40）", suggestIntimacyWindowDays, s), 60)
			}
		}

		// 6.2 情绪告警
		if emotionAlert[c.id] {
			push("近期需关心", fmt.Sprintf("近 %d 天情绪分析有告警记录", suggestEmotionWindowDays), 85)
		}

		// 6.3 往来待跟进
		if openFollowup[c.id] {
			push("有往来待跟进", "存在未完成的金钱/承诺类待跟进", 90)
		}

		// 6.4 画像：地域 / 职业 / 兴趣
		pj := strings.TrimSpace(c.profJSON)
		if pj != "" {
			var p Profile
			if json.Unmarshal([]byte(pj), &p) == nil {
				if loc := strings.TrimSpace(p.BasicInfo.Location); loc != "" {
					push(loc, "画像「所在城市/地区」明确", 80)
				}
				if occ := strings.TrimSpace(p.BasicInfo.Occupation); occ != "" {
					for _, kw := range occupationKeywordMap {
						matched := false
						for _, w := range kw.Keywords {
							if strings.Contains(occ, w) {
								push(kw.Tag, "画像「职业」命中关键词："+w, 70)
								matched = true
								break
							}
						}
						if matched {
							break
						}
					}
				}
				for i, it := range p.Interests {
					if i >= suggestInterestsMax {
						break
					}
					it = strings.TrimSpace(it)
					if it == "" {
						continue
					}
					push(it, "画像「兴趣爱好」列出的兴趣", 60)
				}
			}
		}

		// 每联系人截断：按 confidence desc, tagName asc
		if n := countFor(out, c.id); n > perContactMax {
			trimSuggestions(&out, c.id, perContactMax)
		}
	}

	// —— 7) 全局稳定排序 ——
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ContactID != out[j].ContactID {
			return out[i].ContactID < out[j].ContactID
		}
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		return out[i].TagName < out[j].TagName
	})
	return out, nil
}

// ApplyTagSuggestion 单条采纳：CreateTag 幂等 + INSERT OR IGNORE 挂链接。
// 返回 1 代表新增了 link，0 代表已存在（幂等）。
func ApplyTagSuggestion(db *sql.DB, contactID int64, tagName string) (int, error) {
	tag, err := CreateTag(db, tagName)
	if err != nil {
		return 0, err
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	var exists int
	if err := db.QueryRow(`SELECT 1 FROM contacts WHERE id = ?`, contactID).Scan(&exists); err != nil {
		return 0, fmt.Errorf("联系人不存在")
	}
	now := time.Now().Format(time.RFC3339)
	res, err := db.Exec(
		`INSERT OR IGNORE INTO contact_tag_links (contact_id, tag_id, created_at) VALUES (?, ?, ?)`,
		contactID, tag.ID, now)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ApplyTagSuggestions 批量采纳：一条一条走 CreateTag + INSERT OR IGNORE，累计 affected。
// items 上限护栏 suggestApplyMaxItems，避免冻住服务。
func ApplyTagSuggestions(db *sql.DB, items []struct {
	ContactID int64  `json:"contactId"`
	TagName   string `json:"tagName"`
}) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	if len(items) > suggestApplyMaxItems {
		return 0, fmt.Errorf("一次最多采纳 %d 条建议", suggestApplyMaxItems)
	}
	affected := 0
	for _, it := range items {
		if it.ContactID <= 0 {
			continue
		}
		if strings.TrimSpace(it.TagName) == "" {
			continue
		}
		n, err := ApplyTagSuggestion(db, it.ContactID, it.TagName)
		if err != nil {
			// 单条失败不影响整体；上层 API 里选择忽略继续
			continue
		}
		affected += n
	}
	return affected, nil
}

// normalizeSuggestTag 与 tags.go 的 normalizeTagName 保持语义一致（去多余空白、限长），
// 但不返回 error——建议规则里如果值本身非法（过长），直接截断，宁可少给一条建议也不整体报错。
func normalizeSuggestTag(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return ""
	}
	if utf8.RuneCountInString(name) > maxTagNameRunes {
		r := []rune(name)
		name = string(r[:maxTagNameRunes])
	}
	return name
}

func countFor(list []TagSuggestion, cid int64) int {
	n := 0
	for _, s := range list {
		if s.ContactID == cid {
			n++
		}
	}
	return n
}

// trimSuggestions 就地把某个联系人的建议裁到 keep 条（按 confidence desc, tagName asc 保前 keep）。
func trimSuggestions(out *[]TagSuggestion, cid int64, keep int) {
	var mine []TagSuggestion
	var others []TagSuggestion
	for _, s := range *out {
		if s.ContactID == cid {
			mine = append(mine, s)
		} else {
			others = append(others, s)
		}
	}
	sort.SliceStable(mine, func(i, j int) bool {
		if mine[i].Confidence != mine[j].Confidence {
			return mine[i].Confidence > mine[j].Confidence
		}
		return mine[i].TagName < mine[j].TagName
	})
	if len(mine) > keep {
		mine = mine[:keep]
	}
	res := append(others, mine...)
	*out = res
}
