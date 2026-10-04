package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// 对话预演：基于联系人画像 + 真实聊天记录，让模型扮演对方，陪用户演练一场重要对话。
//
// 设计取舍：完全无状态——不建表、不写库、不发送任何消息。
// 一场预演只活在浏览器的内存里，刷新即丢；用户想留就用「复制全文」。
// 这样做的代价是历史无法回看，收益是零迁移、零数据一致性负担，
// 也避免把「演练时的狠话」误存成真实关系数据。

const (
	rehearsalTimeout        = 120 * time.Second
	rehearsalSampleMsg      = 30 // 取样最近多少条历史消息（含双方）
	rehearsalMaxSampleLines = 12 // 塞进 prompt 的对方真实说话样例条数
	rehearsalMaxMyLines     = 8  // 塞进 prompt 的「我」的说话样例条数
	rehearsalMaxTurns       = 60 // 单场预演最多带多少条对话进 prompt
	rehearsalMaxTextRunes   = 2000
	rehearsalMaxSceneRunes  = 500
	rehearsalReplyMaxRunes  = 400
	rehearsalMaxListItems   = 8 // 复盘各分组最多返回几条
)

// RehearsalTurn 预演里的一条对话。Role 只认 "me"（用户）和 "other"（AI 扮演的对方）。
type RehearsalTurn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// RehearsalContext 预演依据，进页签时展示给用户，让他知道模型是照什么在演。
type RehearsalContext struct {
	Name        string   `json:"name"`
	Hints       []string `json:"hints"`       // 画像要点
	SampleLines []string `json:"sampleLines"` // 对方真实说话样例
	MyStyle     string   `json:"myStyle"`     // 「我」平时的说话风格
	HasProfile  bool     `json:"hasProfile"`
	HasMessages bool     `json:"hasMessages"`
}

// RehearsalReview 一场预演的复盘结论。
type RehearsalReview struct {
	Summary     string   `json:"summary"`
	Good        []string `json:"good"`
	Bad         []string `json:"bad"`
	Risks       []string `json:"risks"`
	Suggestions []string `json:"suggestions"`
	Score       int      `json:"score"`
}

// profileHints 从画像里挑出「扮演这个人」用得上的字段。
// 比 GenerateBlessings 用得更全：预演要还原的是说话方式和情绪反应，
// 所以回复长短、主动程度、不高兴时的表现这些都必须带上。
func profileHints(p Profile) []string {
	hints := make([]string, 0, 16)
	add := func(label, v string) {
		v = strings.TrimSpace(v)
		if v != "" {
			hints = append(hints, label+v)
		}
	}
	addList := func(label string, vs []string) {
		clean := make([]string, 0, len(vs))
		for _, v := range vs {
			if v = strings.TrimSpace(v); v != "" {
				clean = append(clean, v)
			}
		}
		if len(clean) > 0 {
			hints = append(hints, label+strings.Join(clean, "、"))
		}
	}

	add("关系概括：", p.Summary)
	add("职业：", p.BasicInfo.Occupation)
	add("所在城市：", p.BasicInfo.Location)
	add("亲密程度：", p.Relationship.Closeness)
	add("互动模式：", p.Relationship.InteractionPattern)
	add("对方语气：", p.CommunicationStyle.Tone)
	add("回复长短倾向：", p.CommunicationStyle.ReplyLength)
	add("主动程度：", p.CommunicationStyle.Initiative)
	add("表情使用习惯：", p.CommunicationStyle.EmojiUsage)
	addList("性格特征：", p.Personality)
	addList("兴趣爱好：", p.Interests)
	addList("对方口头禅：", p.CommunicationStyle.FrequentPhrases)
	addList("近期共同事件：", p.Relationship.RecentEvents)
	addList("需要避开的雷点：", p.EmotionalPatterns.Stressors)
	addList("能安抚他的话题：", p.EmotionalPatterns.ComfortTopics)
	add("不高兴时的表现：", p.EmotionalPatterns.WhenUpset)
	addList("重要事实：", p.ImportantFacts)
	return hints
}

// LoadRehearsalContext 读取扮演依据。联系人不存在时返回错误，由 handler 转 404。
func LoadRehearsalContext(db *sql.DB, contactID int64) (*RehearsalContext, error) {
	c, err := GetContactByID(db, contactID)
	if err != nil {
		return nil, err
	}
	out := &RehearsalContext{
		Name:        displayName(c),
		Hints:       []string{},
		SampleLines: []string{},
		MyStyle:     "（暂无历史消息可参考）",
	}

	if strings.TrimSpace(c.ProfileJSON) != "" {
		var p Profile
		if err := json.Unmarshal([]byte(c.ProfileJSON), &p); err == nil {
			out.Hints = profileHints(p)
			out.HasProfile = len(out.Hints) > 0
		}
	}

	var otherLines, myLines []string
	if msgs, merr := GetRecentMessages(db, contactID, rehearsalSampleMsg); merr == nil {
		// GetRecentMessages 是 id DESC，遍历顺序为「最新在前」；
		// 展示样例要正序读起来才像对话，所以收集完再反转。
		for _, m := range msgs {
			text := strings.TrimSpace(m.Content)
			if text == "" {
				continue
			}
			if m.Sender == "me" {
				if len(myLines) < rehearsalMaxMyLines {
					myLines = append(myLines, preview(text, 60))
				}
			} else if len(otherLines) < rehearsalMaxSampleLines {
				otherLines = append(otherLines, preview(text, 80))
			}
		}
	}
	reverseStrings(otherLines)
	reverseStrings(myLines)
	out.SampleLines = otherLines
	out.HasMessages = len(otherLines) > 0 || len(myLines) > 0
	if len(myLines) > 0 {
		out.MyStyle = strings.Join(myLines, " / ")
	}
	return out, nil
}

func reverseStrings(ss []string) {
	for i, j := 0, len(ss)-1; i < j; i, j = i+1, j-1 {
		ss[i], ss[j] = ss[j], ss[i]
	}
}

// CleanRehearsalTurns 清洗前端传来的对话历史：
// 丢掉 role 非法/内容为空的条目，截断超长文本，只保留最近 rehearsalMaxTurns 条。
// 不这么做的话，一个构造过的请求就能把几万字塞进 prompt——既炸费用也可能炸上下文窗口。
func CleanRehearsalTurns(in []RehearsalTurn) []RehearsalTurn {
	out := make([]RehearsalTurn, 0, len(in))
	for _, t := range in {
		role := strings.TrimSpace(t.Role)
		if role != "me" && role != "other" {
			continue
		}
		text := strings.TrimSpace(t.Text)
		if text == "" {
			continue
		}
		if utf8.RuneCountInString(text) > rehearsalMaxTextRunes {
			text = truncateRunes(text, rehearsalMaxTextRunes)
		}
		out = append(out, RehearsalTurn{Role: role, Text: text})
	}
	if len(out) > rehearsalMaxTurns {
		out = out[len(out)-rehearsalMaxTurns:]
	}
	return out
}

// CleanRehearsalScene 归一化场景描述。
func CleanRehearsalScene(scene string) string {
	scene = strings.TrimSpace(scene)
	if utf8.RuneCountInString(scene) > rehearsalMaxSceneRunes {
		scene = truncateRunes(scene, rehearsalMaxSceneRunes)
	}
	return scene
}

// rehearsalTranscript 把对话历史渲染成 prompt 里的可读文本。
func rehearsalTranscript(name string, turns []RehearsalTurn) string {
	if len(turns) == 0 {
		return "（还没有人开口，由你先说）"
	}
	var b strings.Builder
	for _, t := range turns {
		who := "用户"
		if t.Role == "other" {
			who = name
		}
		b.WriteString(who)
		b.WriteString("：")
		b.WriteString(t.Text)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// rehearsalProfileBlock 画像 + 真实说话样例，两个 prompt 共用。
func rehearsalProfileBlock(rc *RehearsalContext) string {
	hintBlock := "（该联系人暂无画像信息，只能按常理推测，回复要保守、多反问）"
	if len(rc.Hints) > 0 {
		hintBlock = strings.Join(rc.Hints, "；")
	}
	sampleBlock := "（暂无历史消息可参考）"
	if len(rc.SampleLines) > 0 {
		lines := make([]string, 0, len(rc.SampleLines))
		for _, l := range rc.SampleLines {
			lines = append(lines, "- "+l)
		}
		sampleBlock = strings.Join(lines, "\n")
	}
	return fmt.Sprintf("【画像】%s\n\n【%s 真实聊天中的说话样例，模仿这个语气、句长和用词习惯】\n%s",
		hintBlock, rc.Name, sampleBlock)
}

// RehearsalReply 让模型以对方身份回应一条。turns 为空时表示由对方先开口。
func RehearsalReply(db *sql.DB, llm *LLMClient, contactID int64, scene string, turns []RehearsalTurn) (string, string, error) {
	if llm == nil {
		return "", "", fmt.Errorf("未配置模型接口，无法预演")
	}
	rc, err := LoadRehearsalContext(db, contactID)
	if err != nil {
		return "", "", err
	}
	scene = CleanRehearsalScene(scene)
	turns = CleanRehearsalTurns(turns)
	if scene == "" {
		return "", "", fmt.Errorf("请先描述预演场景")
	}

	prompt := fmt.Sprintf(`你是微信关系助手。用户要跟联系人「%s」预演一场重要对话，你来扮演「%s」本人。
这是一场演练，你的回复不会真的发给任何人，只用于帮用户练习。

%s

【用户平时的说话风格】%s

【本次预演场景，由用户设定】%s

【已有对话，按时间先后】
%s

现在请你以「%s」的身份回应。要求：
1. 第一人称，就是本人在发微信，一次只发一条，不超过 120 个字；
2. 严格贴合上面的性格、语气、回复长短倾向和口头禅——画像里说他回复简短，就绝不要写长篇大论；
3. 不许编造画像和场景里没有的具体事实（人名、金额、日期、承诺、旧事）；拿不准就含糊带过或者反问；
4. 该有情绪就有情绪：用户踩到雷点或说话不当时，要真实表现出不悦、敷衍、回避甚至反击，不要为了配合用户而软化立场；
5. 不要替用户说话，不要写旁白、动作描写、括号里的心理活动，不要提到自己是 AI 或在演练；
6. emotion 填你此刻的情绪，两到四个字。

严格输出如下 JSON（不要输出任何其他内容）：
{"text":"你要发的那条微信","emotion":"情绪"}`,
		rc.Name, rc.Name, rehearsalProfileBlock(rc), rc.MyStyle, scene,
		rehearsalTranscript(rc.Name, turns), rc.Name)

	ctx, cancel := context.WithTimeout(context.Background(), rehearsalTimeout)
	defer cancel()
	raw, err := llm.CallContext(ctx, prompt)
	if err != nil {
		return "", "", err
	}
	var out struct {
		Text    string `json:"text"`
		Emotion string `json:"emotion"`
	}
	if err := json.Unmarshal([]byte(ExtractJSON(raw)), &out); err != nil {
		return "", "", fmt.Errorf("解析模型回复失败: %w", err)
	}
	text := strings.TrimSpace(out.Text)
	if text == "" {
		return "", "", fmt.Errorf("模型没有返回可用内容")
	}
	return truncateRunes(text, rehearsalReplyMaxRunes),
		truncateRunes(strings.TrimSpace(out.Emotion), 12), nil
}

// ReviewRehearsal 预演结束后，让模型退出角色、以沟通顾问身份复盘用户的表现。
func ReviewRehearsal(db *sql.DB, llm *LLMClient, contactID int64, scene string, turns []RehearsalTurn) (*RehearsalReview, error) {
	if llm == nil {
		return nil, fmt.Errorf("未配置模型接口，无法复盘")
	}
	rc, err := LoadRehearsalContext(db, contactID)
	if err != nil {
		return nil, err
	}
	scene = CleanRehearsalScene(scene)
	turns = CleanRehearsalTurns(turns)
	if scene == "" {
		return nil, fmt.Errorf("请先描述预演场景")
	}
	mine := 0
	for _, t := range turns {
		if t.Role == "me" {
			mine++
		}
	}
	if mine == 0 {
		return nil, fmt.Errorf("你还没说过话，没有可复盘的内容")
	}

	prompt := fmt.Sprintf(`你是资深沟通顾问。用户刚跟「%s」做完一场对话预演，现在请你退出角色，客观复盘用户的表现。

%s

【用户设定的场景】%s

【完整预演对话，"用户"是用户本人说的，"%s"是模型扮演的】
%s

请复盘。要求：
1. 只评价"用户"说的话，别把扮演方的回复算成用户的问题；
2. 具体、可操作：指出是哪句原话有问题、为什么、该怎么改，不要空泛地说"要多倾听""要注意语气"；
3. risks 写这么说明天真去谈可能踩的坑；suggestions 给能直接照着发出去的替代话术；
4. score 是 0~10 的整数，衡量用户这场沟通达成目标的可能性，严格打分，不要一律给高分；
5. 每个分组最多 %d 条，按重要性排序，没有就留空数组。

严格输出如下 JSON（不要输出任何其他内容）：
{"summary":"一句话总体评价","good":["做得好的地方"],"bad":["有问题的一句话+为什么"],"risks":["风险"],"suggestions":["替代话术"],"score":6}`,
		rc.Name, rehearsalProfileBlock(rc), scene, rc.Name,
		rehearsalTranscript(rc.Name, turns), rehearsalMaxListItems)

	ctx, cancel := context.WithTimeout(context.Background(), rehearsalTimeout)
	defer cancel()
	raw, err := llm.CallContext(ctx, prompt)
	if err != nil {
		return nil, err
	}
	var out struct {
		Summary     string   `json:"summary"`
		Good        []string `json:"good"`
		Bad         []string `json:"bad"`
		Risks       []string `json:"risks"`
		Suggestions []string `json:"suggestions"`
		Score       int      `json:"score"`
	}
	if err := json.Unmarshal([]byte(ExtractJSON(raw)), &out); err != nil {
		return nil, fmt.Errorf("解析复盘结果失败: %w", err)
	}
	// 一律返回 [] 而不是 null，前端 v-for 才不会炸
	rev := &RehearsalReview{
		Summary:     strings.TrimSpace(out.Summary),
		Good:        cleanList(out.Good, 200),
		Bad:         cleanList(out.Bad, 200),
		Risks:       cleanList(out.Risks, 200),
		Suggestions: cleanList(out.Suggestions, 300),
		Score:       out.Score,
	}
	if rev.Score < 0 {
		rev.Score = 0
	}
	if rev.Score > 10 {
		rev.Score = 10
	}
	if rev.Summary == "" && len(rev.Good)+len(rev.Bad)+len(rev.Suggestions) == 0 {
		return nil, fmt.Errorf("模型没有返回可用的复盘内容")
	}
	return rev, nil
}

// cleanList 去空、截断、限量。
func cleanList(in []string, maxRunes int) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		out = append(out, truncateRunes(s, maxRunes))
		if len(out) >= rehearsalMaxListItems {
			break
		}
	}
	return out
}
