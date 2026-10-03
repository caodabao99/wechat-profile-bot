package main

// 我的社交大盘（增值功能，纯只读）。
//
// 统计维度：消息量、双向平均回复时长、谁更主动、活跃时段/星期分布、
// 聊得最多的人、我和对方的口头禅。
// 全部在本地用 SQL + Go 计算，不调用 LLM，可以随便刷新。

import (
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	socialDefaultDays = 30
	socialMaxDays     = 3650
	socialMaxScan     = 200000   // 单次最多扫描多少条消息（按时间倒序取最近的）
	socialMaxContent  = 20000    // 口头禅统计最多读多少条文本
	socialReplyCap    = 6 * 3600 // 超过 6 小时的间隔不算"回复"（睡觉/上班）
	socialSessionGap  = 3 * 3600 // 超过 3 小时视为新会话，第一条算"主动发起"
	socialTopN        = 10
)

// HourBucket 某个小时的消息量
type HourBucket struct {
	Hour   int `json:"hour"`
	Mine   int `json:"mine"`
	Theirs int `json:"theirs"`
}

// PhraseStat 一个高频短语
type PhraseStat struct {
	Phrase string `json:"phrase"`
	Count  int    `json:"count"`
}

// ContactMsgStat 单个联系人的消息量与主动次数
type ContactMsgStat struct {
	ContactID   int64  `json:"contactId"`
	Name        string `json:"name"`
	Total       int    `json:"total"`
	Mine        int    `json:"mine"`
	Theirs      int    `json:"theirs"`
	MyInitiate  int    `json:"myInitiate"` // 我主动发起会话的次数
	LastMsgTime string `json:"lastMsgTime"`
}

// SocialStats 社交大盘
type SocialStats struct {
	Days        int    `json:"days"`
	GeneratedAt string `json:"generatedAt"`
	From        string `json:"from"`

	TotalMessages    int    `json:"totalMessages"`
	MyMessages       int    `json:"myMessages"`
	TheirMessages    int    `json:"theirMessages"`
	ActiveContacts   int    `json:"activeContacts"`
	ActiveDays       int    `json:"activeDays"`
	LongestStreakDay string `json:"longestStreakDay"` // 消息最多的那一天
	LongestStreakCnt int    `json:"longestStreakCount"`

	MyReplyAvgSec     float64 `json:"myReplyAvgSeconds"`
	MyReplyMedianSec  float64 `json:"myReplyMedianSeconds"`
	MyReplySamples    int     `json:"myReplySamples"`
	TheirReplyAvgSec  float64 `json:"theirReplyAvgSeconds"`
	TheirReplySamples int     `json:"theirReplySamples"`

	MyInitiateTotal    int `json:"myInitiateTotal"`
	TheirInitiateTotal int `json:"theirInitiateTotal"`

	Hourly  []HourBucket     `json:"hourly"`
	Weekday []int            `json:"weekday"` // 下标 0=周日
	Top     []ContactMsgStat `json:"topContacts"`
	// 谁最主动：按 (我主动次数 / 会话总数) 排序，至少 3 次会话才纳入
	Initiators []ContactMsgStat `json:"initiators"`

	MyPhrases    []PhraseStat `json:"myPhrases"`
	TheirPhrases []PhraseStat `json:"theirPhrases"`

	Truncated bool `json:"truncated"` // 消息量超过 socialMaxScan 被截断
}

func avgInt(vals []int64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var sum int64
	for _, v := range vals {
		sum += v
	}
	return float64(sum) / float64(len(vals))
}

func medianInt(vals []int64) float64 {
	if len(vals) == 0 {
		return 0
	}
	cp := make([]int64, len(vals))
	copy(cp, vals)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return float64(cp[mid])
	}
	return float64(cp[mid-1]+cp[mid]) / 2
}

// ComputeSocialStats 计算最近 days 天的社交大盘。
func ComputeSocialStats(db *sql.DB, days int) (*SocialStats, error) {
	if days <= 0 {
		days = socialDefaultDays
	}
	if days > socialMaxDays {
		days = socialMaxDays
	}
	now := time.Now()
	since := now.AddDate(0, 0, -days)
	sinceStr := since.Format(time.RFC3339)

	st := &SocialStats{
		Days:         days,
		GeneratedAt:  now.Format("2006-01-02 15:04:05"),
		From:         since.Format("2006-01-02"),
		Hourly:       make([]HourBucket, 24),
		Weekday:      make([]int, 7),
		Top:          []ContactMsgStat{},
		Initiators:   []ContactMsgStat{},
		MyPhrases:    []PhraseStat{},
		TheirPhrases: []PhraseStat{},
	}
	for i := range st.Hourly {
		st.Hourly[i] = HourBucket{Hour: i}
	}

	type rawMsg struct {
		contactID int64
		sender    string
		ts        int64
	}
	var msgs []rawMsg

	dbMu.Lock()
	rows, err := db.Query(`
		SELECT contact_id, sender, strftime('%s', msg_time)
		FROM messages
		WHERE msg_time IS NOT NULL AND msg_time != ''
		  AND strftime('%s', msg_time) >= strftime('%s', ?)
		ORDER BY strftime('%s', msg_time) DESC, id DESC
		LIMIT ?`, sinceStr, socialMaxScan+1)
	if err != nil {
		dbMu.Unlock()
		return nil, err
	}
	for rows.Next() {
		var m rawMsg
		if err := rows.Scan(&m.contactID, &m.sender, &m.ts); err != nil {
			rows.Close()
			dbMu.Unlock()
			return nil, err
		}
		msgs = append(msgs, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		dbMu.Unlock()
		return nil, err
	}

	// 联系人名（备注优先），一次查完
	names := map[int64]string{}
	if nrs, nerr := db.Query(
		`SELECT id, COALESCE(remark, ''), name FROM contacts WHERE COALESCE(merged_into, 0) = 0`); nerr == nil {
		for nrs.Next() {
			var id int64
			var remark, name string
			if err := nrs.Scan(&id, &remark, &name); err != nil {
				break
			}
			if strings.TrimSpace(remark) != "" {
				names[id] = remark + "（" + name + "）"
			} else {
				names[id] = name
			}
		}
		nrs.Close()
	}

	// 对方的口头禅直接取画像里的 FrequentPhrases（已经是 LLM 提炼过的）
	var profiles []string
	if prs, perr := db.Query(
		`SELECT profile_json FROM contacts
		 WHERE COALESCE(merged_into, 0) = 0 AND profile_json != '' AND profile_json != '{}'
		 LIMIT 500`); perr == nil {
		for prs.Next() {
			var pj string
			if err := prs.Scan(&pj); err != nil {
				break
			}
			profiles = append(profiles, pj)
		}
		prs.Close()
	}

	// 我的口头禅：时间窗内我发的短消息
	var myTexts []string
	if trs, terr := db.Query(`
		SELECT content FROM messages
		WHERE sender = 'me' AND msg_time IS NOT NULL AND msg_time != ''
		  AND strftime('%s', msg_time) >= strftime('%s', ?)
		  AND length(content) BETWEEN 4 AND 200
		ORDER BY strftime('%s', msg_time) DESC
		LIMIT ?`, sinceStr, socialMaxContent); terr == nil {
		for trs.Next() {
			var c string
			if err := trs.Scan(&c); err != nil {
				break
			}
			myTexts = append(myTexts, c)
		}
		trs.Close()
	}
	dbMu.Unlock()

	if len(msgs) > socialMaxScan {
		st.Truncated = true
		msgs = msgs[:socialMaxScan]
	}
	// 查询是倒序，翻转成正序方便算相邻间隔
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}

	st.TotalMessages = len(msgs)
	perContact := map[int64]*ContactMsgStat{}
	myReplyGaps := []int64{}
	theirReplyGaps := []int64{}
	dayCount := map[string]int{}

	byContact := map[int64][]rawMsg{}
	for _, m := range msgs {
		if m.sender == "me" {
			st.MyMessages++
		} else {
			st.TheirMessages++
		}
		t := time.Unix(m.ts, 0)
		hb := st.Hourly[t.Hour()]
		if m.sender == "me" {
			hb.Mine++
		} else {
			hb.Theirs++
		}
		st.Hourly[t.Hour()] = hb
		st.Weekday[int(t.Weekday())]++
		day := t.Format("2006-01-02")
		dayCount[day]++
		byContact[m.contactID] = append(byContact[m.contactID], m)

		cs := perContact[m.contactID]
		if cs == nil {
			cs = &ContactMsgStat{ContactID: m.contactID, Name: names[m.contactID]}
			if cs.Name == "" {
				cs.Name = "未知联系人"
			}
			perContact[m.contactID] = cs
		}
		cs.Total++
		if m.sender == "me" {
			cs.Mine++
		} else {
			cs.Theirs++
		}
		cs.LastMsgTime = t.Format("2006-01-02 15:04")
	}

	st.ActiveContacts = len(perContact)
	st.ActiveDays = len(dayCount)
	for day, n := range dayCount {
		if n > st.LongestStreakCnt {
			st.LongestStreakCnt = n
			st.LongestStreakDay = day
		}
	}

	// 回复时长 + 主动发起：按联系人分别走一遍时间线
	for cid, list := range byContact {
		sort.Slice(list, func(i, j int) bool { return list[i].ts < list[j].ts })
		for i := 1; i < len(list); i++ {
			prev, cur := list[i-1], list[i]
			gap := cur.ts - prev.ts
			if gap <= 0 {
				continue
			}
			if gap > socialSessionGap {
				// 新会话的第一条 = 主动发起方
				if cur.sender == "me" {
					st.MyInitiateTotal++
					if cs := perContact[cid]; cs != nil {
						cs.MyInitiate++
					}
				} else {
					st.TheirInitiateTotal++
				}
				continue
			}
			if prev.sender == cur.sender {
				continue
			}
			if gap > socialReplyCap {
				continue
			}
			if cur.sender == "me" {
				myReplyGaps = append(myReplyGaps, gap)
			} else {
				theirReplyGaps = append(theirReplyGaps, gap)
			}
		}
		// 窗口内第一条也算一次主动
		if len(list) > 0 {
			if list[0].sender == "me" {
				st.MyInitiateTotal++
				if cs := perContact[cid]; cs != nil {
					cs.MyInitiate++
				}
			} else {
				st.TheirInitiateTotal++
			}
		}
	}

	st.MyReplyAvgSec = avgInt(myReplyGaps)
	st.MyReplyMedianSec = medianInt(myReplyGaps)
	st.MyReplySamples = len(myReplyGaps)
	st.TheirReplyAvgSec = avgInt(theirReplyGaps)
	st.TheirReplySamples = len(theirReplyGaps)

	all := make([]ContactMsgStat, 0, len(perContact))
	for _, cs := range perContact {
		all = append(all, *cs)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Total != all[j].Total {
			return all[i].Total > all[j].Total
		}
		return all[i].ContactID < all[j].ContactID
	})
	if len(all) > socialTopN {
		st.Top = all[:socialTopN]
	} else {
		st.Top = all
	}

	init := make([]ContactMsgStat, 0, len(all))
	for _, cs := range all {
		if cs.MyInitiate >= 3 {
			init = append(init, cs)
		}
	}
	sort.Slice(init, func(i, j int) bool {
		if init[i].MyInitiate != init[j].MyInitiate {
			return init[i].MyInitiate > init[j].MyInitiate
		}
		return init[i].Total > init[j].Total
	})
	if len(init) > socialTopN {
		init = init[:socialTopN]
	}
	st.Initiators = init

	st.MyPhrases = extractPhrases(myTexts, 12)
	st.TheirPhrases = phrasesFromProfiles(profiles, 12)
	return st, nil
}

// ---------- 口头禅 ----------

// 只过滤纯功能词，"哈哈""好的"这种恰恰是口头禅本体，不能滤掉
var phraseStopwords = map[string]bool{
	"我们": true, "你们": true, "他们": true, "这个": true, "那个": true,
	"什么": true, "怎么": true, "可以": true, "没有": true, "不是": true,
	"就是": true, "但是": true, "因为": true, "所以": true, "如果": true,
	"这样": true, "那样": true, "时候": true, "自己": true, "已经": true,
	"还是": true, "然后": true, "其实": true, "一个": true, "的话": true,
	"知道": true, "觉得": true, "现在": true, "今天": true, "明天": true,
	"昨天": true, "这里": true, "那里": true, "为什么": true, "怎么样": true,
}

// cjkRuns 把文本切成连续的汉字/字母数字片段
func cjkRuns(s string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// extractPhrases 从一批文本里统计 2~3 字高频片段（简单 n-gram，不用分词库）
func extractPhrases(texts []string, topN int) []PhraseStat {
	counts := map[string]int{}
	for _, t := range texts {
		for _, run := range cjkRuns(t) {
			rs := []rune(run)
			if len(rs) < 2 {
				continue
			}
			for n := 2; n <= 3 && n <= len(rs); n++ {
				for i := 0; i+n <= len(rs); i++ {
					g := string(rs[i : i+n])
					if phraseStopwords[g] {
						continue
					}
					counts[g]++
				}
			}
		}
	}
	// 去掉被更长片段完全覆盖的短片段（"哈哈" 出现在 "哈哈哈" 里）
	type kv struct {
		k string
		v int
	}
	var arr []kv
	for k, v := range counts {
		if v < 3 {
			continue
		}
		arr = append(arr, kv{k, v})
	}
	sort.Slice(arr, func(i, j int) bool {
		if arr[i].v != arr[j].v {
			return arr[i].v > arr[j].v
		}
		return len([]rune(arr[i].k)) > len([]rune(arr[j].k))
	})
	out := []PhraseStat{}
	for _, e := range arr {
		if len(out) >= topN {
			break
		}
		covered := false
		for _, keep := range out {
			if len([]rune(keep.Phrase)) > len([]rune(e.k)) && strings.Contains(keep.Phrase, e.k) && keep.Count == e.v {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		out = append(out, PhraseStat{Phrase: e.k, Count: e.v})
	}
	return out
}

// phrasesFromProfiles 汇总各联系人画像里的 FrequentPhrases
func phrasesFromProfiles(profileJSONs []string, topN int) []PhraseStat {
	type slim struct {
		CommunicationStyle struct {
			FrequentPhrases []string `json:"frequentPhrases"`
		} `json:"communicationStyle"`
	}
	counts := map[string]int{}
	var order []string
	for _, pj := range profileJSONs {
		var s slim
		if err := json.Unmarshal([]byte(pj), &s); err != nil {
			continue
		}
		for _, p := range s.CommunicationStyle.FrequentPhrases {
			p = strings.TrimSpace(p)
			if p == "" || len([]rune(p)) > 12 {
				continue
			}
			if _, ok := counts[p]; !ok {
				order = append(order, p)
			}
			counts[p]++
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
	out := []PhraseStat{}
	for i, p := range order {
		if i >= topN {
			break
		}
		out = append(out, PhraseStat{Phrase: p, Count: counts[p]})
	}
	return out
}
