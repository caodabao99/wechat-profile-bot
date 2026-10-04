package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// 对话式问答「问 TA 的历史」（特性①）。
//
// 做法（两段式，零新依赖，不上向量/Embedding）：
//  1. 让 LLM 从问题里抽检索关键词（弥补同义改写，如「出去玩」→「三亚/旅游」）；
//  2. 复用已有 FTS5/LIKE 检索按关键词召回该联系人的历史消息（含归档），再补最近若干条做上下文；
//  3. 把编号后的原文喂给 LLM 作答，要求逐条标注 [n] 出处，返回答案 + 原文出处。
//
// 边界：未配置模型明确报错，绝不编造；没检索到相关原文时如实说「没找到」，不硬答。

// AskSource 一条被引用的历史消息
type AskSource struct {
	N         int    `json:"n"`
	MessageID int64  `json:"messageId"`
	Sender    string `json:"sender"`
	Who       string `json:"who"`
	MsgTime   string `json:"msgTime"`
	Snippet   string `json:"snippet"`
	Archived  bool   `json:"archived"`
}

// AskResult 问答结果
type AskResult struct {
	Question string      `json:"question"`
	Answer   string      `json:"answer"`
	Sources  []AskSource `json:"sources"`
	Note     string      `json:"note"`
}

const (
	askPerKeyword    = 8  // 每个关键词最多召回条数
	askRecent        = 12 // 兜底附带的最近消息条数
	askMaxCandidates = 40 // 送进 prompt 的原文上限
	askSnippetRunes  = 120
)

// AskContactHistory 基于检索到的历史原文，回答关于某联系人的问题并附出处。
func AskContactHistory(ctx context.Context, db *sql.DB, llm *LLMClient, contactID int64, question string) (*AskResult, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, errors.New("请先输入你的问题")
	}
	if !llm.configured() {
		return nil, ErrLLMNotConfigured
	}

	var name, summary string
	if err := db.QueryRow(
		`SELECT COALESCE(name,''), COALESCE(profile_summary,'') FROM contacts WHERE id=?`,
		contactID).Scan(&name, &summary); err != nil {
		return nil, fmt.Errorf("联系人不存在: %w", err)
	}

	keywords := extractAskKeywords(ctx, llm, question)
	candidates := retrieveRelevantMessages(db, contactID, keywords)
	if len(candidates) == 0 {
		return &AskResult{
			Question: question,
			Answer:   "没有找到与这个问题相关的历史消息，无法基于原文出处作答。可以换个说法，或先补充更多聊天记录。",
			Sources:  []AskSource{},
			Note:     "本功能只根据检索到的真实原文作答，不凭空推测。",
		}, nil
	}

	// 编号原文（正序，最早在前），编号即引用锚点
	captioned := make([]string, 0, len(candidates))
	for i := range candidates {
		candidates[i].N = i + 1
		who := "对方"
		if candidates[i].Sender == "me" {
			who = "我"
		}
		candidates[i].Who = who
		captioned = append(captioned, fmt.Sprintf("[%d] %s（%s）：%s",
			candidates[i].N, who, candidates[i].MsgTime, candidates[i].Snippet))
	}

	prompt := fmt.Sprintf(
		`你在帮助"我"回答关于某个人历史聊天记录的问题。只依据下面给出的、带编号的真实消息原文作答，`+
			`并在引用某条依据时标注对应编号 [n]。若原文不足以回答，如实说明。不要编造原文里没有的信息。\n`+
			`对方昵称：%s\n对方画像概要：%s\n\n可引用的历史消息（编号从 1 开始）：\n%s\n\n我的问题：%s\n\n`+
			`只输出 JSON：{"answer":"你的回答，尽量带上 [n] 出处标注"}`,
		name, summary, strings.Join(captioned, "\n"), question)

	raw, err := llm.CallContext(ctx, prompt)
	if err != nil {
		return nil, err
	}
	answer := parseAskAnswer(raw)
	return &AskResult{
		Question: question,
		Answer:   answer,
		Sources:  candidates,
		Note:     "以上回答仅依据检索到的历史原文，引用编号 [n] 对应下方出处；检索可能不完整，请以原文为准。",
	}, nil
}

// extractAskKeywords 让模型把口语问题改写成若干检索关键词；失败则回退为按空格切分问题。
func extractAskKeywords(ctx context.Context, llm *LLMClient, question string) []string {
	prompt := fmt.Sprintf(
		`从下面的问题里提取 2~6 个用于全文检索微信聊天记录的关键词（名词/地名/事件词为主，`+
			`可包含同义词以提高召回）。只输出 JSON：{"keywords":["词1","词2"]}。\n问题：%s`, question)
	kws := []string{}
	if raw, err := llm.CallContext(ctx, prompt); err == nil {
		var wrapper struct {
			Keywords []string `json:"keywords"`
		}
		if json.Unmarshal([]byte(ExtractJSON(raw)), &wrapper) == nil {
			for _, k := range wrapper.Keywords {
				if k = strings.TrimSpace(k); k != "" {
					kws = append(kws, k)
				}
			}
		}
	}
	if len(kws) == 0 {
		// 回退：把问题本身当关键词，并补上按空白切分的词
		kws = append(kws, question)
		for _, w := range strings.Fields(question) {
			if w = strings.TrimSpace(w); w != "" && w != question {
				kws = append(kws, w)
			}
		}
	}
	if len(kws) > 8 {
		kws = kws[:8]
	}
	return kws
}

// retrieveRelevantMessages 按关键词用 SearchMessages 召回该联系人消息（含归档），去重后
// 再补最近若干条活跃消息做上下文；统一按消息时间正序返回并截断到上限。
func retrieveRelevantMessages(db *sql.DB, contactID int64, keywords []string) []AskSource {
	seen := map[string]bool{}
	var list []AskSource
	add := func(id int64, sender, content, msgTime string, archived bool) {
		key := fmt.Sprintf("%d:%v", id, archived)
		if seen[key] {
			return
		}
		seen[key] = true
		list = append(list, AskSource{
			MessageID: id, Sender: sender, MsgTime: msgTime,
			Snippet:  truncateRunes(strings.Join(strings.Fields(content), " "), askSnippetRunes),
			Archived: archived,
		})
	}

	for _, kw := range keywords {
		if strings.TrimSpace(kw) == "" {
			continue
		}
		res, err := SearchMessages(db, SearchOptions{
			Query: kw, ContactID: contactID, IncludeArchive: true, Limit: askPerKeyword,
		})
		if err != nil {
			continue
		}
		for _, h := range res.List {
			add(h.ID, h.Sender, h.Content, h.MsgTime, h.Archived)
		}
	}

	// 兜底：最近若干条活跃消息（带 id），给「上次/最近」这类时间性问题提供上下文
	rows, err := db.Query(
		`SELECT id, sender, content, COALESCE(msg_time,'') FROM messages
		 WHERE contact_id = ? ORDER BY id DESC LIMIT ?`, contactID, askRecent)
	if err == nil {
		for rows.Next() {
			var id int64
			var sender, content, msgTime string
			if rows.Scan(&id, &sender, &content, &msgTime) == nil {
				add(id, sender, content, msgTime, false)
			}
		}
		rows.Close()
	}

	// 按消息时间正序（最早在前），无法解析时间的排最后
	sort.SliceStable(list, func(i, j int) bool {
		return parseMsgTime(list[i].MsgTime).Before(parseMsgTime(list[j].MsgTime))
	})
	if len(list) > askMaxCandidates {
		list = list[len(list)-askMaxCandidates:] // 保留最近的若干条
		for i := range list {
			list[i].N = 0
		}
	}
	if list == nil {
		return []AskSource{}
	}
	return list
}

// parseAskAnswer 解析模型返回；优先取 {"answer":"..."}，否则退回原始文本。
func parseAskAnswer(raw string) string {
	s := strings.TrimSpace(ExtractJSON(raw))
	var wrapper struct {
		Answer string `json:"answer"`
	}
	if json.Unmarshal([]byte(s), &wrapper) == nil && strings.TrimSpace(wrapper.Answer) != "" {
		return strings.TrimSpace(wrapper.Answer)
	}
	// 直接文本兜底（去掉可能的 JSON 外壳残片）
	if txt := strings.TrimSpace(raw); txt != "" {
		return txt
	}
	return "模型未能给出有效回答，请重试。"
}
