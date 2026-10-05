package main

// v5.3.0 #10 主题演化：parseTopicJSON 分类与坏 JSON 判定；normalizeTopicEntries 规范化；
//   contact_topic_history 按周幂等 + 升序读；未配模型 ComputeTopicEvolution 明确降级不报错编造。

import (
	"context"
	"testing"
	"time"
)

func TestParseTopicJSONValid(t *testing.T) {
	raw := `{"topics":[{"name":"旅行","weight":80,"status":"emergent"},{"name":"工作","weight":30,"status":"fading"}]}`
	topics, ok := parseTopicJSON(raw)
	if !ok {
		t.Fatalf("合法 JSON 应 ok=true")
	}
	if len(topics) != 2 || topics[0].Name != "旅行" || topics[0].Status != "emergent" {
		t.Fatalf("解析结果错: %+v", topics)
	}
	// weight 降序：旅行(80) 在工作(30) 前。
	if !(topics[0].Weight >= topics[1].Weight) {
		t.Errorf("未按 weight 降序: %+v", topics)
	}
}

func TestParseTopicJSONValidEmpty(t *testing.T) {
	topics, ok := parseTopicJSON(`{"topics":[]}`)
	if !ok {
		t.Fatalf("合法空数组应 ok=true（模型如实返回无主题）")
	}
	if len(topics) != 0 {
		t.Errorf("应为空, got %+v", topics)
	}
}

func TestParseTopicJSONBad(t *testing.T) {
	if _, ok := parseTopicJSON("这不是 JSON"); ok {
		t.Error("坏 JSON 应 ok=false（触发调用方回退上周快照）")
	}
}

func TestNormalizeTopicEntries(t *testing.T) {
	in := []TopicEntry{
		{Name: "  旅行  ", Weight: 150, Status: "weird"}, // 名称去空白、weight 夹 100、status 兜底
		{Name: "旅行", Weight: 10, Status: "emergent"},   // 去重
		{Name: "", Weight: 5, Status: "fading"},        // 空名丢弃
		{Name: "家庭", Weight: 999, Status: "persistent"},
	}
	out := normalizeTopicEntries(in)
	if len(out) != 2 {
		t.Fatalf("去空名/去重后应剩 2 条, got %d (%+v)", len(out), out)
	}
	// weight 夹取 + 降序：家庭(100) > 旅行(100)? 两者都夹到 100 → 同分按名称升序。
	for _, e := range out {
		if e.Weight > 100 || e.Weight < 0 {
			t.Errorf("weight 越界: %+v", e)
		}
		switch e.Status {
		case "emergent", "persistent", "fading":
		default:
			t.Errorf("status 非法未兜底: %+v", e)
		}
	}
	if out[0].Status == "weird" {
		t.Errorf("非法 status 应兜底, got %+v", out[0])
	}
}

func TestNormalizeTopicEntriesCap(t *testing.T) {
	in := make([]TopicEntry, 0, topicsMaxPerContact+3)
	for i := 0; i < topicsMaxPerContact+3; i++ {
		in = append(in, TopicEntry{Name: string(rune('A' + i)), Weight: i, Status: "persistent"})
	}
	if out := normalizeTopicEntries(in); len(out) != topicsMaxPerContact {
		t.Errorf("应截到上限 %d, got %d", topicsMaxPerContact, len(out))
	}
}

func TestTopicHistoryUpsertIdempotentAndAscRead(t *testing.T) {
	db := regressionAssistantDB(t)
	if err := ensureTopicHistory(db); err != nil {
		t.Fatal(err)
	}
	id := regressionContact(t, db, "主题对象")
	now := time.Date(2025, 6, 20, 12, 0, 0, 0, time.Local)
	w1 := weekStartOf(now.AddDate(0, 0, -7))
	w2 := weekStartOf(now)

	topics := []TopicEntry{{Name: "攀岩", Weight: 60, Status: "persistent"}}
	upsertTopicHistory(db, id, w1, topics, now.AddDate(0, 0, -7))
	upsertTopicHistory(db, id, w1, topics, now.AddDate(0, 0, -7)) // 同周覆盖，幂等
	upsertTopicHistory(db, id, w2, []TopicEntry{{Name: "新工作", Weight: 80, Status: "emergent"}}, now)

	res, err := GetContactTopics(db, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Weeks) != 2 {
		t.Fatalf("应两周记录, got %d", len(res.Weeks))
	}
	if !(res.Weeks[0].WeekStart < res.Weeks[1].WeekStart) {
		t.Errorf("读回应按 week_start 升序, got %+v", res.Weeks)
	}
	if res.Name == "" {
		t.Error("应带联系人名称")
	}
}

func TestGetContactTopicsEmptyNote(t *testing.T) {
	db := regressionAssistantDB(t)
	if err := ensureTopicHistory(db); err != nil {
		t.Fatal(err)
	}
	id := regressionContact(t, db, "无人分析过")
	res, err := GetContactTopics(db, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Weeks) != 0 {
		t.Errorf("空历史应 0 周, got %d", len(res.Weeks))
	}
	if res.Note == "" {
		t.Error("空历史应有诚实 note")
	}
}

func TestComputeTopicEvolutionUnconfigured(t *testing.T) {
	db := regressionAssistantDB(t)
	llm := NewLLMClient(&Config{}) // 无 apiKey/baseURL → 未配置
	if llm.configured() {
		t.Fatal("测试前置：此 LLM 客户端应为未配置")
	}
	n, note, err := ComputeTopicEvolution(context.Background(), db, llm, time.Now(), topicsWindowDays)
	if err != ErrLLMNotConfigured {
		t.Fatalf("未配模型应返回 ErrLLMNotConfigured, got %v", err)
	}
	if n != 0 || note == "" {
		t.Errorf("未配模型应 0 联系人数 + note, got n=%d note=%q", n, note)
	}
}

func TestDecodeTopicsRobust(t *testing.T) {
	if got := decodeTopics(""); len(got) != 0 {
		t.Errorf("空串应解出空切片, got %+v", got)
	}
	if got := decodeTopics("{坏数据"); len(got) != 0 {
		t.Errorf("坏 JSON 应解出空切片, got %+v", got)
	}
}
