package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// 联系人时间线的 HTTP 接口（网页端专用，需登录）。
// 路由挂载：api.go routeContact 里 `case "timeline"`。

// routeContactTimeline /api/contacts/{id}/timeline
func (s *apiServer) routeContactTimeline(w http.ResponseWriter, r *http.Request, id int64) {
	switch r.Method {
	case http.MethodGet:
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		items, err := GetContactTimeline(s.db, id, limit)
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"list": items})
	case http.MethodPost:
		var req struct {
			Title     string `json:"title"`
			Detail    string `json:"detail"`
			EventTime string `json:"eventTime"` // 可选：2026-01-02 或 RFC3339，缺省为当前时间
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体解析失败")
			return
		}
		when := time.Time{}
		if t, ok := parseTimeLoose(req.EventTime); ok {
			when = t
		}
		eventID, err := AddContactEvent(s.db, id, req.Title, req.Detail, when)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "id": eventID})
	case http.MethodDelete:
		eventID, err := strconv.ParseInt(r.URL.Query().Get("eventId"), 10, 64)
		if err != nil || eventID <= 0 {
			writeErr(w, http.StatusBadRequest, "无效的事件ID")
			return
		}
		if err := DeleteContactEvent(s.db, id, eventID); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}
