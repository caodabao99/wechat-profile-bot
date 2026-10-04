package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
)

// 关系驾驶舱后端接口（Phase 8/10）。
//
// 单联系人（挂在 routeContact 下）：
//
//	GET  /api/contacts/{id}/facts          可信画像事实 + 证据链（空则自愈重建）
//	POST /api/contacts/{id}/facts/rebuild  强制重建事实 + 证据
//	GET  /api/contacts/{id}/trend          关系变化趋势（升温/降温/沉寂）
//
// 全局（route() 里 `case "relationships"`）：
//
//	GET  /api/relationships/suggestions            行动建议列表（默认只 open）
//	POST /api/relationships/suggestions/generate   生成/刷新行动建议（body 可选 contactId）
//	POST /api/relationships/suggestions/{sid}/status 置建议状态 open|done|dismissed
//
// 全部走参数绑定；派生表读到时缺则就地重建（自愈），不依赖后台任务。

// routeContactFacts /api/contacts/{id}/facts[/rebuild]
func (s *apiServer) routeContactFacts(w http.ResponseWriter, r *http.Request, id int64, sub []string) {
	// 联系人必须存在
	if _, err := GetContactByID(s.db, id); err != nil {
		writeErr(w, http.StatusNotFound, "联系人不存在")
		return
	}
	if len(sub) == 1 && sub[0] == "rebuild" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		active, evidence, err := RebuildFactsAndEvidence(s.db, id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "重建事实失败: "+err.Error())
			return
		}
		facts, err := GetFacts(s.db, id, false)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取事实失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"active": active, "evidence": evidence, "facts": facts,
		})
		return
	}
	if len(sub) != 0 {
		writeErr(w, http.StatusNotFound, "未知接口")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	includeRetired := r.URL.Query().Get("includeRetired") == "1"
	facts, err := GetFacts(s.db, id, includeRetired)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取事实失败: "+err.Error())
		return
	}
	// 自愈：库里没有任何事实、但联系人已有画像 JSON 时，就地重建一次再返回。
	// 只有查询本身成功且为空才重建，避免把 SQL 错误误判成「没数据」。
	if len(facts) == 0 {
		if hasProfile, perr := contactHasProfileJSON(s.db, id); perr == nil && hasProfile {
			if _, _, rerr := RebuildFactsAndEvidence(s.db, id); rerr == nil {
				if retried, err2 := GetFacts(s.db, id, includeRetired); err2 == nil {
					facts = retried
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"facts": facts})
}

// routeContactTrend GET /api/contacts/{id}/trend
func (s *apiServer) routeContactTrend(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	if _, err := GetContactByID(s.db, id); err != nil {
		writeErr(w, http.StatusNotFound, "联系人不存在")
		return
	}
	trend, err := GetRelationshipTrend(s.db, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取关系趋势失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, trend)
}

// routeContactAsk POST /api/contacts/{id}/ask  body {"question":"..."}
// “问 TA 的历史”：先检索相关原文，再让模型带出处作答。未配置模型返回 503。
func (s *apiServer) routeContactAsk(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	if _, err := GetContactByID(s.db, id); err != nil {
		writeErr(w, http.StatusNotFound, "联系人不存在")
		return
	}
	var req struct {
		Question string `json:"question"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRehearsalBodyBytes)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	res, err := AskContactHistory(r.Context(), s.db, s.llm, id, req.Question)
	if err != nil {
		if err == ErrLLMNotConfigured {
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// routeRelationships /api/relationships/... 全局建议中心
func (s *apiServer) routeRelationships(w http.ResponseWriter, r *http.Request, sub []string) {
	if len(sub) == 0 {
		writeErr(w, http.StatusNotFound, "未知接口")
		return
	}
	switch sub[0] {
	case "suggestions":
		s.routeSuggestions(w, r, sub[1:])
	default:
		writeErr(w, http.StatusNotFound, "未知接口")
	}
}

func (s *apiServer) routeSuggestions(w http.ResponseWriter, r *http.Request, sub []string) {
	// GET /api/relationships/suggestions
	if len(sub) == 0 && r.Method == http.MethodGet {
		includeHandled := r.URL.Query().Get("includeHandled") == "1"
		list, err := ListSuggestions(s.db, includeHandled)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取行动建议失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"suggestions": list})
		return
	}
	// POST /api/relationships/suggestions/generate
	if len(sub) == 1 && sub[0] == "generate" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		var req struct {
			ContactID int64 `json:"contactId"`
		}
		// 允许空 body（= 全部活跃联系人）
		if r.Body != nil {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil && err.Error() != "EOF" {
				writeErr(w, http.StatusBadRequest, "请求体解析失败")
				return
			}
		}
		if req.ContactID < 0 {
			writeErr(w, http.StatusBadRequest, "contactId 非法")
			return
		}
		if req.ContactID > 0 {
			if _, err := GetContactByID(s.db, req.ContactID); err != nil {
				writeErr(w, http.StatusNotFound, "联系人不存在")
				return
			}
		}
		n, err := GenerateActionSuggestions(s.db, s.llm, req.ContactID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "生成行动建议失败: "+err.Error())
			return
		}
		list, err := ListSuggestions(s.db, false)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取行动建议失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"generated": n, "suggestions": list})
		return
	}
	// POST /api/relationships/suggestions/{sid}/status
	if len(sub) == 2 && sub[1] == "status" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		sid, err := strconv.ParseInt(sub[0], 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "无效的建议ID")
			return
		}
		var req struct {
			Status string `json:"status"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体解析失败")
			return
		}
		if err := SetSuggestionStatus(s.db, sid, req.Status); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
		return
	}
	writeErr(w, http.StatusNotFound, "未知接口")
}

// contactHasProfileJSON 判断联系人是否已存有画像 JSON。
func contactHasProfileJSON(db *sql.DB, contactID int64) (bool, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	var pj sql.NullString
	if err := db.QueryRow(`SELECT profile_json FROM contacts WHERE id=?`, contactID).Scan(&pj); err != nil {
		return false, err
	}
	return pj.String != "", nil
}
