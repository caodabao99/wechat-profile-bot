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
	case len(sub) == 1 && sub[0] == "fts-status" && r.Method == http.MethodGet:
		s.hSearchFTSStatus(w, r)
	case len(sub) == 1 && sub[0] == "fts-rebuild" && r.Method == http.MethodPost:
		s.hSearchFTSRebuild(w, r)
	default:
		writeErr(w, http.StatusNotFound, "未知搜索接口")
	}
}

// hSearchFTSStatus 回报全文索引可用性，供网页端展示「已启用 / 降级」。
func (s *apiServer) hSearchFTSStatus(w http.ResponseWriter, r *http.Request) {
	status := "disabled" // 未建立：搜索走 LIKE
	switch {
	case ftsMessagesEnabled.Load() && ftsArchiveEnabled.Load():
		status = "enabled"
	case ftsMessagesEnabled.Load():
		status = "partial" // 活跃表已启用、归档表未启用
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"messages": ftsMessagesEnabled.Load(),
		"archive":  ftsArchiveEnabled.Load(),
		"status":   status,
		"degraded": !ftsMessagesEnabled.Load(), // 活跃表 FTS 不可用即整体降级为 LIKE
	})
}

// hSearchFTSRebuild 手动重建（回填）全文索引。用于启动时 FTS 建立失败后的修复，
// 或迁移后一次性重建。rebuild 会读全表，耗时随数据量增长，故仅 POST、且复用登录鉴权。
func (s *apiServer) hSearchFTSRebuild(w http.ResponseWriter, r *http.Request) {
	msgOK, archOK := rebuildFTS(s.db)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":       msgOK,
		"messages": msgOK,
		"archive":  archOK,
	})
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
		Cursor:         q.Get("cursor"),
		// §11.1 默认不算总数（不执行全表 COUNT(*)），只有显式 includeTotal=1/true 才回 total。
		IncludeTotal: q.Get("includeTotal") == "1" || q.Get("includeTotal") == "true",
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
