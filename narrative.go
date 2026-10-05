package main

// v5.5.0 #6：关系叙事生成（LLM，复用 summary 管道 + 新增 relationship_narrative 模板）。
//
// 给单个联系人写一段温暖、真诚的「关系故事」，帮助回顾并珍视这段关系：
//   - 有 LLM 时：把关系事实（相识年数/首互动/近期主题/往来条数）+ 近期编号原文喂给模型，产一段叙事；
//   - 双重降级：无 LLM、模型调用失败、或返回空/坏产物时，回落到「确定性叙事」——
//     纯由既有确定性事实（computeAchMetrics 的相识起点/累计条数 + 最近主题快照）拼出一段成文的话。
//   - 绝不 503、绝不编造具体事件：无原文也能基于事实给出真诚叙事。
//
// 设计铁律：
//   - 增量复用：computeAchMetrics / loadSummaryInputs / numberMessagesForSummary /
//     latestTopicsBefore / RenderPrompt / llm.CallContext 全部既有；不重写老模块。
//   - 单连接池分层锁：各读取函数各自取放 dbMu，LLM 调用与 RenderPrompt 一律在锁外发起，绝不嵌套。
//   - 不落库、计算即返回（与 summary 同口径）；不新增配置开关。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	narrativeDefaultDays = 90  // 缺省叙事窗口（比摘要宽，故事需要更多上下文）
	narrativeMaxDays     = 365 // 窗口上限
	narrativeMinRunes    = 12  // 模型产物短于此视为无效，降级到确定性叙事
	narrativeMaxRunes    = 600 // 叙事正文上限（护栏，防模型跑飞）
	narrativeTopicsMax   = 4   // 近期主题取前 N 个
)

// NarrativeResponse 关系叙事响应体。
type NarrativeResponse struct {
	ContactID    int64    `json:"contactId"`
	Name         string   `json:"name"`
	Years        int      `json:"years"`
	MsgCount     int      `json:"msgCount"`
	FirstDate    string   `json:"firstDate"`
	RecentTopics []string `json:"recentTopics"`
	Narrative    string   `json:"narrative"`
	Source       string   `json:"source"` // "llm" | "deterministic"
	Note         string   `json:"note"`
	GeneratedAt  string   `json:"generatedAt"`
}

// clampNarrativeDays 把外部 days 夹到 [1, narrativeMaxDays]，非法/零回落缺省。
func clampNarrativeDays(days int) int {
	if days <= 0 {
		return narrativeDefaultDays
	}
	if days > narrativeMaxDays {
		return narrativeMaxDays
	}
	return days
}

// topNarrativeTopics 纯函数：从主题快照里挑「非消退」主题，按权重降序、名称升序去并列取前 max 个名称。
func topNarrativeTopics(entries []TopicEntry, max int) []string {
	if max <= 0 || len(entries) == 0 {
		return []string{}
	}
	filtered := make([]TopicEntry, 0, len(entries))
	for _, t := range entries {
		if strings.TrimSpace(t.Name) != "" && t.Status != "fading" {
			filtered = append(filtered, t)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].Weight != filtered[j].Weight {
			return filtered[i].Weight > filtered[j].Weight
		}
		return filtered[i].Name < filtered[j].Name
	})
	out := make([]string, 0, max)
	seen := map[string]bool{}
	for _, t := range filtered {
		if seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		out = append(out, t.Name)
		if len(out) >= max {
			break
		}
	}
	return out
}

// joinTopicsCN 纯函数：用中文顿号连接主题名（确定性，末项不加连接词）。
func joinTopicsCN(ts []string) string {
	return strings.Join(ts, "、")
}

// buildDeterministicNarrative 纯函数（可脱离 DB / LLM 单测）：由确定性事实拼一段真诚叙事。
//   - years/msgCount/firstDate/recentTopics 均来自库内既有原语；
//   - 记录太少时如实说明「故事还在开头」，绝不硬编、不虚构具体事件。
func buildDeterministicNarrative(name string, years, msgCount int, firstDate string, recentTopics []string) string {
	var sb strings.Builder
	// 开场：相识时长 / 起点
	switch {
	case years >= 1:
		fmt.Fprintf(&sb, "你和%s已经认识 %d 年了。", name, years)
	case firstDate != "":
		fmt.Fprintf(&sb, "你和%s的故事，始于 %s。", name, firstDate)
	default:
		sb.WriteString("关于" + name + "的可回溯记录还不算多。")
	}
	// 中段：往来体量
	if msgCount > 0 {
		fmt.Fprintf(&sb, "这些日子里，你们累计留下了 %d 条你来我往的消息，", msgCount)
	} else {
		sb.WriteString("你们之间还没有太多往来的消息，")
	}
	// 共同话题
	if len(recentTopics) > 0 {
		fmt.Fprintf(&sb, "最近常聊起%s，", joinTopicsCN(recentTopics))
	} else {
		sb.WriteString("聊天内容还在慢慢积累，")
	}
	// 收尾：温度与前瞻（依体量给不同语气，确定性）
	switch {
	case msgCount >= 500:
		sb.WriteString("点点滴滴攒成了这段关系最真实的温度，值得你偶尔回头看看。")
	case msgCount >= 100:
		sb.WriteString("这些日常的来来往往，正是维系一段关系最朴素的默契。")
	case msgCount > 0:
		sb.WriteString("故事还在开头，往后的每一句问候，都会让它更完整。")
	default:
		sb.WriteString("也许一句主动的问候，就能为这段关系写下第一页。")
	}
	return sb.String()
}

// cleanNarrativeText 清洗模型返回的纯文本叙事：剥 code fence / 包裹引号、去空白、按 rune 截断。
// 返回空串表示产物无效（交由调用方降级）。
func cleanNarrativeText(raw string) string {
	s := strings.TrimSpace(raw)
	// 去掉可能的 ``` 包裹
	if strings.HasPrefix(s, "```") {
		s = strings.TrimLeft(s, "`")
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		if j := strings.LastIndex(s, "```"); j >= 0 {
			s = s[:j]
		}
		s = strings.TrimSpace(s)
	}
	// 去掉整体包裹的中英文引号（按 rune 判首尾，规避多字节）
	for {
		rs := []rune(s)
		if len(rs) < 2 {
			break
		}
		first, last := rs[0], rs[len(rs)-1]
		if (first == '"' && last == '"') ||
			(first == '“' && last == '”') ||
			(first == '「' && last == '」') {
			s = strings.TrimSpace(string(rs[1 : len(rs)-1]))
			continue
		}
		break
	}
	// 压缩多余空行
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t\r")
		if ln == "" && len(kept) > 0 && kept[len(kept)-1] == "" {
			continue
		}
		if ln == "" && len(kept) == 0 {
			continue
		}
		kept = append(kept, ln)
	}
	s = strings.TrimSpace(strings.Join(kept, "\n"))
	if r := []rune(s); len(r) > narrativeMaxRunes {
		s = string(r[:narrativeMaxRunes])
	}
	if len([]rune(s)) < narrativeMinRunes {
		return ""
	}
	return s
}

// GenerateNarrative 生成关系叙事。有 LLM 用模型、失败/无 LLM 双重降级为确定性叙事，永不返回 ErrLLMNotConfigured。
func GenerateNarrative(ctx context.Context, db *sql.DB, llm *LLMClient, contactID int64, days int) (*NarrativeResponse, error) {
	days = clampNarrativeDays(days)
	now := time.Now()

	// —— 确定性事实：相识起点 / 累计条数 / 展示名（computeAchMetrics 内部自锁并释放）——
	metrics, _, err := computeAchMetrics(db, contactID)
	if err != nil {
		return nil, err
	}
	years := wholeYearsSince(metrics.firstTime, now)
	msgCount := metrics.msgTotal
	firstDate := ""
	if !metrics.firstTime.IsZero() {
		firstDate = metrics.firstTime.Format("2006-01-02")
	}

	// —— 展示名 + 近期原文（loadSummaryInputs 内部自锁并释放）——
	c, _, msgs, err := loadSummaryInputs(db, contactID, days)
	if err != nil {
		return nil, err
	}
	name := displayName(c)

	// —— 近期主题（latestTopicsBefore 内部自锁并释放；beforeWeek 传远期取全局最新快照）——
	recentTopics := topNarrativeTopics(latestTopicsBefore(db, contactID, "9999-12-31"), narrativeTopicsMax)

	resp := &NarrativeResponse{
		ContactID:    contactID,
		Name:         name,
		Years:        years,
		MsgCount:     msgCount,
		FirstDate:    firstDate,
		RecentTopics: recentTopics,
		GeneratedAt:  now.Format("2006-01-02 15:04:05"),
	}

	degrade := func(note string) *NarrativeResponse {
		resp.Source = "deterministic"
		resp.Narrative = buildDeterministicNarrative(name, years, msgCount, firstDate, recentTopics)
		resp.Note = note
		return resp
	}

	// —— 无 LLM：确定性降级（不 503）——
	if !llm.configured() {
		return degrade("未配置模型，以上为基于往来事实的确定性叙事（不调模型、不编造）。"), nil
	}

	// —— 原文太少：事实仍在，但叙事以确定性版更稳妥，且不浪费一次模型调用 ——
	if len(msgs) < summaryMinMsgs {
		return degrade("可回溯的近期原文较少，以上为基于往来事实的确定性叙事。"), nil
	}

	// —— 有 LLM：渲染提示词（RenderPrompt 内部取 dbMu，须锁外）+ 调模型（无锁）——
	_, captioned := numberMessagesForSummary(msgs)
	firstTopic := "记录尚浅"
	if firstDate != "" {
		firstTopic = firstDate + " 首次互动"
	}
	rt := "暂无"
	if len(recentTopics) > 0 {
		rt = joinTopicsCN(recentTopics)
	}
	prompt, err := RenderPrompt(db, "relationship_narrative", map[string]string{
		"name":         name,
		"years":        strconv.Itoa(years),
		"firstTopic":   firstTopic,
		"recentTopics": rt,
		"msgCount":     strconv.Itoa(msgCount),
		"captioned":    strings.Join(captioned, "\n"),
	})
	if err != nil {
		return degrade("模型叙事不可用，以上为基于往来事实的确定性叙事。"), nil
	}
	raw, err := llm.CallContext(ctx, prompt)
	if err != nil {
		return degrade("模型调用失败，已降级为基于往来事实的确定性叙事。"), nil
	}
	narr := cleanNarrativeText(raw)
	if narr == "" {
		return degrade("模型未返回有效叙事，已降级为基于往来事实的确定性叙事。"), nil
	}
	resp.Source = "llm"
	resp.Narrative = narr
	resp.Note = "以上叙事由模型依据往来事实与近期原文生成，可能带有偏差，请以真实记录为准。"
	return resp, nil
}

// hContactNarrative POST /api/contacts/{id}/narrative  body {"days":90}：生成关系叙事（LLM + 双重降级，永不 503）。
func (s *apiServer) hContactNarrative(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	if _, err := GetContactByID(s.db, id); err != nil {
		writeErr(w, http.StatusNotFound, "联系人不存在")
		return
	}
	var req struct {
		Days int `json:"days"`
	}
	// 允许空 body（缺省窗口）；解析失败按 days=0 走缺省，不硬失败。
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRehearsalBodyBytes)).Decode(&req)
	res, err := GenerateNarrative(r.Context(), s.db, s.llm, id, req.Days)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
