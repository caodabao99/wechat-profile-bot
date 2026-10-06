package main

// Action Ledger API（蓝图 §5 P1）。挂载点：/api/contacts/{id}/actions（api.go routeContact）。
//
//	GET    /api/contacts/{id}/actions                 → 列表（?limit=，默认 50，最近在前）
//	POST   /api/contacts/{id}/actions                 body {source,source_ref,action_type,action_text} → 新建（status=generated）
//	POST   /api/contacts/{id}/actions/{aid}/transition body {status,deferred_until}                    → 生命周期迁移
//	POST   /api/contacts/{id}/actions/{aid}/outcome     body {outcome,note}                            → 用户确认结果（provenance=confirmed）
//
// 越权保护：元素级操作的记录必须属于路径里的联系人，否则 404。经 API 回填的结果一律是用户手工
// 确认（confirmed），系统估算（estimated）永不能覆盖它——§5.3「不能混淆」在写路径上兜底。

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

func (s *apiServer) routeContactActions(w http.ResponseWriter, r *http.Request, id int64, sub []string) {
	if _, err := GetContactByID(s.db, id); err != nil {
		writeErr(w, http.StatusNotFound, "联系人不存在")
		return
	}
	now := time.Now()

	// —— 集合级：列表 / 新建 ——
	if len(sub) == 0 {
		switch r.Method {
		case http.MethodGet:
			limit := 50
			if v := r.URL.Query().Get("limit"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					limit = n
				}
			}
			list, err := ListActionLog(s.db, id, limit)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "读取行动账本失败: "+err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"actions": list})
		case http.MethodPost:
			var req struct {
				Source     string `json:"source"`
				SourceRef  string `json:"source_ref"`
				ActionType string `json:"action_type"`
				ActionText string `json:"action_text"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRehearsalBodyBytes)).Decode(&req); err != nil {
				writeErr(w, http.StatusBadRequest, "请求体解析失败")
				return
			}
			aid, err := LogAction(s.db, id, req.Source, req.SourceRef, req.ActionType, req.ActionText, now)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err.Error()) // 非法 source / 联系人问题
				return
			}
			e, _ := GetActionLog(s.db, aid)
			writeJSON(w, http.StatusCreated, map[string]any{"action": e})
		default:
			w.Header().Set("Allow", "GET, POST")
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		}
		return
	}

	// —— 元素级：/{aid}/{verb} ——
	if len(sub) != 2 {
		writeErr(w, http.StatusNotFound, "未知接口")
		return
	}
	aid, err := strconv.ParseInt(sub[0], 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "行动 ID 无效")
		return
	}
	e, err := GetActionLog(s.db, aid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取行动失败: "+err.Error())
		return
	}
	if e == nil || e.ContactID != id { // 越权：记录不属于该联系人按不存在处理
		writeErr(w, http.StatusNotFound, "行动记录不存在")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	switch sub[1] {
	case "transition":
		var req struct {
			Status        string `json:"status"`
			DeferredUntil string `json:"deferred_until"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRehearsalBodyBytes)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体解析失败")
			return
		}
		if err := TransitionActionStatus(s.db, aid, req.Status, req.DeferredUntil, now); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		refreshed, _ := GetActionLog(s.db, aid)
		writeJSON(w, http.StatusOK, map[string]any{"action": refreshed})
	case "outcome":
		var req struct {
			Outcome string `json:"outcome"`
			Note    string `json:"note"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRehearsalBodyBytes)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体解析失败")
			return
		}
		if err := SetActionOutcome(s.db, aid, req.Outcome, ActionProvenanceConfirmed, req.Note, now); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		refreshed, _ := GetActionLog(s.db, aid)
		writeJSON(w, http.StatusOK, map[string]any{"action": refreshed})
	case "dismiss":
		var req struct {
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRehearsalBodyBytes)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体解析失败")
			return
		}
		// §6.3 忽略：记录原因、只屏蔽当前窗，绝不永久屏蔽。
		if err := DismissAction(s.db, aid, req.Reason, now); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		refreshed, _ := GetActionLog(s.db, aid)
		writeJSON(w, http.StatusOK, map[string]any{"action": refreshed})
	default:
		writeErr(w, http.StatusNotFound, "未知接口")
	}
}
