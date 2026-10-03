package main

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// 标签相关的 HTTP 接口（网页端专用，均需登录）。
// 路由挂载：api.go route() 里 `case parts[0] == "tags"`，
// 联系人标签在 routeContact 里 `case "tags"` → routeContactTags。

const maxTagBodyBytes = 64 << 10

// routeTags /api/tags/{sub}
func (s *apiServer) routeTags(w http.ResponseWriter, r *http.Request, sub []string) {
	switch {
	case len(sub) == 0 && r.Method == http.MethodGet:
		s.hTagsList(w, r)
	case len(sub) == 0 && r.Method == http.MethodPost:
		s.hTagsCreate(w, r)
	case len(sub) == 1 && sub[0] == "batch" && r.Method == http.MethodPost:
		s.hTagsBatch(w, r)
	case len(sub) == 1:
		id, err := strconv.ParseInt(sub[0], 10, 64)
		if err != nil || id <= 0 {
			writeErr(w, http.StatusBadRequest, "无效的标签ID")
			return
		}
		s.routeTagID(w, r, id)
	default:
		writeErr(w, http.StatusNotFound, "未知标签接口")
	}
}

func (s *apiServer) routeTagID(w http.ResponseWriter, r *http.Request, id int64) {
	switch r.Method {
	case http.MethodPut, http.MethodPost:
		s.hTagRename(w, r, id)
	case http.MethodDelete:
		s.hTagDelete(w, r, id)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}

func (s *apiServer) hTagsList(w http.ResponseWriter, r *http.Request) {
	list, err := ListTags(s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取标签失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"list": list})
}

func (s *apiServer) hTagsCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	tag, err := CreateTag(s.db, req.Name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tag)
}

func (s *apiServer) hTagRename(w http.ResponseWriter, r *http.Request, id int64) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	if err := RenameTag(s.db, id, req.Name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (s *apiServer) hTagDelete(w http.ResponseWriter, r *http.Request, id int64) {
	if err := DeleteTag(s.db, id); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (s *apiServer) hTagsBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ContactIDs []int64 `json:"contactIds"`
		TagIDs     []int64 `json:"tagIds"`
		Remove     bool    `json:"remove"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTagBodyBytes)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	if len(req.ContactIDs) > 2000 {
		writeErr(w, http.StatusBadRequest, "一次最多处理 2000 个联系人")
		return
	}
	n, err := BatchTag(s.db, req.ContactIDs, req.TagIDs, req.Remove)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "affected": n})
}

// routeContactTags /api/contacts/{id}/tags
func (s *apiServer) routeContactTags(w http.ResponseWriter, r *http.Request, id int64) {
	switch r.Method {
	case http.MethodGet:
		list, err := GetContactTags(s.db, id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取标签失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"list": list})
	case http.MethodPut, http.MethodPost:
		var req struct {
			TagIDs []int64 `json:"tagIds"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTagBodyBytes)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体解析失败")
			return
		}
		if err := SetContactTags(s.db, id, req.TagIDs); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}
