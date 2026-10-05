package main

// v5.4.0 七功能纯函数单测（确定性、脱库、零 LLM）。
// 覆盖：projectCooling / latencyStats / timeSignature / detectTagConflicts /
//
//	lexicalRichness / avgLen / markerShare / emojiShare / mirrorDelta / mirrorTraits。
//
// 端点可达性 + 响应键完整性见 feature_sweep_test.go（heatmap/rhythm/mirror/tags-conflicts/data-report）。

import (
	"math"
	"reflect"
	"testing"
)

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestV54ProjectCooling(t *testing.T) {
	cases := []struct {
		name      string
		recent    int
		prior     int
		sinceLast int
		wantLvl   string
		wantEta   int
	}{
		{"持平不算降温", 10, 10, 5, "none", 25},
		{"近期升温(df=1)", 5, 0, 3, "none", 27},
		{"两头皆无即沉寂", 0, 0, 40, "urgent", 0},
		{"急跌urgent(df<0.5)", 2, 10, 10, "urgent", 20},
		{"沉默过多eta<=15", 6, 10, 20, "urgent", 10},
		{"温和冷却watching", 6, 10, 5, "watching", 25},
	}
	for _, c := range cases {
		lvl, eta := projectCooling(c.recent, c.prior, c.sinceLast)
		if lvl != c.wantLvl || eta != c.wantEta {
			t.Errorf("projectCooling(%d,%d,%d)=[%s,%d] 期望 [%s,%d]", c.recent, c.prior, c.sinceLast, lvl, eta, c.wantLvl, c.wantEta)
		}
	}
	// 排序权重：urgent 最靠前。
	if !(alertRank("urgent") < alertRank("watching") && alertRank("watching") < alertRank("none")) {
		t.Errorf("alertRank 排序权重应 urgent<watching<none")
	}
}

func TestV54LatencyStats(t *testing.T) {
	// 空输入 → 全 0。
	if m, f := latencyStats(nil); m != 0 || f != 0 {
		t.Errorf("latencyStats(nil)=%d,%.2f 期望 0,0", m, f)
	}
	// 单条 1 分钟：中位 1，秒回率 100（1<=5）。
	if m, f := latencyStats([][2]int64{{0, 60}}); m != 1 || !almostEqual(f, 100) {
		t.Errorf("latencyStats(1min)=%d,%.2f 期望 1,100", m, f)
	}
	// 两条 5/15 分钟：中位 (5+15)/2=10，秒回 1/2=50%。
	m, f := latencyStats([][2]int64{{0, 300}, {0, 900}})
	if m != 10 || !almostEqual(f, 50) {
		t.Errorf("latencyStats(5,15min)=%d,%.2f 期望 10,50", m, f)
	}
	// 负间隔应被跳过（脏数据不污染统计）。
	if m, f := latencyStats([][2]int64{{600, 0}}); m != 0 || f != 0 {
		t.Errorf("latencyStats(负间隔)=%d,%.2f 期望 0,0（脏数据应过滤）", m, f)
	}
}

func TestV54TimeSignature(t *testing.T) {
	mk := func(pairs map[int]int) []int {
		h := make([]int, 24)
		for k, v := range pairs {
			h[k] = v
		}
		return h
	}
	if got := timeSignature(mk(nil)); got != "" {
		t.Errorf("timeSignature(全零)=%q 期望空串", got)
	}
	if got := timeSignature(mk(map[int]int{23: 10})); got != "深夜聊友" {
		t.Errorf("timeSignature(深夜)=%q 期望 深夜聊友", got)
	}
	if got := timeSignature(mk(map[int]int{8: 10})); got != "早安伙伴" {
		t.Errorf("timeSignature(早安)=%q 期望 早安伙伴", got)
	}
	if got := timeSignature(mk(map[int]int{15: 10})); got != "日间型" {
		t.Errorf("timeSignature(下午)=%q 期望 日间型", got)
	}
}

func TestV54DetectTagConflicts(t *testing.T) {
	tags := map[int64][]string{
		1: {"密友", "点头之交"}, // 亲密 × 疏远 冲突
		2: {"密友", "同事"},   // 无冲突（分属不同组、不成对）
		3: {"家人", "同事"},   // 家人 × 职场 冲突
		4: {"老同学"},        // 单标签无冲突
	}
	names := map[int64]string{1: "张三", 2: "李四", 3: "王五", 4: "赵六"}
	got := detectTagConflicts(tags, names)
	if len(got) != 2 {
		t.Fatalf("期望 2 条冲突，实际 %d: %+v", len(got), got)
	}
	// 按 contactId 升序：1 然后 3。
	if got[0].ContactID != 1 || got[1].ContactID != 3 {
		t.Errorf("冲突应按 contactId 升序，实际 %d,%d", got[0].ContactID, got[1].ContactID)
	}
	if got[0].Name != "张三" || got[0].TagA == "" || got[0].TagB == "" || got[0].TagA == got[0].TagB {
		t.Errorf("冲突1 字段异常: %+v", got[0])
	}
	if got[1].TagA != "家人" || got[1].TagB != "同事" {
		t.Errorf("冲突2 期望 TagA=家人 TagB=同事，实际 %+v", got[1])
	}
	// 空输入 → 非 nil 空切片（前端可安全遍历）。
	if out := detectTagConflicts(map[int64][]string{}, map[int64]string{}); out == nil || len(out) != 0 {
		t.Errorf("空输入应返回非 nil 空切片，实际 %+v", out)
	}
}

func TestV54MirrorTextStats(t *testing.T) {
	// lexicalRichness：去重率。
	if v := lexicalRichness(nil); v != 0 {
		t.Errorf("lexicalRichness(nil)=%v 期望 0", v)
	}
	if v := lexicalRichness([]string{"aa"}); !almostEqual(v, 50) {
		t.Errorf("lexicalRichness(aa)=%v 期望 50", v)
	}
	if v := lexicalRichness([]string{"ab"}); !almostEqual(v, 100) {
		t.Errorf("lexicalRichness(ab)=%v 期望 100", v)
	}
	if v := lexicalRichness([]string{"a a"}); !almostEqual(v, 50) { // 空白不计入
		t.Errorf("lexicalRichness('a a')=%v 期望 50", v)
	}
	// avgLen：rune 均值。
	if v := avgLen([]string{"ab", "cd"}); !almostEqual(v, 2) {
		t.Errorf("avgLen(ab,cd)=%v 期望 2", v)
	}
	if v := avgLen([]string{"中文词"}); !almostEqual(v, 3) {
		t.Errorf("avgLen(中文词)=%v 期望 3（按 rune）", v)
	}
	// markerShare：命中 set 的消息占比。
	if v := markerShare([]string{"a?b", "c"}, "?？"); !almostEqual(v, 50) {
		t.Errorf("markerShare 期望 50，实际 %v", v)
	}
	if v := markerShare(nil, "?"); v != 0 {
		t.Errorf("markerShare(nil) 期望 0，实际 %v", v)
	}
	// emojiShare：非 BMP（rune>0xFFFF）占比。
	if v := emojiShare([]string{"hi😀", "no"}); !almostEqual(v, 50) {
		t.Errorf("emojiShare 期望 50，实际 %v", v)
	}
	if v := emojiShare([]string{"普通中文"}); v != 0 {
		t.Errorf("emojiShare(纯 BMP 中文) 期望 0，实际 %v", v)
	}
}

func TestV54MirrorDelta(t *testing.T) {
	cases := []struct {
		val, base float64
		want      string
	}{
		{0, 0, "持平"},
		{5, 0, "独有"},
		{15, 10, "明显高于平时"},
		{12, 10, "略高于平时"},
		{10, 10, "与平时持平"},
		{8, 10, "略低于平时"},
		{6, 10, "明显低于平时"},
	}
	for _, c := range cases {
		if got := mirrorDelta(c.val, c.base); got != c.want {
			t.Errorf("mirrorDelta(%v,%v)=%q 期望 %q", c.val, c.base, got, c.want)
		}
	}
}

func TestV54MirrorTraits(t *testing.T) {
	// 话痨 + 表情党。
	dims := []MirrorDimension{
		{Key: "avgLen", Value: 20, Baseline: 10},
		{Key: "emoji", Value: 40, Baseline: 10},
	}
	got := mirrorTraits(dims)
	if !hasTrait(got, "话痨型") || !hasTrait(got, "表情党") {
		t.Errorf("mirrorTraits 期望含 话痨型/表情党，实际 %v", got)
	}
	// 无显著差异 → 兜底标签。
	neutral := []MirrorDimension{
		{Key: "avgLen", Value: 10, Baseline: 10},
		{Key: "emoji", Value: 5, Baseline: 10},
	}
	if got := mirrorTraits(neutral); !reflect.DeepEqual(got, []string{"风格与平时一致"}) {
		t.Errorf("mirrorTraits(中性) 期望 [风格与平时一致]，实际 %v", got)
	}
	// 简洁型。
	if got := mirrorTraits([]MirrorDimension{{Key: "avgLen", Value: 5, Baseline: 10}}); !hasTrait(got, "简洁型") {
		t.Errorf("mirrorTraits(简洁) 期望含 简洁型，实际 %v", got)
	}
}

func hasTrait(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
