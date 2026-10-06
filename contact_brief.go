package main

// 蓝图 §14 P2：Contact Brief（联系前简报）。
//
// 铁律——不新建第二套 Brief：联系前简报直接复用 Phase 5 的 Context Engine 编排
// （projectBrief + projectStrategy），只在其之上补两项 §14 明确要求、而 Session 未单列的要点：
//   - 最近谈过什么（RecentTopics）
//   - 避免事项（AvoidItems，确定性推导，绝不臆测）
//
// §14 纪律：优先确定性数据；LLM 只做语言整理；即使 LLM 不可用，确定性版本也必须正常工作。
// 因此本简报是**纯确定性、零 LLM 依赖**的一页——无 LLM 也永远 200。

import (
	"database/sql"
	"net/http"
	"time"
)

// ContactBrief 联系前简报：一页看清「当前状态 / 变化 / 事实 / 冲突 / 谈过什么 / 项目 / 目标 /
// 未完成 / 风险 / 机会 / 推荐策略 / 避免事项」。全部来自 Context Engine（单一真相）。
type ContactBrief struct {
	ContactID      int64           `json:"contact_id"`
	Name           string          `json:"name"`
	Now            string          `json:"now"`
	ContextVersion string          `json:"context_version"`
	Layer          string          `json:"layer"` // 恒为 DETERMINISTIC：不依赖 LLM
	Brief          BeforeBrief     `json:"brief"`
	Strategy       SessionStrategy `json:"strategy"`
	RecentTopics   []string        `json:"recent_topics"` // 最近谈过什么
	AvoidItems     []string        `json:"avoid_items"`   // 避免事项（确定性）
}

// BuildContactBrief 组装联系前简报。联系人不存在时 BuildContactContext 返回错误 → 调用方转 404。
func BuildContactBrief(db *sql.DB, contactID int64, now time.Time) (*ContactBrief, error) {
	cc, err := BuildContactContext(db, contactID, TaskDecision, "", now)
	if err != nil {
		return nil, err
	}
	return &ContactBrief{
		ContactID:      contactID,
		Name:           cc.Identity.Name,
		Now:            now.Format(time.RFC3339),
		ContextVersion: cc.ContextVersion,
		Layer:          "DETERMINISTIC",
		Brief:          projectBrief(cc, now),
		Strategy:       projectStrategy(db, cc, contactID, now),
		RecentTopics:   recentTopicNames(cc),
		AvoidItems:     deterministicAvoidItems(cc),
	}, nil
}

// recentTopicNames 取最近一周的主题名（老→新，故取末周）。无主题历史则空。
func recentTopicNames(cc *ContactContext) []string {
	out := []string{}
	if cc == nil || cc.RecentTopics == nil || len(cc.RecentTopics.Weeks) == 0 {
		return out
	}
	latest := cc.RecentTopics.Weeks[len(cc.RecentTopics.Weeks)-1]
	for _, t := range latest.Topics {
		if t.Name != "" {
			out = append(out, t.Name)
		}
	}
	return out
}

// deterministicAvoidItems 由关系状态/冲突事实确定性推出「此刻应避免什么」。措辞保守、绝不因果断言；
// 没有任何可依据信号时返回空——诚实留白，绝不为凑数编造。
func deterministicAvoidItems(cc *ContactContext) []string {
	out := []string{}
	if cc == nil || cc.CurrentRelationshipState == nil {
		return out
	}
	st := cc.CurrentRelationshipState
	switch st.DynamicState {
	case dynCooling, dynAtRisk:
		out = append(out, "避免频繁追问或施压（关系正在降温/流失，先降压力）")
	case dynDormant:
		out = append(out, "避免一次性群发式打扰（长期断联，宜轻量、单点重启）")
	}
	if st.Alert == "urgent" || st.Alert == "watching" {
		out = append(out, "避免在此刻推进敏感或索取型话题（风险偏高，先修复信任）")
	}
	if len(cc.ConflictingFacts) > 0 {
		out = append(out, "避免把未澄清的冲突事实当作定论（先确认再引用）")
	}
	if st.DynamicState == dynWarming || st.DynamicState == dynReconnecting {
		out = append(out, "避免提旧账或负面话题打断正在回暖的势头")
	}
	return out
}

// hContactBrief GET /api/contacts/{id}/brief → 联系前简报（纯确定性，无 LLM 也 200）。
func (s *apiServer) hContactBrief(w http.ResponseWriter, r *http.Request, id int64) {
	brief, err := BuildContactBrief(s.db, id, time.Now())
	if err != nil {
		writeErr(w, http.StatusNotFound, "联系人不存在或简报构建失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "brief": brief})
}
