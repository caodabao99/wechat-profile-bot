package main

// v5.3.0 #7 关系健康度：纯函数 fuseHealth 各因子与夹取；ComputeHealth 空数据/护栏/分档确定性与幂等。

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestFuseHealthBaseNoSignals(t *testing.T) {
	h, sig := fuseHealth(80, "stable", 50, true, 0, 0.5)
	if h != 80 {
		t.Fatalf("stable/情绪中性/无沉默应等于基座 80, got %d", h)
	}
	if len(sig) != 1 || sig[0].Label != "亲密度" {
		t.Errorf("应只有亲密度一项贡献, got %+v", sig)
	}
}

func TestFuseHealthTrendDeltas(t *testing.T) {
	if h, _ := fuseHealth(80, "warming", 50, true, 0, 0.5); h != 88 {
		t.Errorf("warming 应 +8 → 88, got %d", h)
	}
	if h, _ := fuseHealth(80, "cooling", 50, true, 0, 0.5); h != 68 {
		t.Errorf("cooling 应 -12 → 68, got %d", h)
	}
	if h, _ := fuseHealth(80, "dormant", 50, true, 0, 0.5); h != 60 {
		t.Errorf("dormant 应 -20 → 60, got %d", h)
	}
}

func TestFuseHealthEmotion(t *testing.T) {
	// emoAvg=100 → (100-50)/50*10 = +10；emoAvg=0 → -10。
	if h, _ := fuseHealth(50, "stable", 100, true, 0, 0.5); h != 60 {
		t.Errorf("满情绪应 +10 → 60, got %d", h)
	}
	if h, _ := fuseHealth(50, "stable", 0, true, 0, 0.5); h != 40 {
		t.Errorf("零情绪应 -10 → 40, got %d", h)
	}
	// emoOK=false 时情绪因子完全不参与。
	if h, _ := fuseHealth(50, "stable", 100, false, 0, 0.5); h != 50 {
		t.Errorf("无情绪记录应忽略该因子 → 50, got %d", h)
	}
}

func TestFuseHealthSilenceCap(t *testing.T) {
	// 60 天 → 60/7*2 = 16（整除），扣 16。
	if h, _ := fuseHealth(60, "stable", 50, true, 60, 0.5); h != 44 {
		t.Errorf("沉默 60 天应 -16 → 44, got %d", h)
	}
	// 140 天 → 140/7*2=40 但封顶 20。
	if h, _ := fuseHealth(60, "stable", 50, true, 140, 0.5); h != 40 {
		t.Errorf("沉默封顶应 -20 → 40, got %d", h)
	}
}

func TestFuseHealthOneSidedPenalty(t *testing.T) {
	if h, _ := fuseHealth(60, "stable", 50, true, 0, 0.90); h != 55 {
		t.Errorf("我方占比>0.85 应 -5 → 55, got %d", h)
	}
	if h, _ := fuseHealth(60, "stable", 50, true, 0, 0.85); h != 60 {
		t.Errorf("我方占比=0.85（非>）不应惩罚, got %d", h)
	}
}

func TestFuseHealthClamp(t *testing.T) {
	if h, _ := fuseHealth(0, "dormant", 0, true, 365, 0.99); h != 0 {
		t.Errorf("下界应夹到 0, got %d", h)
	}
	if h, _ := fuseHealth(100, "warming", 100, true, 0, 0.5); h != 100 {
		t.Errorf("上界应夹到 100, got %d", h)
	}
}

func TestBandOfHealth(t *testing.T) {
	cases := map[int]string{90: "优秀", 80: "优秀", 79: "良好", 60: "良好", 59: "一般", 40: "一般", 39: "需关注", 20: "需关注", 19: "危险", 0: "危险"}
	for h, want := range cases {
		if got := bandOfHealth(h); got != want {
			t.Errorf("bandOfHealth(%d)=%s, want %s", h, got, want)
		}
	}
}

func TestComputeHealthEmpty(t *testing.T) {
	db := regressionAssistantDB(t)
	d, err := ComputeHealth(db, time.Now(), healthWindowDays)
	if err != nil {
		t.Fatal(err)
	}
	if d.Summary.Total != 0 || len(d.Items) != 0 {
		t.Fatalf("空库应为 0 条, got total=%d items=%d", d.Summary.Total, len(d.Items))
	}
	// 直方固定 5 档、全 0。
	if len(d.Summary.Bands) != len(healthBandOrder) {
		t.Fatalf("bands 应为固定 %d 档, got %d", len(healthBandOrder), len(d.Summary.Bands))
	}
	for _, b := range d.Summary.Bands {
		if b.Count != 0 {
			t.Errorf("空库各档应为 0: %+v", b)
		}
	}
}

func TestComputeHealthDeterministic(t *testing.T) {
	db := regressionAssistantDB(t)
	now := time.Date(2025, 6, 20, 12, 0, 0, 0, time.Local)
	// 若干联系人 + 各自不同活跃度/互动分布。
	for i := 0; i < 5; i++ {
		id := regressionContact(t, db, fmt.Sprintf("健康样本%d", i))
		for d := 0; d <= i*3; d++ {
			day := now.AddDate(0, 0, -d)
			_, err := SaveMessages(db, id, []Message{
				{Sender: "me", Content: fmt.Sprintf("在%d", d), Timestamp: day},
				{Sender: "other", Content: fmt.Sprintf("好的%d-%d", i, d), Timestamp: day},
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := RebuildDailyMetrics(db, 0); err != nil {
		t.Fatal(err)
	}

	first, err := ComputeHealth(db, now, healthWindowDays)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ComputeHealth(db, now, healthWindowDays)
	if err != nil {
		t.Fatal(err)
	}

	if first.Summary.Total != 5 {
		t.Fatalf("应覆盖 5 个联系人, got %d", first.Summary.Total)
	}
	// 幂等：两次结果（去掉生成时间戳）逐字节一致。
	strip := func(d *HealthDashboard) string {
		cp := *d
		cp.GeneratedAt = ""
		b, _ := json.Marshal(cp)
		return string(b)
	}
	if strip(first) != strip(second) {
		t.Errorf("ComputeHealth 应确定性可复现")
	}
	// 不变量：health ∈ [0,100]，bands 求和 == total，items 按 health 升序。
	sum := map[string]int{}
	prev := -1
	for _, it := range first.Items {
		if it.Health < 0 || it.Health > 100 {
			t.Errorf("health 越界: %+v", it)
		}
		if prev > it.Health {
			t.Errorf("items 未按 health 升序: %d 后又 %d", prev, it.Health)
		}
		prev = it.Health
		sum[it.Band]++
	}
	for _, b := range first.Summary.Bands {
		if b.Count != sum[b.Band] {
			t.Errorf("band %s 直方 %d ≠ 实际 %d", b.Band, b.Count, sum[b.Band])
		}
	}
}
