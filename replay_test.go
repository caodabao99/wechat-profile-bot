package main

// Memory Replay「重新认识 TA」（OS 2.0 Phase 7，规格第十章）专项测试。
// 覆盖：确定性拼装（同输入同结果）、每段结论必带来源证据（10.1）、关键段落存在、
// 缺联系人 ErrNoRows、渲染文本结构、无数据优雅降级（不编造段落）。

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

var allowedEvidenceSources = map[string]bool{
	"timeline": true, "state": true, "fact": true, "topic": true, "metric": true, "message": true,
}

func TestBuildRelationshipReplaySectionsAndEvidence(t *testing.T) {
	db := regressionDB(t)
	if err := ensureFollowupTables(db); err != nil {
		t.Fatal(err)
	}
	cid := regressionContact(t, db, "回放对象")
	if err := SaveProfile(db, cid, `{"basic_info":{"occupation":"工程师"},"interests":["跑步"],"summary":"s"}`, "s", "i"); err != nil {
		t.Fatal(err)
	}
	regressionMessages(t, db, cid, "一起跑步吗", "今天跑五公里")
	if _, err := CreateProject(db, CreateProjectInput{ContactID: cid, Title: "合作", NextAction: "发方案"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := AddFollowup(db, cid, "question", "等他回复", "", ""); err != nil {
		t.Fatal(err)
	}
	// 铺状态（首次评估产一条历史变迁 → stages 段）。
	if _, _, err := RefreshRelationshipStates(db, time.Now(), 0); err != nil {
		t.Fatal(err)
	}

	rep, err := BuildRelationshipReplay(db, cid, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.ContactID != cid || rep.Name != "回放对象" {
		t.Errorf("回放身份不符: %+v", rep)
	}

	keys := map[string]bool{}
	for _, s := range rep.Sections {
		keys[s.Key] = true
		// 每段标题非空、条目非空。
		if s.Title == "" || len(s.Items) == 0 {
			t.Errorf("段落应标题非空且有条目: %+v", s)
		}
		// 10.1：每条结论至少一条合法来源证据。
		for _, it := range s.Items {
			if len(it.Evidence) == 0 {
				t.Errorf("段落 %s 存在无证据结论（不得编造）: %q", s.Key, it.Text)
				continue
			}
			for _, e := range it.Evidence {
				if !allowedEvidenceSources[e.Source] {
					t.Errorf("非法证据来源层 %q（段 %s）", e.Source, s.Key)
				}
			}
		}
	}

	// 关键段落应存在。
	for _, want := range []string{replayFirstMeeting, replayStages, replayCurrentState, replayGoals, replayOpenItems} {
		if !keys[want] {
			t.Errorf("应含段落 %s, got keys=%v", want, keys)
		}
	}

	// 确定性：再建一次段落数应一致（不依赖 LLM、可复现）。
	rep2, err := BuildRelationshipReplay(db, cid, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep2.Sections) != len(rep.Sections) {
		t.Errorf("回放应确定可复现: %d vs %d", len(rep.Sections), len(rep2.Sections))
	}
}

func TestBuildRelationshipReplayMissingContact(t *testing.T) {
	db := regressionDB(t)
	if _, err := BuildRelationshipReplay(db, 999999, time.Now()); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("不存在联系人应 ErrNoRows, got %v", err)
	}
}

func TestRenderReplayText(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "渲染回放")
	if err := SaveProfile(db, cid, `{"summary":"s"}`, "s", "i"); err != nil {
		t.Fatal(err)
	}
	regressionMessages(t, db, cid, "在吗")
	if _, _, err := RefreshRelationshipStates(db, time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	rep, err := BuildRelationshipReplay(db, cid, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	text := RenderReplayText(rep)
	if !strings.Contains(text, "# 重新认识 TA") {
		t.Errorf("渲染应含标题, got:\n%s", text)
	}
	if !strings.Contains(text, "〔来源：") {
		t.Error("渲染应展示每条结论的来源层（可解释）")
	}

	// nil 安全。
	if RenderReplayText(nil) != "" {
		t.Error("nil 回放应返回空串")
	}
}

func TestRenderReplayTextEmptyGraceful(t *testing.T) {
	// 无数据回放（空段落集）应给出可读的「数据不足」而非编造。
	empty := &RelationshipReplay{ContactID: 1, Name: "空", Sections: []ReplaySection{}}
	if out := RenderReplayText(empty); !strings.Contains(out, "尚无足够") {
		t.Errorf("空回放应降级提示, got:\n%s", out)
	}
}
