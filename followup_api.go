package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"
)

const maxFollowupBodyBytes = 32 << 10

// routeFollowups /api/assistant/followups[/{id}]
func (s *apiServer) routeFollowups(w http.ResponseWriter, r *http.Request, sub []string) {
	if len(sub) == 0 {
		switch r.Method {
		case http.MethodGet:
			s.hFollowupList(w, r)
		case http.MethodPost:
			s.hFollowupAdd(w, r)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		}
		return
	}
	if sub[0] == "scan" {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hFollowupScan(w, r)
		return
	}
	id, err := strconv.ParseInt(sub[0], 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "无效的待跟进事项ID")
		return
	}
	switch r.Method {
	case http.MethodPut, http.MethodPost:
		s.hFollowupSetStatus(w, r, id)
	case http.MethodDelete:
		s.hFollowupDelete(w, r, id)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}

// GET /api/assistant/followups?status=open&limit=100
func (s *apiServer) hFollowupList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	items, err := ListFollowups(s.db, q.Get("status"), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取待跟进列表失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"list": items, "total": len(items)})
}

// POST /api/assistant/followups  {contactId, kind, content, amount, dueDate}
func (s *apiServer) hFollowupAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ContactID int64  `json:"contactId"`
		Kind      string `json:"kind"`
		Content   string `json:"content"`
		Amount    string `json:"amount"`
		DueDate   string `json:"dueDate"` // 可选截止日期 YYYY-MM-DD（空=无截止），格式校验在 AddFollowup
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFollowupBodyBytes)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	id, err := AddFollowup(s.db, req.ContactID, req.Kind, req.Content, req.Amount, req.DueDate)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "id": id})
}

// PUT|POST /api/assistant/followups/{id}  {status: open|done|ignored}
func (s *apiServer) hFollowupSetStatus(w http.ResponseWriter, r *http.Request, id int64) {
	var req struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFollowupBodyBytes)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	if err := SetFollowupStatus(s.db, id, req.Status); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// DELETE /api/assistant/followups/{id}
func (s *apiServer) hFollowupDelete(w http.ResponseWriter, r *http.Request, id int64) {
	if err := DeleteFollowup(s.db, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "删除待跟进事项失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// POST /api/assistant/followups/scan  {contactId?} 立即用 LLM 抽取
// 不传 contactId 时按配置的每日上限扫描最近活跃的一批联系人。
func (s *apiServer) hFollowupScan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ContactID int64 `json:"contactId"`
	}
	// 只容忍真正的空 body（前端不指定联系人时就是空 POST）。解析失败必须报 400：
	// 静默忽略的话，超过 32KB 的坏请求会退化成 contactId=0，也就是「全量扫描」，
	// 一口气把每日上限次数的模型调用全打出去。
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFollowupBodyBytes)).Decode(&req); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	if req.ContactID < 0 {
		writeErr(w, http.StatusBadRequest, "contactId 必须是非负整数")
		return
	}

	if s.llm == nil {
		writeErr(w, http.StatusBadRequest, "未配置模型接口，无法抽取待跟进事项")
		return
	}
	st, err := loadAssistantSettings(s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取助手配置失败: "+err.Error())
		return
	}
	now := time.Now()
	window := st.FollowupWindowDays
	if window <= 0 {
		window = 30
	}

	scanned, added := 0, 0
	if req.ContactID > 0 {
		scanned = 1
		n, err := extractFollowups(r.Context(), s.db, s.llm, req.ContactID, now, window)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "抽取失败: "+err.Error())
			return
		}
		added = n
	} else {
		max := st.FollowupDailyMax
		if max <= 0 {
			max = 8
		}
		scanned, added = RefreshFollowups(s.db, s.llm, now, max, window)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true, "scanned": scanned, "added": added,
	})
}
