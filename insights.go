package main

// 疑似重复联系人推荐（增值功能）。
//
// 设计约束：
//   - 纯只读，绝不修改任何数据；合并动作仍由用户在前端走既有的 /api/contacts/{id}/merge 流程，
//     这里只负责"挑出可能是同一个人的组合"。
//   - 单次查询内完成所有 rows 迭代（dbMu 不可重入，db 只有 1 个连接）。

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

const (
	dupMaxContacts  = 2000 // 参与两两比较的联系人上限，防止 O(n²) 失控
	dupMaxPairs     = 100  // 最多返回多少组建议
	dupMinScore     = 70   // 低于该相似度不提示
	dupMaxNameRunes = 32   // 参与编辑距离计算的名字长度上限
	dupMaxKeys      = 6    // 单个联系人最多参与比较的候选名（昵称+备注+别名）
)

// DuplicateContact 建议里的一个联系人摘要
type DuplicateContact struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Remark      string `json:"remark"`
	MsgCount    int    `json:"msgCount"`
	LastUpdated string `json:"lastUpdated"`
}

// DuplicatePair 一组疑似重复
type DuplicatePair struct {
	A             DuplicateContact `json:"a"`
	B             DuplicateContact `json:"b"`
	Reason        string           `json:"reason"`
	Score         int              `json:"score"`
	SuggestedKeep int64            `json:"suggestedKeep"` // 建议保留（作为合并目标）的那个 id
}

// DuplicateResult 推荐结果
type DuplicateResult struct {
	List      []DuplicatePair `json:"list"`
	Scanned   int             `json:"scanned"`   // 实际参与比较的联系人数
	Truncated bool            `json:"truncated"` // 联系人太多被截断
}

// normalizeForMatch 归一化用于比较的名字：小写、只保留字母/数字/汉字，
// 丢掉空格、标点、emoji 等装饰字符。
func normalizeForMatch(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func levenshteinRunes(a, b []rune) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := cur[j-1] + 1
			sub := prev[j-1] + cost
			best := del
			if ins < best {
				best = ins
			}
			if sub < best {
				best = sub
			}
			cur[j] = best
		}
		prev, cur = cur, prev
	}
	return prev[lb]
}

// compareNames 给两个归一化后的名字打相似度分（0~100）。
// 低于 dupMinScore 时 reason 返回空串，调用方据此过滤。
func compareNames(a, b string) (int, string) {
	if a == "" || b == "" {
		return 0, ""
	}
	if a == b {
		return 100, "名字完全相同"
	}
	ra, rb := []rune(a), []rune(b)
	shorter, longer := ra, rb
	if len(shorter) > len(longer) {
		shorter, longer = rb, ra
	}
	if len(shorter) >= 2 && strings.Contains(string(longer), string(shorter)) {
		// 包含关系：长度差越大越可能是不同的人（"小王" vs "王志强办公室"）
		score := 90 - (len(longer)-len(shorter))*4
		if score < dupMinScore {
			return 0, ""
		}
		return score, fmt.Sprintf("「%s」包含「%s」", string(longer), string(shorter))
	}
	la, lb := ra, rb
	if len(la) > dupMaxNameRunes {
		la = la[:dupMaxNameRunes]
	}
	if len(lb) > dupMaxNameRunes {
		lb = lb[:dupMaxNameRunes]
	}
	maxLen := len(la)
	if len(lb) > maxLen {
		maxLen = len(lb)
	}
	if maxLen < 2 {
		return 0, ""
	}
	dist := levenshteinRunes(la, lb)
	score := int(float64(maxLen-dist) / float64(maxLen) * 100)
	if score < dupMinScore {
		return 0, ""
	}
	return score, fmt.Sprintf("名字高度相似（相差 %d 字）", dist)
}

// candidateKeys 一个联系人参与比较的候选名集合（昵称/备注/别名，归一化去重）
func candidateKeys(c DuplicateContact, aliases []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s, label string) {
		k := normalizeForMatch(s)
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, k)
	}
	add(c.Name, "昵称")
	add(c.Remark, "备注")
	for _, a := range aliases {
		if len(out) >= dupMaxKeys {
			break
		}
		add(a, "别名")
	}
	return out
}

// sortedRunes 字符排序签名，用于抓"字一样但顺序不同"的组合（张伟 / 伟张）
func sortedRunes(s string) string {
	rs := []rune(s)
	sort.Slice(rs, func(i, j int) bool { return rs[i] < rs[j] })
	return string(rs)
}

type dupCandidate struct {
	id   int64
	c    DuplicateContact
	keys []string
}

// loadDuplicateCandidates 一次性把判重需要的数据从库里捞出来（锁只覆盖这一步）。
// 后面的两两比较是纯内存的 O(n²)，绝不能抱着 dbMu 做——那会把整个服务的读写全冻住。
func loadDuplicateCandidates(db *sql.DB) (int, []DuplicateContact, map[int64][]string, map[int64]int, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	total := 0
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM contacts WHERE COALESCE(merged_into, 0) = 0`).Scan(&total); err != nil {
		return 0, nil, nil, nil, err
	}
	if total < 2 {
		return total, nil, nil, nil, nil
	}

	rows, err := db.Query(`
		SELECT c.id, c.name, COALESCE(c.remark, ''), COALESCE(c.other_msg_count, 0), COALESCE(c.last_updated, '')
		FROM contacts c
		WHERE COALESCE(c.merged_into, 0) = 0
		ORDER BY c.id
		LIMIT ?`, dupMaxContacts)
	if err != nil {
		return 0, nil, nil, nil, err
	}
	var list []DuplicateContact
	for rows.Next() {
		var c DuplicateContact
		if err := rows.Scan(&c.ID, &c.Name, &c.Remark, &c.MsgCount, &c.LastUpdated); err != nil {
			rows.Close()
			return 0, nil, nil, nil, err
		}
		list = append(list, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, nil, nil, err
	}

	// 别名（表可能不存在——老库未跑过 merge 相关迁移时跳过）
	aliasMap := map[int64][]string{}
	if arows, aerr := db.Query(`SELECT contact_id, alias FROM contact_aliases`); aerr == nil {
		for arows.Next() {
			var cid int64
			var alias string
			if err := arows.Scan(&cid, &alias); err != nil {
				break
			}
			aliasMap[cid] = append(aliasMap[cid], alias)
		}
		arows.Close()
	}

	// 真实消息条数（other_msg_count 只是画像用的近似值）
	cntMap := map[int64]int{}
	if crows, cerr := db.Query(`SELECT contact_id, COUNT(*) FROM messages GROUP BY contact_id`); cerr == nil {
		for crows.Next() {
			var cid, n int64
			if err := crows.Scan(&cid, &n); err != nil {
				break
			}
			cntMap[cid] = int(n)
		}
		crows.Close()
	}
	return total, list, aliasMap, cntMap, nil
}

// FindDuplicateContacts 扫描未合并的联系人，给出疑似重复的组合。
// 为了控制两两比较的规模，用"首字 + 字符排序签名"分桶，只在同桶内比较。
func FindDuplicateContacts(db *sql.DB) (*DuplicateResult, error) {
	res := &DuplicateResult{List: []DuplicatePair{}}

	total, list, aliasMap, cntMap, err := loadDuplicateCandidates(db)
	if err != nil {
		return nil, err
	}
	res.Scanned = total
	if total < 2 {
		return res, nil
	}
	res.Truncated = total > dupMaxContacts

	cands := make([]dupCandidate, 0, len(list))
	buckets := map[string][]int{}
	for i, c := range list {
		if n, ok := cntMap[c.ID]; ok {
			c.MsgCount = n
			list[i] = c
		}
		al := aliasMap[c.ID]
		keys := candidateKeys(c, al)
		if len(keys) == 0 {
			continue
		}
		idx := len(cands)
		var bs []string
		for _, k := range keys {
			rs := []rune(k)
			bs = append(bs, "p:"+string(rs[0]))
			if len(k) >= 3 {
				bs = append(bs, "s:"+sortedRunes(k))
			}
		}
		for _, b := range bs {
			buckets[b] = append(buckets[b], idx)
		}
		cands = append(cands, dupCandidate{id: c.ID, c: c, keys: keys})
	}
	res.Scanned = len(cands)

	// 同桶内两两比较；用 map 去重（同一对可能落在多个桶）
	type pairKey struct{ a, b int64 }
	best := map[pairKey]DuplicatePair{}
	for _, idxs := range buckets {
		if len(idxs) < 2 || len(idxs) > 200 {
			// 桶太大（比如一堆以"小"开头的备注）意义不大且很慢，直接跳过
			continue
		}
		for x := 0; x < len(idxs); x++ {
			for y := x + 1; y < len(idxs); y++ {
				pa, pb := cands[idxs[x]], cands[idxs[y]]
				if pa.id == pb.id {
					continue
				}
				score := 0
				reason := ""
				for _, ka := range pa.keys {
					for _, kb := range pb.keys {
						s, r := compareNames(ka, kb)
						if s > score {
							score, reason = s, r
						}
					}
				}
				if score < dupMinScore {
					continue
				}
				k := pairKey{pa.id, pb.id}
				if pa.id > pb.id {
					k = pairKey{pb.id, pa.id}
				}
				if old, ok := best[k]; ok && old.Score >= score {
					continue
				}
				keep := pa.id
				if pb.c.MsgCount > pa.c.MsgCount || (pb.c.MsgCount == pa.c.MsgCount && pb.id < pa.id) {
					keep = pb.id
				}
				best[k] = DuplicatePair{
					A: pa.c, B: pb.c, Reason: reason, Score: score, SuggestedKeep: keep,
				}
			}
		}
	}

	out := make([]DuplicatePair, 0, len(best))
	for _, p := range best {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		total := func(p DuplicatePair) int { return p.A.MsgCount + p.B.MsgCount }
		return total(out[i]) > total(out[j])
	})
	if len(out) > dupMaxPairs {
		out = out[:dupMaxPairs]
	}
	res.List = out
	return res, nil
}
