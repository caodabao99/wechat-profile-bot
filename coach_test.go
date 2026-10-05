package main

// v5.3.0 #5 关系教练：纯函数 selectTopK/formatHourRange 众数与并列确定性；
//   computeContactTiming 小样本诚实降级；BuildCoach 无周计划缓存时回退 note。

import (
	"testing"
	"time"
)

func TestSelectTopKDeterministic(t *testing.T) {
	counts := []int{0, 5, 3, 5, 0, 9} // 索引 5 最大(9)，其次 1、3 并列(5)，再 2(3)
	top := selectTopK(counts, 3)
	if len(top) != 3 || top[0] != 5 || top[1] != 1 || top[2] != 3 {
		t.Errorf("并列应取索引小者: got %v", top)
	}
	// k 大于非零桶数：只返回非零。
	top2 := selectTopK([]int{4, 0, 0}, 10)
	if len(top2) != 1 || top2[0] != 0 {
		t.Errorf("只应返回非零桶, got %v", top2)
	}
	// 全零 → 空。
	if got := selectTopK([]int{0, 0, 0}, 2); len(got) != 0 {
		t.Errorf("全零应空, got %v", got)
	}
}

func TestFormatHourRange(t *testing.T) {
	if got := formatHourRange([]int{20, 19}); got != "19-20 点" {
		t.Errorf("相邻两小时应成区间, got %q", got)
	}
	if got := formatHourRange([]int{8, 21}); got != "8 点、21 点" {
		t.Errorf("非相邻应逐列举, got %q", got)
	}
	if got := formatHourRange(nil); got != "" {
		t.Errorf("空应空串, got %q", got)
	}
}

func TestComputeContactTimingSmallSample(t *testing.T) {
	db := regressionAssistantDB(t)
	now := time.Now()
	id := regressionContact(t, db, "样本不足")
	// 只塞 2 条对方消息，远低于 coachMinSamples。
	for i := 0; i < 2; i++ {
		if _, err := SaveMessages(db, id, []Message{{Sender: "other", Content: "在", Timestamp: now.AddDate(0, 0, -i)}}); err != nil {
			t.Fatal(err)
		}
	}
	// 统一 Metrics Layer：消息入库后需重建日聚合指标。
	if _, err := RebuildDailyMetrics(db, id); err != nil {
		t.Fatal(err)
	}
	tt := computeContactTiming(db, id, now)
	if tt.Sample >= coachMinSamples {
		t.Fatalf("样本应不足, got %d", tt.Sample)
	}
	if tt.Note == "" {
		t.Error("样本不足应给出诚实 note")
	}
	if len(tt.BestHours) != 0 || len(tt.BestWeekdays) != 0 {
		t.Errorf("样本不足不应硬凑时段: %+v", tt)
	}
}

func TestComputeContactTimingFindsPeakHour(t *testing.T) {
	db := regressionAssistantDB(t)
	now := time.Now()
	id := regressionContact(t, db, "夜猫子")
	// 统一 Metrics Layer：computeContactTiming 读 other_hour_hist（「对方」发言小时分布——
	// 何时触达更易获得回应取决于对方何时活跃）。插入 other 消息以填充直方。
	// 时间戳须在近 180 天窗口内，否则 GetAggregatedMetrics 不会统计到。
	// 每条消息内容须不同，避免 msg_hash 去重。
	for i := 0; i < 10; i++ {
		ts := now.AddDate(0, 0, -i)
		ts = time.Date(ts.Year(), ts.Month(), ts.Day(), 21, 5, 0, 0, ts.Location())
		if _, err := SaveMessages(db, id, []Message{{Sender: "other", Content: "聊几句" + string(rune('a'+i)), Timestamp: ts}}); err != nil {
			t.Fatal(err)
		}
	}
	// 统一 Metrics Layer：消息入库后需重建日聚合指标，computeContactTiming 从 metrics 读取。
	if _, err := RebuildDailyMetrics(db, id); err != nil {
		t.Fatal(err)
	}
	tt := computeContactTiming(db, id, now)
	if tt.Sample < coachMinSamples {
		t.Fatalf("样本应充足, got %d", tt.Sample)
	}
	if len(tt.BestHours) == 0 || tt.BestHours[0] != 21 {
		t.Errorf("峰值小时应为 21, got %v", tt.BestHours)
	}
}

func TestBuildCoachNoteWithoutPlan(t *testing.T) {
	db := regressionAssistantDB(t)
	// 无周计划缓存 → 应回退提示，且 items 空、不报错。
	resp, err := BuildCoach(db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 0 {
		t.Errorf("无周计划应空 items, got %d", len(resp.Items))
	}
	if resp.Note == "" {
		t.Error("应给出周计划未生成的 note")
	}
}
