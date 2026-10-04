package main

// 人生模拟器 HTTP 接口。全部只读展示 + 一个异步重算入口，零操作减负：
//   GET  /api/life/state       → 人生状态快照（缓存缺失/过期即现算，纯 SQL 很快）
//   GET  /api/life/projection  → 90 天推演（同上）
//   GET  /api/life/timeline    → 人生年表（每次现算，聚合很轻）
//   POST /api/life/recompute   → 后台异步重算 state+projection，立即返回
//
// 子路径深度校验：只认这三个已知子路径，其余一律 404（对齐 weekly-plan 教训）。

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
)

func (s *apiServer) routeLife(w http.ResponseWriter, r *http.Request, sub []string) {
	if len(sub) == 0 {
		writeErr(w, http.StatusNotFound, "未知接口: /api/life")
		return
	}
	// 已知子路径不接受更深层级
	known := map[string]bool{"state": true, "projection": true, "timeline": true, "recompute": true}
	if !known[sub[0]] || len(sub) > 1 {
		writeErr(w, http.StatusNotFound, "未知接口: /api/life/"+strings.Join(sub, "/"))
		return
	}

	switch sub[0] {
	case "state":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hLifeState(w, r)
	case "projection":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hLifeProjection(w, r)
	case "timeline":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hLifeTimeline(w, r)
	case "recompute":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.hLifeRecompute(w, r)
	}
}

// lifeEnabled 读取人生模拟器开关（读失败按启用处理，纯展示不打断）。
func (s *apiServer) lifeEnabled() bool {
	st, err := loadAssistantSettings(s.db)
	if err != nil {
		return true
	}
	return st.LifeSimEnabled
}

func (s *apiServer) hLifeState(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	st, genAt, err := GetCachedLifeState(s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取人生状态失败: "+err.Error())
		return
	}
	// 自愈：无缓存或过期即现算（纯 SQL，零模型成本，首次打开就能看到）
	if st == nil || IsLifeStateStale(genAt, now) {
		if err := ComputeLifeState(s.db, now); err != nil {
			writeErr(w, http.StatusInternalServerError, "计算人生状态失败: "+err.Error())
			return
		}
		st, genAt, err = GetCachedLifeState(s.db)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取人生状态失败: "+err.Error())
			return
		}
	}
	if st == nil {
		st = &LifeState{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":     s.lifeEnabled(),
		"generatedAt": genAt.Format(time.RFC3339),
		"state":       st,
	})
}

func (s *apiServer) hLifeProjection(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	proj, genAt, err := GetCachedLifeProjection(s.db)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取人生推演失败: "+err.Error())
		return
	}
	if proj == nil || IsLifeStateStale(genAt, now) {
		if err := ProjectLifeForward(s.db, now, 90); err != nil {
			writeErr(w, http.StatusInternalServerError, "计算人生推演失败: "+err.Error())
			return
		}
		proj, genAt, err = GetCachedLifeProjection(s.db)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取人生推演失败: "+err.Error())
			return
		}
	}
	if proj == nil {
		proj = &LifeProjection{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":     s.lifeEnabled(),
		"generatedAt": genAt.Format(time.RFC3339),
		"projection":  proj,
	})
}

func (s *apiServer) hLifeTimeline(w http.ResponseWriter, r *http.Request) {
	nar, err := BuildLifeTimeline(s.db, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成人生年表失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":   s.lifeEnabled(),
		"narrative": nar,
	})
}

func (s *apiServer) hLifeRecompute(w http.ResponseWriter, r *http.Request) {
	db := s.db
	go safeAssistantTask("人生模拟器手动重算", func() {
		now := time.Now()
		if err := ComputeLifeState(db, now); err != nil {
			slog.Warn("人生状态手动重算失败", "err", err)
			return
		}
		if err := ProjectLifeForward(db, now, 90); err != nil {
			slog.Warn("人生推演手动重算失败", "err", err)
		}
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "msg": "人生模拟器已在后台重算，稍后刷新查看"})
}
