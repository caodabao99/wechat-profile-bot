package main

// v5.5.0 #7：关系目标 HTTP 层。routeAssistant 的 "goals" 子分发 + CRUD 端点。
//   GET    /api/assistant/goals            列出目标（含自动达标检测）
//   POST   /api/assistant/goals            新建目标 body {contactId,title,metric,targetCount,periodStart,periodEnd}
//   POST   /api/assistant/goals/{id}/complete  手动达成
//   DELETE /api/assistant/goals/{id}       删除

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// routeGoals /api/assistant/goals/... 子路由。
func (s *apiServer) routeGoals(w http.ResponseWriter, r *http.Request, sub []string) {
	if len(sub) == 0 {
		switch r.Method {
		case http.MethodGet:
			s.hGoalsList(w, r)
		case http.MethodPost:
			s.hGoalsCreate(w, r)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		}
		return
	}
	// /goals/{id}[/complete]
	id, err := strconv.ParseInt(sub[0], 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "无效的目标ID")
		return
	}
	if len(sub) >= 2 && sub[1] == "complete" {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hGoalsComplete(w, r, id)
		return
	}
	if len(sub) > 1 {
		writeErr(w, http.StatusNotFound, "未知接口: /api/assistant/goals/"+sub[0]+"/"+sub[1])
		return
	}
	switch r.Method {
	case http.MethodDelete:
		s.hGoalsDelete(w, r, id)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}

// hGoalsList GET /api/assistant/goals：目标清单（先跑一次达标检测）。
func (s *apiServer) hGoalsList(w http.ResponseWriter, r *http.Request) {
	resp, err := BuildGoals(s.db, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取关系目标失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// hGoalsCreate POST /api/assistant/goals：新建一条目标，成功后回最新清单。
func (s *apiServer) hGoalsCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ContactID   int64  `json:"contactId"`
		Title       string `json:"title"`
		Metric      string `json:"metric"`
		TargetCount int    `json:"targetCount"`
		PeriodStart string `json:"periodStart"`
		PeriodEnd   string `json:"periodEnd"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRehearsalBodyBytes)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	now := time.Now()
	newID, err := CreateGoal(s.db, CreateGoalInput{
		ContactID:   body.ContactID,
		Title:       body.Title,
		Metric:      body.Metric,
		TargetCount: body.TargetCount,
		PeriodStart: body.PeriodStart,
		PeriodEnd:   body.PeriodEnd,
	}, now)
	if err != nil {
		switch {
		case errors.Is(err, errGoalBadInput):
			writeErr(w, http.StatusBadRequest, "目标参数不合法（标题必填、达标数 1~999；周期口径需完整起止日期）")
		case errors.Is(err, sql.ErrNoRows):
			writeErr(w, http.StatusNotFound, "联系人不存在")
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	resp, err := readGoals(s.db, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取关系目标失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		CreatedID int64          `json:"createdId"`
		Goals     *GoalsResponse `json:"goals"`
	}{newID, resp})
}

// hGoalsComplete POST /api/assistant/goals/{id}/complete：手动达成（幂等）。
func (s *apiServer) hGoalsComplete(w http.ResponseWriter, r *http.Request, id int64) {
	done, err := CompleteGoal(s.db, id, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "完成目标失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": done, "id": id})
}

// hGoalsDelete DELETE /api/assistant/goals/{id}：删除目标。
func (s *apiServer) hGoalsDelete(w http.ResponseWriter, r *http.Request, id int64) {
	deleted, err := DeleteGoal(s.db, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "删除目标失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": deleted, "id": id})
}
