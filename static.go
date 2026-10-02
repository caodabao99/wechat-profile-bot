package main

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFS embed.FS

// handleWebUI 服务网页管理界面入口页（内嵌，无需外部文件）。
// 前端是 hash 路由（#/xxx）的单页应用，任意路径都返回同一个 index.html。
// 静态页面本身不含敏感数据，无需认证；所有数据接口都在 /api/ 下受 Bearer Token 保护。
func handleWebUI(w http.ResponseWriter, r *http.Request) {
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "前端资源缺失: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// handleAssets 服务内嵌静态资源（vue / app.js / style.css）
func handleAssets(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.StripPrefix("/assets/", http.FileServer(http.FS(sub))).ServeHTTP(w, r)
}
