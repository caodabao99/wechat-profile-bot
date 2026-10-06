package main

import (
	"database/sql"
	"testing"
	"time"
)

// mustBuildVersion 构造 profile 任务上下文并返回其 context_version。
func mustBuildVersion(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	cc, err := BuildContactContext(db, id, TaskProfile, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return cc.ContextVersion
}

// TestContextVersionDeterministicAndInvalidates 校验蓝图 §4.2 context_version：
// 同一 DB 状态 → 同一指纹（确定性铁律）；新消息 / 画像变更 → 指纹变化（精确失效）。
func TestContextVersionDeterministicAndInvalidates(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "版本指纹")
	regressionMessages(t, db, id, "我是律师", "我准备换公司")

	v1 := mustBuildVersion(t, db, id)
	v2 := mustBuildVersion(t, db, id)
	if v1 == "" {
		t.Fatal("empty context_version")
	}
	if v1 != v2 {
		t.Fatalf("non-deterministic: %s != %s", v1, v2)
	}

	// 新消息 → 版本变化（recent messages 是版本相关分块之一）。
	regressionMessages(t, db, id, "刚加了一条新消息")
	v3 := mustBuildVersion(t, db, id)
	if v3 == v1 {
		t.Fatal("version must change after new message")
	}

	// 画像变更 → 版本变化（identity.summary 是版本相关分块之一）。
	if err := SaveProfile(db, id, `{"summary":"更新后的画像"}`, "更新后的画像", "generated"); err != nil {
		t.Fatal(err)
	}
	v4 := mustBuildVersion(t, db, id)
	if v4 == v3 {
		t.Fatal("version must change after profile update")
	}
}
