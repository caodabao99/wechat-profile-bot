package main

import (
	"strings"
	"testing"
)

func TestFormatSizeMB(t *testing.T) {
	cases := []struct {
		mb   int64
		want string
	}{
		{0, "0 MB"},
		{512, "512 MB"},
		{1024, "1.0 GB"},
		{40960, "40.0 GB"},
	}
	for _, c := range cases {
		if got := formatSizeMB(c.mb); got != c.want {
			t.Fatalf("formatSizeMB(%d) = %q, want %q", c.mb, got, c.want)
		}
	}
}

func TestFormatUptime(t *testing.T) {
	cases := []struct {
		sec  int64
		want string
	}{
		{0, "0分"},
		{59, "0分"},
		{60, "1分"},
		{3600, "1小时"},
		{3600 + 20*60, "1小时20分"},
		{3*86400 + 2*3600 + 10*60, "3天2小时10分"},
	}
	for _, c := range cases {
		if got := formatUptime(c.sec); got != c.want {
			t.Fatalf("formatUptime(%d) = %q, want %q", c.sec, got, c.want)
		}
	}
	// 负值兜底，不应出现负号
	if got := formatUptime(-1); !strings.Contains(got, "0分") {
		t.Fatalf("formatUptime(-1) = %q", got)
	}
}

func TestWeChatServerBlockDisk(t *testing.T) {
	base := SysInfo{
		DiskPath:    "/config",
		DiskTotalMB: 40 * 1024,
		MemTotalMB:  8 * 1024,
		Load1:       0.5, Load5: 0.4, Load15: 0.3,
		NumCPU:       4,
		ProcessRSSMB: 45,
		UptimeSec:    3*86400 + 2*3600 + 10*60,
	}

	// 用 63%：无告警
	ok := base
	ok.DiskFreeMB = 15 * 1024 // 15 GB 剩余 → 已用 25/40 = 62.5%
	ok.DiskUsedPercent = 63
	out := ok.WeChatServerBlock()
	for _, want := range []string{"数据目录: /config", "磁盘:", "40.0 GB", "剩余 15.0 GB", "负载:", "程序占用:", "已运行: 3天2小时10分"} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "【提醒】") || strings.Contains(out, "【紧急】") {
		t.Fatalf("63%% 不应告警:\n%s", out)
	}

	// 78%：提醒
	warn := base
	warn.DiskFreeMB = 9 * 1024
	warn.DiskUsedPercent = 78
	out = warn.WeChatServerBlock()
	if !strings.Contains(out, "【提醒】") || strings.Contains(out, "【紧急】") {
		t.Fatalf("78%% 应为提醒:\n%s", out)
	}
	if !strings.Contains(out, "备份") || !strings.Contains(out, "迁移") {
		t.Fatalf("提醒应包含备份/迁移指引:\n%s", out)
	}

	// 91%：紧急
	bad := base
	bad.DiskFreeMB = 3686 // ≈3.6 GB
	bad.DiskUsedPercent = 91
	out = bad.WeChatServerBlock()
	if !strings.Contains(out, "【紧急】") || strings.Contains(out, "【提醒】") {
		t.Fatalf("91%% 应为紧急:\n%s", out)
	}

	// 磁盘采集失败（总量 0）：显示采集失败，不误报告警
	fail := base
	fail.DiskTotalMB, fail.DiskFreeMB, fail.DiskUsedPercent = 0, 0, 0
	out = fail.WeChatServerBlock()
	if !strings.Contains(out, "磁盘: 采集失败") || strings.Contains(out, "【") {
		t.Fatalf("采集失败应明确提示且不告警:\n%s", out)
	}

	// 类 Windows：负载字段为 -1 且无内存数据时不渲染对应行
	win := base
	win.Load1, win.Load5, win.Load15, win.MemTotalMB = -1, -1, -1, 0
	win.DiskFreeMB = 15 * 1024
	out = win.WeChatServerBlock()
	if strings.Contains(out, "负载:") || strings.Contains(out, "内存:") {
		t.Fatalf("不支持的平台不应输出负载/内存行:\n%s", out)
	}
}
