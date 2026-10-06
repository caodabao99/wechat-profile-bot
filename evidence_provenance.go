package main

import "strings"

// Evidence Provenance 2.0（蓝图 §7 P3）。
//
// 目标：证据不能只靠 fact_value LIKE message_content 一刀切。为每条命中消息给出可解释、
// 可核对的「证据类型」，并据此决定它是否、以及在多大程度上提高事实置信度。既有
// profile_facts / profile_fact_evidence 全部保留，本文件只做分类，不改数据模型（除新增
// evidence_type 一列外）。分类完全确定性、不引入新 LLM 调用。
//
// §7.1 六档证据类型：
const (
	EvDirect        = "direct"         // §7.2 明确第一人称陈述的事实本身
	EvStrongContext = "strong_context" // §7.3 强语境（计划/传闻/过去），不足以断言为事实
	EvWeakContext   = "weak_context"   // 弱相关（无主语线索的零散命中）
	EvTopicRelated  = "topic_related"  // §7.4 纯关键词/他人话题，禁止据此提高 confidence
	EvConflict      = "conflict"       // §7.5 与该事实相矛盾的证据
	EvInsufficient  = "insufficient"   // 无任何支撑证据（事实级，不落证据行）
)

// identityFactTypes 是需要「第一人称断言」才算 direct 的单值型身份事实（职业/城市/亲密度）。
// 其余（兴趣/性格/口头禅/重要事实…）为集合型：本人范围内的字面提及即视为确有该特征。
var identityFactTypes = map[string]bool{"occupation": true, "location": true, "closeness": true}

// 语气/线索标记（确定性关键词表，可后续扩充；命中即影响分类）。
var (
	evHedge    = []string{"准备", "打算", "考虑", "计划", "可能", "也许", "好像", "听说", "应该", "即将", "下家", "以前", "曾经", "之前", "原来", "过去", "当时"}
	evConflict = []string{"不是", "不再是", "不做", "不干", "不当", "不再", "辞职", "离职", "转行", "改行", "换了", "换到", "搬走", "搬离", "搬出", "离开", "已经没", "早就不"}
	evSelf     = []string{"我", "本人", "自己", "咱", "俺"}
	evThird    = []string{"你", "他", "她", "它", "别人", "人家", "谁", "那个", "这位"}
)

// classifyEvidenceType 依据事实类型与命中消息内容，给出 §7.1 的证据类型。判定优先级：
// 冲突 > 强语境(不确定语气) > 集合型直判 direct > 身份型看主语(我=direct / 他人=topic_related / 无线索=weak_context)。
// 注意：调用方保证 content 已字面包含 factValue（当前召回均为 LIKE %value%）。
func classifyEvidenceType(factType, factValue, content string) string {
	if factValue != "" && !strings.Contains(content, factValue) {
		return EvWeakContext // 字面未命中：保守判弱相关（当前召回路径不会到这）
	}
	// 1) 否定/反转标记与事实值同现 → 冲突（§7.5）
	if containsAny(content, evConflict...) {
		return EvConflict
	}
	// 2) 计划/传闻/过去等不确定语气 → 强语境，不足以据此断言为事实本身（§7.3）
	if containsAny(content, evHedge...) {
		return EvStrongContext
	}
	// 3) 集合型事实（兴趣/性格/口头禅…）：本人范围内提及即视为确有 → direct
	if !identityFactTypes[factType] {
		// 但纯他人话题（无第一人称、明确指向第三者）→ topic_related，不提高置信（§7.4）
		if !containsAny(content, evSelf...) && containsAny(content, evThird...) {
			return EvTopicRelated
		}
		return EvDirect
	}
	// 4) 身份型事实（职业/城市/亲密度）：需第一人称断言才算 direct（§7.2）
	if containsAny(content, evSelf...) {
		return EvDirect
	}
	if containsAny(content, evThird...) {
		return EvTopicRelated // 纯关键词 / 他人话题：§7.4 禁止据此提高 confidence
	}
	return EvWeakContext
}

// evidenceConfContrib 返回该证据类型对数值置信度的增量贡献。
// 仅 direct(+0.08) 与 strong_context(+0.03) 提高；topic_related/weak_context/conflict/insufficient 一律 0（§7.4）。
func evidenceConfContrib(evType string) float64 {
	switch evType {
	case EvDirect:
		return 0.08
	case EvStrongContext:
		return 0.03
	default:
		return 0
	}
}
