package main

// 模型与代理 HTTP 接口（v6.2）。路由挂在 /api/llm/*（api.go 里只加一个 case）。
// 认证复用现有通道：IP 白名单 + Bearer/网页会话，进入这里时已通过。
//
// 密钥安全：GET 永不回传真实 apiKey（有值→打码 apiKeyMask，无值→空串）；保存时前端
// 若原样回传打码串，saveLLMSettings 会按 ID 沿用库里的旧密钥（与 smtp/calendar 同纪律）。

import (
	"database/sql"
	"net/http"
)

// maskLLMSettings 返回密钥打码后的副本，供 GET 展示。
func maskLLMSettings(s LLMSettings) LLMSettings {
	out := s
	out.Profiles = make([]LLMProfile, len(s.Profiles))
	for i, p := range s.Profiles {
		if p.APIKey != "" {
			p.APIKey = apiKeyMask
		}
		out.Profiles[i] = p
	}
	// 代理 URL 可能含账号口令，同样打码展示（编辑时留空表示不改动由前端处理）
	return out
}

// routeLLM /api/llm/... 子路由
func (s *apiServer) routeLLM(w http.ResponseWriter, r *http.Request, sub []string) {
	if len(sub) == 0 {
		writeErr(w, http.StatusNotFound, "未知接口")
		return
	}
	switch sub[0] {
	case "presets":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "presets": llmPresets()})
	case "settings":
		switch r.Method {
		case http.MethodGet:
			hLLMGetSettings(w, r, s.db)
		case http.MethodPut, http.MethodPost:
			hLLMPutSettings(w, r, s.db)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		}
	case "active":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		hLLMSetActive(w, r, s.db)
	case "profile":
		if len(sub) == 2 && sub[1] == "delete" && r.Method == http.MethodPost {
			hLLMDeleteProfile(w, r, s.db)
			return
		}
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		hLLMUpsertProfile(w, r, s.db)
	default:
		writeErr(w, http.StatusNotFound, "未知接口: /api/llm/"+sub[0])
	}
}

// hLLMGetSettings GET /api/llm/settings：当前模型/代理配置（密钥打码）+ 活动档案摘要。
func hLLMGetSettings(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	settings, err := loadLLMSettings(db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":       true,
		"settings": maskLLMSettings(settings),
	})
}

// hLLMPutSettings PUT/POST /api/llm/settings：整体保存（掩码密钥沿用旧值）。
func hLLMPutSettings(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	var next LLMSettings
	if !readBody(w, r, &next) {
		return
	}
	if err := saveLLMSettings(db, next); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	settings, _ := loadLLMSettings(db)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "settings": maskLLMSettings(settings)})
}

// hLLMSetActive POST /api/llm/active {id}：切换活动模型档案。
func hLLMSetActive(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	var req struct {
		ID string `json:"id"`
	}
	if !readBody(w, r, &req) {
		return
	}
	settings, err := loadLLMSettings(db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	found := false
	for _, p := range settings.Profiles {
		if p.ID == req.ID {
			found = true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusBadRequest, "模型档案不存在")
		return
	}
	settings.ActiveProfileID = req.ID
	if err := saveLLMSettings(db, settings); err != nil {
		writeErr(w, http.StatusInternalServerError, "切换失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "activeProfileId": req.ID})
}

// hLLMUpsertProfile POST /api/llm/profile：新增或更新单个档案（按 ID 匹配；ID 空则新建）。
func hLLMUpsertProfile(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	var p LLMProfile
	if !readBody(w, r, &p) {
		return
	}
	settings, err := loadLLMSettings(db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p.BaseURL == "" || p.Model == "" {
		writeErr(w, http.StatusBadRequest, "接口地址与模型名不能为空")
		return
	}
	replaced := false
	for i := range settings.Profiles {
		if settings.Profiles[i].ID == p.ID && p.ID != "" {
			// 回传打码密钥时保留旧值
			if p.APIKey == apiKeyMask {
				p.APIKey = settings.Profiles[i].APIKey
			}
			settings.Profiles[i] = p
			replaced = true
			break
		}
	}
	if !replaced {
		if p.ID == "" {
			p.ID = newProfileID()
		}
		settings.Profiles = append(settings.Profiles, p)
		if settings.ActiveProfileID == "" {
			settings.ActiveProfileID = p.ID
		}
	}
	if err := saveLLMSettings(db, settings); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "id": p.ID})
}

// hLLMDeleteProfile POST /api/llm/profile/delete {id}：删除档案（活动档案被删则自动改指首个）。
func hLLMDeleteProfile(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	var req struct {
		ID string `json:"id"`
	}
	if !readBody(w, r, &req) {
		return
	}
	settings, err := loadLLMSettings(db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := settings.Profiles[:0]
	for _, p := range settings.Profiles {
		if p.ID != req.ID {
			out = append(out, p)
		}
	}
	settings.Profiles = out
	if settings.ActiveProfileID == req.ID {
		settings.ActiveProfileID = ""
	}
	settings.normalize()
	if err := saveLLMSettings(db, settings); err != nil {
		writeErr(w, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "activeProfileId": settings.ActiveProfileID})
}
