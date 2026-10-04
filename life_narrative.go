package main

// 人生模拟器 · Phase C：人生年表（叙事层）。
//
// 设计原则：零操作减负 —— 全部里程碑由 SQL 聚合自动派生，不要求用户记录、不调模型。
//   - 起点（第一次对话）、最活跃的一年、最长情的联系人、最近一年的重心、手动记录的大事
//   - 不建缓存表（聚合查询很轻），API 读取时现算；联系人少或数据稀疏时优雅降级为空表

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Milestone 一条人生里程碑。
type Milestone struct {
	Kind        string `json:"kind"` // start / peak_year / loyal / recent_top / event
	Title       string `json:"title"`
	Detail      string `json:"detail"`
	Date        string `json:"date"` // YYYY-MM-DD 或 YYYY
	ContactName string `json:"contactName,omitempty"`
}

// LifeNarrative 人生年表快照。
type LifeNarrative struct {
	GeneratedAt  string      `json:"generatedAt"`
	YearsCovered int         `json:"yearsCovered"`
	TotalMsgs    int         `json:"totalMsgs"`
	Milestones   []Milestone `json:"milestones"`
}

// BuildLifeTimeline 聚合出人生年表。
func BuildLifeTimeline(db *sql.DB, now time.Time) (*LifeNarrative, error) {
	nar := &LifeNarrative{GeneratedAt: now.Format("2006-01-02 15:04:05")}

	// 先在锁外取缓存资产（GetCachedLifeState 内部自取锁，避免与下面的持锁段嵌套死锁）
	assets, _ := loadLifeAssetsNoRecompute(db)

	dbMu.Lock()
	defer dbMu.Unlock()

	// 1) 按年聚合：总数、每年消息数、覆盖年份
	yearCount := map[string]int{}
	totalMsgs := 0
	if rows, err := db.Query(`
		SELECT substr(msg_time,1,4) y, COUNT(*) FROM messages
		WHERE msg_time IS NOT NULL AND msg_time != '' GROUP BY y`); err == nil {
		for rows.Next() {
			var y string
			var c int
			if rows.Scan(&y, &c) == nil && len(y) == 4 {
				yearCount[y] = c
				totalMsgs += c
			}
		}
		rows.Close()
	}
	nar.TotalMsgs = totalMsgs
	nar.YearsCovered = len(yearCount)

	// 名字查找表
	names := map[int64]string{}
	if rows, err := db.Query(`SELECT id, COALESCE(NULLIF(remark,''), name) FROM contacts`); err == nil {
		for rows.Next() {
			var id int64
			var nm string
			if rows.Scan(&id, &nm) == nil {
				names[id] = nm
			}
		}
		rows.Close()
	}

	// 2) 起点：最早一条消息
	var firstTime, firstSender string
	var firstCID int64
	_ = db.QueryRow(`
		SELECT contact_id, sender, msg_time FROM messages
		WHERE msg_time IS NOT NULL AND msg_time != ''
		ORDER BY msg_time ASC, id ASC LIMIT 1`).Scan(&firstCID, &firstSender, &firstTime)
	if firstTime != "" {
		who := names[firstCID]
		title := "一切的起点"
		detail := fmt.Sprintf("%s 你们说出了第一句话", dateCn(firstTime))
		if who != "" {
			detail = fmt.Sprintf("%s 你和 %s 有了第一句对话", dateCn(firstTime), who)
		}
		nar.Milestones = append(nar.Milestones, Milestone{
			Kind: "start", Title: title, Detail: detail, Date: normDate(firstTime), ContactName: who,
		})
	}

	// 3) 最活跃的一年
	if totalMsgs > 0 {
		bestYear, bestCnt := "", 0
		for y, c := range yearCount {
			if c > bestCnt {
				bestYear, bestCnt = y, c
			}
		}
		if bestYear != "" {
			nar.Milestones = append(nar.Milestones, Milestone{
				Kind:   "peak_year",
				Title:  bestYear + " 年，你们聊得最多",
				Detail: fmt.Sprintf("那一年共 %d 条往来，是你社交最活跃的一年", bestCnt),
				Date:   bestYear,
			})
		}
	}

	// 4) 最长情：首条最早、且横跨年份最多的联系人
	type loyalRow struct {
		cid    int64
		firstY string
		spans  int
	}
	var cands []loyalRow
	if rows, err := db.Query(`
		SELECT contact_id, substr(MIN(msg_time),1,4) fy,
			COUNT(DISTINCT substr(msg_time,1,4)) spans
		FROM messages WHERE msg_time IS NOT NULL AND msg_time != ''
		GROUP BY contact_id`); err == nil {
		for rows.Next() {
			var lr loyalRow
			var fy sql.NullString
			if rows.Scan(&lr.cid, &fy, &lr.spans) == nil {
				lr.firstY = fy.String
				cands = append(cands, lr)
			}
		}
		rows.Close()
	}
	// 优先跨年最多，其次最早
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].spans != cands[j].spans {
			return cands[i].spans > cands[j].spans
		}
		return cands[i].firstY < cands[j].firstY
	})
	for _, c := range cands {
		if c.spans >= 2 && c.firstY != "" {
			who := names[c.cid]
			if who == "" {
				break
			}
			nar.Milestones = append(nar.Milestones, Milestone{
				Kind:        "loyal",
				Title:       fmt.Sprintf("和 %s 从 %s 年一路走到现在", who, c.firstY),
				Detail:      fmt.Sprintf("你们的对话跨越了 %d 个年头，是最长情的一段关系", c.spans),
				Date:        c.firstY,
				ContactName: who,
			})
			break
		}
	}

	// 5) 最近一年的重心：读人生状态里余额最高的联系人（缓存缺失则跳过，不报错）
	if len(assets) > 0 {
		top := assets[0]
		for _, a := range assets {
			if a.Balance > top.Balance {
				top = a
			}
		}
		if top.Name != "" && top.Balance > 0 {
			nar.Milestones = append(nar.Milestones, Milestone{
				Kind:        "recent_top",
				Title:       fmt.Sprintf("最近，%s 是你最在意的人", top.Name),
				Detail:      fmt.Sprintf("近半年亲密度 %d 分，互动最紧密", top.Balance),
				Date:        now.Format("2006-01-02"),
				ContactName: top.Name,
			})
		}
	}

	// 6) 手动记录的大事（用户此前在时间线补记的），取最近 5 条
	if rows, err := db.Query(`
		SELECT contact_id, title, event_time FROM contact_events
		ORDER BY event_time DESC LIMIT 5`); err == nil {
		for rows.Next() {
			var cid int64
			var title, et string
			if rows.Scan(&cid, &title, &et) == nil {
				who := names[cid]
				detail := title
				if who != "" {
					detail = who + "：" + title
				}
				nar.Milestones = append(nar.Milestones, Milestone{
					Kind: "event", Title: "你记下的一件事", Detail: detail,
					Date: normDate(et), ContactName: who,
				})
			}
		}
		rows.Close()
	}

	return nar, nil
}

// loadLifeAssetsNoRecompute 只读缓存里的资产清单，不触发现算（避免叙事层递归重算）。
func loadLifeAssetsNoRecompute(db *sql.DB) ([]LifeAsset, error) {
	st, _, err := GetCachedLifeState(db)
	if err != nil || st == nil {
		return nil, err
	}
	return st.Assets, nil
}

// normDate 把消息/事件时间规整成 YYYY-MM-DD。
func normDate(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// dateCn 把 YYYY-MM-DD 显示为中文年月日。
func dateCn(s string) string {
	if len(s) < 10 {
		return s
	}
	return fmt.Sprintf("%s 年 %s 月 %s 日", s[:4], trimLead(s[5:7]), trimLead(s[8:10]))
}

func trimLead(x string) string {
	x = strings.TrimLeft(x, "0")
	if x == "" {
		return "0"
	}
	return x
}
