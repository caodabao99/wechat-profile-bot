package main

// v5.0.0 智能分组建议的 HTTP 入口。
//
// 挂在 /api/assistant/tags/* 下：
//   - GET  /api/assistant/tags/suggest?ids=1,2,3&perContactMax=4
//   - POST /api/assistant/tags/apply   body {"items":[{"contactId":1,"tagName":"技术"}]}
//
// 铁律：只读不写 suggest；apply 内部 CreateTag 幂等 + INSERT OR IGNORE。
// ids 缺省时后端会自行按「未合并活跃联系人」+ suggestMaxContacts 护栏选样。

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxTagsuggestBodyBytes = 64 << 10

func (s *apiServer) routeAssistantTags(w http.ResponseWriter, r *http.Request, sub []string) {
	if len(sub) == 0 {
		writeErr(w, http.StatusNotFound, "未知接口")
		return
	}
	switch sub[0] {
	case "suggest":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hTagsSuggest(w, r)
	case "apply":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hTagsApply(w, r)
	case "conflicts":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hTagsConflicts(w, r)
	default:
		writeErr(w, http.StatusNotFound, "未知接口: /api/assistant/tags/"+sub[0])
	}
}

func (s *apiServer) hTagsSuggest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var ids []int64
	if raw := strings.TrimSpace(q.Get("ids")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			id, err := strconv.ParseInt(part, 10, 64)
			if err != nil || id <= 0 {
				writeErr(w, http.StatusBadRequest, "ids 参数格式错误，应形如 1,2,3")
				return
			}
			ids = append(ids, id)
		}
	}
	perMax := suggestDefaultPerMax
	if v := strings.TrimSpace(q.Get("perContactMax")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "perContactMax 需为正整数")
			return
		}
		perMax = n
	}
	list, err := SuggestTags(s.db, ids, perMax)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成标签建议失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"suggestions": list,
		"total":       len(list),
	})
}

func (s *apiServer) hTagsApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Items []struct {
			ContactID int64  `json:"contactId"`
			TagName   string `json:"tagName"`
		} `json:"items"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTagsuggestBodyBytes)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	if len(req.Items) == 0 {
		writeErr(w, http.StatusBadRequest, "items 不能为空")
		return
	}
	if len(req.Items) > suggestApplyMaxItems {
		writeErr(w, http.StatusBadRequest, "一次最多采纳 "+strconv.Itoa(suggestApplyMaxItems)+" 条建议")
		return
	}
	affected, err := ApplyTagSuggestions(s.db, req.Items)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"affected": affected})
}

// hTagsConflicts GET /api/assistant/tags/conflicts：检测同联系人相互矛盾的标签，提醒确认。
func (s *apiServer) hTagsConflicts(w http.ResponseWriter, r *http.Request) {
	tags, names := collectAppliedTagsByContact(s.db)
	conflicts := detectTagConflicts(tags, names)
	note := ""
	if len(conflicts) == 0 {
		note = "标签体系自洽，未发现矛盾分组。"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"generatedAt": time.Now().Format("2006-01-02 15:04:05"),
		"conflicts":   conflicts,
		"total":       len(conflicts),
		"note":        note,
	})
}
