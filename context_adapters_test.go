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
