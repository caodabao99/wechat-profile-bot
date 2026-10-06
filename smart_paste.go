package main

// ═══════════════════════════════════════════════════════════════════════════
// §12 P2：Smart Paste 2.0（PERSONAL RELATIONSHIP OS 3.0）
//
// 蓝图三条硬约束：
//   12.1 粘贴闭环四指标——「输入 = 新增 + 重复 + 异常」，用户看得见这次粘贴到底帮了多少忙；
//   12.2 去重优先消息身份哈希（contact + sender + 时间戳 + 内容），**不能只依赖内容**；
//   12.3 AI 触发分级——普通消息只入库；只有「重要变化」（新事实 / 重大意图 / 关系变化 /
//        项目变化 / 目标变化 / 高风险事件）才惊动 AI。
//
// 收口纪律（不重写已测试的写路径）：
//   · 12.2 已由 messages 表的 UNIQUE(contact_id, msg_hash) 满足——msg_hash =
//     md5(sender[:RFC3339时间戳]:content)，身份维度含 contact、非纯内容。无时间戳时
//     降级为 sender+content 的去重行为被 bugfix_regression 钉死，**本文件只显式化该契约
//     并加测试锁死，绝不改哈希/去重语义**。
//   · 12.1 在既有 ingest_stats 轻量表上增量补「异常」列，复用同一 ensure/聚合/清理链路。
//   · 12.3 用**确定性词法信号门**叠加在既有阈值之上：冷启动路径完全不变（画像照常生成）；
//     周期性更新路径改为「到间隔且有变化信号」或「累积到 间隔×2 的安全刷新」——减少无变化
//     的重复 AI 调用，与 Phase 2 成本治理同向。禁 LLM 决定是否触发（本门纯词法、可复现）。
//
// 单一来源纪律：信号检测/触发决策全部从既有 profile_facts / contacts 状态推导，不另建
// 一套画像调度真相；AI 触发仍复用既有 profileInFlight 去重槽位与后台生成闭包。
// ═══════════════════════════════════════════════════════════════════════════

import (
	"database/sql"
	"fmt"
	"strings"
)

// ---- 12.1 粘贴闭环：输入 = 新增 + 重复 + 异常 ----

// PasteAudit 一次粘贴的确定性四路分解（对齐蓝图「输入500 / 新增37 / 重复463 / 异常0」）。
type PasteAudit struct {
	Input   int `json:"input"`   // 输入：解析器产出的候选消息总数（含随后被判空/不可用的）
	New     int `json:"new"`     // 新增：实际入库
	Dup     int `json:"dup"`     // 重复：命中 UNIQUE(contact_id,msg_hash) 被忽略
	Anomaly int `json:"anomaly"` // 异常：解析出来但内容为空/不可用而丢弃
}

// ComputePasteAudit 从解析与入库计数闭式推出四指标，保证恒等式 Input = New + Dup + Anomaly。
//
//	candidates = ParseClipboard 原始产出条数（过滤空之前）
//	usable     = 过滤空内容后的有效条数（既有 ParsedCount）
//	newCount   = SaveMessages 实际新增
//
// 全部钳制为非负且互相自洽，绝不伪造出「新增 > 输入」之类的破口。
func ComputePasteAudit(candidates, usable, newCount int) PasteAudit {
	if usable < 0 {
		usable = 0
	}
	if candidates < usable {
		candidates = usable
	}
	if newCount < 0 {
		newCount = 0
	}
	if newCount > usable {
		newCount = usable
	}
	return PasteAudit{
		Input:   candidates,
		New:     newCount,
		Dup:     usable - newCount,
		Anomaly: candidates - usable,
	}
}

// ---- 12.3 AI 触发分级：确定性「重要变化」信号检测 ----

// PasteSignal 一类值得惊动 AI 的「重要变化」信号（蓝图 §12.3 六类）。
type PasteSignal string

const (
	SigNewFact      PasteSignal = "new_fact"     // 新事实：我是…/我在…/入职/升职/转行/结婚…
	SigIntent       PasteSignal = "intent"       // 重大意图：打算/计划/考虑/准备/明年…
	SigRelationship PasteSignal = "relationship" // 关系变化：介绍/引荐/认识/合伙人/我同事…
	SigProject      PasteSignal = "project"      // 项目变化：项目/合作/创业/上线/融资/投资…
	SigGoal         PasteSignal = "goal"         // 目标变化：目标/争取/今年要/申请…
	SigRisk         PasteSignal = "risk"         // 高风险事件：生病/住院/离婚/欠/纠纷/被裁…
)

// signalKeywords 每类信号的确定性触发词。纯词法、无 LLM、可复现——触发判定不得交给模型。
var signalKeywords = map[PasteSignal][]string{
	SigNewFact:      {"我是", "我在", "我做", "我住", "我换", "刚辞", "入职", "升职", "转行", "结婚", "生孩子", "搬到"},
	SigIntent:       {"打算", "计划", "考虑", "准备", "想要", "下个月", "明年"},
	SigRelationship: {"介绍", "引荐", "认识", "加了我", "我朋友", "我同学", "我同事", "合伙人"},
	SigProject:      {"项目", "合作", "创业", "上线", "融资", "投资", "开公司", "接单"},
	SigGoal:         {"目标", "争取", "今年要", "明年要", "申请"},
	SigRisk:         {"生病", "住院", "手术", "离婚", "分手", "欠", "纠纷", "被裁", "失业", "出事"},
}

// signalOrder 固定输出顺序，保证确定性（map 遍历无序 → 显式排序）。
var signalOrder = []PasteSignal{SigNewFact, SigIntent, SigRelationship, SigProject, SigGoal, SigRisk}

// DetectChangeSignals 在一批消息里确定性地扫描「重要变化」信号类别，返回按固定顺序去重的
// 信号列表；无信号返回 nil（→ 普通消息只入库）。只看「对方(other)」的话：自己(me)的话
// 不作为对方画像事实来源。
func DetectChangeSignals(messages []Message) []PasteSignal {
	hit := make(map[PasteSignal]bool, len(signalOrder))
	for _, m := range messages {
		if m.Sender != "other" {
			continue
		}
		c := m.Content
		for _, sig := range signalOrder {
			if hit[sig] {
				continue
			}
			for _, w := range signalKeywords[sig] {
				if strings.Contains(c, w) {
					hit[sig] = true
					break
				}
			}
		}
	}
	var out []PasteSignal
	for _, s := range signalOrder {
		if hit[s] {
			out = append(out, s)
		}
	}
	return out
}

// staleRefreshMultiplier 无信号时的安全刷新倍数：累积到 更新间隔×此值 仍强制刷新一次，
// 保证画像不会因「一直没信号」而永久陈旧（分级门只做减频、不做停更）。
const staleRefreshMultiplier = 2

// AITriggerDecision 一次 ingest 的 AI 触发决策与其确定性理由（可解释，供 API/前端展示）。
type AITriggerDecision struct {
	Trigger bool
	Path    string // cold_start | signal | periodic_refresh | none
	Signals []PasteSignal
	Reason  string
}

// DecideAITrigger §12.3 分级门：
//
//	无画像（冷启动）→ 沿用既有阈值：达阈值即首次生成（画像必须先建起来，不受信号门影响）。
//	已有画像 → 到更新间隔时，仅「检测到变化信号」或「累积到 间隔×2 的安全刷新」才触发；
//	  普通无变化粘贴只入库，不惊动 AI。
//
// 读不到状态时保守回退既有「生成或更新」阈值判断，绝不因新门吞掉冷启动。
func DecideAITrigger(db *sql.DB, contactID int64, newMessages []Message) AITriggerDecision {
	signals := DetectChangeSignals(newMessages)
	otherCount, pj, lastCount, err := getContactProfileFields(db, contactID)
	if err != nil {
		return AITriggerDecision{
			Trigger: ShouldGenerateProfile(db, contactID) || ShouldUpdateProfile(db, contactID),
			Path:    "none", Signals: signals, Reason: "画像状态读取失败，回退既有阈值",
		}
	}
	pj = strings.TrimSpace(pj)
	hasProfile := pj != "" && pj != "{}"

	if !hasProfile {
		if ShouldGenerateProfile(db, contactID) {
			return AITriggerDecision{Trigger: true, Path: "cold_start", Signals: signals, Reason: "累计对方消息达冷启动阈值且无画像，首次生成"}
		}
		return AITriggerDecision{Path: "none", Signals: signals, Reason: "未达冷启动阈值"}
	}

	interval := config.Profile.UpdateInterval
	if interval <= 0 {
		return AITriggerDecision{Path: "none", Signals: signals, Reason: "更新间隔未配置"}
	}
	gap := otherCount - lastCount
	if gap >= interval*staleRefreshMultiplier {
		return AITriggerDecision{Trigger: true, Path: "periodic_refresh", Signals: signals, Reason: fmt.Sprintf("距上次画像累积 %d 条(≥%d)，安全刷新", gap, interval*staleRefreshMultiplier)}
	}
	if gap >= interval && len(signals) > 0 {
		return AITriggerDecision{Trigger: true, Path: "signal", Signals: signals, Reason: "达更新间隔且检测到重要变化信号"}
	}
	return AITriggerDecision{Path: "none", Signals: signals, Reason: fmt.Sprintf("普通粘贴：累积 %d 条未达 %d×%d 安全刷新且无重要变化信号，只入库", gap, interval, staleRefreshMultiplier)}
}
