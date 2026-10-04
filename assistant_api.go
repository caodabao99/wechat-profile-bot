package main

// 关系助手 HTTP 接口。路由挂在 /api/assistant/*（api.go 里只加了一个 case）。
// 认证复用现有通道：IP 白名单 + Bearer/网页会话，进入这里时已通过。

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const smtpPassMask = "******"

// calendarKeyMask 日历订阅密钥的打码值。真实密钥只通过 /api/assistant/calendar/key 读取，
// 避免它随配置接口散落到前端各处；PUT 回传打码值时保留库里的原值。
const calendarKeyMask = "******"

func (s *apiServer) routeAssistant(w http.ResponseWriter, r *http.Request, sub []string) {
	if len(sub) == 0 {
		writeErr(w, http.StatusNotFound, "未知接口")
		return
	}
	switch sub[0] {
	case "dashboard":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hAssistantDashboard(w, r)
	case "settings":
		switch r.Method {
		case http.MethodGet:
			s.hAssistantGetSettings(w, r)
		case http.MethodPut, http.MethodPost:
			s.hAssistantPutSettings(w, r)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		}
	case "test-email":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hAssistantTestEmail(w, r)
	case "email-log":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hAssistantEmailLog(w, r)
	case "analyze-emotion":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hAssistantAnalyzeEmotion(w, r)
	case "run-now":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hAssistantRunNow(w, r)
	case "followups":
		s.routeFollowups(w, r, sub[1:])
	case "blessing":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hAssistantBlessing(w, r)
	case "calendar":
		s.routeCalendar(w, r, sub[1:])
	default:
		writeErr(w, http.StatusNotFound, "未知接口: /api/assistant/"+sub[0])
	}
}

// ---------- 看板 ----------

func (s *apiServer) hAssistantDashboard(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	st, err := loadAssistantSettings(s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取助手配置失败: "+err.Error())
		return
	}

	resp := map[string]interface{}{
		"settings":  maskSMTPPass(st),
		"smtpReady": st.SMTP.Ready(),
	}

	// 各板块单独容错：某一块查询失败不影响整页
	// 两个分支都保证是数组而不是 null：网页端直接读 .length，拿到 null 会让整个页面白屏
	if dates, unparsed, err := collectUpcomingDates(s.db, now, 14); err == nil {
		if dates == nil {
			dates = []AssistantDateItem{}
		}
		if unparsed == nil {
			unparsed = []AssistantDateItem{}
		}
		resp["upcoming"] = dates
		resp["unparsedDates"] = unparsed
	} else {
		slog.Warn("助手看板：重要日子查询失败", "err", err)
		resp["upcoming"] = []AssistantDateItem{}
		resp["unparsedDates"] = []AssistantDateItem{}
	}
	if cooling, err := collectCoolingContacts(s.db, now, st.CoolingDays); err == nil {
		if len(cooling) > 10 {
			cooling = cooling[:10]
		}
		resp["cooling"] = cooling
	} else {
		slog.Warn("助手看板：冷却联系人查询失败", "err", err)
		resp["cooling"] = []AssistantCoolingItem{}
	}
	if intimacy, err := computeIntimacy(s.db, now, 30); err == nil {
		if len(intimacy) > 10 {
			intimacy = intimacy[:10]
		}
		resp["intimacy"] = intimacy
	} else {
		slog.Warn("助手看板：亲密度查询失败", "err", err)
		resp["intimacy"] = []AssistantIntimacyItem{}
	}
	if emotions, err := recentEmotions(s.db, now, 7); err == nil {
		resp["emotions"] = emotions
	} else {
		slog.Warn("助手看板：情绪记录查询失败", "err", err)
		resp["emotions"] = []map[string]interface{}{}
	}
	resp["runs"] = s.assistantRecentRuns(10)
	resp["emailLog"] = s.assistantEmailLog(10)

	writeJSON(w, http.StatusOK, resp)
}

func (s *apiServer) assistantRecentRuns(limit int) []map[string]interface{} {
	dbMu.Lock()
	defer dbMu.Unlock()
	rows, err := s.db.Query(`SELECT kind, run_date, detail, created_at FROM assistant_runs
		ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return []map[string]interface{}{}
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var kind, runDate, detail, createdAt string
		if rows.Scan(&kind, &runDate, &detail, &createdAt) == nil {
			out = append(out, map[string]interface{}{
				"kind": kind, "runDate": runDate, "detail": detail, "createdAt": createdAt,
			})
		}
	}
	return out
}

func (s *apiServer) assistantEmailLog(limit int) []map[string]interface{} {
	dbMu.Lock()
	defer dbMu.Unlock()
	rows, err := s.db.Query(`SELECT kind, subject, recipient, status, error, created_at
		FROM email_send_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return []map[string]interface{}{}
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var kind, subject, recipient, status, errMsg, createdAt string
		if rows.Scan(&kind, &subject, &recipient, &status, &errMsg, &createdAt) == nil {
			out = append(out, map[string]interface{}{
				"kind": kind, "subject": subject, "recipient": recipient,
				"status": status, "error": errMsg, "createdAt": createdAt,
			})
		}
	}
	return out
}

// ---------- 配置 ----------

func maskSMTPPass(st AssistantSettings) AssistantSettings {
	if st.SMTP.Pass != "" {
		st.SMTP.Pass = smtpPassMask
	}
	if st.CalendarKey != "" {
		st.CalendarKey = calendarKeyMask
	}
	return st
}

func (s *apiServer) hAssistantGetSettings(w http.ResponseWriter, r *http.Request) {
	st, err := loadAssistantSettings(s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取助手配置失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, maskSMTPPass(st))
}

func (s *apiServer) hAssistantPutSettings(w http.ResponseWriter, r *http.Request) {
	var st AssistantSettings
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&st); err != nil {
		writeErr(w, http.StatusBadRequest, "配置格式错误: "+err.Error())
		return
	}
	// 前端回传打码密码时保留库里的原值
	// 读不到原值必须报错：否则 "******" 会被当成真密码写进库，
	// 既毁掉 SMTP 密码，也让 .ics 订阅密钥退化成公开可猜的固定串
	if st.SMTP.Pass == smtpPassMask || st.CalendarKey == calendarKeyMask {
		old, err := loadAssistantSettings(s.db)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取原配置失败，请稍后重试: "+err.Error())
			return
		}
		if st.SMTP.Pass == smtpPassMask {
			st.SMTP.Pass = old.SMTP.Pass
		}
		if st.CalendarKey == calendarKeyMask {
			st.CalendarKey = old.CalendarKey
		}
	}
	st.normalize()
	if err := saveAssistantSettings(s.db, st); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存助手配置失败: "+err.Error())
		return
	}
	slog.Info("关系助手配置已更新", "enabled", st.Enabled, "smtpReady", st.SMTP.Ready())
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "settings": maskSMTPPass(st)})
}

// ---------- 测试邮件 ----------

func (s *apiServer) hAssistantTestEmail(w http.ResponseWriter, r *http.Request) {
	st, err := loadAssistantSettings(s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取助手配置失败: "+err.Error())
		return
	}
	if !st.SMTP.Ready() {
		writeErr(w, http.StatusBadRequest, "SMTP 配置不完整，请先保存 host/port/from/to")
		return
	}
	subject := "【关系助手】测试邮件"
	body := emailHeader("测试邮件") +
		`<p style="color:#333;font-size:14px;line-height:1.8;">收到这封邮件说明 SMTP 配置正确，每日提醒和每周报告将发送到本邮箱。</p>` +
		emailFooter
	// 第三参是收件人（不是发件人），status 与日报/周报统一用 ok/fail，
	// 否则邮件日志里这一栏显示的是自己的发件地址、状态值也和别的记录对不上
	rcpt := strings.Join(recipients(st.SMTP.To), ",")
	if err := sendAssistantMail(st.SMTP, subject, body); err != nil {
		logEmail(s.db, "test", subject, rcpt, "fail", err.Error())
		writeErr(w, http.StatusBadGateway, "发送失败: "+err.Error())
		return
	}
	logEmail(s.db, "test", subject, rcpt, "ok", "")
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// ---------- 邮件日志 ----------

func (s *apiServer) hAssistantEmailLog(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"logs": s.assistantEmailLog(limit)})
}

// ---------- 手动情绪分析（同步，单个联系人） ----------

func (s *apiServer) hAssistantAnalyzeEmotion(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ContactID int64 `json:"contactId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.ContactID <= 0 {
		writeErr(w, http.StatusBadRequest, "请提供有效的 contactId")
		return
	}
	if s.llm == nil {
		writeErr(w, http.StatusServiceUnavailable, "LLM 未配置，无法进行情绪分析")
		return
	}
	res, err := analyzeContactEmotion(s.db, s.llm, req.ContactID, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- 立即运行（异步，避免 HTTP 超时） ----------

func (s *apiServer) hAssistantRunNow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind string `json:"kind"` // daily | weekly
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.Kind != "daily" && req.Kind != "weekly" {
		writeErr(w, http.StatusBadRequest, "kind 只能是 daily 或 weekly")
		return
	}
	st, err := loadAssistantSettings(s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取助手配置失败: "+err.Error())
		return
	}
	if !st.Enabled {
		writeErr(w, http.StatusBadRequest, "关系助手未启用，请先在设置中开启")
		return
	}
	db, llm := s.db, s.llm
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("关系助手手动任务 panic", "kind", req.Kind, "rec", rec)
			}
		}()
		var runErr error
		if req.Kind == "daily" {
			_, runErr = runDailyCheck(db, llm, time.Now(), true)
		} else {
			_, runErr = runWeeklyReport(db, llm, time.Now(), true)
		}
		if runErr != nil {
			slog.Warn("关系助手手动任务失败", "kind", req.Kind, "err", runErr)
		}
	}()
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "msg": req.Kind + " 任务已在后台启动，稍后刷新看板查看结果"})
}
