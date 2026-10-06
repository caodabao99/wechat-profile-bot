package main

// Relationship Experiment API（蓝图 §9 P5）。挂载点：/api/contacts/{id}/experiments（api.go routeContact）。
//
//	GET    /api/contacts/{id}/experiments                → 列表（?status=，?limit=，默认 50，最近在前）
//	POST   /api/contacts/{id}/experiments                body {goal,strategy,avoid_strategy,metrics,duration_days,start_date} → 新建（status=draft）
//	POST   /api/contacts/{id}/experiments/{eid}/start     → draft → running
//	POST   /api/contacts/{id}/experiments/{eid}/abandon   → 任意 → abandoned
//	POST   /api/contacts/{id}/experiments/{eid}/cancel    → 任意 → cancelled
//	POST   /api/contacts/{id}/experiments/{eid}/measure   → 据真实历史数据做前/后对照测量并落库结论（§9.3 四类，绝不宣称因果）
//
// 越权保护：元素级操作的实验必须属于路径里的联系人，否则 404。measure 可重复调用（每次据最新数据重算并覆盖）。

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

func (s *apiServer) routeContactExperiments(w http.ResponseWriter, r *http.Request, id int64, sub []string) {
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
			status := r.URL.Query().Get("status")
			list, err := ListExperiments(s.db, id, status, limit)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "读取关系实验失败: "+err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"experiments": list})
		case http.MethodPost:
			var req struct {
				Goal          string `json:"goal"`
				Strategy      string `json:"strategy"`
				AvoidStrategy string `json:"avoid_strategy"`
				Metrics       string `json:"metrics"`
				DurationDays  int    `json:"duration_days"`
				StartDate     string `json:"start_date"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRehearsalBodyBytes)).Decode(&req); err != nil {
				writeErr(w, http.StatusBadRequest, "请求体解析失败")
				return
			}
			eid, err := CreateExperiment(s.db, CreateExperimentInput{
				ContactID: id, Goal: req.Goal, Strategy: req.Strategy, AvoidStrategy: req.AvoidStrategy,
				Metrics: req.Metrics, DurationDays: req.DurationDays, StartDate: req.StartDate,
			}, now)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err.Error()) // 周期非正 / 联系人问题 / 日期不可解析
				return
			}
			e, _ := GetExperiment(s.db, eid)
			writeJSON(w, http.StatusCreated, map[string]any{"experiment": e})
		default:
			w.Header().Set("Allow", "GET, POST")
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		}
		return
	}

	// —— 元素级：/{eid}/{verb} ——
	if len(sub) != 2 {
		writeErr(w, http.StatusNotFound, "未知接口")
		return
	}
	eid, err := strconv.ParseInt(sub[0], 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "实验 ID 无效")
		return
	}
	e, err := GetExperiment(s.db, eid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取实验失败: "+err.Error())
		return
	}
	if e == nil || e.ContactID != id { // 越权：实验不属于该联系人按不存在处理
		writeErr(w, http.StatusNotFound, "实验不存在")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	switch sub[1] {
	case "start":
		if err := SetExperimentStatus(s.db, eid, ExpStatusRunning, now); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	case "abandon":
		if err := SetExperimentStatus(s.db, eid, ExpStatusAbandoned, now); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	case "cancel":
		if err := SetExperimentStatus(s.db, eid, ExpStatusCancelled, now); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	case "measure":
		measured, err := MeasureExperiment(s.db, eid, now)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "测量实验失败: "+err.Error())
			return
		}
		if !measured { // draft（尚未开始）或已被删除：诚实返回，不谎称已测量
			writeJSON(w, http.StatusOK, map[string]any{"measured": false, "experiment": e,
				"message": "实验尚未开始或已不存在，未做测量"})
			return
		}
	default:
		writeErr(w, http.StatusNotFound, "未知接口")
		return
	}
	refreshed, _ := GetExperiment(s.db, eid)
	writeJSON(w, http.StatusOK, map[string]any{"experiment": refreshed})
}
