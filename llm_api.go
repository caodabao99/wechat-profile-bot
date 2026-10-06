package main

// 模型与代理 HTTP 接口（v6.2）。路由挂在 /api/llm/*（api.go 里只加一个 case）。
// 认证复用现有通道：IP 白名单 + Bearer/网页会话，进入这里时已通过。
//
// 密钥安全：GET 永不回传真实 apiKey（有值→打码 apiKeyMask，无值→空串）；保存时前端
// 若原样回传打码串，saveLLMSettings 会按 ID 沿用库里的旧密钥（与 smtp/calendar 同纪律）。

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
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
	case "usage":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		usage, err := ComputeLLMUsage(s.db)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// 预算另作同级字段：ComputeLLMUsage 内部自持 dbMu，此处在其释锁后才读预算（不嵌套）。
		// 不内嵌进 usage 结构体，是为了让其它不读预算的调用方（如状态快照）不会拿零值冒充「已用尽」。
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "usage": usage, "budget": llmBudgetStatus(s.db)})
	case "budget":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		hLLMSetBudget(w, r, s.db)
	case "router":
		switch r.Method {
		case http.MethodGet:
			hLLMGetRouter(w, r, s.db)
		case http.MethodPut, http.MethodPost:
			hLLMPutRouter(w, r, s.db)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		}
		return
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
	case "proxy":
		if len(sub) == 2 && sub[1] == "test" && r.Method == http.MethodPost {
			s.hLLMProxyTest(w, r)
			return
		}
		writeErr(w, http.StatusNotFound, "未知接口: /api/llm/proxy")
	case "model":
		if len(sub) == 2 && sub[1] == "test" && r.Method == http.MethodPost {
			s.hLLMModelTest(w, r)
			return
		}
		writeErr(w, http.StatusNotFound, "未知接口: /api/llm/model")
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

// hLLMSetBudget POST /api/llm/budget {dailyTokenBudget, weeklyTokenBudget, monthlyTokenBudget,
// perContactDailyTokenBudget}：设各维「真实模型调用」token 上限（0=不限制）。
// 用指针区分「没传」与「传了 0」——后者是有意义的动作（取消该维限制）。任一维未传则保持原值。
// 写库走 load→modify→save 既有入口，其余字段（含被掩码的密钥、任务策略）原样保留。
func hLLMSetBudget(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	var req struct {
		DailyTokenBudget           *int64 `json:"dailyTokenBudget"`
		WeeklyTokenBudget          *int64 `json:"weeklyTokenBudget"`
		MonthlyTokenBudget         *int64 `json:"monthlyTokenBudget"`
		PerContactDailyTokenBudget *int64 `json:"perContactDailyTokenBudget"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if req.DailyTokenBudget == nil && req.WeeklyTokenBudget == nil &&
		req.MonthlyTokenBudget == nil && req.PerContactDailyTokenBudget == nil {
		writeErr(w, http.StatusBadRequest, "缺少预算字段（值 0 表示该维不限制）")
		return
	}
	settings, err := loadLLMSettings(db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.DailyTokenBudget != nil {
		settings.DailyTokenBudget = *req.DailyTokenBudget
	}
	if req.WeeklyTokenBudget != nil {
		settings.WeeklyTokenBudget = *req.WeeklyTokenBudget
	}
	if req.MonthlyTokenBudget != nil {
		settings.MonthlyTokenBudget = *req.MonthlyTokenBudget
	}
	if req.PerContactDailyTokenBudget != nil {
		settings.PerContactDailyTokenBudget = *req.PerContactDailyTokenBudget
	}
	if err := saveLLMSettings(db, settings); err != nil {
		writeErr(w, http.StatusBadRequest, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "budget": llmBudgetStatus(db)})
}

// hLLMGetRouter GET /api/llm/router：全部任务的「有效路由策略 + 建议档位」视图 + 可绑定档案清单。
// 视图单一来源 = LLMSettings.TaskPolicies + Context Task Registry（前端不另建映射）。
func hLLMGetRouter(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	settings, err := loadLLMSettings(db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 档案清单仅暴露 id/label/region/是否本地（不含密钥/端点），供下拉选择主/备档案。
	type profileRef struct {
		ID     string `json:"id"`
		Label  string `json:"label"`
		Region string `json:"region"`
		Local  bool   `json:"local"`
		Usable bool   `json:"usable"`
	}
	refs := make([]profileRef, 0, len(settings.Profiles))
	for i := range settings.Profiles {
		p := &settings.Profiles[i]
		refs = append(refs, profileRef{ID: p.ID, Label: p.Label, Region: p.Region, Local: isLocalEndpoint(p), Usable: p.usable()})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":            true,
		"policies":      ModelPolicyViews(settings),
		"profiles":      refs,
		"activeProfile": settings.ActiveProfileID,
	})
}

// hLLMPutRouter PUT/POST /api/llm/router {policies:{task:TaskModelPolicy,...}}：整体替换任务级
// Model Router 策略。空 map = 清空全部策略（所有任务回落活动档案）。保存前经 normalize 清洗。
func hLLMPutRouter(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	var req struct {
		Policies map[string]TaskModelPolicy `json:"policies"`
	}
	if !readBody(w, r, &req) {
		return
	}
	settings, err := loadLLMSettings(db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	settings.TaskPolicies = req.Policies
	if err := saveLLMSettings(db, settings); err != nil {
		writeErr(w, http.StatusBadRequest, "保存失败: "+err.Error())
		return
	}
	saved, _ := loadLLMSettings(db)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "policies": ModelPolicyViews(saved)})
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

// hLLMProxyTest POST /api/llm/proxy/test：代理连通性测试。
// 可选 body 携带未保存的代理配置（{enabled,url,noProxy}）：提供则先落库再测（“配好即测”），
// 不提供则直接测当前已保存配置。探测目标走默认公网地址。失败不 503：单项失败作为数据记入结果。
func (s *apiServer) hLLMProxyTest(w http.ResponseWriter, r *http.Request) {
	settings, err := loadLLMSettings(s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 可选覆盖：允许测试尚未保存的代理配置（空 body 则保持已存配置）。
	var override struct {
		Enabled *bool   `json:"enabled"`
		URL     *string `json:"url"`
		NoProxy *string `json:"noProxy"`
	}
	if dec := json.NewDecoder(r.Body); dec.Decode(&override) != io.EOF {
		if override.Enabled != nil {
			settings.Proxy.Enabled = *override.Enabled
		}
		if override.URL != nil {
			settings.Proxy.URL = strings.TrimSpace(*override.URL)
		}
		if override.NoProxy != nil {
			settings.Proxy.NoProxy = strings.TrimSpace(*override.NoProxy)
		}
	}
	outcome := runProxyTest(settings.Proxy, proxyTestOptionsFn())
	settings.Proxy.TestedAt = outcome.TestedAt
	settings.Proxy.EgressIP = outcome.EgressIP
	settings.Proxy.DirectIP = outcome.DirectIP
	settings.Proxy.Sites = outcome.Sites
	// 持久化最近一次测试结果（含本次生效的代理配置），供状态页展示。
	if err := saveLLMSettings(s.db, settings); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存测试结果失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":       true,
		"proxy":    settings.Proxy,
		"settings": maskLLMSettings(settings),
	})
}

// hLLMModelTest POST /api/llm/model/test：对当前活动模型发一次最小调用，验证接口可达。
// 未配置模型时返回 ok=false 不报错；调用失败也作 200 结构化结果（失败是数据）。
func (s *apiServer) hLLMModelTest(w http.ResponseWriter, r *http.Request) {
	if s.llm == nil || !s.llm.configured() {
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": false, "error": "未配置可用的活动模型"})
		return
	}
	start := time.Now()
	_, err := s.llm.CallContext(r.Context(), "ping")
	res := map[string]interface{}{
		"ok":        err == nil,
		"latencyMs": time.Since(start).Milliseconds(),
	}
	if err != nil {
		res["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, res)
}
