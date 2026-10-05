package main

// 高阶洞察 HTTP 接口：四个只读展示端点 + 一个异步重算入口。零操作减负：
//   GET  /api/insight/network     → 社交网络洞察（缓存缺失/过期即现算整条流水线）
//   GET  /api/insight/self        → 自我关系画像
//   GET  /api/insight/intervention→ 证据化干预学习
//   GET  /api/insight/briefing    → 主动人生简报
//   POST /api/insight/recompute   → 后台异步重算四件套 + 简报，立即返回
//
// 子路径深度校验：只认这四个已知子路径，其余一律 404（对齐 routeLife / weekly-plan 教训）。
// 自愈统一走 ComputeAdvancedInsights（先把基座人生状态补齐，再算四层，最后合成简报），
//   这样任一子页首次打开都能拿到一致且完整的一批数据。

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *apiServer) routeInsight(w http.ResponseWriter, r *http.Request, sub []string) {
	if len(sub) == 0 {
		writeErr(w, http.StatusNotFound, "未知接口: /api/insight")
		return
	}
	known := map[string]bool{"network": true, "self": true, "intervention": true, "briefing": true, "trend": true, "recompute": true}
	if !known[sub[0]] || len(sub) > 1 {
		writeErr(w, http.StatusNotFound, "未知接口: /api/insight/"+strings.Join(sub, "/"))
		return
	}

	switch sub[0] {
	case "network", "self", "intervention", "briefing":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hInsight(w, r, sub[0])
	case "trend":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hInsightTrend(w, r)
	case "recompute":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hInsightRecompute(w, r)
	}
}

// insightEnabled 读取高阶洞察开关（读失败按启用处理，纯展示不打断）。
func (s *apiServer) insightEnabled() bool {
	st, err := loadAssistantSettings(s.db)
	if err != nil {
		return true
	}
	return st.AdvancedInsightsEnabled
}

// hInsight 统一处理四个只读子页：各自缓存缺失/过期即跑整条流水线再读。
func (s *apiServer) hInsight(w http.ResponseWriter, r *http.Request, kind string) {
	now := time.Now()
	var (
		data  interface{}
		genAt time.Time
		err   error
	)

	needPipeline := false
	switch kind {
	case "network":
		data, genAt, err = GetCachedNetwork(s.db)
		needPipeline = data == nil || IsNetworkStale(genAt, now)
	case "self":
		data, genAt, err = GetCachedSelfPortrait(s.db)
		needPipeline = data == nil || IsSelfPortraitStale(genAt, now)
	case "intervention":
		data, genAt, err = GetCachedIntervention(s.db)
		needPipeline = data == nil || IsInterventionStale(genAt, now)
	case "briefing":
		data, genAt, err = GetCachedBriefing(s.db)
		needPipeline = data == nil || IsBriefingStale(genAt, now)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取失败: "+err.Error())
		return
	}

	if needPipeline {
		if err := ComputeAdvancedInsights(s.db, now); err != nil {
			writeErr(w, http.StatusInternalServerError, "计算失败: "+err.Error())
			return
		}
		switch kind {
		case "network":
			data, genAt, _ = GetCachedNetwork(s.db)
		case "self":
			data, genAt, _ = GetCachedSelfPortrait(s.db)
		case "intervention":
			data, genAt, _ = GetCachedIntervention(s.db)
		case "briefing":
			data, genAt, _ = GetCachedBriefing(s.db)
		}
	}

	payload := map[string]interface{}{
		"enabled":     s.insightEnabled(),
		"generatedAt": genAt.Format(time.RFC3339),
		kind:          data,
	}
	writeJSON(w, http.StatusOK, payload)
}

// hInsightTrend 只读趋势历史（非自愈：无历史即空数组，不触发重算）。供前端 sparkline。
func (s *apiServer) hInsightTrend(w http.ResponseWriter, r *http.Request) {
	weeks := 0
	if v := r.URL.Query().Get("weeks"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= trendMaxWeeks {
			weeks = n
		}
	}
	series, err := GetTrendSeries(s.db, weeks)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取洞察趋势失败")
		return
	}
	var latest *TrendSnapshot
	if len(series) > 0 {
		latest = &series[len(series)-1]
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled": s.insightEnabled(),
		"weeks":   series,
		"latest":  latest,
	})
}

func (s *apiServer) hInsightRecompute(w http.ResponseWriter, r *http.Request) {
	db := s.db
	go safeAssistantTask("高阶洞察手动重算", func() {
		now := time.Now()
		if err := ComputeAdvancedInsights(db, now); err != nil {
			slog.Warn("高阶洞察手动重算失败", "err", err)
		}
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":  true,
		"msg": "高阶洞察（网络/自我/干预/简报）已在后台重算，稍后刷新查看",
	})
}
