package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// newCountingLLM 返回一个统计调用次数的假 LLM 服务 + 客户端。
func newCountingLLM(t *testing.T, replies string) (*LLMClient, *int32, func()) {
	t.Helper()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, replies)
	}))
	cfg := &Config{}
	cfg.LLM.BaseURL = server.URL
	cfg.LLM.ApiKey = "test-key"
	cfg.LLM.Model = "m-test"
	return NewLLMClient(cfg), &calls, server.Close
}

// TestCallLLMCachedHitThenMiss 校验接管原语：相同 (contact,task,prompt,model) 命中缓存不再调 LLM；
// 不同 prompt 未命中再调一次（问题/草稿等即时输入变化不会误命中）。
func TestCallLLMCachedHitThenMiss(t *testing.T) {
	llm, calls, closeFn := newCountingLLM(t, "回答X")
	defer closeFn()
	db := regressionDB(t)
	id := regressionContact(t, db, "缓存接管")
	ctx := context.Background()

	r1, err := callLLMCached(ctx, db, llm, id, TaskAsk, "", "PROMPT-A")
	if err != nil || r1 != "回答X" {
		t.Fatalf("first: r=%q err=%v", r1, err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("expected 1 LLM call, got %d", got)
	}
	// 相同 prompt → 命中，不再调 LLM。
	r2, err := callLLMCached(ctx, db, llm, id, TaskAsk, "", "PROMPT-A")
	if err != nil || r2 != "回答X" {
		t.Fatalf("second(cached): r=%q err=%v", r2, err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("cache hit must not call LLM, calls=%d", got)
	}
	// 不同 prompt → 未命中，再调一次。
	if _, err := callLLMCached(ctx, db, llm, id, TaskAsk, "", "PROMPT-B"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("different prompt must miss cache, calls=%d", got)
	}
	// 不同 contact → 键隔离，未命中。
	id2 := regressionContact(t, db, "另一个")
	if _, err := callLLMCached(ctx, db, llm, id2, TaskAsk, "", "PROMPT-A"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Fatalf("different contact must isolate cache, calls=%d", got)
	}
}

// TestCallLLMCachedNotConfigured 校验未配置 LLM 时原样返回错误、不写缓存。
func TestCallLLMCachedNotConfigured(t *testing.T) {
	db := regressionDB(t)
	if _, err := callLLMCached(context.Background(), db, nil, 1, TaskAsk, "", "x"); err == nil {
		t.Fatal("nil llm must error")
	}
	unconfigured := NewLLMClient(&Config{}) // 无 apiKey/baseURL
	if _, err := callLLMCached(context.Background(), db, unconfigured, 1, TaskAsk, "", "x"); err == nil {
		t.Fatal("unconfigured llm must error")
	}
}
