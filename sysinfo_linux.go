//go:build linux

package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// readMemory 返回（系统总内存、可用内存、进程 RSS），单位字节。
// 数据来源：/proc/meminfo 与 /proc/self/stat（第 24 字段 rss，单位页）。
func readMemory() (total, available, rss uint64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total = parseProcKB(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			available = parseProcKB(line)
		}
	}
	if err := sc.Err(); err != nil {
		return 0, 0, 0, err
	}

	// /proc/self/stat 第 24 个字段为 resident set size（页数）
	if data, rerr := os.ReadFile("/proc/self/stat"); rerr == nil {
		fields := strings.Fields(string(data))
		if len(fields) >= 24 {
			if pages, perr := strconv.ParseUint(fields[23], 10, 64); perr == nil {
				rss = pages * uint64(os.Getpagesize())
			}
		}
	}
	return total, available, rss, nil
}

// parseProcKB 解析 "MemTotal:       16384000 kB" 为字节
func parseProcKB(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	kb, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return kb * 1024
}

// readLoadAvg 读 /proc/loadavg 的 1/5/15 分钟平均负载
func readLoadAvg() (float64, float64, float64, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return 0, 0, 0, nil
	}
	l1, _ := strconv.ParseFloat(fields[0], 64)
	l5, _ := strconv.ParseFloat(fields[1], 64)
	l15, _ := strconv.ParseFloat(fields[2], 64)
	return l1, l5, l15, nil
}

// processCPUTimeSeconds 返回本进程累计 CPU 时间（用户态+内核态），单位秒。
// /proc/self/stat：第 14 字段 utime、第 15 字段 stime，单位 clock tick。
func processCPUTimeSeconds() (float64, error) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 15 {
		return 0, nil
	}
	utime, _ := strconv.ParseFloat(fields[13], 64)
	stime, _ := strconv.ParseFloat(fields[14], 64)
	// USER_HZ 在 Linux 上固定为 100（内核 ABI 保证，与发行版无关）
	return (utime + stime) / 100, nil
}

// diskUsage 返回路径所在文件系统的（总容量、可用容量），单位字节
func diskUsage(path string) (uint64, uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)
	return total, free, nil
}
