package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// appVersion 程序版本号，/api/status 与日志使用
const appVersion = "v2.3.2"

// progStart 进程启动时刻（包初始化即记录，早于 main 里的扫码登录）
var progStart = time.Now()

// SysInfo 服务器与进程运行状态。全部为快照值，字段名即对外 JSON 键名。
type SysInfo struct {
	Version  string `json:"version"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	NumCPU   int    `json:"numCpu"`

	UptimeSec int64 `json:"uptimeSec"`

	// 进程指标
	ProcessCPUPercent float64 `json:"processCpuPercent"` // 占整机 CPU 百分比（0~100*核数，但这里按整机口径折算到 0~100）
	ProcessRSSMB      int64   `json:"processRssMb"`      // 进程常驻内存（工作集）
	Goroutines        int     `json:"goroutines"`
	GoHeapAllocMB     int64   `json:"goHeapAllocMb"` // Go 堆在用

	// 系统内存（MB）
	MemTotalMB     int64   `json:"memTotalMb"`
	MemAvailableMB int64   `json:"memAvailableMb"`
	MemUsedPercent float64 `json:"memUsedPercent"`
	Load1          float64 `json:"load1"` // 不支持的平台为 -1
	Load5          float64 `json:"load5"`
	Load15         float64 `json:"load15"`

	// 数据目录所在磁盘
	DiskPath        string  `json:"diskPath"`
	DiskTotalMB     int64   `json:"diskTotalMb"`
	DiskFreeMB      int64   `json:"diskFreeMb"`
	DiskUsedPercent float64 `json:"diskUsedPercent"`
}

// cpuSampleMu/lastCPUTicks/lastCPUTime 用于两次采样间计算进程 CPU 占用率。
// 进程 CPU 时间由平台代码读取（Linux: /proc/self/stat；Windows: GetProcessTimes）。
var (
	cpuSampleMu  sync.Mutex
	lastCPUTicks float64
	lastCPUTime  time.Time
)

// CollectSysInfo 采集一次系统与进程快照。diskPath 为要统计的磁盘路径（数据目录）。
// 任何单项采集失败都不阻断整体，失败项保持零值。
func CollectSysInfo(diskPath string) SysInfo {
	info := SysInfo{
		Version:    appVersion,
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		NumCPU:     runtime.NumCPU(),
		Load1:      -1,
		Load5:      -1,
		Load15:     -1,
		UptimeSec:  int64(time.Since(progStart).Seconds()),
		Goroutines: runtime.NumGoroutine(),
		DiskPath:   diskPath,
	}
	info.Hostname, _ = os.Hostname()

	// Go 堆
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	info.GoHeapAllocMB = int64(ms.HeapAlloc / 1024 / 1024)

	// 进程 RSS 与系统内存（平台实现）
	if total, avail, rss, err := readMemory(); err == nil {
		info.MemTotalMB = int64(total / 1024 / 1024)
		info.MemAvailableMB = int64(avail / 1024 / 1024)
		info.ProcessRSSMB = int64(rss / 1024 / 1024)
		if total > 0 {
			info.MemUsedPercent = roundPercent(float64(total-avail) * 100 / float64(total))
		}
	}

	// 负载（仅类 Unix）
	if l1, l5, l15, err := readLoadAvg(); err == nil {
		info.Load1, info.Load5, info.Load15 = l1, l5, l15
	}

	// 进程 CPU：与上次采样比较
	if ticks, err := processCPUTimeSeconds(); err == nil {
		now := time.Now()
		cpuSampleMu.Lock()
		if !lastCPUTime.IsZero() {
			wall := now.Sub(lastCPUTime).Seconds()
			if wall > 0 {
				// ticks/wall 是相对单核满载的倍数；除以核数折算为占整机百分比
				pct := (ticks - lastCPUTicks) / wall / float64(runtime.NumCPU()) * 100
				if pct < 0 {
					pct = 0
				}
				if pct > 100 {
					pct = 100
				}
				info.ProcessCPUPercent = roundPercent(pct)
			}
		}
		lastCPUTicks = ticks
		lastCPUTime = now
		cpuSampleMu.Unlock()
	}

	// 数据目录所在磁盘
	if total, free, err := diskUsage(diskPath); err == nil {
		info.DiskTotalMB = int64(total / 1024 / 1024)
		info.DiskFreeMB = int64(free / 1024 / 1024)
		if total > 0 {
			info.DiskUsedPercent = roundPercent(float64(total-free) * 100 / float64(total))
		}
	}

	return info
}

func roundPercent(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

// formatSizeMB 把 MB 数转成易读字符串，与网页端 fmtMB 口径一致（≥1024MB 显示一位小数 GB）。
func formatSizeMB(mb int64) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(mb)/1024)
	}
	return fmt.Sprintf("%d MB", mb)
}

// formatUptime 把秒数转成「3天2小时10分」式中文时长。
func formatUptime(sec int64) string {
	if sec < 0 {
		sec = 0
	}
	d := sec / 86400
	h := sec % 86400 / 3600
	m := sec % 3600 / 60
	var parts []string
	if d > 0 {
		parts = append(parts, fmt.Sprintf("%d天", d))
	}
	if h > 0 {
		parts = append(parts, fmt.Sprintf("%d小时", h))
	}
	if m > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%d分", m))
	}
	return strings.Join(parts, "")
}

// WeChatServerBlock 渲染微信「状态」命令里的服务器资源段落。
// 重点是数据盘剩余空间：磁盘写满会导致数据库写入失败，
// 阈值与网页状态页一致（≥70% 提醒、≥90% 紧急），提示用户提前备份迁移。
func (s SysInfo) WeChatServerBlock() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n已运行: %s", formatUptime(s.UptimeSec))
	fmt.Fprintf(&b, "\n\n服务器资源（数据目录: %s）", s.DiskPath)

	if s.DiskTotalMB > 0 {
		usedMB := s.DiskTotalMB - s.DiskFreeMB
		fmt.Fprintf(&b, "\n磁盘: 已用 %s / %s（%.0f%%），剩余 %s",
			formatSizeMB(usedMB), formatSizeMB(s.DiskTotalMB), s.DiskUsedPercent, formatSizeMB(s.DiskFreeMB))
	} else {
		b.WriteString("\n磁盘: 采集失败")
	}

	if s.MemTotalMB > 0 {
		fmt.Fprintf(&b, "\n内存: %s / %s（%.0f%%）",
			formatSizeMB(s.MemTotalMB-s.MemAvailableMB), formatSizeMB(s.MemTotalMB), s.MemUsedPercent)
	}
	if s.Load1 >= 0 {
		fmt.Fprintf(&b, "\n负载: %.2f / %.2f / %.2f（%d 核）", s.Load1, s.Load5, s.Load15, s.NumCPU)
	}
	fmt.Fprintf(&b, "\n程序占用: CPU %.1f%%，内存 %s", s.ProcessCPUPercent, formatSizeMB(s.ProcessRSSMB))

	switch {
	case s.DiskTotalMB > 0 && s.DiskUsedPercent >= 90:
		fmt.Fprintf(&b, "\n【紧急】磁盘已用 %.0f%%，仅剩 %s！磁盘写满会导致数据库异常，请尽快到网页端备份下载，然后迁移到磁盘更大的服务器",
			s.DiskUsedPercent, formatSizeMB(s.DiskFreeMB))
	case s.DiskTotalMB > 0 && s.DiskUsedPercent >= 70:
		fmt.Fprintf(&b, "\n【提醒】磁盘已用 %.0f%%，剩余 %s，建议提前在网页端备份；空间继续减少请准备迁移服务器",
			s.DiskUsedPercent, formatSizeMB(s.DiskFreeMB))
	}
	return b.String()
}
