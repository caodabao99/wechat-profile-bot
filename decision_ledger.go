package main

// Decision→Action→Outcome 闭环（蓝图 §6 P2）：把 Decision Engine 产出的「今天值得做」候选
// 落到 Action Ledger，用 decision_fingerprint 去重，并让 [接受/稍后/忽略/已完成] 复用 §5 生命周期。
//
//   - §6.1 防重复：指纹 = hash(contact + action_type + sorted(reason_codes) + context_version + time_window)。
//     相同指纹（DB 级部分唯一索引）不得重复生成 → 同一份认知快照下同一决策只落一条账本记录。
//   - §6.2 Deferred：deferred_until 到期后重新进入候选。
//   - §6.3 Dismissed：记录 dismiss_reason，只屏蔽当前窗口（time_window 换 → 新指纹 → 自然重入），绝不永久屏蔽。
//
// 指纹含 context_version，故「稳定即去重、变化即重生成」：同一天画像/状态/事实不变则重复首页不会堆积
// 记录；一旦关系数据推进（context_version 变），即视为新决策合法再生成。这与 §4.2 的 AI cache 失效同源。
//
// 锁纪律：与 action_log.go 一致——自锁、单层 dbMu、持锁内先查表存在再操作，绝不嵌套。

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// decisionFingerprint 计算 §6.1 防重复指纹。reason_codes 先排序保证与顺序无关、确定性可复现。
func decisionFingerprint(contactID int64, actionType string, reasonCodes []string, contextVersion, windowKey string) string {
	codes := append([]string(nil), reasonCodes...)
	sort.Strings(codes)
	var b strings.Builder
	sep := "\x1f"
	b.WriteString("decfp1")
	b.WriteString(sep)
	b.WriteString(strconv.FormatInt(contactID, 10))
	b.WriteString(sep)
	b.WriteString(actionType)
	b.WriteString(sep)
	b.WriteString(strings.Join(codes, ","))
	b.WriteString(sep)
	b.WriteString(contextVersion)
	b.WriteString(sep)
	b.WriteString(windowKey)
	return shortHash(b.String())
}

// DecisionWindowKey 返回决策去重的时间窗键。取自然日：同一决策同日内去重；跨日（或 context 变）自然重入。
func DecisionWindowKey(now time.Time) string {
	return now.Format("2006-01-02")
}

// RecordDecision 幂等地把一条决策候选写入行动账本（source=decision）。
// 命中相同指纹则不重复生成，返回既有 id 与 created=false；否则新建（status=generated）返回新 id 与 created=true。
func RecordDecision(db *sql.DB, contactID int64, actionType string, reasonCodes []string, contextVersion, windowKey, actionText string, now time.Time) (id int64, created bool, err error) {
	if contactID <= 0 {
		return 0, false, fmt.Errorf("contact_id 非法")
	}
	fp := decisionFingerprint(contactID, actionType, reasonCodes, contextVersion, windowKey)
	actionText = clipRunes(strings.TrimSpace(actionText), actionTextMaxLen)
	nowStr := now.Format(time.RFC3339)

	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "relationship_action_log") {
		return 0, false, fmt.Errorf("行动账本表尚未就绪")
	}
	res, err := db.Exec(`
		INSERT INTO relationship_action_log
			(contact_id, source, source_ref, action_type, action_text, status, decision_fingerprint, created_at, updated_at)
		VALUES (?, 'decision', ?, ?, ?, 'generated', ?, ?, ?)
		ON CONFLICT DO NOTHING`,
		contactID, windowKey, clipRunes(strings.TrimSpace(actionType), 80), actionText, fp, nowStr, nowStr)
	if err != nil {
		return 0, false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		newID, err := res.LastInsertId()
		if err != nil {
			return 0, false, err
		}
		return newID, true, nil
	}
	// 指纹冲突：既有记录不重复生成，回查其 id。
	var existing int64
	if err := db.QueryRow(`SELECT id FROM relationship_action_log WHERE decision_fingerprint = ?`, fp).Scan(&existing); err != nil {
		return 0, false, err
	}
	return existing, false, nil
}

// GetActionByFingerprint 按指纹查既有决策账本记录（不存在返回 nil,nil）。
func GetActionByFingerprint(db *sql.DB, fingerprint string) (*ActionLogEntry, error) {
	if fingerprint == "" {
		return nil, nil
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "relationship_action_log") {
		return nil, nil
	}
	var e ActionLogEntry
	err := db.QueryRow(`SELECT id, contact_id, source, source_ref, action_type, action_text, status,
		deferred_until, created_at, updated_at, acted_at, outcome, outcome_provenance,
		outcome_observed_at, outcome_days, outcome_note, decision_fingerprint, dismiss_reason
		FROM relationship_action_log WHERE decision_fingerprint = ?`, fingerprint).
		Scan(&e.ID, &e.ContactID, &e.Source, &e.SourceRef, &e.ActionType, &e.ActionText, &e.Status,
			&e.DeferredUntil, &e.CreatedAt, &e.UpdatedAt, &e.ActedAt, &e.Outcome, &e.OutcomeProvenance,
			&e.OutcomeObservedAt, &e.OutcomeDays, &e.OutcomeNote, &e.DecisionFingerp, &e.DismissReason)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// DismissAction 忽略一条决策（§6.3）：置 status=dismissed 并记录 dismiss_reason。
// 只屏蔽当前时间窗（同指纹不再呈现），跨窗自然重入——绝不永久屏蔽，防一次误操作影响未来。
func DismissAction(db *sql.DB, id int64, reason string, now time.Time) error {
	if id <= 0 {
		return fmt.Errorf("行动记录 id 非法")
	}
	reason = clipRunes(strings.TrimSpace(reason), actionTextMaxLen)
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "relationship_action_log") {
		return fmt.Errorf("行动账本表尚未就绪")
	}
	res, err := db.Exec(`UPDATE relationship_action_log SET status='dismissed', dismiss_reason=?, updated_at=? WHERE id=?`,
		reason, now.Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("行动记录不存在")
	}
	return nil
}

// DecisionShouldSurface 判断一条已记录的决策当前是否仍应出现在候选里（供首页过滤）：
//   - generated/viewed/accepted：待处理 → 呈现；
//   - deferred：到期（或无明确到期日）才重新呈现；
//   - dismissed：本窗屏蔽（跨窗由新指纹自然重入）→ 不呈现；
//   - acted/completed/expired：已处理 → 不呈现。
func DecisionShouldSurface(status, deferredUntil string, now time.Time) bool {
	switch status {
	case ActionStatusGenerated, ActionStatusViewed, ActionStatusAccepted:
		return true
	case ActionStatusDeferred:
		if deferredUntil == "" {
			return true
		}
		return now.Format("2006-01-02") >= deferredUntil
	case ActionStatusDismissed, ActionStatusActed, ActionStatusCompleted, ActionStatusExpired:
		return false
	default:
		return true
	}
}

// decisionActionType 从候选的 reason codes 确定性导出一个 action_type（排序后取首，与顺序无关）。
func decisionActionType(codes []string) string {
	s := append([]string(nil), codes...)
	sort.Strings(s)
	if len(s) == 0 {
		return "maintain"
	}
	return s[0]
}

// decisionContextVersionForFingerprint 取一个「不含账本」的认知版本用于指纹：记录决策自身会写
// relationship_action_log，若指纹含这部分则每次首页都会使 context_version 前进→新指纹→再生成→行
// 数无限增长。故此处把 ActionLog 置空后重算，使指纹只随真实关系输入变化、不随自家记录变化。
func decisionContextVersionForFingerprint(cc *ContactContext) string {
	if cc == nil {
		return ""
	}
	cc2 := *cc
	cc2.ActionLog = nil
	cc2.ContextVersion = ""
	return computeContextVersion(&cc2)
}

// SurfaceDecisions 产出「今天值得做」并打通行动账本（§6 闭环）：
//  1. 取全量候选（BuildDecisionCandidates 已确定性排序）；
//  2. 逐个幂等写入 relationship_action_log（同指纹不重复生成）；
//  3. 过滤掉已处理（dismissed/acted/completed/expired 或 deferred 未到期）的；
//  4. 取前 topN，回填 action_log_id/指纹/生命周期状态，并补最佳触达时段。
//
// 这样首页每项都带一个可操作的账本 id：[接受]→transition accepted、[稍后]→deferred+until、
// [忽略]→dismiss+reason、[已完成]→completed；结果由 §5.4 自动观察或用户确认回填。
func SurfaceDecisions(db *sql.DB, now time.Time, topN int) ([]DecisionCandidate, error) {
	if topN <= 0 || topN > 20 {
		topN = 3
	}
	cands, err := BuildDecisionCandidates(db, now)
	if err != nil {
		return nil, err
	}
	windowKey := DecisionWindowKey(now)
	out := []DecisionCandidate{}
	for _, c := range cands {
		cc, err := BuildContactContext(db, c.ContactID, TaskDecision, "", now)
		if err != nil {
			continue // 单条上下文构建失败不阻断整体
		}
		cv := decisionContextVersionForFingerprint(cc)
		at := decisionActionType(c.ReasonCodes)
		id, _, err := RecordDecision(db, c.ContactID, at, c.ReasonCodes, cv, windowKey, c.Action, now)
		if err != nil {
			continue
		}
		fp := decisionFingerprint(c.ContactID, at, c.ReasonCodes, cv, windowKey)
		if e, err := GetActionByFingerprint(db, fp); err == nil && e != nil {
			if !DecisionShouldSurface(e.Status, e.DeferredUntil, now) {
				continue // 已处理 / 本窗屏蔽 / 稍后未到期 → 不再呈现
			}
			c.ActionLogID, c.DecisionFingerprint, c.LedgerStatus = e.ID, fp, e.Status
		} else {
			c.ActionLogID, c.DecisionFingerprint, c.LedgerStatus = id, fp, ActionStatusGenerated
		}
		out = append(out, c)
		if len(out) >= topN {
			break
		}
	}
	for i := range out {
		if m, err := GetAggregatedMetrics(db, int(out[i].ContactID), decisionBestTimeWindowDays); err == nil && m != nil {
			out[i].BestTime = peakHourLabel(m.OtherHourHist)
		}
	}
	// §8.3 决策集成：只读地给候选附上策略类型与历史表现折算的 strategy_score（不改 Priority、
	// 不重排）。策略历史读取失败不阻断首页——降级为「无历史参考」，绝不影响确定性推荐本身。
	if hist, err := ComputeStrategyHistory(db, now); err == nil && hist != nil {
		annotateStrategyScore(out, hist.Stats)
	}
	// §9 个性化校准：把用户软权重叠加到 strategy_score（不改 Priority、不重排）。
	applyCalibrationToCandidates(db, out, now)
	return out, nil
}
