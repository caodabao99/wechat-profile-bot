package main

// v5.1.0 提示词模板管理 HTTP 接口。挂在 /api/assistant/prompts/*（assistant_api.go 加一个 case）。
// 认证复用既有通道（IP 白名单 + Bearer/网页会话），进入这里时已通过。
//
// 端点：
//   - GET    /api/assistant/prompts                列表（key/title/feature/vars/isCustom/updatedAt）
//   - GET    /api/assistant/prompts/{key}          详情（默认 + 当前生效 + 是否自定义）
//   - PUT    /api/assistant/prompts/{key}          body {"content":"..."} 保存覆盖（校验失败/超限 400、未知 key 404）
//   - DELETE /api/assistant/prompts/{key}          重置为默认
//   - POST   /api/assistant/prompts/{key}/preview  body {"vars":{...}} 试渲染（只读、不调模型）
//
// 铁律：编辑提示词等价于编辑本工具的模型指令，属自托管单主能力；仅字面占位符、
// 必填变量校验、8KB 上限、每模板可一键回滚默认，覆盖只改文本不改代码路径与降级语义。

import (
	"encoding/json"
	"net/http"
)

// maxPromptBodyBytes 覆盖 PUT/preview 请求体上限：模板正文 8KB + JSON 包裹余量。
const maxPromptBodyBytes = 16 << 10

func (s *apiServer) routeAssistantPrompts(w http.ResponseWriter, r *http.Request, sub []string) {
	// GET /api/assistant/prompts —— 列表
	if len(sub) == 0 {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hPromptsList(w, r)
		return
	}
	key := sub[0]
	if _, ok := promptRegistry[key]; !ok {
		writeErr(w, http.StatusNotFound, "未知提示词模板: "+key)
		return
	}
	// /api/assistant/prompts/{key}[/preview]
	if len(sub) == 2 && sub[1] == "preview" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hPromptsPreview(w, r, key)
		return
	}
	if len(sub) > 1 {
		writeErr(w, http.StatusNotFound, "未知接口: /api/assistant/prompts/"+key)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.hPromptsGet(w, r, key)
	case http.MethodPut, http.MethodPost:
		s.hPromptsSave(w, r, key)
	case http.MethodDelete:
		s.hPromptsReset(w, r, key)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}

func (s *apiServer) hPromptsList(w http.ResponseWriter, r *http.Request) {
	list := listPromptTemplates(s.db)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"items": list,
		"total": len(list),
	})
}

func (s *apiServer) hPromptsGet(w http.ResponseWriter, r *http.Request, key string) {
	detail, ok := getPromptDetail(s.db, key)
	if !ok {
		writeErr(w, http.StatusNotFound, "未知提示词模板: "+key)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *apiServer) hPromptsSave(w http.ResponseWriter, r *http.Request, key string) {
	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPromptBodyBytes)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	// 先按注册表校验（空/超限/缺必填变量/含未知变量）→ 400；未知 key 已在路由层挡下。
	if err := savePromptOverride(s.db, key, req.Content); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	detail, _ := getPromptDetail(s.db, key)
	writeJSON(w, http.StatusOK, detail)
}

func (s *apiServer) hPromptsReset(w http.ResponseWriter, r *http.Request, key string) {
	if err := resetPromptOverride(s.db, key); err != nil {
		writeErr(w, http.StatusInternalServerError, "重置失败: "+err.Error())
		return
	}
	detail, _ := getPromptDetail(s.db, key)
	writeJSON(w, http.StatusOK, detail)
}

func (s *apiServer) hPromptsPreview(w http.ResponseWriter, r *http.Request, key string) {
	var req struct {
		Vars map[string]string `json:"vars"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPromptBodyBytes)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	rendered, err := RenderPrompt(s.db, key, req.Vars)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"prompt": rendered})
}
