package main

// v5.5.0 #6/#7 单元测试：
//   - 叙事：buildDeterministicNarrative/cleanNarrativeText/topNarrativeTopics/joinTopicsCN/clampNarrativeDays 纯函数；
//   - 目标：goalProgressPct/normalizeGoalMetric/goalDateWindow 纯函数 + ensureGoals 幂等、CreateGoal 校验、
//     CompleteGoal 加 XP 幂等、周达标自动完成（真库、RebuildDailyMetrics 造数据）。

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestV55ClampNarrativeDays(t *testing.T) {
	if got := clampNarrativeDays(0); got != narrativeDefaultDays {
		t.Errorf("0 → %d, want %d", got, narrativeDefaultDays)
	}
	if got := clampNarrativeDays(-5); got != narrativeDefaultDays {
		t.Errorf("-5 → %d, want %d", got, narrativeDefaultDays)
	}
	if got := clampNarrativeDays(narrativeMaxDays + 10); got != narrativeMaxDays {
		t.Errorf("超限 → %d, want %d", got, narrativeMaxDays)
	}
	if got := clampNarrativeDays(120); got != 120 {
		t.Errorf("120 → %d", got)
	}
}

func TestV55BuildDeterministicNarrative(t *testing.T) {
	// 相识多年 + 有往来 + 有主题
	s := buildDeterministicNarrative("小王", 3, 800, "2020-01-02", []string{"旅行", "工作"})
	if !strings.Contains(s, "小王") || !strings.Contains(s, "3 年") || !strings.Contains(s, "800 条") ||
		!strings.Contains(s, "旅行") || !strings.Contains(s, "工作") {
		t.Errorf("多年叙事缺要素: %q", s)
	}
	// 无相识年数但有起点
	s2 := buildDeterministicNarrative("小李", 0, 20, "2025-06-01", nil)
	if !strings.Contains(s2, "2025-06-01") {
		t.Errorf("应含首次互动日期: %q", s2)
	}
	// 完全无记录：如实说开头，不硬编
	s3 := buildDeterministicNarrative("张三", 0, 0, "", nil)
	if s3 == "" || !strings.Contains(s3, "张三") {
		t.Errorf("空记录也应有真诚叙事且含名字: %q", s3)
	}
	// 确定性：同输入两次调用一致
	if buildDeterministicNarrative("A", 1, 5, "", []string{"x"}) != buildDeterministicNarrative("A", 1, 5, "", []string{"x"}) {
		t.Error("确定性叙事应稳定")
	}
}

func TestV55CleanNarrativeText(t *testing.T) {
	body := "这是一段足够长的关系叙事正文内容，用来通过最小长度校验。"
	// 直接正文
	if got := cleanNarrativeText(body); got != body {
		t.Errorf("纯正文应原样: %q", got)
	}
	// code fence 包裹
	fenced := "```json\n" + body + "\n```"
	if got := cleanNarrativeText(fenced); !strings.Contains(got, body) || strings.Contains(got, "```") {
		t.Errorf("应剥 code fence: %q", got)
	}
	// 中文引号包裹
	if got := cleanNarrativeText("“" + body + "”"); got != body {
		t.Errorf("应剥中文引号: %q", got)
	}
	// 太短 → 空
	if got := cleanNarrativeText("你好"); got != "" {
		t.Errorf("过短应视为无效: %q", got)
	}
	// 超长截断到上限
	long := strings.Repeat("字", narrativeMaxRunes+100)
	if r := []rune(cleanNarrativeText(long)); len(r) > narrativeMaxRunes {
		t.Errorf("应截断到上限, got %d", len(r))
	}
}

func TestV55TopNarrativeTopics(t *testing.T) {
	in := []TopicEntry{
		{Name: "旅行", Weight: 30, Status: "emergent"},
		{Name: "工作", Weight: 80, Status: "persistent"},
		{Name: "游戏", Weight: 50, Status: "fading"},
		{Name: "健康", Weight: 80, Status: "persistent"},
		{Name: "", Weight: 99, Status: "emergent"},
		{Name: "工作", Weight: 10, Status: "emergent"}, // 重复名
	}
	got := topNarrativeTopics(in, 4)
	// fading「游戏」与空名应被剔除；同权重按名称升序（健康<工作）；重复「工作」去重后只留首个（高权重那次）
	want := []string{"健康", "工作", "旅行"}
	if len(got) != len(want) {
		t.Fatalf("topNarrativeTopics = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项 = %q, want %q (%v)", i, got[i], want[i], got)
		}
	}
	if out := topNarrativeTopics(nil, 3); out == nil || len(out) != 0 {
		t.Errorf("空输入应返回非 nil 空切片: %#v", out)
	}
}

func TestV55GoalProgressPct(t *testing.T) {
	cases := []struct{ count, target, want int }{
		{0, 5, 0}, {1, 5, 20}, {5, 5, 100}, {9, 5, 100}, {3, 0, 100}, {0, 0, 0}, {-1, 5, 0},
	}
	for _, c := range cases {
		if got := goalProgressPct(c.count, c.target); got != c.want {
			t.Errorf("goalProgressPct(%d,%d)=%d, want %d", c.count, c.target, got, c.want)
		}
	}
}

func TestV55NormalizeGoalMetric(t *testing.T) {
	if normalizeGoalMetric("bogus") != goalMetricWeeklyMe || normalizeGoalMetric("") != goalMetricWeeklyMe {
		t.Error("未知/空 metric 应回落周口径")
	}
	if normalizeGoalMetric(goalMetricPeriodMe) != goalMetricPeriodMe {
		t.Error("period 口径应保留")
	}
}

func TestV55GoalDateWindow(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local) // 周一附近
	ws := weekStartOf(now)
	today := now.Format("2006-01-02")
	// 周口径
	from, to := goalDateWindow(goalMetricWeeklyMe, "2000-01-01", "2000-01-02", now)
	if from != ws || to != today {
		t.Errorf("周口径应忽略 period: got [%s,%s] want [%s,%s]", from, to, ws, today)
	}
	// 周期口径，正常起止
	from, to = goalDateWindow(goalMetricPeriodMe, "2026-01-01", "2026-03-01", now)
	if from != "2026-01-01" || to != "2026-03-01" {
		t.Errorf("周期口径应取起止: got [%s,%s]", from, to)
	}
	// 周期口径，起止反了 → 收敛为有序
	from, to = goalDateWindow(goalMetricPeriodMe, "2026-05-01", "2026-02-01", now)
	if from > to {
		t.Errorf("反区间应被纠正: got [%s,%s]", from, to)
	}
}

// —— 真库：ensureGoals 幂等 / CreateGoal 校验 / CompleteGoal 加 XP 幂等 / 周达标自动完成 ——

func TestV55EnsureGoalsIdempotent(t *testing.T) {
	db := regressionAssistantDB(t)
	if err := ensureGoals(db); err != nil {
		t.Fatal(err)
	}
	if err := ensureGoals(db); err != nil { // 再建不报错
		t.Fatal(err)
	}
	dbMu.Lock()
	ok := tableExistsLocked(db, "relationship_goals")
	dbMu.Unlock()
	if !ok {
		t.Error("relationship_goals 表应存在")
	}
}

func TestV55CreateGoalValidation(t *testing.T) {
	db := regressionAssistantDB(t)
	if err := ensureTimelineTables(db); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	id := regressionContact(t, db, "目标校验人")
	if _, err := CreateGoal(db, CreateGoalInput{ContactID: id, Title: "   ", TargetCount: 3}, now); !errors.Is(err, errGoalBadInput) {
		t.Errorf("空白标题应报参数不合法, got %v", err)
	}
	if _, err := CreateGoal(db, CreateGoalInput{ContactID: 999999, Title: "有效标题", TargetCount: 3}, now); err == nil {
		t.Error("不存在联系人应报错")
	}
	if _, err := CreateGoal(db, CreateGoalInput{ContactID: id, Title: "周期", Metric: goalMetricPeriodMe, TargetCount: 3}, now); !errors.Is(err, errGoalBadInput) {
		t.Errorf("周期口径缺起止应报参数不合法, got %v", err)
	}
	// 合法
	if _, err := CreateGoal(db, CreateGoalInput{ContactID: id, Title: "本周多聊几句", TargetCount: 5}, now); err != nil {
		t.Errorf("合法目标创建失败: %v", err)
	}
}

func TestV55CompleteGoalIdempotentXP(t *testing.T) {
	db := regressionAssistantDB(t)
	if err := ensureTimelineTables(db); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	id := regressionContact(t, db, "目标完成人")
	if _, err := RebuildDailyMetrics(db, 0); err != nil {
		t.Fatal(err)
	}
	gid, err := CreateGoal(db, CreateGoalInput{ContactID: id, Title: "本周发 50 条", Metric: goalMetricWeeklyMe, TargetCount: 50}, now)
	if err != nil {
		t.Fatal(err)
	}
	before, err := readGoals(db, now)
	if err != nil {
		t.Fatal(err)
	}
	xpBefore := before.XP
	// 本周发言不足，自动检测不应达成
	if before.Active != 1 {
		t.Fatalf("新目标应 active, got active=%d done=%d", before.Active, before.Done)
	}
	done, err := CompleteGoal(db, gid, now)
	if err != nil || !done {
		t.Fatalf("手动完成应成功, done=%v err=%v", done, err)
	}
	after, _ := readGoals(db, now)
	if after.XP != xpBefore+challengeXPPerDone {
		t.Errorf("首次完成应 +=%d XP, got %d→%d", challengeXPPerDone, xpBefore, after.XP)
	}
	// 幂等：再次完成应 false，XP 不再变
	done2, err := CompleteGoal(db, gid, now)
	if err != nil || done2 {
		t.Errorf("重复完成应幂等 false, done=%v err=%v", done2, err)
	}
	after2, _ := readGoals(db, now)
	if after2.XP != after.XP {
		t.Errorf("重复完成不应再加 XP, %d→%d", after.XP, after2.XP)
	}
	if after2.Done != 1 {
		t.Errorf("应有一条 done 目标, got %d", after2.Done)
	}
}

func TestV55GoalAutoCompleteWeekly(t *testing.T) {
	db := regressionAssistantDB(t)
	if err := ensureTimelineTables(db); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	id := regressionContact(t, db, "高频目标人")
	for d := 0; d < 3; d++ {
		day := now.AddDate(0, 0, -d)
		if _, err := SaveMessages(db, id, []Message{{Sender: "me", Content: "在吗", Timestamp: day}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RebuildDailyMetrics(db, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateGoal(db, CreateGoalInput{ContactID: id, Title: "本周发 1 条", Metric: goalMetricWeeklyMe, TargetCount: 1}, now); err != nil {
		t.Fatal(err)
	}
	resp, err := BuildGoals(db, now)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Done != 1 {
		t.Errorf("本周已达标应自动达成 1 条, got done=%d active=%d", resp.Done, resp.Active)
	}
	for _, g := range resp.Goals {
		if g.Status == goalStatusDone && g.Current < g.TargetCount {
			t.Errorf("done 目标进度异常: %+v", g)
		}
	}
}
