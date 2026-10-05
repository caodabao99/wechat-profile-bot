package main

// v5.4.0 #5：对话风格镜像（零 LLM、纯本地文本统计、确定性、可离线单测）。
//
// 不是「关系好不好」，而是「我在每段关系里是什么样」：统计用户与某联系人「我方」消息的
//   用词丰富度、平均句长、表情/问号/感叹频率，与用户全局「我方」均值对比，产出「你在 TA 面前的样子」。
//
// 设计铁律：
//   - 零第三方依赖：按 rune 迭代，emoji 以「非 BMP（码点 > 0xFFFF）」启发式计数，不引分词/emoji 库。
//   - 单连接池分层锁：该联系人与全局的「我方」消息各一趟锁内取尽即释放，绝不嵌套、绝不边迭代边写。
//   - 计算即读、不落库、不新增表；样本上限护栏（mirrorContactSampleMax/mirrorGlobalSampleMax）。

import (
	"database/sql"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	mirrorContactSampleMax = 400  // 单联系人「我方」样本上限（取最近若干条）
	mirrorGlobalSampleMax  = 4000 // 全局「我方」基线样本上限
)

// MirrorDimension 一个风格维度（该联系人的值 vs 用户全局基线）。
type MirrorDimension struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Value    float64 `json:"value"`
	Baseline float64 `json:"baseline"`
	Delta    string  `json:"delta"` // 明显高于/略高于/持平/略低于/明显低于
}

// MirrorResponse 风格镜像响应。
type MirrorResponse struct {
	ContactID   int64             `json:"contactId"`
	Name        string            `json:"name"`
	Sample      int               `json:"sample"`
	Dimensions  []MirrorDimension `json:"dimensions"`
	Traits      []string          `json:"traits"` // 「话痨型/简洁型/表情党/问询型/热情型/用词丰富」等
	Note        string            `json:"note"`
	GeneratedAt string            `json:"generatedAt"`
}

// lexicalRichness 纯函数：去重字数 / 总字数（不含空白），0-100。确定性。
func lexicalRichness(texts []string) float64 {
	seen := map[rune]bool{}
	total := 0
	for _, t := range texts {
		for _, r := range t {
			if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
				continue
			}
			seen[r] = true
			total++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(len(seen)) / float64(total) * 100
}

// avgLen 纯函数：每条消息平均字符数（按 rune）。确定性。
func avgLen(texts []string) float64 {
	if len(texts) == 0 {
		return 0
	}
	total := 0
	for _, t := range texts {
		total += utf8.RuneCountInString(t)
	}
	return float64(total) / float64(len(texts))
}

// markerShare 纯函数：含 set 中任意标点的消息占比（0-100）。set 为待匹配字符集合。确定性。
func markerShare(texts []string, set string) float64 {
	if len(texts) == 0 {
		return 0
	}
	hit := 0
	for _, t := range texts {
		if strings.ContainsAny(t, set) {
			hit++
		}
	}
	return float64(hit) / float64(len(texts)) * 100
}

// emojiShare 纯函数：含表情（非 BMP 码点 >0xFFFF）的消息占比（0-100）。零依赖启发式。确定性。
func emojiShare(texts []string) float64 {
	if len(texts) == 0 {
		return 0
	}
	hit := 0
	for _, t := range texts {
		for _, r := range t {
			if r > 0xFFFF {
				hit++
				break
			}
		}
	}
	return float64(hit) / float64(len(texts)) * 100
}

// mirrorDelta 纯函数：value 相对 baseline 的人话差异（固定阈值、确定性）。baseline=0 时按有无判定。
func mirrorDelta(value, baseline float64) string {
	if baseline <= 0 {
		if value > 0 {
			return "独有"
		}
		return "持平"
	}
	r := value / baseline
	switch {
	case r >= 1.5:
		return "明显高于平时"
	case r >= 1.15:
		return "略高于平时"
	case r <= 0.67:
		return "明显低于平时"
	case r <= 0.87:
		return "略低于平时"
	default:
		return "与平时持平"
	}
}

// mirrorTraits 纯函数：由维度对比得出「你在 TA 面前的样子」标签（固定阈值、确定性、去并列）。
func mirrorTraits(dims []MirrorDimension) []string {
	get := func(key string) (MirrorDimension, bool) {
		for _, d := range dims {
			if d.Key == key {
				return d, true
			}
		}
		return MirrorDimension{}, false
	}
	out := []string{}
	if d, ok := get("avgLen"); ok {
		if d.Value >= d.Baseline*1.5 && d.Value > 0 {
			out = append(out, "话痨型")
		} else if d.Baseline > 0 && d.Value <= d.Baseline*0.67 {
			out = append(out, "简洁型")
		}
	}
	if d, ok := get("emoji"); ok && d.Value >= 20 && d.Value >= d.Baseline*1.2 {
		out = append(out, "表情党")
	}
	if d, ok := get("question"); ok && d.Value >= 20 && d.Value >= d.Baseline*1.2 {
		out = append(out, "问询型")
	}
	if d, ok := get("exclaim"); ok && d.Value >= 20 && d.Value >= d.Baseline*1.2 {
		out = append(out, "热情型")
	}
	if d, ok := get("richness"); ok && d.Baseline > 0 && d.Value >= d.Baseline*1.2 {
		out = append(out, "用词更丰富")
	}
	if len(out) == 0 {
		out = append(out, "风格与平时一致")
	}
	return out
}

// loadMeTexts 一趟锁内取「我方」消息文本（contactID<=0 表示全局），按 id 倒序取最近 limit 条后翻回正序。
func loadMeTexts(db *sql.DB, contactID int64, limit int) []string {
	var out []string
	dbMu.Lock()
	var q string
	var args []interface{}
	if contactID > 0 {
		q = `SELECT content FROM messages WHERE contact_id=? AND sender='me' ORDER BY id DESC LIMIT ?`
		args = []interface{}{contactID, limit}
	} else {
		q = `SELECT content FROM messages WHERE sender='me' ORDER BY id DESC LIMIT ?`
		args = []interface{}{limit}
	}
	if r, err := db.Query(q, args...); err == nil {
		for r.Next() {
			var c string
			if r.Scan(&c) == nil {
				out = append(out, c)
			}
		}
		r.Close()
	}
	dbMu.Unlock()
	// 倒序取回，翻成正序（确定性、与阅读顺序一致）。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// buildMirror 组装某联系人的风格镜像。
func buildMirror(db *sql.DB, contactID int64, now time.Time) *MirrorResponse {
	cTexts := loadMeTexts(db, contactID, mirrorContactSampleMax)
	gTexts := loadMeTexts(db, 0, mirrorGlobalSampleMax)

	resp := &MirrorResponse{
		ContactID:   contactID,
		Sample:      len(cTexts),
		Dimensions:  []MirrorDimension{},
		Traits:      []string{},
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
	}
	if len(cTexts) < 3 {
		resp.Note = "该联系人「我方」消息太少，暂无可靠风格画像。"
		return resp
	}

	mk := func(key, label string, v, b float64) MirrorDimension {
		return MirrorDimension{Key: key, Label: label, Value: round2(v), Baseline: round2(b), Delta: mirrorDelta(v, b)}
	}
	resp.Dimensions = []MirrorDimension{
		mk("avgLen", "平均句长", avgLen(cTexts), avgLen(gTexts)),
		mk("richness", "用词丰富度", lexicalRichness(cTexts), lexicalRichness(gTexts)),
		mk("emoji", "表情使用", emojiShare(cTexts), emojiShare(gTexts)),
		mk("question", "提问比例", markerShare(cTexts, "?？"), markerShare(gTexts, "?？")),
		mk("exclaim", "感叹比例", markerShare(cTexts, "!！"), markerShare(gTexts, "!！")),
	}
	resp.Traits = mirrorTraits(resp.Dimensions)
	return resp
}

// hContactMirror GET /api/contacts/{id}/mirror：你在 TA 面前的样子（风格镜像）。
func (s *apiServer) hContactMirror(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	c, err := GetContactByID(s.db, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "联系人不存在")
		return
	}
	resp := buildMirror(s.db, id, time.Now())
	resp.Name = displayName(c)
	// 维度按固定键序输出已在 buildMirror 保证；traits 稳定排序去并列。
	sort.Strings(resp.Traits)
	writeJSON(w, http.StatusOK, resp)
}
