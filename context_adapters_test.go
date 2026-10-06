package main

import (
	"testing"
	"time"
)

// TestContextAdaptersProduceVersionedContext 校验 §4.1 四适配器内部一律走 BuildContactContext：
// 产出正确 Task 且带非空 context_version。
func TestContextAdaptersProduceVersionedContext(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "适配器")
	regressionMessages(t, db, id, "我准备换公司", "我是律师")
	now := time.Now()

	cases := []struct {
		name  string
		build func() (*ContactContext, error)
		want  ContextTask
	}{
		{"profile", func() (*ContactContext, error) { return buildProfileContext(db, id, now) }, TaskProfile},
		{"ask", func() (*ContactContext, error) { return buildAskContext(db, id, "换工作", now) }, TaskAsk},
		{"coach", func() (*ContactContext, error) { return buildCoachContext(db, id, now) }, TaskCoach},
		{"simulation", func() (*ContactContext, error) { return buildSimulationContext(db, id, "草稿", now) }, TaskSimulation},
	}
	for _, c := range cases {
		cc, err := c.build()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if cc.Task != c.want {
			t.Fatalf("%s: task=%q want %q", c.name, cc.Task, c.want)
		}
		if cc.ContextVersion == "" {
			t.Fatalf("%s: adapter must yield non-empty context_version", c.name)
		}
	}
}

// TestBuildTaskContextDispatch 校验分派：四大 legacy 任务走适配器，其余任务回退通用构造。
func TestBuildTaskContextDispatch(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "分派")
	regressionMessages(t, db, id, "你好")
	now := time.Now()

	// legacy 任务：分派后 Task 正确。
	for _, task := range []ContextTask{TaskProfile, TaskAsk, TaskCoach, TaskSimulation} {
		cc, err := buildTaskContext(db, id, task, "关键词", now)
		if err != nil || cc.Task != task {
			t.Fatalf("dispatch task=%q cc.Task=%q err=%v", task, cc.Task, err)
		}
	}
	// 非 legacy 任务（如 narrative）走 default 分支，仍产出正确 Task。
	cc, err := buildTaskContext(db, id, TaskNarrative, "", now)
	if err != nil || cc.Task != TaskNarrative {
		t.Fatalf("default branch: task=%q err=%v", cc.Task, err)
	}
}

// TestEveryRegisteredTaskDispatchesVersionedContext 锁定 v7.0 Phase 1 唯一入口化不变式：
// 注册表内每个 AI 任务都能经 buildTaskContext 走引擎、产出正确 Task 与非空 context_version。
// 新增任务若忘登记或未接入适配器，本测试立即失败，杜绝「注册了 spec 却仍传 ""」的回潮。
func TestEveryRegisteredTaskDispatchesVersionedContext(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "全量分派")
	regressionMessages(t, db, id, "我准备换公司", "你好")
	now := time.Now()

	tasks := RegisteredTasks()
	if len(tasks) < 20 {
		t.Fatalf("registry unexpectedly small: %d tasks", len(tasks))
	}
	for _, task := range tasks {
		cc, err := buildTaskContext(db, id, task, "换工作", now)
		if err != nil {
			t.Fatalf("task=%q: %v", task, err)
		}
		if cc.Task != task {
			t.Fatalf("task=%q dispatch mismatch: cc.Task=%q", task, cc.Task)
		}
		if cc.ContextVersion == "" {
			t.Fatalf("task=%q must yield non-empty context_version", task)
		}
	}
}

// TestProfileBlockPopulatedAndInvalidates 校验画像块经引擎暴露且参与版本指纹：
// 仅改数组类派生字段（summary 不变）也应使 context_version 变化。
func TestProfileBlockPopulatedAndInvalidates(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "画像块")
	regressionMessages(t, db, id, "你好")

	rich := `{"summary":"核心概括","personality":["谨慎"],"interests":["登山"],"emotional_patterns":{"stressors":["被催促"]}}`
	if err := SaveProfile(db, id, rich, "核心概括", "generated"); err != nil {
		t.Fatal(err)
	}
	cc, err := buildBlessingContext(db, id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(cc.Profile.Personality) == 0 || cc.Profile.Personality[0] != "谨慎" {
		t.Fatalf("profile block not populated: %+v", cc.Profile)
	}
	if len(cc.Profile.Stressors) == 0 {
		t.Fatalf("profile stressors not populated: %+v", cc.Profile)
	}
	v1 := cc.ContextVersion

	rich2 := `{"summary":"核心概括","personality":["开朗"],"interests":["登山"],"emotional_patterns":{"stressors":["被催促"]}}`
	if err := SaveProfile(db, id, rich2, "核心概括", "generated"); err != nil {
		t.Fatal(err)
	}
	cc2, err := buildBlessingContext(db, id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if cc2.ContextVersion == v1 {
		t.Fatal("context_version must change when profile array changes even if summary is identical")
	}
}
