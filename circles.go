package main

// v5.3.0 #6：智能关系圈层管理（零 LLM、纯确定性、可离线单测）。
//
// 把联系人按「互动频率 + 亲密度」自动分入四层圈层（核心/亲密/社交/弱联系），每层配一套
//   差异化维护打法（节奏 cadence + 策略文案）。与 v5.0.0 tagsuggest 打标签是不同视角——
//   这里产出的是「分层视图 + 分层维护 playbook」，不重复写标签逻辑。
//
// 设计铁律：
//   - 增量复用：亲密度与活跃天数直接取 computeIntimacy(90)（Days=窗口内有互动的自然日数）；
//     圈子归属标签读 GetCachedNetwork（只读、缺缓存即省略该属性，绝不触发网络重算）。
//   - 单连接池分层锁：computeIntimacy 与 GetCachedNetwork 各自取锁、在其外调用；本函数
//     的联系人全集取数单独一趟锁，绝不嵌套。
//   - 计算即读、不落库、不新增表；规模护栏复用 healthMaxContacts。

import (
	"database/sql"
	"sort"
	"time"
)

// 圈层固定顺序与元信息。
const (
	circleCore     = "core"
	circleIntimate = "intimate"
	circleSocial   = "social"
	circleWeak     = "weak"
)

type circleMetaDef struct {
	Key         string
	Label       string
	Strategy    string
	CadenceDays int
}

var circleMetas = []circleMetaDef{
	{circleCore, "核心圈", "每周至少深聊 1 次、优先回复、重要日子必看", 7},
	{circleIntimate, "亲密圈", "每两周问候 1 次、留意降温信号", 14},
	{circleSocial, "社交圈", "每月轻触 1 次、节日送祝福", 30},
	{circleWeak, "弱联系", "季度激活 / 断舍离评估", 90},
}

// CircleTier 一个圈层的汇总（含维护打法）。
type CircleTier struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Strategy    string `json:"strategy"`
	CadenceDays int    `json:"cadenceDays"`
	Count       int    `json:"count"`
}

// CircleMember 一个联系人所属圈层。
type CircleMember struct {
	ContactID  int64  `json:"contactId"`
	Name       string `json:"name"`
	Tier       string `json:"tier"`
	Score      int    `json:"score"`
	ActiveDays int    `json:"activeDays"`
	Cluster    string `json:"cluster,omitempty"` // 命中的网络圈子名（无则省略）
}

// CircleDashboard 圈层响应体。
type CircleDashboard struct {
	GeneratedAt string         `json:"generatedAt"`
	WindowDays  int            `json:"windowDays"`
	Truncated   bool           `json:"truncated"`
	Tiers       []CircleTier   `json:"tiers"`
	Members     []CircleMember `json:"members"`
}

// assignTier 纯函数：亲密度 + 活跃天数 → 圈层键（确定性、可脱库单测）。
//   - 核心圈：亲密度≥75 且 活跃天数≥12
//   - 亲密圈：亲密度 40–74（或≥75 但活跃不足）
//   - 社交圈：亲密度 15–39
//   - 弱联系：亲密度 <15
func assignTier(intimacy, activeDays int) string {
	if intimacy >= 75 && activeDays >= 12 {
		return circleCore
	}
	switch {
	case intimacy >= 40:
		return circleIntimate
	case intimacy >= 15:
		return circleSocial
	default:
		return circleWeak
	}
}

// circleRank 圈层排序权重（核心在前）。
func circleRank(key string) int {
	for i, m := range circleMetas {
		if m.Key == key {
			return i
		}
	}
	return len(circleMetas)
}

// ComputeCircles 计算全局圈层分布。
func ComputeCircles(db *sql.DB, now time.Time) (*CircleDashboard, error) {
	// 亲密度 + 活跃天数批量查找表（computeIntimacy 内部自锁）。
	type ii struct {
		score, days int
	}
	intimMap := map[int64]ii{}
	if list, err := computeIntimacy(db, now, healthWindowDays); err == nil {
		for _, it := range list {
			intimMap[it.ContactID] = ii{it.Score, it.Days}
		}
	}

	// 可选：网络圈子归属（只读缓存，缺则整列留空）。
	clusterOf := map[int64]string{}
	if net, _, err := GetCachedNetwork(db); err == nil && net != nil {
		for _, cl := range net.Clusters {
			for _, mid := range cl.MemberIDs {
				if _, dup := clusterOf[mid]; !dup {
					clusterOf[mid] = cl.Label
				}
			}
		}
	}

	// 联系人全集（未合并），规模护栏。
	type contactRow struct {
		id     int64
		name   string
		remark string
	}
	var universe []contactRow
	truncated := false
	dbMu.Lock()
	if rows, err := db.Query(
		`SELECT id, COALESCE(name,''), COALESCE(remark,'') FROM contacts
		 WHERE merged_into IS NULL ORDER BY id ASC LIMIT ?`, healthMaxContacts+1); err == nil {
		for rows.Next() {
			var c contactRow
			if rows.Scan(&c.id, &c.name, &c.remark) == nil {
				universe = append(universe, c)
			}
		}
		rows.Close()
	}
	dbMu.Unlock()
	if len(universe) > healthMaxContacts {
		universe = universe[:healthMaxContacts]
		truncated = true
	}

	counts := map[string]int{}
	members := make([]CircleMember, 0, len(universe))
	for _, c := range universe {
		v := intimMap[c.id]
		tier := assignTier(v.score, v.days)
		counts[tier]++
		members = append(members, CircleMember{
			ContactID:  c.id,
			Name:       displayName(&Contact{ID: c.id, Name: c.name, Remark: c.remark}),
			Tier:       tier,
			Score:      v.score,
			ActiveDays: v.days,
			Cluster:    clusterOf[c.id],
		})
	}

	// 排序：圈层靠前 → 亲密度降序 → contactId 升序（确定性）。
	sort.SliceStable(members, func(i, j int) bool {
		ri, rj := circleRank(members[i].Tier), circleRank(members[j].Tier)
		if ri != rj {
			return ri < rj
		}
		if members[i].Score != members[j].Score {
			return members[i].Score > members[j].Score
		}
		return members[i].ContactID < members[j].ContactID
	})

	tiers := make([]CircleTier, 0, len(circleMetas))
	for _, m := range circleMetas {
		tiers = append(tiers, CircleTier{Key: m.Key, Label: m.Label, Strategy: m.Strategy, CadenceDays: m.CadenceDays, Count: counts[m.Key]})
	}

	return &CircleDashboard{
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
		WindowDays:  healthWindowDays,
		Truncated:   truncated,
		Tiers:       tiers,
		Members:     members,
	}, nil
}
