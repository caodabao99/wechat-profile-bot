package main

// Relationship Projects HTTP 层（OS 2.0 Phase 5，规格第八章）。
// 挂在 routeRelationships 的 "projects" 子分发下：
//   GET    /api/relationships/projects            列表（?contactId=&status=；status=open 取 active+paused）
//   POST   /api/relationships/projects            新建 body {contactId,title,description,status,stage,priority,...}
//   GET    /api/relationships/projects/{id}       单条
//   PUT    /api/relationships/projects/{id}       局部更新（PATCH 语义，仅传字段被覆写）
//   DELETE /api/relationships/projects/{id}       删除

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// routeProjects /api/relationships/projects/... 子路由。
func (s *apiServer) routeProjects(w http.ResponseWriter, r *http.Request, sub []string) {
	if len(sub) == 0 {
		switch r.Method {
		case http.MethodGet:
			s.hProjectsList(w, r)
		case http.MethodPost:
			s.hProjectCreate(w, r)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		}
		return
	}
	id, err := strconv.ParseInt(sub[0], 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "无效的项目ID")
		return
	}
	if len(sub) > 1 {
		writeErr(w, http.StatusNotFound, "未知接口: /api/relationships/projects/"+sub[0]+"/"+sub[1])
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.hProjectGet(w, r, id)
	case http.MethodPut:
		s.hProjectUpdate(w, r, id)
	case http.MethodDelete:
		s.hProjectDelete(w, r, id)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}

func (s *apiServer) hProjectsList(w http.ResponseWriter, r *http.Request) {
	var contactID int64
	if v := r.URL.Query().Get("contactId"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			contactID = n
		}
	}
	list, err := ListProjects(s.db, contactID, r.URL.Query().Get("status"))
	if err != nil {
		if errors.Is(err, errProjectBadInput) {
			writeErr(w, http.StatusBadRequest, "状态参数不合法")
			return
		}
		writeErr(w, http.StatusInternalServerError, "读取关系项目失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(list), "projects": list})
}

// projectBody 新建/更新共用请求体；更新时用指针区分「未传」与「传空」。
type projectBody struct {
	ContactID     int64   `json:"contactId"`
	Title         *string `json:"title"`
	Description   *string `json:"description"`
	Status        *string `json:"status"`
	Stage         *string `json:"stage"`
	Priority      *int    `json:"priority"`
	StartDate     *string `json:"startDate"`
	TargetDate    *string `json:"targetDate"`
	NextAction    *string `json:"nextAction"`
	NextActionDue *string `json:"nextActionDue"`
	BlockedReason *string `json:"blockedReason"`
}

func (s *apiServer) hProjectCreate(w http.ResponseWriter, r *http.Request) {
	var body projectBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRehearsalBodyBytes)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	title := ""
	if body.Title != nil {
		title = *body.Title
	}
	in := CreateProjectInput{
		ContactID: body.ContactID, Title: title,
		StartDate: derefOrEmpty(body.StartDate), TargetDate: derefOrEmpty(body.TargetDate),
		NextAction: derefOrEmpty(body.NextAction), NextActionDue: derefOrEmpty(body.NextActionDue),
		BlockedReason: derefOrEmpty(body.BlockedReason), Description: derefOrEmpty(body.Description),
		Status: derefOrEmpty(body.Status), Stage: derefOrEmpty(body.Stage),
	}
	if body.Priority != nil {
		in.Priority = *body.Priority
	}
	newID, err := CreateProject(s.db, in, time.Now())
	if err != nil {
		switch {
		case errors.Is(err, errProjectBadInput):
			writeErr(w, http.StatusBadRequest, "项目参数不合法（标题必填；status∈active/paused/completed/cancelled；stage∈discovery/building/maintaining/negotiating/closing/completed）")
		case errors.Is(err, sql.ErrNoRows):
			writeErr(w, http.StatusNotFound, "联系人不存在")
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	v, err := GetProject(s.db, newID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取新建项目失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "createdId": newID, "project": v})
}

func (s *apiServer) hProjectGet(w http.ResponseWriter, r *http.Request, id int64) {
	v, err := GetProject(s.db, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "项目不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "读取关系项目失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "project": v})
}

func (s *apiServer) hProjectUpdate(w http.ResponseWriter, r *http.Request, id int64) {
	var body projectBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRehearsalBodyBytes)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	in := UpdateProjectInput{
		Title: body.Title, Description: body.Description, Status: body.Status, Stage: body.Stage,
		Priority: body.Priority, StartDate: body.StartDate, TargetDate: body.TargetDate,
		NextAction: body.NextAction, NextActionDue: body.NextActionDue, BlockedReason: body.BlockedReason,
	}
	v, err := UpdateProject(s.db, id, in, time.Now())
	if err != nil {
		switch {
		case errors.Is(err, errProjectBadInput):
			writeErr(w, http.StatusBadRequest, "项目参数不合法（标题不可为空；status/stage 取值非法）")
		case errors.Is(err, sql.ErrNoRows):
			writeErr(w, http.StatusNotFound, "项目不存在")
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "project": v})
}

func (s *apiServer) hProjectDelete(w http.ResponseWriter, r *http.Request, id int64) {
	deleted, err := DeleteProject(s.db, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "删除项目失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "id": id})
}

func derefOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
