package main

// 微信 iLink 会话重绑接口（网页端自助扫码）。
//
// 为什么要有它（审计 C1）：会话过期(-14) 是必然事件，而唯一旧出路是「让用户在微信里发
// 『重登』」——可会话一过期就收不到任何消息，这条路径把自己堵死了；重启也没用（凭据还在
// → 跳过扫码 → 再次 -14）。等于上线后机器人必然静默死亡一次，只能手工删凭据文件。
// 现在把恢复动作搬到永远可达的网页端：删凭据 → 出二维码 → 用户手机微信扫码 → 自动恢复轮询。

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// routeWechat /api/wechat/* 子路由。
//
//	GET  /api/wechat/bind    → 当前重绑与会话状态（waiting 时含二维码 dataURL，前端可直接 <img>）
//	POST /api/wechat/rebind  → 启动一次重新扫码（幂等；5 分钟冷却，冷却中返回 429）
func (s *apiServer) routeWechat(w http.ResponseWriter, r *http.Request, sub []string) {
	if len(sub) != 1 {
		writeErr(w, http.StatusNotFound, "未知接口: /api/wechat/"+strings.Join(sub, "/"))
		return
	}
	if s.client == nil {
		writeErr(w, http.StatusServiceUnavailable, "iLink 客户端未初始化")
		return
	}
	switch sub[0] {
	case "bind":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET")
			return
		}
		writeJSON(w, http.StatusOK, s.client.RebindSnapshot())
	case "rebind":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
			return
		}
		started, err := s.client.StartRebind()
		if err != nil {
			if errors.Is(err, errRebindCooldown) { // 冷却中：可重试，不是服务故障
				w.Header().Set("Retry-After", strconv.Itoa(int(rebindCooldown.Seconds())))
				writeErr(w, http.StatusTooManyRequests, "重绑刚试过，正在冷却中（连续取码会被微信服务端限流）")
				return
			}
			writeErr(w, http.StatusInternalServerError, "启动重绑失败: "+err.Error())
			return
		}
		slog.Info("网页发起微信重绑", "started", started)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"started": started,
			"note":    "二维码就绪后用页面显示的图片扫码；扫码成功即自动恢复消息轮询，无需重启程序",
			"state":   s.client.RebindSnapshot(),
		})
	default:
		writeErr(w, http.StatusNotFound, "未知接口: /api/wechat/"+sub[0])
	}
}
