package main

// Relationship Portfolio API（蓝图 §12 · P8）。
//   GET  /api/portfolio               → 组合仪表盘（§12.3：预算/已使用/建议分配/最值得投入的人）
//   GET  /api/portfolio/settings      → 当前组合设置（预算/类别权重/逐人手指定）
//   PUT  /api/portfolio/settings      → 保存组合设置（§12.2 用户可改预算/权重/手指定）
//   POST /api/portfolio/settings      → 同 PUT（便于前端 fetch）
// 只建议、不替用户决定；响应 Layer 恒 INFERENCE。设置是用户自建配置（参与备份、不可重建）。

import (
	"net/http"
	"strconv"
	"time"
)

func (s *apiServer) routePortfolio(w http.ResponseWriter, r *http.Request, sub []string) {
	// /api/portfolio —— 仪表盘（只读聚合）
	if len(sub) == 0 {
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		topN := 0
		if v := r.URL.Query().Get("topN"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				topN = n
			}
		}
		view, err := ComputePortfolio(s.db, time.Now(), topN)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "构建关系组合失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, view)
		return
	}

	// /api/portfolio/settings —— 设置读写
	if len(sub) == 1 && sub[0] == "settings" {
		switch r.Method {
		case http.MethodGet:
			set, err := loadPortfolioSettings(s.db)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "读取组合设置失败: "+err.Error())
				return
			}
			writeJSON(w, http.StatusOK, set)
		case http.MethodPut, http.MethodPost:
			var in PortfolioSettings
			if !readBody(w, r, &in) {
				return
			}
			in.normalize()
			if err := savePortfolioSettings(s.db, in); err != nil {
				writeErr(w, http.StatusInternalServerError, "保存组合设置失败: "+err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "settings": in})
		default:
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		}
		return
	}

	writeErr(w, http.StatusNotFound, "未知接口")
}
