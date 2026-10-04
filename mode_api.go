package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// 运行模式预设接口（特性③）。
//
//	GET    /api/mode-presets              内置 + 自定义预设列表
//	POST   /api/mode-presets/apply {name} 一键应用某预设到既有设置表
//	POST   /api/mode-presets {name}       把当前运行设置另存为自定义预设
//	DELETE /api/mode-presets/{name}       删除自定义预设（内置不可删）

// maskPresetCreds 抹掉预设 payload 里的明文凭据（ SMTP 密码与日历密钥）。
// 凭据只在「关系助手」表单一处管理，模式接口无需也不应把它们原样回传前端；
// 旧版已落库的自定义预设仍可能带明文密码，list/apply/save 三个响应出口统一打码。
func maskPresetCreds(p ModePresetPayload) ModePresetPayload {
	if p.Assistant.SMTP.Pass != "" {
		p.Assistant.SMTP.Pass = smtpPassMask
	}
	if p.Assistant.CalendarKey != "" {
		p.Assistant.CalendarKey = calendarKeyMask
	}
	return p
}

func (s *apiServer) routeModePresets(w http.ResponseWriter, r *http.Request, sub []string) {
	switch {
	case len(sub) == 0 && r.Method == http.MethodGet:
		list, err := ListModePresets(s.db)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取运行模式预设失败: "+err.Error())
			return
		}
		for i := range list {
			list[i].Payload = maskPresetCreds(list[i].Payload)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"presets": list})

	case len(sub) == 1 && sub[0] == "apply" && r.Method == http.MethodPost:
		s.hModePresetApply(w, r)

	case len(sub) == 0 && r.Method == http.MethodPost:
		s.hModePresetSave(w, r)

	case len(sub) == 1 && r.Method == http.MethodDelete:
		if err := DeleteModePreset(s.db, sub[0]); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": sub[0]})

	default:
		writeErr(w, http.StatusNotFound, "未知运行模式接口")
	}
}

func (s *apiServer) hModePresetApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "缺少预设名称")
		return
	}
	payload, err := ApplyModePreset(s.db, name)
	if err != nil {
		// 找不到/数据损坏等归为 400；真正的存储错误也一并回给用户
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"applied": name, "payload": maskPresetCreds(payload)})
}

func (s *apiServer) hModePresetSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	payload, err := SaveModePreset(s.db, req.Name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	writeJSON(w, http.StatusOK, map[string]interface{}{"saved": name, "payload": maskPresetCreds(payload)})
}
