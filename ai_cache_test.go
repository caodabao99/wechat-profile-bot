package main

import (
	"strings"
	"testing"
)

func aiCacheTestKey(mut func(*aiCacheKey)) aiCacheKey {
	k := aiCacheKey{ContactID: 1, Task: "profile", ContextVersion: "cv-aaa", Model: "m1", PromptVersion: "pv-111"}
	if mut != nil {
		mut(&k)
	}
	return k
}

// TestAICacheRoundTripAndInvalidate 校验 §4.2 语义键缓存：命中/覆盖 + 任一分量变化精确失效。
func TestAICacheRoundTripAndInvalidate(t *testing.T) {
	db := regressionDB(t)
	if _, ok := aiCacheGet(db, aiCacheTestKey(nil)); ok {
		t.Fatal("expected miss before put")
	}
	aiCachePut(db, aiCacheTestKey(nil), `{"summary":"甲"}`)
	if got, ok := aiCacheGet(db, aiCacheTestKey(nil)); !ok || got != `{"summary":"甲"}` {
		t.Fatalf("expected hit, got %q ok=%v", got, ok)
	}
	// 键任一分量变化 → 必须未命中（context_version/prompt_version/model/task/contact 各自独立失效）。
	for name, mut := range map[string]func(*aiCacheKey){
		"contact": func(k *aiCacheKey) { k.ContactID = 2 },
		"task":    func(k *aiCacheKey) { k.Task = "ask" },
		"context": func(k *aiCacheKey) { k.ContextVersion = "cv-bbb" },
		"model":   func(k *aiCacheKey) { k.Model = "m2" },
		"prompt":  func(k *aiCacheKey) { k.PromptVersion = "pv-222" },
	} {
		if _, ok := aiCacheGet(db, aiCacheTestKey(mut)); ok {
			t.Fatalf("cache must miss when %s changes", name)
		}
	}
	// 覆盖同键 → 新值。
	aiCachePut(db, aiCacheTestKey(nil), `{"summary":"乙"}`)
	if got, _ := aiCacheGet(db, aiCacheTestKey(nil)); got != `{"summary":"乙"}` {
		t.Fatalf("expected overwrite, got %q", got)
	}
}

// TestAICacheInvalidKeyGuard 校验缺分量键与空响应都不写不命中（避免脏写/误命中）。
func TestAICacheInvalidKeyGuard(t *testing.T) {
	db := regressionDB(t)
	invalid := aiCacheKey{ContactID: 0, Task: "", ContextVersion: "", Model: "", PromptVersion: ""}
	aiCachePut(db, invalid, "X")
	if _, ok := aiCacheGet(db, invalid); ok {
		t.Fatal("invalid key must never hit")
	}
	aiCachePut(db, aiCacheTestKey(nil), "") // 空响应不写
	if _, ok := aiCacheGet(db, aiCacheTestKey(nil)); ok {
		t.Fatal("empty response must not be cached")
	}
}

// TestPromptVersionStable 校验 §4.2 prompt_version：解析后模板正文的确定短哈希 + 未知键报错。
func TestPromptVersionStable(t *testing.T) {
	db := regressionDB(t)
	v1, err := PromptVersion(db, "profile_update")
	if err != nil || v1 == "" {
		t.Fatalf("PromptVersion err=%v val=%q", err, v1)
	}
	if v2, _ := PromptVersion(db, "profile_update"); v1 != v2 {
		t.Fatal("PromptVersion must be deterministic")
	}
	if _, err := PromptVersion(db, "不存在的模板"); err == nil {
		t.Fatal("unknown key must error")
	}
}

// TestAICacheGovernanceSync 断言新增缓存表三处同步：registry / 备份派生清单 / 删联系人级联清单。
func TestAICacheGovernanceSync(t *testing.T) {
	if GetTableMeta("ai_response_cache") == nil {
		t.Fatal("ai_response_cache 未登记 registry")
	}
	inDerived := false
	for _, tb := range derivedTables {
		if tb == "ai_response_cache" {
			inDerived = true
		}
	}
	if !inDerived {
		t.Fatal("ai_response_cache 未入 backup.derivedTables（恢复应清空自愈重建）")
	}
	inCleanup := false
	for _, tb := range contactCleanupTables {
		if tb == "ai_response_cache" {
			inCleanup = true
		}
	}
	if !inCleanup {
		t.Fatal("ai_response_cache 未入 cleanup.contactCleanupTables（删联系人应级联）")
	}
	found := false
	for _, s := range contactCleanupStmts() {
		if strings.Contains(s, "ai_response_cache") {
			found = true
		}
	}
	if !found {
		t.Fatal("contactCleanupStmts 缺 ai_response_cache 级联语句")
	}
}
