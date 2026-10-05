package main

// v5.1.0 插件化分析引擎（第一阶）：LLM 提示词模板外置。
//
// 设计铁律（与计划一致）：
//   - 模板变量用字面占位符 {{key}}，不用 text/template：渲染 = 对已声明变量做单趟
//     精确字符串替换（strings.NewReplacer），杜绝模板注入、结果确定、校验简单。
//   - 内置默认 = 逐字迁移现硬编码文案（prompts/*.txt，经 //go:embed 内嵌）；
//     覆盖项存 SQLite（prompt_templates 表，懒建、不进版本化 migrate）。
//   - 读取优先级：DB 覆盖 → 内嵌默认；覆盖非法（空/超限/缺必填变量/含未知变量）
//     记日志并回退默认，绝不因坏模板阻断业务。
//   - 单连接池：只在读取覆盖时取一层 dbMu（不可重入），故 RenderPrompt 必须在
//     「锁外」构建 prompt 的站点调用——与 loadAssistantSettings 同一约束。
//   - 不做进程内缓存：LLM 调用稀少，每次读一行主键即可实现真·热加载，且多库
//     （测试）场景下全局缓存不安全。

import (
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"strings"
)

//go:embed prompts
var promptsFS embed.FS

// maxPromptTemplateBytes 是单个覆盖模板的字节上限（8KB，足够任何提示词）。
const maxPromptTemplateBytes = 8 << 10

// PromptSpec 描述一个可外置的提示词模板：注册表是 UI / 校验 / 渲染的单一事实源。
type PromptSpec struct {
	Key     string   // 模板唯一键，同时是 prompts/<Key>.txt 文件名与 prompt_templates.template_key
	Title   string   // 人类可读标题（管理界面展示）
	Feature string   // 所属功能（分组展示）
	Vars    []string // 该模板必须出现的占位符变量名
	Default string   // 内嵌默认文案（init 从 prompts/<Key>.txt 装配，逐字迁移自现硬编码）
}

// promptSpecMeta 声明全部 15 个模板的元信息；Default 由 init() 从内嵌文件装配。
var promptSpecMeta = []PromptSpec{
	{Key: "profile_update", Title: "画像更新", Feature: "画像生成", Vars: []string{"contactName", "oldJSON", "messages"}},
	{Key: "profile_supplement", Title: "手动补充画像", Feature: "画像生成", Vars: []string{"contactName", "oldJSON", "userNote"}},
	{Key: "profile_change_summary", Title: "画像变化摘要", Feature: "画像生成", Vars: []string{"oldJSON", "newJSON"}},
	{Key: "intent_analysis", Title: "消息意图分析", Feature: "意图分析", Vars: []string{"profileSummary", "messages", "newMessage"}},
	{Key: "ask_keywords", Title: "提问关键词提取", Feature: "智能问答", Vars: []string{"question"}},
	{Key: "ask_answer", Title: "历史问答作答", Feature: "智能问答", Vars: []string{"name", "summary", "captioned", "question"}},
	{Key: "summary", Title: "回顾摘要", Feature: "智能摘要", Vars: []string{"name", "days", "summary", "captioned", "topicsMax", "todoTextMax", "todosMax"}},
	{Key: "emotion_analyze", Title: "情绪分析", Feature: "关系助手", Vars: []string{"name", "messages", "emotionHint"}},
	{Key: "followup_extract", Title: "待跟进事项抽取", Feature: "待跟进", Vars: []string{"name", "maxPerContact", "messages"}},
	{Key: "relationship_draft", Title: "关系维护开场白", Feature: "关系维护", Vars: []string{"name", "summary", "kind"}},
	{Key: "weekly_plan", Title: "每周行动开场白", Feature: "每周计划", Vars: []string{"name", "summary", "reasonHint"}},
	{Key: "simulate_reply", Title: "回复推演", Feature: "对话演练", Vars: []string{"name", "summary", "convo", "draft"}},
	{Key: "calendar_blessing", Title: "节日祝福语", Feature: "日历祝福", Vars: []string{"name", "kind", "dateLabel", "when", "hintBlock", "styleHint", "maxCount"}},
	{Key: "topic_evolution", Title: "主题演化聚类", Feature: "主题演化", Vars: []string{"name", "days", "captioned", "prevTopics"}},
	{Key: "relationship_narrative", Title: "关系叙事生成", Feature: "关系叙事", Vars: []string{"name", "years", "firstTopic", "recentTopics", "msgCount", "captioned"}},
}

// promptRegistry 是 key → 完整 PromptSpec（含 Default）的索引，作为唯一事实源。
var promptRegistry = map[string]PromptSpec{}

func init() {
	for _, spec := range promptSpecMeta {
		raw, err := promptsFS.ReadFile("prompts/" + spec.Key + ".txt")
		if err != nil {
			// 内嵌文件在编译期即可保证存在；缺失属构建期不变量破坏，尽早失败。
			panic("prompts: 缺少内嵌默认模板 " + spec.Key + ": " + err.Error())
		}
		// 末尾换行是编辑器/POSIX 习惯，非文案的一部分：渲染须与原 fmt.Sprintf
		// 逐字节一致，而原串结尾无换行，故 TrimRight 归一化掉尾部 \r\n。
		spec.Default = strings.TrimRight(string(raw), "\r\n")
		promptRegistry[spec.Key] = spec
	}
}

// RenderPrompt 渲染指定模板：DB 覆盖优先（校验通过），否则内嵌默认；
// 用字面占位符 {{key}} 把已预先算好的变量值单趟替换进去。
// 未知 key 返回错误；坏覆盖回退默认，不返回错误（不阻断业务）。
//
// 约束：内部会取 dbMu 读覆盖，故调用点必须处于「锁外」构建 prompt 的位置。
func RenderPrompt(db *sql.DB, key string, vars map[string]string) (string, error) {
	spec, ok := promptRegistry[key]
	if !ok {
		return "", fmt.Errorf("未知提示词模板: %s", key)
	}
	content := resolvePromptContent(db, key, spec)
	return renderTokens(content, spec.Vars, vars), nil
}

// resolvePromptContent 取覆盖内容并校验，非法/缺失回退内嵌默认。
func resolvePromptContent(db *sql.DB, key string, spec PromptSpec) string {
	content, ok := getPromptOverride(db, key)
	if !ok {
		return spec.Default
	}
	if err := validateOverride(spec, content); err != nil {
		slog.Warn("提示词覆盖模板校验失败，回退内置默认", "key", key, "err", err)
		return spec.Default
	}
	return content
}

// renderTokens 用单趟 NewReplacer 把声明过的 {{var}} 换成 vars 里的值。
// 单趟替换保证 value 内的 {{x}} 不会被二次扫描（防级联注入）。
func renderTokens(content string, declared []string, vars map[string]string) string {
	oldnew := make([]string, 0, len(declared)*2)
	for _, v := range declared {
		oldnew = append(oldnew, "{{"+v+"}}", vars[v])
	}
	return strings.NewReplacer(oldnew...).Replace(content)
}

// validateOverride 校验一段覆盖模板：非空、不超限、变量集合与声明完全一致
// （每个必填 {{key}} 都要出现、且不含未声明或游离的 {{/}}）。
func validateOverride(spec PromptSpec, content string) error {
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("模板内容不能为空")
	}
	if len(content) > maxPromptTemplateBytes {
		return fmt.Errorf("模板内容超过上限 %d 字节", maxPromptTemplateBytes)
	}
	tokens, err := extractTokens(content)
	if err != nil {
		return err
	}
	declared := map[string]bool{}
	for _, v := range spec.Vars {
		declared[v] = true
	}
	seen := map[string]bool{}
	for _, t := range tokens {
		if !declared[t] {
			return fmt.Errorf("包含未声明的变量 {{%s}}", t)
		}
		seen[t] = true
	}
	for _, v := range spec.Vars {
		if !seen[v] {
			return fmt.Errorf("缺少必填变量 {{%s}}", v)
		}
	}
	return nil
}

// extractTokens 扫描内容里所有成对的 {{name}}；发现未闭合 {{ 或多余 }} 即报错。
func extractTokens(content string) ([]string, error) {
	var out []string
	for i := 0; i < len(content); {
		switch {
		case strings.HasPrefix(content[i:], "{{"):
			end := strings.Index(content[i+2:], "}}")
			if end < 0 {
				return nil, fmt.Errorf("模板存在未闭合的 {{")
			}
			out = append(out, content[i+2:i+2+end])
			i += 2 + end + 2
		case strings.HasPrefix(content[i:], "}}"):
			return nil, fmt.Errorf("模板存在多余的 }}")
		default:
			i++
		}
	}
	return out, nil
}

// ---------- 数据表（懒建，不进版本化 migrate，不 bump user_version）----------

// ensurePromptTemplates 建提示词覆盖表（幂等，DDL 用 IF NOT EXISTS）。
func ensurePromptTemplates(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS prompt_templates (
		template_key TEXT PRIMARY KEY,
		content TEXT NOT NULL,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	return err
}

// getPromptOverride 读一行覆盖（持 dbMu 单层锁）；表缺失或无记录返回 ("", false)。
func getPromptOverride(db *sql.DB, key string) (string, bool) {
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "prompt_templates") {
		return "", false
	}
	var content string
	if err := db.QueryRow(`SELECT content FROM prompt_templates WHERE template_key=?`, key).Scan(&content); err != nil {
		return "", false
	}
	return content, true
}

// PromptTemplateInfo 是管理界面列表项（不含正文）。
type PromptTemplateInfo struct {
	Key       string   `json:"key"`
	Title     string   `json:"title"`
	Feature   string   `json:"feature"`
	Vars      []string `json:"vars"`
	IsCustom  bool     `json:"isCustom"`
	UpdatedAt string   `json:"updatedAt"`
}

// listPromptTemplates 按注册表顺序列出全部模板的元信息 + 是否已被自定义。
func listPromptTemplates(db *sql.DB) []PromptTemplateInfo {
	overrides := loadAllOverrideMeta(db)
	list := make([]PromptTemplateInfo, 0, len(promptSpecMeta))
	for _, spec := range promptSpecMeta {
		info := PromptTemplateInfo{
			Key:     spec.Key,
			Title:   spec.Title,
			Feature: spec.Feature,
			Vars:    spec.Vars,
		}
		if ua, ok := overrides[spec.Key]; ok {
			info.IsCustom = true
			info.UpdatedAt = ua
		}
		list = append(list, info)
	}
	return list
}

// loadAllOverrideMeta 一次取回 key→updated_at（表缺失返回空 map）。
func loadAllOverrideMeta(db *sql.DB) map[string]string {
	out := map[string]string{}
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "prompt_templates") {
		return out
	}
	rows, err := db.Query(`SELECT template_key, COALESCE(updated_at,'') FROM prompt_templates`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, ua string
		if err := rows.Scan(&k, &ua); err == nil {
			out[k] = ua
		}
	}
	return out
}

// PromptTemplateDetail 是单个模板的完整信息：默认文案 + 当前生效文案 + 是否自定义。
type PromptTemplateDetail struct {
	PromptTemplateInfo
	Default   string `json:"default"`
	Effective string `json:"effective"`
	IsCustom  bool   `json:"isCustom"`
	Override  string `json:"override,omitempty"`
}

// getPromptDetail 取单个模板详情；未知 key 返回 false。
func getPromptDetail(db *sql.DB, key string) (PromptTemplateDetail, bool) {
	spec, ok := promptRegistry[key]
	if !ok {
		return PromptTemplateDetail{}, false
	}
	content, hasOverride := getPromptOverride(db, key)
	d := PromptTemplateDetail{
		PromptTemplateInfo: PromptTemplateInfo{
			Key:     spec.Key,
			Title:   spec.Title,
			Feature: spec.Feature,
			Vars:    spec.Vars,
		},
		Default:   spec.Default,
		Effective: spec.Default,
	}
	if hasOverride && validateOverride(spec, content) == nil {
		d.IsCustom = true
		d.Override = content
		d.Effective = content
	}
	return d, true
}

// savePromptOverride 校验并写入/更新覆盖（持 dbMu，UPSERT）；返回校验错误供 API 转 400。
func savePromptOverride(db *sql.DB, key, content string) error {
	spec, ok := promptRegistry[key]
	if !ok {
		return fmt.Errorf("未知提示词模板: %s", key)
	}
	if err := validateOverride(spec, content); err != nil {
		return err
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err := db.Exec(`INSERT INTO prompt_templates (template_key, content, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(template_key) DO UPDATE SET content=excluded.content, updated_at=CURRENT_TIMESTAMP`,
		key, content)
	return err
}

// resetPromptOverride 删除覆盖，恢复内嵌默认（持 dbMu）。
func resetPromptOverride(db *sql.DB, key string) error {
	if _, ok := promptRegistry[key]; !ok {
		return fmt.Errorf("未知提示词模板: %s", key)
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err := db.Exec(`DELETE FROM prompt_templates WHERE template_key=?`, key)
	return err
}
