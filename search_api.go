package main

import (
	"net/http"
	"strconv"
)

// 聊天记录搜索的 HTTP 接口（网页端专用，需登录）。
// 路由挂载：api.go route() 里 `case parts[0] == "search"`。

// routeSearch /api/search/{sub}
func (s *apiServer) routeSearch(w http.ResponseWriter, r *http.Request, sub []string) {
	switch {
	case len(sub) == 1 && sub[0] == "messages" && r.Method == http.MethodGet:
		s.hSearchMessages(w, r)
	default:
		writeErr(w, http.StatusNotFound, "未知搜索接口")
	}
}

func (s *apiServer) hSearchMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// 坏参数要明确报 400：静默当成 0 的话，「只搜某个联系人」会悄悄变成全库搜索，
	// offset 变 0 会让"加载更多"从头再来一遍（前端拿到重复数据）
	var contactID int64
	if v := q.Get("contactId"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "contactId 必须是非负整数")
			return
		}
		contactID = n
	}
	var offset, limit int
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "offset 必须是非负整数")
			return
		}
		offset = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "limit 必须是非负整数")
			return
		}
		limit = n
	}
	res, err := SearchMessages(s.db, SearchOptions{
		Query:          q.Get("q"),
		ContactID:      contactID,
		From:           q.Get("from"),
		To:             q.Get("to"),
		IncludeArchive: q.Get("archive") == "1",
		Offset:         offset,
		Limit:          limit,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
