package main

// 核心纯函数/工具函数测试（此前均 0% 覆盖）。这些是散落在各模块、被业务逻辑依赖的
// 小函数——看似简单但一旦语义错（如 clamp/去注入/最小 id/列表截断）会静默影响功能。

import (
	"errors"
	"strings"
	"testing"
)

func TestCoachKindAction(t *testing.T) {
	cases := map[string]string{
		"cooling":  "主动问候，重启话题",
		"silence":  "发条消息打破沉默",
		"no_reply": "回复对方那条还没回的消息",
		"":         "保持联系",
		"unknown":  "保持联系",
	}
	for kind, want := range cases {
		if got := coachKindAction(kind); got != want {
			t.Errorf("coachKindAction(%q)=%q 期望=%q", kind, got, want)
		}
	}
}

func TestIfEmpty(t *testing.T) {
	if got := ifEmpty("", "fb"); got != "fb" {
		t.Errorf("空串应回落，得 %q", got)
	}
	if got := ifEmpty("x", "fb"); got != "x" {
		t.Errorf("非空应原样，得 %q", got)
	}
	// 仅空格不算空（实现只判 == ""）
	if got := ifEmpty(" ", "fb"); got != " " {
		t.Errorf("空格串应保持原样，得 %q", got)
	}
}

func TestFirstID(t *testing.T) {
	if got := firstID(NetworkCluster{}); got != 0 {
		t.Errorf("空簇应返回 0，得 %d", got)
	}
	if got := firstID(NetworkCluster{MemberIDs: []int64{}}); got != 0 {
		t.Errorf("空 MemberIDs 应返回 0，得 %d", got)
	}
	if got := firstID(NetworkCluster{MemberIDs: []int64{9, 3, 7, 12, 5}}); got != 3 {
		// 联系人 id 恒为正（SQLite 自增），取最小值
		t.Errorf("应取最小 id，得 %d", got)
	}
	if got := firstID(NetworkCluster{MemberIDs: []int64{42}}); got != 42 {
		t.Errorf("单元素应返回自身，得 %d", got)
	}
	if got := firstID(NetworkCluster{MemberIDs: []int64{10, 2, 8}}); got != 2 {
		t.Errorf("应取最小，得 %d", got)
	}
}

func TestIsNoSuchTable(t *testing.T) {
	if !isNoSuchTable(errors.New("no such table: foo")) {
		t.Error("应识别 no such table")
	}
	if isNoSuchTable(nil) {
		t.Error("nil 不应为真")
	}
	if isNoSuchTable(errors.New("syntax error")) {
		t.Error("其它错误不应误判为表不存在")
	}
}

func TestCleanList(t *testing.T) {
	// 去空 + trim + 限量（rehearsalMaxListItems=8）
	in := []string{"  ", "a", "", "b", "   c   "}
	got := cleanList(in, 10)
	want := []string{"a", "b", "c"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("cleanList=%v 期望=%v", got, want)
	}
	// 超过上限截断到 8 条
	var many []string
	for i := 0; i < 20; i++ {
		many = append(many, "x")
	}
	if got := cleanList(many, 10); len(got) != rehearsalMaxListItems {
		t.Fatalf("应限量 %d，得 %d", rehearsalMaxListItems, len(got))
	}
	// 按 rune 截断并加省略号
	long := cleanList([]string{"一二三四五六七八九十里"}, 5)
	if len(long) != 1 || long[0] != "一二三四五…" {
		t.Fatalf("应按 rune 截断加省略号，得 %#v", long)
	}
	if empty := cleanList(nil, 5); len(empty) != 0 {
		t.Fatalf("nil 输入应返回空切片，得 %v", empty)
	}
}

func TestHeaderSafe(t *testing.T) {
	// 邮件头注入防护：CR/LF 必须被压成空格
	if got := headerSafe("A\r\nBcc: evil@x"); got != "A  Bcc: evil@x" {
		t.Fatalf("CR/LF 未清除: %q", got)
	}
	if got := headerSafe("no-newline"); got != "no-newline" {
		t.Fatalf("正常值不应被改: %q", got)
	}
	if strings.ContainsAny(headerSafe("a\nb\rc"), "\r\n") {
		t.Fatal("结果不应含裸换行")
	}
}

func TestExtractQRPayload(t *testing.T) {
	// 历史形态：第三方二维码服务把真正 payload 放在 ?data=
	if got := extractQRPayload("https://api.qrserver.com/v1/create-qr-code/?data=weixin://dl/business%2Fxxx"); got == "" || got == "https://api.qrserver.com" {
		t.Fatalf("应取出 data 参数值，得 %q", got)
	}
	if got := extractQRPayload("https://x/y?data=HELLO"); got != "HELLO" {
		t.Fatalf("应取到 HELLO，得 %q", got)
	}
	// 无 data 参数 → 原样返回
	orig := "weixin://dl/business/?t=abc"
	if got := extractQRPayload(orig); got != orig {
		t.Fatalf("无 data 参数应原样返回，得 %q", got)
	}
	// 非法 URL（含控制字符）→ url.Parse 报错，原样返回不 panic
	bad := "://not a url"
	if got := extractQRPayload(bad); got != bad {
		t.Fatalf("解析失败应原样返回，得 %q", got)
	}
}

func TestOrUnknown(t *testing.T) {
	if got := orUnknown(""); got != "暂无" {
		t.Errorf("空应显示暂无，得 %q", got)
	}
	if got := orUnknown("   "); got != "暂无" {
		t.Errorf("全空格应显示暂无，得 %q", got)
	}
	if got := orUnknown("张三"); got != "张三" {
		t.Errorf("非空应原样，得 %q", got)
	}
}

func TestClampSummaryDays(t *testing.T) {
	cases := map[int]int{
		0:                    summaryDefaultDays,
		-7:                   summaryDefaultDays,
		1:                    1,
		7:                    7,
		summaryMaxDays:       summaryMaxDays,
		summaryMaxDays + 999: summaryMaxDays,
	}
	for in, want := range cases {
		if got := clampSummaryDays(in); got != want {
			t.Errorf("clampSummaryDays(%d)=%d 期望=%d", in, got, want)
		}
	}
}

func TestSortQualityDims(t *testing.T) {
	dims := []QualityDim{
		{Key: "responsiveness", Label: "回应"},
		{Key: "balance", Label: "均衡"},
		{Key: "depth", Label: "深度"},
		{Key: "initiation", Label: "主动"},
	}
	sortQualityDims(dims)
	for i := 1; i < len(dims); i++ {
		if dims[i-1].Key > dims[i].Key {
			t.Fatalf("未按 Key 升序: %v", dims)
		}
	}
	// 空 / 单元素不 panic
	sortQualityDims(nil)
	sortQualityDims([]QualityDim{{Key: "z"}})
}
