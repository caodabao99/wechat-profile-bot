package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 对话推演 / 回复前模拟（Phase 9）。
//
// 场景：用户在给自己写好的一句"回复草稿"发出去之前，想先看看对方大概率会怎么接。
// 依据：对方画像 + 最近真实对话 + 用户草稿，交给 LLM 推演 2~3 种可能的反应。
// 边界：这是**推演/模拟**，不是预测事实——UI 必须显著标注「AI 模拟」。未配置模型时
// 明确报错返回，绝不编造一段看起来像结果的假数据。

// SimulatedReply 一种可能的对方反应
type SimulatedReply struct {
	Text      string `json:"text"`
	Mood      string `json:"mood"`
	Rationale string `json:"rationale"`
}

// SimulationResult 推演结果
type SimulationResult struct {
	ContactName string           `json:"contactName"`
	Replies     []SimulatedReply `json:"replies"`
	Note        string           `json:"note"`
}

// ErrLLMNotConfigured 模型未配置（区别于网络错误）
var ErrLLMNotConfigured = errors.New("未配置对话模型，无法进行回复推演")

// SimulateReply 基于对方画像与最近对话，推演用户草稿发出后对方可能的几种反应。
func SimulateReply(ctx context.Context, db *sql.DB, llm *LLMClient, contactID int64, draft string, recent []Message) (*SimulationResult, error) {
	draft = strings.TrimSpace(draft)
	if draft == "" {
		return nil, errors.New("请先输入你要发送的草稿内容")
	}
	if !llm.configured() {
		return nil, ErrLLMNotConfigured
	}

	var name, summary string
	err := db.QueryRow(
		`SELECT COALESCE(name,''), COALESCE(profile_summary,'') FROM contacts WHERE id=?`,
		contactID).Scan(&name, &summary)
	if err != nil {
		return nil, fmt.Errorf("联系人不存在: %w", err)
	}

	// 组织最近对话上下文（正序，最新在后），控制在合理长度内避免 prompt 过长
	var convo []string
	for _, m := range recent {
		who := "对方"
		if m.Sender == "me" {
			who = "我"
		}
		text := strings.Join(strings.Fields(m.Content), " ")
		if text == "" {
			continue
		}
		convo = append(convo, who+": "+text)
	}
	ctxTail := convo
	if len(ctxTail) > 24 {
		ctxTail = ctxTail[len(ctxTail)-24:]
	}

	prompt := fmt.Sprintf(
		`你在帮助"我"预判一段微信对话。请根据对方画像与最近对话，推演"我"发出下面这句草稿后，对方最可能的 2~3 种反应。\n`+
			`对方昵称：%s\n对方画像概要：%s\n最近对话：\n%s\n\n我的草稿：%s\n\n`+
			`要求：贴合对方性格与当前关系，不要鸡汤、不像群发。只输出 JSON：`+
			`{"replies":[{"text":"对方的回复","mood":"积极|中性|消极","rationale":"为什么会这样反应，一句话"}]}`,
		name, summary, strings.Join(ctxTail, "\n"), draft)

	raw, err := llm.CallContext(ctx, prompt)
	if err != nil {
		return nil, err
	}
	replies, err := parseSimulatedReplies(ExtractJSON(raw))
	if err != nil {
		return nil, err
	}
	return &SimulationResult{
		ContactName: name,
		Replies:     replies,
		Note:        "以上为 AI 基于画像与历史对话的推测，仅供参考，不代表对方真实反应",
	}, nil
}

// parseSimulatedReplies 解析模型返回的 replies 数组；容忍被包在 {"replies":[...]} 或裸 [...]。
func parseSimulatedReplies(s string) ([]SimulatedReply, error) {
	// 先按对象解析
	var wrapper struct {
		Replies []SimulatedReply `json:"replies"`
	}
	if err := json.Unmarshal([]byte(s), &wrapper); err == nil && len(wrapper.Replies) > 0 {
		return cleanReplies(wrapper.Replies), nil
	}
	var arr []SimulatedReply
	if err := json.Unmarshal([]byte(s), &arr); err == nil && len(arr) > 0 {
		return cleanReplies(arr), nil
	}
	return nil, errors.New("模型返回格式无法解析，请重试")
}

func cleanReplies(in []SimulatedReply) []SimulatedReply {
	out := make([]SimulatedReply, 0, len(in))
	for _, r := range in {
		r.Text = strings.TrimSpace(r.Text)
		if r.Text == "" {
			continue
		}
		out = append(out, r)
		if len(out) >= 3 {
			break
		}
	}
	return out
}
