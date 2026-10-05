package main

// v5.3.0 #5 关系教练：纯函数 hourWeekdayHist/selectTopK/formatHourRange 众数与并列确定性；
//   computeContactTiming 小样本诚实降级；BuildCoach 无周计划缓存时回退 note。

import (
	"testing"
	"time"
)

func TestHourWeekdayHist(t *testing.T) {
	pairs := [][2]int{
		{9, 1}, {9, 1}, {20, 3}, {20, 3}, {20, 3}, {25, 9}, // 越界项应被忽略
	}
	hours, wdays := hourWeekdayHist(pairs)
	if len(hours) != 24 || len(wdays) != 7 {
		t.Fatalf("长度应为 24/7, got %d/%d", len(hours), len(wdays))
	}
	if hours[9] != 2 || hours[20] != 3 {
		t.Errorf("小时直方错: h9=%d h20=%d", hours[9], hours[20])
	}
	if wdays[1] != 2 || wdays[3] != 3 {
		t.Errorf("周几直方错: w1=%d w3=%d", wdays[1], wdays[3])
	}
	// 越界 (25,9) 不计入任何桶：小时有效计数和应等于样本内合法项 5。
	total := 0
	for _, v := range hours {
		total += v
	}
	if total != 5 {
		t.Errorf("越界样本不应计入, 有效计数和应为 5, got %d", total)
	}
}

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
	now := time.Date(2025, 6, 20, 12, 0, 0, 0, time.Local)
	id := regressionContact(t, db, "样本不足")
	// 只塞 2 条对方消息，远低于 coachMinSamples。
	for i := 0; i < 2; i++ {
		if _, err := SaveMessages(db, id, []Message{{Sender: "other", Content: "在", Timestamp: now.AddDate(0, 0, -i)}}); err != nil {
			t.Fatal(err)
		}
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
	now := time.Date(2025, 6, 20, 12, 0, 0, 0, time.Local)
	id := regressionContact(t, db, "夜猫子")
	// 对方多在 21 点发言，覆盖 coachMinSamples 以上。
	for i := 0; i < 10; i++ {
		ts := time.Date(2025, 6, 1+(i%9), 21, 5, 0, 0, time.Local)
		if _, err := SaveMessages(db, id, []Message{{Sender: "other", Content: "聊几句", Timestamp: ts}}); err != nil {
			t.Fatal(err)
		}
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
