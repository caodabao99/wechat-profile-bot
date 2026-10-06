package main

// ═══════════════════════════════════════════════════════════════════════════
// Relationship Risk Center（v6.1 蓝图 §13 · P9）
//
// 定位：不再新建任何分数，而是把系统里**已经存在**的负向信号收敛成一张可解释的
// 风险清单。七种风险类型（§13.1）全部来自既有产出：
//   - relationship_cooling  ← ListRelationshipStates 的 DynamicState（cooling/at_risk）
//   - long_silence          ← ListRelationshipStates 的 DynamicState（dormant）
//   - interaction_imbalance  ← relationship_daily_metrics 近窗我方占比（我一头热）
//   - project_overdue        ← ListProjects(open) 的 next_action_due/target_date 逾期
//   - followup_overdue       ← ListFollowups(open) 的 due_date 逾期
//   - fact_conflict          ← profile_fact_evidence evidence_type='conflict'（§7.5）
//   - important_date_missed  ← buildCalendarEvents 近窗内已过的生日/纪念日（INFERENCE）
//
// 设计红线（对齐协作约束与 §13.2 可解释要求）：
//   - 顺序调用各「自锁」顶层函数取数，绝不嵌套 dbMu；本模块自身的直读单独自锁。
//   - severity 是**分类**（high/medium/low），非新分数——由既有 alert/逾期天数/占比推得。
//   - AI 分层：硬数据事实（逾期/待办）标 FACT；行为推断（降温/失衡/错过/冲突）标
//     INFERENCE——「可能错过」「一头热」只是提示，绝不臆断为事实、不宣称因果。
//   - value-added 表缺失时该源优雅跳过，绝不让整张风险清单 500。
//   - 派生数据按需计算、不落新表（与 Decision Engine 同一取舍）。
// ═══════════════════════════════════════════════════════════════════════════

import (
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// 七种风险类型（§13.1 原样）。
const (
	riskCooling         = "relationship_cooling"
	riskProjectOverdue  = "project_overdue"
	riskFollowupOverdue = "followup_overdue"
	riskFactConflict    = "fact_conflict"
	riskImbalance       = "interaction_imbalance"
	riskLongSilence     = "long_silence"
	riskDateMissed      = "important_date_missed"
)

// 阈值（确定性常量；imbalance 需足够样本才判，避免少量噪声误报）。
const (
	riskImbalanceWindowDays = 90  // 失衡判定读近 90 天我方/对方占比
	riskImbalanceMinTotal   = 8   // 近窗总互动低于此不判失衡（样本不足）
	riskImbalanceMeRatio    = 0.8 // 我方占比 ≥ 此视为「一头热」
	riskDateMissedLookDays  = 7   // 重要日子已过且在此回溯窗内 → 提示可能错过
	riskOverdueHighDays     = 7   // 逾期 ≥ 此天数升级为 high
)

// RiskItem 一条风险（§13.2：数据来源 / 最近变化 / 涉及联系人 / 建议行动 均可解释）。
type RiskItem struct {
	ContactID       int64  `json:"contactId"`
	ContactName     string `json:"contactName"`
	Type            string `json:"type"`
	Severity        string `json:"severity"` // high|medium|low（分类，非分数）
	Layer           string `json:"layer"`    // FACT|INFERENCE
	Title           string `json:"title"`
	DataOrigin      string `json:"dataOrigin"`      // 数据来源
	RecentChange    string `json:"recentChange"`    // 最近变化
	SuggestedAction string `json:"suggestedAction"` // 建议行动
}

// riskSeverityRank 用于确定性排序：高→低。
func riskSeverityRank(sev string) int {
	switch sev {
	case "high":
		return 0
	case "medium":
		return 1
	default:
		return 2
	}
}

// riskDaysUntil 解析 YYYY-MM-DD 相对 now 的符号天数（过去为负）；空/非法返回 ok=false。
// 与 decision.go 的 daysUntilDate 区别：显式把「无效」与「昨天(-1)」分开，避免逾期误判。
// 两边都归一到 UTC 零点的日历日，使天差为整天、不受运行机时区偏移影响。
func riskDaysUntil(now time.Time, dateStr string) (int, bool) {
	d := first8Date(dateStr)
	if d == "" {
		return 0, false
	}
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return 0, false
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return int(t.Sub(today).Hours() / 24), true
}

// imbalanceRow 近窗我方/对方互动计数（内部）。
type imbalanceRow struct {
	contactID int64
	name      string
	me, other int
}

// imbalancedContactsLocked 自锁读近窗关系日聚合，返回「我方一头热」的联系人计数行。
// 表为空时先就地播种（ensureDailyMetricsSeededLocked 已归档感知，全局门槛复用）。
func imbalancedContactsLocked(db *sql.DB, now time.Time) ([]imbalanceRow, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	ensureDailyMetricsSeededLocked(db)
	from := now.AddDate(0, 0, -riskImbalanceWindowDays).Format("2006-01-02")
	rows, err := db.Query(
		`SELECT rdm.contact_id, COALESCE(c.name,''), COALESCE(SUM(rdm.me_count),0), COALESCE(SUM(rdm.other_count),0)
		 FROM relationship_daily_metrics rdm LEFT JOIN contacts c ON c.id=rdm.contact_id
		 WHERE rdm.day >= ?
		 GROUP BY rdm.contact_id`, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []imbalanceRow{}
	for rows.Next() {
		var r imbalanceRow
		if err := rows.Scan(&r.contactID, &r.name, &r.me, &r.other); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// factConflictRow 一条挂了矛盾证据的事实（内部）。
type factConflictRow struct {
	contactID int64
	name      string
	factType  string
	factValue string
}

// factConflictsLocked 自锁跨全体联系人读取「存在矛盾证据」的当前事实（§7.5 evidence_type=conflict）。
// profile_fact_evidence 表缺失时静默返回空（不 500）。一个联系人只保留首条作展示明细。
func factConflictsLocked(db *sql.DB) ([]factConflictRow, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "profile_fact_evidence") || !tableExistsLocked(db, "profile_facts") {
		return nil, nil
	}
	rows, err := db.Query(
		`SELECT DISTINCT pf.contact_id, COALESCE(c.name,''), pf.fact_type, pf.fact_value
		 FROM profile_facts pf
		 JOIN profile_fact_evidence pfe ON pfe.fact_id = pf.id AND pfe.evidence_type = ?
		 LEFT JOIN contacts c ON c.id = pf.contact_id
		 WHERE pf.status NOT IN ('retired','superseded','rejected')
		 ORDER BY pf.contact_id ASC`, EvConflict)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []factConflictRow{}
	seen := map[int64]bool{}
	for rows.Next() {
		var r factConflictRow
		if err := rows.Scan(&r.contactID, &r.name, &r.factType, &r.factValue); err != nil {
			return nil, err
		}
		if seen[r.contactID] {
			continue // 每人一条明细即可，去重防刷屏
		}
		seen[r.contactID] = true
		out = append(out, r)
	}
	return out, rows.Err()
}

// BuildRisks 汇聚既有产出，生成可解释的关系风险清单（§13.1 七类），按 severity→contact_id→type 排序。
// 顺序调用自锁顶层函数，绝不嵌套 dbMu。now 注入便于确定性单测。
func BuildRisks(db *sql.DB, now time.Time) ([]RiskItem, error) {
	out := []RiskItem{}

	// 懒建表防御：followup_items 非 migrate 建表，某些环境未初始化时待办源应能优雅跳过
	// （与 Decision Engine 同一处理；ensureFollowupTables 幂等、自锁，此处不持外层锁）。
	if err := ensureFollowupTables(db); err != nil {
		return nil, err
	}

	// 1&2) 关系降温 / 长期沉默：来自状态看板 DynamicState。
	states, err := ListRelationshipStates(db)
	if err != nil {
		return nil, err
	}
	for _, st := range states {
		switch st.DynamicState {
		case dynAtRisk, dynCooling:
			sev := "medium"
			if st.DynamicState == dynAtRisk || st.Alert == "urgent" {
				sev = "high"
			}
			out = append(out, RiskItem{
				ContactID: st.ContactID, ContactName: st.Name, Type: riskCooling,
				Severity: sev, Layer: "INFERENCE", Title: "关系正在降温",
				DataOrigin:      "关系状态看板（Health/State）",
				RecentChange:    firstNonEmpty(st.Reason, fmt.Sprintf("动态状态=%s，亲密度=%d", st.DynamicState, st.Intimacy)),
				SuggestedAction: "主动发起一次轻量触达（问候/分享近况），别再等对方先开口",
			})
		case dynDormant:
			out = append(out, RiskItem{
				ContactID: st.ContactID, ContactName: st.Name, Type: riskLongSilence,
				Severity: "medium", Layer: "INFERENCE", Title: "长期无互动",
				DataOrigin:      "关系状态看板（Health/State）",
				RecentChange:    firstNonEmpty(st.Reason, "动态状态=dormant，近期无互动记录"),
				SuggestedAction: "挑一个由头重启联系（节日问候/共同话题），观察是否回升",
			})
		}
	}

	// 3) 互动失衡（我一头热）：近窗我方占比过高。
	imb, err := imbalancedContactsLocked(db, now)
	if err != nil {
		return nil, err
	}
	for _, r := range imb {
		total := r.me + r.other
		if total < riskImbalanceMinTotal {
			continue // 样本不足不判
		}
		ratio := float64(r.me) / float64(total)
		if ratio < riskImbalanceMeRatio {
			continue
		}
		out = append(out, RiskItem{
			ContactID: r.contactID, ContactName: r.name, Type: riskImbalance,
			Severity: "low", Layer: "INFERENCE", Title: "互动失衡：多为主动",
			DataOrigin:      fmt.Sprintf("关系日聚合（近 %d 天）", riskImbalanceWindowDays),
			RecentChange:    fmt.Sprintf("近窗我方发起 %d 条、对方 %d 条，我方占比 %d%%", r.me, r.other, int(ratio*100+0.5)),
			SuggestedAction: "适当降低主动频率、留出对方发起空间；观察对方是否回应，避免单方面付出",
		})
	}

	// 4) 项目逾期：active/paused 项目的 next_action_due（无则 target_date）已过。
	projects, err := ListProjects(db, 0, "open")
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		due := p.NextActionDue
		if due == "" {
			due = p.TargetDate
		}
		days, ok := riskDaysUntil(now, due)
		if !ok || days >= 0 {
			continue
		}
		out = append(out, RiskItem{
			ContactID: p.ContactID, ContactName: p.Name, Type: riskProjectOverdue,
			Severity: overdueSeverity(-days), Layer: "FACT", Title: "关系项目已逾期",
			DataOrigin:      "relationship_projects（活跃/暂停）",
			RecentChange:    fmt.Sprintf("项目「%s」计划 %s 到期，已逾期 %d 天", p.Title, due, -days),
			SuggestedAction: firstNonEmpty("推进下一步："+p.NextAction, "重新评估并推进该项目"),
		})
	}

	// 5) 待办逾期：open 跟进项 due_date 已过。
	followups, err := ListFollowups(db, "open", 0)
	if err != nil {
		return nil, err
	}
	for _, f := range followups {
		days, ok := riskDaysUntil(now, f.DueDate)
		if !ok || days >= 0 {
			continue
		}
		out = append(out, RiskItem{
			ContactID: f.ContactID, ContactName: f.Name, Type: riskFollowupOverdue,
			Severity: overdueSeverity(-days), Layer: "FACT", Title: "跟进/承诺已逾期",
			DataOrigin:      "followup_items（open）",
			RecentChange:    fmt.Sprintf("「%s」截止 %s，已逾期 %d 天", f.Content, f.DueDate, -days),
			SuggestedAction: "尽快兑现或回复该承诺；如已处理请标记完成",
		})
	}

	// 6) 事实冲突：挂了矛盾证据的当前事实（§7.5，视图级派生，不改生命周期）。
	conflicts, err := factConflictsLocked(db)
	if err != nil {
		return nil, err
	}
	for _, c := range conflicts {
		out = append(out, RiskItem{
			ContactID: c.contactID, ContactName: c.name, Type: riskFactConflict,
			Severity: "low", Layer: "INFERENCE", Title: "画像事实存在矛盾证据",
			DataOrigin:      "profile_fact_evidence（evidence_type=conflict）",
			RecentChange:    fmt.Sprintf("当前事实「%s=%s」出现与之矛盾的对话线索", c.factType, c.factValue),
			SuggestedAction: "核对该事实是否已变化（去 Memory Review 确认/更新），别让画像基于过期信息",
		})
	}

	// 7) 重要日子可能错过：回溯窗内已过的生日/纪念日（不臆断「一定错过」，只提示确认）。
	past, perr := buildCalendarEvents(db, now.AddDate(0, 0, -riskDateMissedLookDays), now)
	if perr != nil {
		return nil, perr
	}
	for _, ev := range past {
		if ev.Kind != "birthday" && ev.Kind != "anniversary" {
			continue
		}
		days, ok := riskDaysUntil(now, ev.Date)
		if !ok || days >= 0 {
			continue
		}
		sev := "medium"
		if -days <= 3 {
			sev = "high" // 刚过最可补救
		}
		out = append(out, RiskItem{
			ContactID: ev.ContactID, ContactName: ev.Name, Type: riskDateMissed,
			Severity: sev, Layer: "INFERENCE", Title: "重要日子刚过（建议确认是否补救）",
			DataOrigin:      "关系维护日历（画像重要日子/生日/纪念日）",
			RecentChange:    fmt.Sprintf("%s 于 %d 天前（%s）", ev.Title, -days, ev.Date),
			SuggestedAction: "若尚未祝贺，补一句问候往往仍受欢迎；并把该日子记入下次提醒",
		})
	}

	// 确定性排序：severity 高→低 → contact_id 升 → type 升。
	sort.SliceStable(out, func(i, j int) bool {
		if ri, rj := riskSeverityRank(out[i].Severity), riskSeverityRank(out[j].Severity); ri != rj {
			return ri < rj
		}
		if out[i].ContactID != out[j].ContactID {
			return out[i].ContactID < out[j].ContactID
		}
		return out[i].Type < out[j].Type
	})
	return out, nil
}

// overdueSeverity 逾期天数 → 分类严重度（分类，非分数）。
func overdueSeverity(overdueDays int) string {
	if overdueDays >= riskOverdueHighDays {
		return "high"
	}
	return "medium"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// CountRisksByType 供首页「N 个关系风险」摘要（§14.2）与分层展示复用。
func CountRisksByType(items []RiskItem) map[string]int {
	m := map[string]int{}
	for _, it := range items {
		m[it.Type]++
	}
	return m
}
