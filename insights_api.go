package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// routeInsights /api/insights/{sub} 子路由（全部只读）
func (s *apiServer) routeInsights(w http.ResponseWriter, r *http.Request, sub []string) {
	switch {
	case len(sub) == 1 && sub[0] == "duplicates" && r.Method == http.MethodGet:
		s.hInsightDuplicates(w, r)
	case len(sub) == 1 && sub[0] == "social" && r.Method == http.MethodGet:
		s.hInsightSocial(w, r)
	case len(sub) == 1 && sub[0] == "report" && r.Method == http.MethodGet:
		s.hInsightReport(w, r)
	case len(sub) == 1 && sub[0] == "period-report" && r.Method == http.MethodGet:
		s.hInsightPeriodReport(w, r)
	default:
		writeErr(w, http.StatusNotFound, "未知洞察接口")
	}
}

// hInsightDuplicates GET /api/insights/duplicates 疑似重复联系人
func (s *apiServer) hInsightDuplicates(w http.ResponseWriter, r *http.Request) {
	res, err := FindDuplicateContacts(s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "扫描重复联系人失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// hInsightSocial GET /api/insights/social?days=30 社交大盘
func (s *apiServer) hInsightSocial(w http.ResponseWriter, r *http.Request) {
	days := 0
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "days 必须是非负整数")
			return
		}
		days = n
	}
	st, err := ComputeSocialStats(s.db, days)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "统计社交数据失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// hInsightReport GET /api/insights/report?year=2026&format=html 年度关系报告
func (s *apiServer) hInsightReport(w http.ResponseWriter, r *http.Request) {
	year := time.Now().Year()
	if v := r.URL.Query().Get("year"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 2000 || n > 2100 {
			writeErr(w, http.StatusBadRequest, "year 必须是 2000~2100 之间的年份")
			return
		}
		year = n
	}
	rep, err := BuildAnnualReport(s.db, year)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成年度报告失败: "+err.Error())
		return
	}
	if r.URL.Query().Get("format") == "html" {
		body := RenderReportHTML(rep)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(body))
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// hInsightPeriodReport GET /api/insights/period-report?period=day|week|month|quarter|half|year&anchor=YYYY-MM-DD[&format=html]
// 多周期关系报告（日/周/月/季/半年/年）。anchor 缺省为今天；返回 JSON 供网页渲染，format=html 输出可分享的自包含长页。
func (s *apiServer) hInsightPeriodReport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	period := strings.TrimSpace(q.Get("period"))
	if period == "" {
		period = "week"
	}
	if !validPeriods[period] {
		writeErr(w, http.StatusBadRequest, "period 只能是 day/week/month/quarter/half/year")
		return
	}
	anchor := time.Now()
	if v := strings.TrimSpace(q.Get("anchor")); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, time.Local)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "anchor 必须是 YYYY-MM-DD 日期")
			return
		}
		anchor = t
	}
	rep, err := BuildPeriodReport(s.db, period, anchor)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成报告失败: "+err.Error())
		return
	}
	if q.Get("format") == "html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(RenderPeriodHTML(rep)))
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
