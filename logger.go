package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// 日志自转参数（投产前审计 N5）：本程序长跑下每条消息都会写 INFO，再叠加安全事
// 件与 panic/账本告警，无上限日志在 NAS/容器盘上是一定会被填完的东西；磁盘满后
// 备份、游标落盘、VACUUM INTO 会一起失败（跟 C9 属同一类“平时不出事”的故障）。
const (
	logMaxBytes = 20 << 20 // 单文件上限 20MB
	logKeep     = 3        // 保留 .1 .2 .3 三份历史
)

// rotateIfNeeded 用「先复制再截断」的方式轮转日志：不换文件、不关 fd，所以对已经
// 持有 *os.File 的 slog handler（bot.log / security.log）安全，无需重建默认 logger。
// 代价：复制与截断之间写的几行可能丢，对日志可接受；相比爆盘，这不是问题。
func rotateIfNeeded(path string, maxBytes int64, keep int) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() < maxBytes {
		return false, nil // 不存在或未超限：不动
	}
	for i := keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", path, i), fmt.Sprintf("%s.%d", path, i+1))
	}
	src, err := os.Open(path)
	if err != nil {
		return false, err
	}
	dst, err := os.OpenFile(path+".1", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		src.Close()
		return false, err
	}
	_, cpErr := io.Copy(dst, src)
	closeErr1 := src.Close()
	closeErr2 := dst.Close()
	if cpErr != nil || closeErr1 != nil || closeErr2 != nil {
		// 复制不完整就保留截断前的原文件：宁可文件大，也不丢内容
		return false, cpErr
	}
	if err := os.Truncate(path, 0); err != nil {
		return false, err
	}
	return true, nil
}

// rotateAllLogs 轮转本程序自管的两份日志（bot.log 与 security.log）。
// 由启动时与长期运行中的周期性维护调用（副本不关 fd，所以随时调用都安全）。
func rotateAllLogs() {
	for _, p := range []string{filepath.Join(filepath.Dir(dbPath()), "bot.log"), securityLogPath()} {
		rotated, err := rotateIfNeeded(p, logMaxBytes, logKeep)
		if err != nil {
			slog.Warn("日志轮转失败，本次跳过", "file", p, "err", err)
		} else if rotated {
			slog.Info("日志已轮转", "file", p, "keep", logKeep)
		}
	}
}

// setupLogging 初始化日志：同时输出到控制台和 bot.log（与数据库同目录）。
// 返回的文件的关闭时机由 main 的 defer 负责；打开失败时退回仅控制台输出。
func setupLogging() *os.File {
	logPath := filepath.Join(filepath.Dir(dbPath()), "bot.log")
	if rotated, err := rotateIfNeeded(logPath, logMaxBytes, logKeep); err != nil {
		slog.Warn("bot.log 轮转失败，本次跳过", "err", err)
	} else if rotated {
		slog.Info("bot.log 已轮转", "file", logPath, "keep", logKeep)
	}
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
