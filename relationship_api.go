package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// 关系驾驶舱后端接口（Phase 8/10）。
//
// 单联系人（挂在 routeContact 下）：
//
//	GET  /api/contacts/{id}/facts          可信画像事实 + 证据链（空则自愈重建）
//	POST /api/contacts/{id}/facts/rebuild  强制重建事实 + 证据
//	POST /api/contacts/{id}/facts/confirm/{factId}  将事实提升为用户确认（最高可信来源，5.5）
//	GET  /api/contacts/{id}/state          关系状态机单联系人视图（base_state×dynamic_state，?history=1 附变迁）
//	GET  /api/contacts/{id}/trend          关系变化趋势（升温/降温/沉寂）
//
// 全局（route() 里 `case "relationships"`）：
//
//	GET  /api/relationships/suggestions            行动建议列表（默认只 open）
//	GET  /api/relationships/state                   全局关系状态看板（缺则自愈刷新）
//	POST /api/relationships/state/recompute          强制重算全体关系状态（仅跨阈值产变迁事件）
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
	if len(sub) == 2 && sub[0] == "confirm" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		factID, err := strconv.ParseInt(sub[1], 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "事实 ID 无效")
			return
		}
		// 越权保护：事实必须属于该联系人
		var owner int64
		if err := s.db.QueryRow(`SELECT contact_id FROM profile_facts WHERE id=?`, factID).Scan(&owner); err != nil {
			writeErr(w, http.StatusNotFound, "事实不存在")
			return
		}
		if owner != id {
			writeErr(w, http.StatusForbidden, "事实不属于该联系人")
			return
		}
		if err := ConfirmFact(s.db, factID); err != nil {
			writeErr(w, http.StatusInternalServerError, "确认事实失败: "+err.Error())
			return
		}
		facts, err := GetFacts(s.db, id, false)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取事实失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"facts": facts})
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

// routeContactQuality GET /api/contacts/{id}/quality?days=90
// 对话质量四维评分（纯确定性、零 LLM）。days 缺省/非法回落默认值并夹到上限。
func (s *apiServer) routeContactQuality(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	if _, err := GetContactByID(s.db, id); err != nil {
		writeErr(w, http.StatusNotFound, "联系人不存在")
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	q, err := ComputeContactQuality(s.db, id, days)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "计算对话质量失败: "+err.Error())
		return
	}
	// v5.2.1：惰性追加当周综合分快照并回填历史趋势（best-effort，失败不影响评分响应）。
	if err := ensureQualityHistory(s.db); err == nil {
		upsertQualitySnapshot(s.db, q)
		if hist, herr := getQualityHistory(s.db, id, qualityHistoryDefaultShow); herr == nil {
			q.History = hist
		}
	}
	writeJSON(w, http.StatusOK, q)
}

// routeContactSummary POST /api/contacts/{id}/summary  body {"days":30}
// 智能回顾摘要：同窗口内的真实原文 → LLM 抽 overview/topics/todos（带 [n] 出处）。
// 未配置模型 503；days 缺省/非法回落默认值。body 大小护栏复用 maxRehearsalBodyBytes。
func (s *apiServer) routeContactSummary(w http.ResponseWriter, r *http.Request, id int64) {
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
		Days int `json:"days"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRehearsalBodyBytes)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	res, err := SummarizeContact(r.Context(), s.db, s.llm, id, req.Days)
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
	case "connections":
		s.routeConnections(w, r, sub[1:])
	case "health":
		s.hRelationshipHealth(w, r)
	case "state":
		s.routeRelationshipState(w, r, sub[1:])
	case "decisions":
		s.routeTodayDecisions(w, r)
	case "projects":
		s.routeProjects(w, r, sub[1:])
	case "circles":
		s.hRelationshipCircles(w, r)
	default:
		writeErr(w, http.StatusNotFound, "未知接口")
	}
}

// routeContactReplay GET /api/contacts/{id}/replay：Memory Replay「重新认识 TA」（Phase 7）。
// 确定性拼装关系回放（首次认识/阶段/转折/兴趣职业/升降温/主题/当前/目标/未完成），
// 每条结论带来源证据；不依赖 LLM，返回结构化 `replay` + `rendered` 文本。
func (s *apiServer) routeContactReplay(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	rep, err := BuildRelationshipReplay(s.db, id, time.Now())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "联系人不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "回放生成失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "replay": rep, "rendered": RenderReplayText(rep)})
}

// routeContactContext GET /api/contacts/{id}/context?task=ask&q=关键词：AI Context Engine（Phase 6）。
// 分层构造该联系人的认知快照（身份/状态/事实/证据/目标/项目/待办/主题/指标/消息/行动），
// 并按任务预算渲染为提示词上下文块（只读，供新 AI 代码统一取数与可观测）。
func (s *apiServer) routeContactContext(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	task := ContextTask(r.URL.Query().Get("task"))
	if task == "" {
		task = TaskProfile
	}
	q := r.URL.Query().Get("q")
	cc, err := BuildContactContext(s.db, id, task, q, time.Now())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "联系人不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "上下文构造失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "context": cc, "rendered": RenderContextText(cc)})
}

// routeTodayDecisions GET /api/relationships/decisions/today?top=3：Decision Engine（Phase 4）。
// 汇聚 State Machine + Health + Followup + Goal + 重要日子 + 行动建议，以确定性打分
// （非 LLM 排序）产出「今天谁最值得投入时间、为什么、做什么」（规格 7.2/7.3）。
func (s *apiServer) routeTodayDecisions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	top := 3
	if v := r.URL.Query().Get("top"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			top = n
		}
	}
	list, err := TodayDecisions(s.db, time.Now(), top)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "决策计算失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(list), "decisions": list})
}

// hRelationshipHealth GET /api/relationships/health?window=90：全局关系健康度仪表盘（只读、计算即读）。
func (s *apiServer) hRelationshipHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	window := 0
	if v := r.URL.Query().Get("window"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			window = n
		}
	}
	dash, err := ComputeHealth(s.db, time.Now(), window)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "计算健康度仪表盘失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dash)
}

// hRelationshipCircles GET /api/relationships/circles：全局关系圈层（只读、计算即读）。
func (s *apiServer) hRelationshipCircles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	dash, err := ComputeCircles(s.db, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "计算关系圈层失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dash)
}

// routeContactState GET /api/contacts/{id}/state（?history=1 附带变迁历史）。
// 关系状态机单联系人视图（只读；存量缺失时自愈刷新整板一次）。
func (s *apiServer) routeContactState(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	if _, err := GetContactByID(s.db, id); err != nil {
		writeErr(w, http.StatusNotFound, "联系人不存在")
		return
	}
	v, err := GetRelationshipState(s.db, id)
	if err == sql.ErrNoRows {
		writeErr(w, http.StatusNotFound, "该联系人尚无状态快照")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取关系状态失败: "+err.Error())
		return
	}
	resp := map[string]interface{}{"state": v}
	if r.URL.Query().Get("history") == "1" {
		hist, herr := GetRelationshipStateHistory(s.db, id, 50)
		if herr == nil {
			resp["history"] = hist
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// routeRelationshipState /api/relationships/state[/recompute]：全局关系状态看板。
func (s *apiServer) routeRelationshipState(w http.ResponseWriter, r *http.Request, sub []string) {
	if len(sub) == 1 && sub[0] == "recompute" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		changed, total, err := RefreshRelationshipStates(s.db, time.Now(), 0)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "重算关系状态失败: "+err.Error())
			return
		}
		list, err := ListRelationshipStates(s.db)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取关系状态看板失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"changed": changed, "total": total, "states": list})
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
	list, err := ListRelationshipStates(s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取关系状态看板失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"states": list})
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
