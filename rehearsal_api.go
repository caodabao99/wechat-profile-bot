package main

import (
	"encoding/json"
	"net/http"
)

// 对话预演的 HTTP 接口（网页端专用，需登录）。
// 路由挂载：api.go routeContact 里 `case "rehearsal"`。
//
//   GET  /api/contacts/{id}/rehearsal/context  扮演依据（画像要点 + 说话样例）
//   POST /api/contacts/{id}/rehearsal/turn     生成对方的一条回应
//   POST /api/contacts/{id}/rehearsal/review   结束后复盘
//
// 全部无状态：不写库、不发送、不碰微信侧。

// 预演请求体可以带整场对话历史（最多 60 条 × 2000 字），比标签那类接口宽松得多。
const maxRehearsalBodyBytes = 512 << 10

func (s *apiServer) routeContactRehearsal(w http.ResponseWriter, r *http.Request, id int64, sub []string) {
	if len(sub) == 0 {
		writeErr(w, http.StatusNotFound, "未知接口")
		return
	}
	switch sub[0] {
	case "context":
		s.hRehearsalContext(w, r, id)
	case "turn":
		s.hRehearsalTurn(w, r, id)
	case "review":
		s.hRehearsalReview(w, r, id)
	default:
		writeErr(w, http.StatusNotFound, "未知接口")
	}
}

// checkRehearsal 三个接口共用的前置校验：方法、LLM 配置、联系人是否存在。
func (s *apiServer) checkRehearsal(w http.ResponseWriter, r *http.Request, id int64, method string) bool {
	if r.Method != method {
		w.Header().Set("Allow", method)
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return false
	}
	if s.llm == nil {
		writeErr(w, http.StatusServiceUnavailable, "LLM 未配置，无法使用对话预演")
		return false
	}
	if _, err := GetContactByID(s.db, id); err != nil {
		// 与 hAssistantBlessing 一致：查不到就当作不存在，不向前端泄漏库层细节。
		// 名字等信息一律以库里的为准，请求体里带什么都不信。
		writeErr(w, http.StatusNotFound, "联系人不存在")
		return false
	}
	return true
}

func (s *apiServer) hRehearsalContext(w http.ResponseWriter, r *http.Request, id int64) {
	if !s.checkRehearsal(w, r, id, http.MethodGet) {
		return
	}
	rc, err := LoadRehearsalContext(s.db, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取预演依据失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rc)
}

func (s *apiServer) hRehearsalTurn(w http.ResponseWriter, r *http.Request, id int64) {
	if !s.checkRehearsal(w, r, id, http.MethodPost) {
		return
	}
	var req struct {
		Scene string          `json:"scene"`
		Turns []RehearsalTurn `json:"turns"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRehearsalBodyBytes)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	text, emotion, err := RehearsalReply(s.db, s.llm, id, req.Scene, req.Turns)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"text": text, "emotion": emotion})
}

func (s *apiServer) hRehearsalReview(w http.ResponseWriter, r *http.Request, id int64) {
	if !s.checkRehearsal(w, r, id, http.MethodPost) {
		return
	}
	var req struct {
		Scene string          `json:"scene"`
		Turns []RehearsalTurn `json:"turns"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRehearsalBodyBytes)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	rev, err := ReviewRehearsal(s.db, s.llm, id, req.Scene, req.Turns)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rev)
}
