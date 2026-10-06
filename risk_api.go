package main

// Relationship Risk Center API（蓝图 §13 · P9）。
//   GET /api/risks                 → 全部风险清单（可选 ?type= 过滤、?severity= 过滤）
//   响应：{ok, count, byType:{...}, items:[RiskItem...]}
// 纯只读聚合：不新建分数、不落库、不触发因果断言；异常时 500 前先尽力降级由各源独立容错。

import (
	"net/http"
	"time"
)

func (s *apiServer) routeRisks(w http.ResponseWriter, r *http.Request, sub []string) {
	// 仅暴露根列表：/api/risks 或 /api/risks/（无子路径）
	if len(sub) > 0 {
		writeErr(w, http.StatusNotFound, "未知接口")
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	items, err := BuildRisks(s.db, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "构建风险清单失败: "+err.Error())
		return
	}
	q := r.URL.Query()
	if ty := q.Get("type"); ty != "" {
		filtered := items[:0:0]
		for _, it := range items {
			if it.Type == ty {
				filtered = append(filtered, it)
			}
		}
		items = filtered
	}
	if sev := q.Get("severity"); sev != "" {
		filtered := items[:0:0]
		for _, it := range items {
			if it.Severity == sev {
				filtered = append(filtered, it)
			}
		}
		items = filtered
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":     true,
		"count":  len(items),
		"byType": CountRisksByType(items),
		"items":  items,
	})
}
