package main

// v6.3 §5.2 Context/AI 接管验收：迁移到 callLLMCached 后，联系人内容生成任务
// （此处以关系叙事为代表）应经由「AI 响应缓存」这一单一原语——相同认知输入 + 相同
// prompt 的第二次调用必须命中缓存、不再触达模型 HTTP 端点。
//
// 这是「代码事实」级验收，而非 README 声称：若哪天有人把某模块的调用改回裸 llm.CallContext，
// 本测试会因两次都打到 server（calls==2）而失败。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// stubLLMServer 返回固定内容并累计被真实调用的次数。
func stubLLMServer(t *testing.T, content string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, content)
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func configuredClient(t *testing.T, url string) *LLMClient {
	t.Helper()
	cfg := &Config{}
	cfg.LLM.BaseURL = url
	cfg.LLM.ApiKey = "test-key"
	return NewLLMClient(cfg)
}

// TestNarrativeTakeoverHitsCache 验证 GenerateNarrative 走缓存原语：两次同输入只调一次模型。
// 叙事任务只发一次 LLM 调用，故 HTTP 计数唯一，适合作为接管锚点（画像另有变化说明辅助调用）。
func TestNarrativeTakeoverHitsCache(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "cache-target")
	// 向近期窗口（≤ narrativeMaxDays=365）内写入 ≥ summaryMinMsgs 条、内容互异的消息，
	// 时间戳取当下→落在 loadSummaryInputs 的窗口内。
	recent := time.Now().Add(-24 * time.Hour)
	for _, text := range []string{"最近项目进度如何", "周末有空聚聚", "那个方案我看了", "回头一起吃饭"} {
		if _, err := SaveMessages(db, id, []Message{{Sender: "other", Content: text, Timestamp: recent}}); err != nil {
			t.Fatal(err)
		}
	}

	server, calls := stubLLMServer(t, "这是一段依据往来事实生成的关系叙事。")
	client := configuredClient(t, server.URL)

	first, err := GenerateNarrative(context.Background(), db, client, id, 365)
	if err != nil {
		t.Fatal(err)
	}
	if first.Source != "llm" {
		t.Fatalf("首次应为 llm 来源，实得 source=%q note=%q", first.Source, first.Note)
	}
	if calls.Load() != 1 {
		t.Fatalf("首次应恰好调用模型 1 次，实得 %d", calls.Load())
	}

	second, err := GenerateNarrative(context.Background(), db, client, id, 365)
	if err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("§5.2 接管失败：相同输入第二次仍调用模型（calls=%d），说明未走 AI 响应缓存", got)
	}
	if second.Narrative != first.Narrative {
		t.Fatalf("缓存命中应返回相同叙事，first=%q second=%q", first.Narrative, second.Narrative)
	}
}
