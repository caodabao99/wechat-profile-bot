package main

// v5.1.0 提示词引擎测试：
//   - golden：对每个 key，用同一组变量分别走「迁移前 fmt.Sprintf」与「RenderPrompt 默认」，
//     断言逐字节一致（保证纯重构零行为变化）；
//   - 注册表自洽：每个声明变量都在默认模板出现、且默认模板不含未声明的游离 {{}}；
//   - 覆盖生命周期：保存覆盖生效 → 重置回默认；
//   - 校验拒绝：空 / 超限 / 缺必填 / 含未知变量；
//   - 坏覆盖回退：绕过校验直接塞入非法覆盖，渲染仍回退内置默认，绝不阻断业务。

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// goldRender 渲染并失败即 fatal。
func goldRender(t *testing.T, db *sql.DB, key string, vars map[string]string) string {
	t.Helper()
	got, err := RenderPrompt(db, key, vars)
	if err != nil {
		t.Fatalf("RenderPrompt(%s) 报错: %v", key, err)
	}
	return got
}

func TestPromptsGolden(t *testing.T) {
	db := regressionDB(t)
	if err := ensurePromptTemplates(db); err != nil {
		t.Fatal(err)
	}

	// —— profile_update ——
	{
		v := map[string]string{"contactName": "小李", "oldJSON": `{"summary":"技术同事"}`, "messages": "对方：在吗\n我：在的"}
		expected := fmt.Sprintf(`你是人物画像分析助手。请根据【旧画像】和【新增聊天记录】，更新该联系人的人物画像。
要求：
1. 只基于聊天记录中的证据，不要编造。
2. 如果新信息与旧画像冲突，以新信息为准。
3. 输出完整 JSON，结构同旧画像。
4. intent_patterns 的键（意图名称）必须用中文，如"分享资源"、"技术支持"、"闲聊问候"。
5. summary 字段用 100 字以内概括这个人的核心特征。

画像 JSON 结构如下：
{
  "basic_info": {"occupation": "", "location": "", "important_dates": []},
  "personality": [],
  "communication_style": {"reply_length": "", "tone": "", "frequent_phrases": [], "emoji_usage": "", "initiative": ""},
  "interests": [],
  "emotional_patterns": {"stressors": [], "comfort_topics": [], "when_upset": ""},
  "relationship": {"closeness": "", "recent_events": [], "interaction_pattern": ""},
  "intent_patterns": {"中文意图名称": "描述该意图的典型表现"},
  "important_facts": [],
  "summary": ""
}

联系人：%s
【旧画像】
%s
【新增聊天记录】
%s
请只输出 JSON，不要其他内容。`, v["contactName"], v["oldJSON"], v["messages"])
		assertGolden(t, "profile_update", expected, goldRender(t, db, "profile_update", v))
	}

	// —— profile_supplement ——
	{
		v := map[string]string{"contactName": "小王", "oldJSON": "{}", "userNote": "他下个月出差去上海"}
		expected := fmt.Sprintf(`你是人物画像分析助手。用户手动提供了关于联系人的新信息，请把这些信息合并到现有画像中。
要求：
1. 用户手动提供的信息是准确的第一手资料，优先级最高，直接更新到画像对应字段。
2. 不要删除原有画像中没有被新信息覆盖的内容。
3. 日期类信息（如生日、纪念日）严格按照用户提供的精度记录：提供了年月日就记年月日，只提供月日就只记月日，不要自行补全或猜测缺失的部分。
4. 输出完整 JSON，结构同旧画像。
5. summary 字段用 100 字以内概括这个人的核心特征。

联系人：%s
【旧画像】
%s
【用户手动补充的信息】
%s
请只输出 JSON，不要其他内容。`, v["contactName"], v["oldJSON"], v["userNote"])
		assertGolden(t, "profile_supplement", expected, goldRender(t, db, "profile_supplement", v))
	}

	// —— profile_change_summary ——
	{
		v := map[string]string{"oldJSON": `{"a":1}`, "newJSON": `{"a":2}`}
		expected := fmt.Sprintf(`对比下面两份人物画像 JSON，用一句中文（30 字以内）概括新画像相对旧画像的主要变化。
如果除了首次生成外没有实质变化，也请简述新增了哪些信息。
只输出 JSON：{"change_summary": "一句话"}
【旧画像】
%s
【新画像】
%s`, v["oldJSON"], v["newJSON"])
		assertGolden(t, "profile_change_summary", expected, goldRender(t, db, "profile_change_summary", v))
	}

	// —— intent_analysis ——
	{
		v := map[string]string{"profileSummary": "爱分享技术文章", "messages": "对方：看下这个链接", "newMessage": "帮我看看这个报错"}
		expected := fmt.Sprintf(`你是聊天分析助手。下面是联系人的人物画像和本次对话，请分析对方最新消息的意图。
【人物画像】
%s
【本次对话】
%s
【当前对方最新消息】
对方：%s
请输出 JSON：
{
  "surface": "表面意思",
  "intent": "潜在意图，从[邀约/试探/求安慰/敷衍/婉拒/分享/日常寒暄/其他]中选择",
  "emotion": "情绪状态",
  "subtext": "潜台词",
  "suggested_replies": [
    {"style": "稳妥得体", "text": "该风格的回复"},
    {"style": "简洁直接", "text": "该风格的回复"},
    {"style": "亲切热情", "text": "该风格的回复"},
    {"style": "委婉留余地", "text": "该风格的回复"}
  ],
  "confidence": 0.0
}
suggested_replies 规则：
1. style 只能从【稳妥得体、简洁直接、亲切热情、委婉留余地】四个名称中原样选择，不得自造名称，四种各给一条，一个都不能少，也不要重复。
2. 四种风格的含义：稳妥得体=礼貌周全有分寸，不犯错的默认选择；简洁直接=最少字数一句话说清，不寒暄；亲切热情=有温度、表达关心、拉近距离；委婉留余地=不把话说死、给对方面子，适合拒绝或敏感话题。
3. 固定按【稳妥得体、简洁直接、亲切热情、委婉留余地】的顺序输出。即使某种风格在当前语境下不是最优，也要写出该风格下最得体、不违和的版本（例如对方求安慰时，简洁直接也要简短而不失温度）。
4. 每条 text 都要真正体现对应风格，四条之间要有可感知的明显差异，而不是换几个字；不要编造事实，不要替用户做承诺。
只输出 JSON，不要其他内容。`, v["profileSummary"], v["messages"], v["newMessage"])
		assertGolden(t, "intent_analysis", expected, goldRender(t, db, "intent_analysis", v))
	}

	// —— ask_keywords（单行、含字面 \n）——
	{
		v := map[string]string{"question": "他上次说要请我看电影是哪个片子"}
		expected := fmt.Sprintf(`从下面的问题里提取 2~6 个用于全文检索微信聊天记录的关键词（名词/地名/事件词为主，可包含同义词以提高召回）。只输出 JSON：{"keywords":["词1","词2"]}。\n问题：%s`, v["question"])
		assertGolden(t, "ask_keywords", expected, goldRender(t, db, "ask_keywords", v))
	}

	// —— ask_answer（单行、含字面 \n）——
	{
		v := map[string]string{"name": "小张", "summary": "同事", "captioned": "[1] 我：早\n[2] 对方：早呀", "question": "他一般几点到公司"}
		expected := fmt.Sprintf(`你在帮助"我"回答关于某个人历史聊天记录的问题。只依据下面给出的、带编号的真实消息原文作答，并在引用某条依据时标注对应编号 [n]。若原文不足以回答，如实说明。不要编造原文里没有的信息。\n对方昵称：%s\n对方画像概要：%s\n\n可引用的历史消息（编号从 1 开始）：\n%s\n\n我的问题：%s\n\n只输出 JSON：{"answer":"你的回答，尽量带上 [n] 出处标注"}`,
			v["name"], v["summary"], v["captioned"], v["question"])
		assertGolden(t, "ask_answer", expected, goldRender(t, db, "ask_answer", v))
	}

	// —— summary（单行、含字面 \n，含 %d）——
	{
		name := "小美"
		summary := "老同学，爱聊旅行"
		captioned := "[1] 对方：国庆去哪玩\n[2] 我：还没想好"
		v := map[string]string{"name": name, "days": "30", "summary": summary, "captioned": captioned, "topicsMax": "8", "todoTextMax": "60", "todosMax": "12"}
		expected := fmt.Sprintf(`你是微信关系管理助手。下面是我（"我"）与联系人「%s」近 %d 天的聊天记录（已按时间正序编号，编号即引用锚点）。请基于原文给我一份结构化回顾，严格只依据记录本身，不臆测、不补充原文里没有的信息。\n对方画像概要：%s\n\n聊天记录（编号从 1 开始）：\n%s\n\n输出要求（严格 JSON，不要任何解释文字）：\n{\n  "overview": "一段 80~200 字的中文总结，讲清这段时间主要聊了什么、有没有悬而未决的事；若引用具体消息，用 [n] 标注出处",\n  "topics": ["3~%d 个话题标签词，短名词为主，例如 旅行/工作/家庭/健康"],\n  "todos": [\n    {"text": "一句中文待办，不超过 %d 字", "owner": "我 或 对方", "ref": "[n] 对应编号"}\n  ]\n}\n若原文里没有明确待办，todos 输出空数组 []。最多 %d 条。`,
			name, 30, summary, captioned, 8, 60, 12)
		assertGolden(t, "summary", expected, goldRender(t, db, "summary", v))
	}

	// —— emotion_analyze（多行、含 {{emotionHint}} 紧跟空行结构）——
	{
		v := map[string]string{"name": "老王", "messages": "我：最近怎么样\n对方：还行吧", "emotionHint": "\n该联系人的已知情绪特征：压力源：加班。\n"}
		expected := fmt.Sprintf(`你是微信关系分析助手。下面是用户（"我"）与联系人「%s」（"对方"）最近一周的聊天记录：

%s
%s
请分析"对方"最近的情绪状态。要求：
1. 只依据聊天记录，不要臆测记录之外的事实；
2. 对方消息大多是事务性内容（约时间、收发文件等）时，emotion 用"中性"、alert 用 false；
3. 只有出现明显负面情绪（持续低落、烦躁、冷淡、抱怨）且值得用户主动关心时 alert 才为 true。

严格输出如下 JSON（不要输出任何其他内容）：
{"emotion":"积极|中性|低落|愤怒|焦虑 五选一","score":0到100的整数（情绪积极程度）,"summary":"50字以内概括对方近期状态","advice":"50字以内给用户的具体建议","alert":true或false}`,
			v["name"], v["messages"], v["emotionHint"])
		assertGolden(t, "emotion_analyze", expected, goldRender(t, db, "emotion_analyze", v))
	}

	// —— followup_extract（多行、含 %d）——
	{
		name := "小陈"
		messages := "我[2025-06-01 10:00]: 明天发给你"
		v := map[string]string{"name": name, "maxPerContact": "5", "messages": messages}
		expected := fmt.Sprintf(`你是微信关系管理助手。下面是用户（"我"）与联系人「%s」（"对方"）最近的聊天记录。
请从中找出"我"容易忘掉、需要主动跟进的事项，只挑这三类：
1. kind="question"：对方明确问了"我"一个问题，而后续记录里"我"没有给出答复；
2. kind="promise"："我"答应/承诺过要做但记录里看不出已经完成的事（例如"我明天发给你""下周请你吃饭"）；
3. kind="money"：涉及借钱、还钱、代付、转账金额的事项，务必在 amount 里写清金额和方向（如"借给对方500元""对方欠我200元"）。

硬性要求：
- 只依据记录本身，不要臆测；记录里已经解决的、已经回复过的，一律不要输出；
- 寒暄、闲聊、纯事务性对话不算；
- content 用一句中文说清"要做什么"，不超过 40 字，不要复述原文；
- sourceTime 填对应消息的时间（照抄方括号里的时间即可）；
- 最多输出 %d 条，按重要程度排序；确实没有就输出空数组。

聊天记录：
%s

严格输出如下 JSON（不要输出任何其他内容）：
{"items":[{"kind":"question|promise|money","content":"要跟进的事","amount":"金额与方向，没有就留空","sourceTime":"消息时间"}]}`,
			name, 5, messages)
		assertGolden(t, "followup_extract", expected, goldRender(t, db, "followup_extract", v))
	}

	// —— relationship_draft（单行、无 \n）——
	{
		v := map[string]string{"name": "小刘", "summary": "前同事，偶尔约饭", "kind": "cooling"}
		expected := fmt.Sprintf(`你在帮用户维护微信关系。对方昵称：%s。对方画像概要：%s。当前状态：%s。请生成 1 句自然的、不油腻、不像群发的中文开场白，用于重新开启聊天。只输出 JSON：{"draft":"开场白内容"}`,
			v["name"], v["summary"], v["kind"])
		assertGolden(t, "relationship_draft", expected, goldRender(t, db, "relationship_draft", v))
	}

	// —— weekly_plan（双引号拼接、无 \n）——
	{
		v := map[string]string{"name": "小赵", "summary": "大学室友", "reasonHint": "已很久没有互动"}
		expected := fmt.Sprintf("你是一位社交顾问。用户想维护与「%s」的关系（画像：%s）。原因：%s。请为用户生成一句简短自然的微信开场白（不超过50字），像朋友之间随口发的那种，不要写得太正式。只输出这句话，不要解释。",
			v["name"], v["summary"], v["reasonHint"])
		assertGolden(t, "weekly_plan", expected, goldRender(t, db, "weekly_plan", v))
	}

	// —— simulate_reply（单行、含字面 \n）——
	{
		v := map[string]string{"name": "小孙", "summary": "客户", "convo": "对方：方案发我看看\n我：稍等", "draft": "今天下午给您"}
		expected := fmt.Sprintf(`你在帮助"我"预判一段微信对话。请根据对方画像与最近对话，推演"我"发出下面这句草稿后，对方最可能的 2~3 种反应。\n对方昵称：%s\n对方画像概要：%s\n最近对话：\n%s\n\n我的草稿：%s\n\n要求：贴合对方性格与当前关系，不要鸡汤、不像群发。只输出 JSON：{"replies":[{"text":"对方的回复","mood":"积极|中性|消极","rationale":"为什么会这样反应，一句话"}]}`,
			v["name"], v["summary"], v["convo"], v["draft"])
		assertGolden(t, "simulate_reply", expected, goldRender(t, db, "simulate_reply", v))
	}

	// —— calendar_blessing（多行、含 %d 与 %s%s 相邻）——
	{
		name := "小周"
		kind := "生日"
		dateLabel := "5月20日"
		when := "5 天后（2025-05-20）"
		hintBlock := "关系概括：好友；兴趣爱好：登山"
		styleHint := "生日快乐 / 好久不见"
		v := map[string]string{"name": name, "kind": kind, "dateLabel": dateLabel, "when": when, "hintBlock": hintBlock, "styleHint": styleHint, "maxCount": "3"}
		expected := fmt.Sprintf(`你是微信关系助手。联系人「%s」的%s%s，就是%s。

已知画像信息：%s

用户（"我"）平时的说话风格参考：%s

请替用户起草 %d 条微信祝福语。要求：
1. 三条风格各不相同：第一条走心真诚、第二条轻松俏皮、第三条简短干脆；
2. 每条不超过 60 个字，口语化，像真人发的微信，不要书面套话、不要"值此…之际"；
3. 可以自然带入画像里的兴趣爱好或近期共同事件，但不要编造画像中没有的事实；
4. 避开已知雷点；不要使用表情符号以外的花哨符号，最多可用 1~2 个常见 emoji。

严格输出如下 JSON（不要输出任何其他内容）：
{"blessings":["第一条","第二条","第三条"]}`,
			name, kind, dateLabel, when, hintBlock, styleHint, 3)
		assertGolden(t, "calendar_blessing", expected, goldRender(t, db, "calendar_blessing", v))
	}
}

func assertGolden(t *testing.T, key, expected, got string) {
	t.Helper()
	if got != expected {
		t.Errorf("[%s] golden 不一致\n--- expected ---\n%q\n--- got ---\n%q", key, expected, got)
	}
}

// TestPromptRegistryConsistency 校验注册表与内嵌默认模板自洽：
// 每个声明变量都在默认模板出现，且默认模板只含声明过的占位符（无游离/未声明）。
func TestPromptRegistryConsistency(t *testing.T) {
	if len(promptRegistry) != 14 {
		t.Fatalf("注册表应有 14 个模板, got %d", len(promptRegistry))
	}
	for _, spec := range promptRegistry {
		spec := spec
		t.Run(spec.Key, func(t *testing.T) {
			tokens, err := extractTokens(spec.Default)
			if err != nil {
				t.Fatalf("默认模板含未配对 {{}}: %v", err)
			}
			declared := map[string]bool{}
			for _, v := range spec.Vars {
				declared[v] = true
			}
			seen := map[string]bool{}
			for _, tk := range tokens {
				if !declared[tk] {
					t.Errorf("默认模板含未声明变量 {{%s}}", tk)
				}
				seen[tk] = true
			}
			for _, v := range spec.Vars {
				if !seen[v] {
					t.Errorf("默认模板缺少声明变量 {{%s}}", v)
				}
			}
		})
	}
}

// TestPromptOverrideLifecycle 覆盖生效 → 读取 → 详情 → 重置回默认。
func TestPromptOverrideLifecycle(t *testing.T) {
	db := regressionDB(t)
	if err := ensurePromptTemplates(db); err != nil {
		t.Fatal(err)
	}
	key := "ask_keywords"
	v := map[string]string{"question": "测试问题"}
	baseDef := goldRender(t, db, key, v)

	custom := "关键词提取自定义模板 {{question}}（额外说明）"
	if err := savePromptOverride(db, key, custom); err != nil {
		t.Fatal(err)
	}
	got := goldRender(t, db, key, v)
	if got == baseDef {
		t.Fatal("保存覆盖后渲染仍等于默认，覆盖未生效")
	}
	if !strings.Contains(got, "自定义模板") || !strings.Contains(got, "测试问题") {
		t.Fatalf("覆盖渲染异常: %q", got)
	}

	detail, ok := getPromptDetail(db, key)
	if !ok || !detail.IsCustom {
		t.Fatalf("详情应标记 isCustom=true, got %+v ok=%v", detail, ok)
	}

	if err := resetPromptOverride(db, key); err != nil {
		t.Fatal(err)
	}
	afterReset := goldRender(t, db, key, v)
	if afterReset != baseDef {
		t.Fatal("重置后未回到内置默认")
	}
	detail2, _ := getPromptDetail(db, key)
	if detail2.IsCustom {
		t.Fatal("重置后详情仍标记 isCustom")
	}
}

// TestPromptValidateRejections 非法覆盖一律被拒绝（不落库）。
func TestPromptValidateRejections(t *testing.T) {
	db := regressionDB(t)
	if err := ensurePromptTemplates(db); err != nil {
		t.Fatal(err)
	}
	key := "profile_update" // 需 contactName/oldJSON/messages
	cases := map[string]string{
		"空内容":    "   ",
		"缺必填变量":  "只有 {{contactName}} 没有别的",
		"含未知变量":  "{{contactName}}{{oldJSON}}{{messages}}{{unknown}}",
		"未闭合大括号": "{{contactName}}{{oldJSON}}{{messages}",
		"多余右括号":  "{{contactName}}{{oldJSON}}{{messages}}}}",
	}
	for name, content := range cases {
		if err := savePromptOverride(db, key, content); err == nil {
			t.Errorf("[%s] 期望校验失败但通过了", name)
		}
	}
	// 超限
	big := strings.Repeat("x", maxPromptTemplateBytes+1) + "{{contactName}}{{oldJSON}}{{messages}}"
	if err := savePromptOverride(db, key, big); err == nil {
		t.Error("超限模板应被拒绝")
	}
	// 未知 key
	if err := savePromptOverride(db, "no_such_key", "anything"); err == nil {
		t.Error("未知 key 保存应报错")
	}
	// 合法覆盖能通过
	if err := savePromptOverride(db, key, "A{{contactName}}B{{oldJSON}}C{{messages}}"); err != nil {
		t.Errorf("合法覆盖被误拒: %v", err)
	}
}

// TestPromptBadOverrideFallsBack 绕过校验直接写入非法覆盖，渲染仍回退内置默认。
func TestPromptBadOverrideFallsBack(t *testing.T) {
	db := regressionDB(t)
	if err := ensurePromptTemplates(db); err != nil {
		t.Fatal(err)
	}
	key := "ask_keywords"
	v := map[string]string{"question": "问题X"}
	goodDefault := goldRender(t, db, key, v)

	// 直接塞入缺必填变量的坏覆盖（绕过 savePromptOverride 校验）
	if _, err := db.Exec(`INSERT INTO prompt_templates (template_key, content) VALUES (?, ?)
		ON CONFLICT(template_key) DO UPDATE SET content=excluded.content`,
		key, "坏模板：缺少占位符"); err != nil {
		t.Fatal(err)
	}
	got := goldRender(t, db, key, v)
	if got != goodDefault {
		t.Fatalf("坏覆盖应回退默认, got %q want %q", got, goodDefault)
	}
}

// TestPromptRenderNoCascade 变量值里含 {{token}} 不被二次替换（单趟 NewReplacer 防级联）。
func TestPromptRenderNoCascade(t *testing.T) {
	db := regressionDB(t)
	if err := ensurePromptTemplates(db); err != nil {
		t.Fatal(err)
	}
	v := map[string]string{"question": "字面量 {{keywords}} 不应被再解析"}
	got := goldRender(t, db, "ask_keywords", v)
	if !strings.Contains(got, "{{keywords}}") {
		t.Fatalf("变量值内的 {{keywords}} 被级联替换了: %q", got)
	}
}

// TestPromptUnknownKey RenderPrompt 对未知 key 返回错误。
func TestPromptUnknownKey(t *testing.T) {
	db := regressionDB(t)
	if _, err := RenderPrompt(db, "does_not_exist", nil); err == nil {
		t.Fatal("未知 key 应返回错误")
	}
}
