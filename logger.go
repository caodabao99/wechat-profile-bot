package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// setupLogging 初始化日志：同时输出到控制台和 bot.log（与数据库同目录）。
// 返回的文件的关闭时机由 main 的 defer 负责；打开失败时退回仅控制台输出。
func setupLogging() *os.File {
	logPath := filepath.Join(filepath.Dir(dbPath()), "bot.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		slog.Warn("无法打开日志文件，仅输出到控制台", "path", logPath, "err", err)
		return nil
	}
	// TextHandler 每行自带 time=...（本地时间）level=... msg=...，方便直接贴日志定位问题
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(os.Stdout, f), nil)))
	slog.Info("日志已初始化", "file", logPath)
	return f
}

// preview 截取文本前 n 个字符用于日志展示，避免长聊天记录刷爆日志
func preview(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
