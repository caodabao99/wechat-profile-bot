package main

// 消息智能摘要（增值功能⑤，LLM）。
//
// 给单个联系人 + 时间窗口，让模型基于窗口内的真实聊天原文，产出一份结构化回顾：
//   - overview  ：一段话总结这段时间讨论了哪些事；
//   - topics    ：若干话题标签（供快速识别主线）；
//   - todos     ：待办事项（每条带「我 / 对方」归属 + [n] 出处编号）；
//   - sources   ：被引用的原文列表（供前端展示出处）。
//
// 设计铁律：
//   - 复用 ask.go 的编号原文 + [n] 出处口径与 AskSource 结构；一次 LLM 调用即可产出。
//   - 未配置模型明确返回 ErrLLMNotConfigured → HTTP 503，绝不编造。
//   - 消息不足阈值：如实返回空摘要 + Note 说明「原文不足」，不硬编。
//   - LLM 返回坏 JSON：overview 回退原文首行截断（不新增信息），topics/todos 空数组。
//   - 单连接池：只取一层 dbMu（一次消息查询 + 一次 contacts 取名字/画像摘要），不嵌套锁。
//   - 摘要本身一次性返回，不落库、不新增配置开关。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	summaryDefaultDays     = 30  // 缺省回顾窗口
	summaryMaxDays         = 365 // 窗口上限（一年）
	summaryScanCap         = 400 // 一次拉取的消息条数上限护栏
	summaryMinMsgs         = 4   // 少于此数不硬编摘要（与 extractFollowups 一致）
	summaryMaxForPrompt    = 120 // 送进 prompt 的原文条数上限（保最近若干）
	summarySnippetRunes    = 200 // 单条原文截断（比 ask 略宽，方便模型抽 topics/todos）
	summaryTopicsMax       = 8   // topics 上限
	summaryTodosMax        = 12  // todos 上限
	summaryTodoTextMax     = 60  // 单条 todo 正文长度上限
	summaryOverviewMaxRune = 800 // 概览回退原文截断
)

// SummaryTodo 摘要里抽出的单条待办
type SummaryTodo struct {
	Text  string `json:"text"`
	Owner string `json:"owner"` // "我" 或 "对方"
	Ref   string `json:"ref"`   // 出处编号，形如 "[3]"
}

// ContactSummary 某联系人某时间窗口的结构化回顾
type ContactSummary struct {
	ContactID int64         `json:"contactId"`
	Name      string        `json:"name"`
	From      string        `json:"from"`
	To        string        `json:"to"`
	Overview  string        `json:"overview"`
	Topics    []string      `json:"topics"`
	Todos     []SummaryTodo `json:"todos"`
	Sources   []AskSource   `json:"sources"`
	Note      string        `json:"note"`
}

// clampSummaryDays 把外部传入的 days 夹到 [1, summaryMaxDays]，非法/零回落默认。
func clampSummaryDays(days int) int {
	if days <= 0 {
		return summaryDefaultDays
	}
	if days > summaryMaxDays {
		return summaryMaxDays
	}
	return days
}

// SummarizeContact 生成结构化回顾。未配置 LLM 返回 ErrLLMNotConfigured。
func SummarizeContact(ctx context.Context, db *sql.DB, llm *LLMClient, contactID int64, days int) (*ContactSummary, error) {
	if !llm.configured() {
		return nil, ErrLLMNotConfigured
	}
	if days <= 0 {
		days = summaryDefaultDays
	}
	if days > summaryMaxDays {
		days = summaryMaxDays
	}

	// —— 一次性取联系人 + 窗口内消息（分层单锁、非嵌套）——
	c, summary, msgs, err := loadSummaryInputs(db, contactID, days)
	if err != nil {
		return nil, err
	}

	from := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
	to := time.Now().Format("2006-01-02")

	base := &ContactSummary{
		ContactID: contactID,
		Name:      displayName(c),
		From:      from,
		To:        to,
		Overview:  "",
		Topics:    []string{},
		Todos:     []SummaryTodo{},
		Sources:   []AskSource{},
	}

	// —— 消息不足：如实返回，不硬编 ——
	if len(msgs) < summaryMinMsgs {
		base.Overview = ""
		base.Note = fmt.Sprintf("近 %d 天可回顾的消息不足 %d 条，暂不生成摘要（不硬编、不推测）。", days, summaryMinMsgs)
		return base, nil
	}

	// —— 编号原文（时间正序，最早在前），编号即 [n] 出处锚点 ——
	sources, captioned := numberMessagesForSummary(msgs)
	base.Sources = sources

	prompt, err := RenderPrompt(db, "summary", map[string]string{
		"name":        base.Name,
		"days":        strconv.Itoa(days),
		"summary":     summary,
		"captioned":   strings.Join(captioned, "\n"),
		"topicsMax":   strconv.Itoa(summaryTopicsMax),
		"todoTextMax": strconv.Itoa(summaryTodoTextMax),
		"todosMax":    strconv.Itoa(summaryTodosMax),
	})
	if err != nil {
		return nil, err
	}

	raw, err := callLLMCached(ctx, db, llm, contactID, TaskSummary, "", prompt)
	if err != nil {
		return nil, err
	}
	overview, topics, todos, ok := parseSummaryJSON(raw)
	if !ok {
		// 坏 JSON：不新增信息，overview 回退为原文首段截断，topics/todos 空
		overview = fallbackOverviewFromMessages(msgs)
		topics = []string{}
		todos = []SummaryTodo{}
	}
	base.Overview = overview
	base.Topics = normalizeTopics(topics)
	base.Todos = normalizeTodos(todos, sources)
	base.Note = "以上回顾仅依据窗口内的真实聊天原文生成，[n] 编号对应下方出处；模型输出可能有偏差，请以原文为准。"
	return base, nil
}

// loadSummaryInputs 取联系人 + 窗口内消息（时间正序）。
// 与 ask.go 不同：这里刻意把取名字与取消息合到同一函数、但分成两次 dbMu.Lock
// （GetContactByID 与后续消息查询各自加锁），避免嵌套锁；也不改动 GetContactByID 签名。
//
// 取数走统一历史访问层（P9）：必须并上 messages_archive。之前只查 messages，
// 用户点一次「立即归档」，下次摘要就会基于被截断的语料生成——不报错、不提示，属于静默失真。
func loadSummaryInputs(db *sql.DB, contactID int64, days int) (*Contact, string, []HistoryMessage, error) {
	c, err := GetContactByID(db, contactID)
	if err != nil {
		return nil, "", nil, err
	}
	f := HistoryFilter{
		ContactID:    contactID,
		SinceUnix:    time.Now().AddDate(0, 0, -days).Unix(),
		WithTimeOnly: true,
	}
	// 层的 limit 是每张表各取这么多（归档后语料分散在两表），后面再按 prompt 上限裁。
	msgs, err := HistoryMessages(db, f, summaryScanCap)
	if err != nil {
		return nil, "", nil, err
	}
	if len(msgs) > summaryMaxForPrompt {
		msgs = msgs[len(msgs)-summaryMaxForPrompt:] // 保留最近的若干条（已按时间正序）
	}
	return c, c.ProfileSummary, msgs, nil
}

// numberMessagesForSummary 把窗口内的消息按时间正序编号 [1..N]，返回带编号的出处列表 + prompt 行文本。
func numberMessagesForSummary(msgs []HistoryMessage) ([]AskSource, []string) {
	sources := make([]AskSource, 0, len(msgs))
	lines := make([]string, 0, len(msgs))
	for i, m := range msgs {
		who := "我"
		if m.Sender == "other" {
			who = "对方"
		} else if m.Sender != "me" {
			// 兜底：非标准 sender 也归到对方，与 ask.go 语义保持一致
			who = "对方"
		}
		ts := m.Timestamp.Format("2006-01-02 15:04")
		snippet := truncateRunes(strings.Join(strings.Fields(m.Content), " "), summarySnippetRunes)
		sources = append(sources, AskSource{
			N: i + 1, MessageID: m.ID, Sender: m.Sender, Who: who, MsgTime: ts, Snippet: snippet,
			// 归档行的 id 属于 messages_archive，与 messages 的 id 序列互不相干；
			// 带上来源标记，前端才能判断这条出处能否直跳到高亮节点。
			Archived: m.Archived,
		})
		lines = append(lines, fmt.Sprintf("[%d] %s（%s）：%s", i+1, who, ts, snippet))
	}
	return sources, lines
}

// parseSummaryJSON 解析模型返回的 {"overview":..., "topics":[...], "todos":[...]}
// 返回 ok=false 表示模型返回的不是合法 JSON 或 overview 为空。
func parseSummaryJSON(raw string) (overview string, topics []string, todos []SummaryTodo, ok bool) {
	s := strings.TrimSpace(ExtractJSON(raw))
	var wrapper struct {
		Overview string   `json:"overview"`
		Topics   []string `json:"topics"`
		Todos    []struct {
			Text  string `json:"text"`
			Owner string `json:"owner"`
			Ref   string `json:"ref"`
		} `json:"todos"`
	}
	if err := json.Unmarshal([]byte(s), &wrapper); err != nil {
		return "", nil, nil, false
	}
	overview = strings.TrimSpace(wrapper.Overview)
	if overview == "" {
		return "", nil, nil, false
	}
	for _, tp := range wrapper.Topics {
		if tp = strings.TrimSpace(tp); tp != "" {
			topics = append(topics, tp)
		}
	}
	for _, td := range wrapper.Todos {
		txt := strings.TrimSpace(td.Text)
		if txt == "" {
			continue
		}
		owner := strings.TrimSpace(td.Owner)
		if owner != "我" && owner != "对方" {
			owner = "我" // 归属不明时保守归为「我」
		}
		todos = append(todos, SummaryTodo{Text: txt, Owner: owner, Ref: strings.TrimSpace(td.Ref)})
	}
	return overview, topics, todos, true
}

// fallbackOverviewFromMessages 坏 JSON 时的兜底：只拼接原文前若干条首行，
// 明确不新增信息、不做主观总结。
func fallbackOverviewFromMessages(msgs []HistoryMessage) string {
	var sb strings.Builder
	sb.WriteString("（模型未返回有效结构化摘要，以下为窗口内原文摘录，非自动总结）\n")
	used := 0
	for _, m := range msgs {
		who := "我"
		if m.Sender == "other" {
			who = "对方"
		}
		line := fmt.Sprintf("%s[%s]: %s\n", who, m.Timestamp.Format("2006-01-02 15:04"),
			preview(strings.Join(strings.Fields(m.Content), " "), 80))
		if used+len([]rune(line)) > summaryOverviewMaxRune {
			break
		}
		sb.WriteString(line)
		used += len([]rune(line))
	}
	return sb.String()
}

// normalizeTopics 去重 + 截断
func normalizeTopics(in []string) []string {
	if in == nil {
		return []string{}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		if r := []rune(t); len(r) > 20 {
			t = string(r[:20])
		}
		seen[t] = true
		out = append(out, t)
		if len(out) >= summaryTopicsMax {
			break
		}
	}
	return out
}

// normalizeTodos 截断 text、去重、上限；ref 校验必须是 [n] 且 1<=n<=len(sources)，非法则清空。
func normalizeTodos(in []SummaryTodo, sources []AskSource) []SummaryTodo {
	if in == nil {
		return []SummaryTodo{}
	}
	seen := map[string]bool{}
	out := make([]SummaryTodo, 0, len(in))
	maxN := len(sources)
	for _, td := range in {
		txt := strings.TrimSpace(td.Text)
		if txt == "" {
			continue
		}
		if r := []rune(txt); len(r) > summaryTodoTextMax {
			txt = string(r[:summaryTodoTextMax])
		}
		if seen[txt] {
			continue
		}
		seen[txt] = true
		owner := td.Owner
		if owner != "我" && owner != "对方" {
			owner = "我"
		}
		ref := strings.TrimSpace(td.Ref)
		if !validRefLabel(ref, maxN) {
			ref = ""
		}
		out = append(out, SummaryTodo{Text: txt, Owner: owner, Ref: ref})
		if len(out) >= summaryTodosMax {
			break
		}
	}
	return out
}

// validRefLabel 判断形如 "[n]" 且 n 在 [1, maxN] 内。
func validRefLabel(s string, maxN int) bool {
	if len(s) < 3 || s[0] != '[' || s[len(s)-1] != ']' {
		return false
	}
	var n int
	if _, err := fmt.Sscanf(s[1:len(s)-1], "%d", &n); err != nil {
		return false
	}
	return n >= 1 && n <= maxN
}
