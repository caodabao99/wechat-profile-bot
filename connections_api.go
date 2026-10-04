package main

// 关系图谱 HTTP 接口。
//
// 全局（route() 里 case "relationships" 子路径分发）：
//   GET  /api/relationships/connections           → 全量连线列表
//   POST /api/relationships/connections/rebuild   → 手动重建
//
// 单联系人（routeContact 下）：
//   GET  /api/contacts/{id}/connections           → 某联系人的关联

import (
	"net/http"
)

// routeConnections 处理 /api/relationships/connections[/rebuild]
func (s *apiServer) routeConnections(w http.ResponseWriter, r *http.Request, sub []string) {
	// GET /api/relationships/connections
	if len(sub) == 0 && r.Method == http.MethodGet {
		s.hConnectionsList(w, r)
		return
	}
	// POST /api/relationships/connections/rebuild
	if len(sub) == 1 && sub[0] == "rebuild" && r.Method == http.MethodPost {
		s.hConnectionsRebuild(w, r)
		return
	}
	writeErr(w, http.StatusNotFound, "未知接口")
}

func (s *apiServer) hConnectionsList(w http.ResponseWriter, r *http.Request) {
	items, err := ListConnections(s.db, 0, 500)
	// 自愈：表不存在（老库尚未跑 v15）或从未算过时自动重建（纯 SQL，零模型成本）
	if err != nil || len(items) == 0 {
		if _, rebuildErr := RebuildAllConnections(s.db); rebuildErr == nil {
			items, err = ListConnections(s.db, 0, 500)
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "查询关系图谱失败: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"connections": items})
}

func (s *apiServer) hConnectionsRebuild(w http.ResponseWriter, r *http.Request) {
	n, err := RebuildAllConnections(s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "重建关系图谱失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "count": n})
}

// routeContactConnections 处理 GET /api/contacts/{id}/connections
func (s *apiServer) routeContactConnections(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	items, err := ListConnections(s.db, id, 100)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询联系人关联失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"connections": items})
}
