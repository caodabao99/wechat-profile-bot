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
	contactID, _ := strconv.ParseInt(q.Get("contactId"), 10, 64)
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
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
