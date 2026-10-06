package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSuggestedReplies(t *testing.T) {
	for _, tt := range []struct {
		input map[string]interface{}
		want  []string
	}{
		{map[string]interface{}{"suggested_replies": []interface{}{" 你好 ", "", "你好", 12, "明天见", "谢谢", "第四条", "第五条挤掉"}}, []string{"你好", "明天见", "谢谢", "第四条"}},
		{map[string]interface{}{"suggested_replies": []string{" a ", "a", "b"}}, []string{"a", "b"}},
		{map[string]interface{}{"suggested_replies": []interface{}{nil}, "suggested_reply": " 旧回复 "}, []string{"旧回复"}},
		// 新版对象数组：取 text/reply 字段，风格名非法时回退默认风格
		{map[string]interface{}{"suggested_replies": []interface{}{
			map[string]interface{}{"style": "亲切热情", "text": "嗨～"},
			map[string]interface{}{"style": "胡编的风格", "text": "在吗"},
			map[string]interface{}{"style": "简洁直接", "reply": "说"},
		}}, []string{"嗨～", "在吗", "说"}},
		{map[string]interface{}{}, []string{}},
	} {
		if got := suggestedReplies(tt.input); !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("got=%v want=%v", got, tt.want)
		}
	}
}

func TestAnalyzeRepliesSingleCall(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "reply")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"suggested_replies\":[\" 好的 \",\"好的\",\"收到\",\"谢谢\",\"行吧\"]}"}}]}`)
	}))
	defer server.Close()
	cfg := &Config{}
	cfg.LLM.BaseURL = server.URL
	cfg.LLM.ApiKey = "test-key" // 迁移后 AnalyzeIntent 经 callLLMCached，需 configured()（key+baseURL）
	result, err := AnalyzeIntent(context.Background(), db, NewLLMClient(cfg), id, "你好", nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || result["suggested_reply"] != "好的" || len(suggestedReplies(result)) != 4 {
		t.Fatalf("calls=%d result=%v", calls.Load(), result)
	}
	text, replies := formatIntentResult(result)
	// 建议回复不进主体文本，逐条单独返回（微信里方便长按复制单条）
	if strings.Contains(text, "【稳妥得体】") || strings.Contains(text, "建议回复 1") {
		t.Fatalf("建议回复不应出现在主体文本中: %s", text)
	}
	if len(replies) != 4 {
		t.Fatalf("replies=%v", replies)
	}
	// 纯字符串数组按顺序赋予固定风格，四种风格全部给出
	want := []SuggestedReply{
		{Style: "稳妥得体", Text: "好的"},
		{Style: "简洁直接", Text: "收到"},
		{Style: "亲切热情", Text: "谢谢"},
		{Style: "委婉留余地", Text: "行吧"},
	}
	if !reflect.DeepEqual(replies, want) {
		t.Fatalf("replies=%v want=%v", replies, want)
	}
	if !strings.Contains(text, "长按单条即可复制") {
		t.Fatal(text)
	}
}

func TestSuggestedReplyItemsCap(t *testing.T) {
	// 模型多给（5 条）或重复风格时，最多保留 4 条且四种风格各一，不出现第 5 种/重复风格
	in := map[string]interface{}{"suggested_replies": []interface{}{
		map[string]interface{}{"style": "稳妥得体", "text": "一"},
		map[string]interface{}{"style": "简洁直接", "text": "二"},
		map[string]interface{}{"style": "亲切热情", "text": "三"},
		map[string]interface{}{"style": "委婉留余地", "text": "四"},
		map[string]interface{}{"style": "稳妥得体", "text": "五（应被丢弃）"},
	}}
	got := suggestedReplyItems(in)
	if len(got) != 4 {
		t.Fatalf("got=%v", got)
	}
	seen := map[string]bool{}
	for _, r := range got {
		if seen[r.Style] {
			t.Fatalf("风格重复: %v", got)
		}
		seen[r.Style] = true
	}
	for _, s := range replyStyles {
		if !seen[s] {
			t.Fatalf("缺少风格 %s: %v", s, got)
		}
	}
}

func TestSuggestedReplyItemsStyle(t *testing.T) {
	// 模型给了非法风格名 → 回退稳妥得体；同风格重复 → 补一个未使用的风格
	in := map[string]interface{}{"suggested_replies": []interface{}{
		map[string]interface{}{"style": "火星语", "text": "第一条"},
		map[string]interface{}{"style": "稳妥得体", "text": "第二条"},
		map[string]interface{}{"style": "简洁直接", "text": "第三条"},
	}}
	got := suggestedReplyItems(in)
	want := []SuggestedReply{
		{Style: "稳妥得体", Text: "第一条"},
		{Style: "简洁直接", Text: "第二条"},
		{Style: "亲切热情", Text: "第三条"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

func TestEditProfileTransaction(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "edit")
	regressionMessages(t, db, id, "processed")
	if err := SaveProfile(db, id, `{"summary":"old","interests":["old"]}`, "old", "initial"); err != nil {
		t.Fatal(err)
	}
	regressionMessages(t, db, id, "pending")
	c, _ := GetContactByID(db, id)
	epoch := currentProfileEpoch()
	p := Profile{Summary: "用户摘要", Interests: []string{}, IntentPatterns: map[string]string{"问候": "主动关心"}}
	if err := EditProfile(db, id, p, c.ProfileJSON); err != nil {
		t.Fatal(err)
	}
	got, _ := GetContactByID(db, id)
	var decoded Profile
	if err := json.Unmarshal([]byte(got.ProfileJSON), &decoded); err != nil {
		t.Fatal(err)
	}
	if got.ProfileSummary != p.Summary || !reflect.DeepEqual(decoded, p) {
		t.Fatalf("profile=%+v", got)
	}
	var count int
	if err := db.QueryRow(`SELECT profile_msg_count FROM contacts WHERE id=?`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("advanced count %d", count)
	}
	history, err := GetProfileHistory(db, id, 10)
	if err != nil || len(history) != 2 || history[0].ProfileJSON != got.ProfileJSON || history[0].ChangeSummary != "手动编辑画像" {
		t.Fatalf("history=%v err=%v", history, err)
	}
	if err := EditProfile(db, id, Profile{}, c.ProfileJSON); !errors.Is(err, ErrProfileConflict) {
		t.Fatalf("conflict: %v", err)
	}
	if err := saveProfileAtEpoch(db, id, `{}`, "stale", "stale", epoch); !errors.Is(err, ErrProfileStale) {
		t.Fatalf("stale: %v", err)
	}
	history, _ = GetProfileHistory(db, id, 10)
	if len(history) != 2 {
		t.Fatal("failed writes added history")
	}
	if err := EditProfile(db, id, Profile{}, got.ProfileJSON); err != nil {
		t.Fatal(err)
	}
	got, _ = GetContactByID(db, id)
	if got.ProfileSummary != "" {
		t.Fatal("summary not cleared")
	}
}

func TestEditInvalidatesRunningGeneration(t *testing.T) {
	for _, supplement := range []bool{false, true} {
		t.Run(fmt.Sprint(supplement), func(t *testing.T) {
			db := regressionDB(t)
			id := regressionContact(t, db, "running")
			regressionMessages(t, db, id, "hello")
			c, _ := GetContactByID(db, id)
			messages, _ := GetAllMessages(db, id)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				once.Do(func() { close(entered); <-release })
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"summary\":\"stale\"}"}}]}`)
			}))
			defer server.Close()
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			cfg := &Config{}
			cfg.LLM.BaseURL = server.URL
			cfg.LLM.ApiKey = "test-key" // 画像生成/补充经 callLLMCached，需配好密钥才算已配置
			client := NewLLMClient(cfg)
			done := make(chan error, 1)
			go func() {
				if supplement {
					done <- SupplementProfile(context.Background(), db, client, id, "", "note")
				} else {
					done <- GenerateOrUpdateProfile(context.Background(), db, client, id, "", messages)
				}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("LLM timeout")
			}
			if err := EditProfile(db, id, Profile{Summary: "edited"}, c.ProfileJSON); err != nil {
				t.Fatal(err)
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case err := <-done:
				if !errors.Is(err, ErrProfileStale) {
					t.Fatalf("err=%v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("generation timeout")
			}
			got, _ := GetContactByID(db, id)
			if got.ProfileSummary != "edited" {
				t.Fatal("edit overwritten")
			}
		})
	}
}

func TestConcurrentProfileEdits(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "concurrent edits")
	contact, err := GetContactByID(db, id)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, summary := range []string{"first", "second"} {
		go func(summary string) {
			<-start
			results <- EditProfile(db, id, Profile{Summary: summary}, contact.ProfileJSON)
		}(summary)
	}
	close(start)
	var saved, conflicts int
	for i := 0; i < 2; i++ {
		err := <-results
		switch {
		case err == nil:
			saved++
		case errors.Is(err, ErrProfileConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	history, err := GetProfileHistory(db, id, 10)
	if saved != 1 || conflicts != 1 || err != nil || len(history) != 1 {
		t.Fatalf("saved=%d conflicts=%d history=%v err=%v", saved, conflicts, history, err)
	}
}

func TestProfileEditHistoryFailureRollsBack(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "rollback edit")
	contact, err := GetContactByID(db, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_edit_history BEFORE INSERT ON profile_history BEGIN SELECT RAISE(ABORT, 'test history failure'); END`); err != nil {
		t.Fatal(err)
	}
	epoch := currentProfileEpoch()
	if err := EditProfile(db, id, Profile{Summary: "must rollback"}, contact.ProfileJSON); err == nil {
		t.Fatal("expected history failure")
	}
	got, err := GetContactByID(db, id)
	if err != nil || got.ProfileJSON != contact.ProfileJSON || got.ProfileSummary != contact.ProfileSummary || currentProfileEpoch() != epoch {
		t.Fatalf("failed edit changed profile or epoch: %+v err=%v", got, err)
	}
}

func TestProfileEditHTTPValidation(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "api")
	c, _ := GetContactByID(db, id)
	s := &apiServer{db: db}
	valid, _ := json.Marshal(map[string]interface{}{"profile": Profile{Summary: "saved"}, "baseProfileJson": c.ProfileJSON})
	for _, tt := range []struct {
		method, body string
		id           int64
		status       int
	}{
		{"GET", string(valid), id, 405}, {"PUT", `{}`, id, 400}, {"PUT", `{"profile":null,"baseProfileJson":""}`, id, 400},
		{"PUT", `{"profile":{"basic_info":{"important_dates":123}},"baseProfileJson":""}`, id, 400},
		{"PUT", `{"profile":{"unknown":1},"baseProfileJson":""}`, id, 400},
		{"PUT", string(valid) + ` {}`, id, 400}, {"PUT", string(valid), -1, 400}, {"PUT", string(valid), 999999, 404},
		{"PUT", string(valid), id, 200}, {"PUT", string(valid), id, 409},
	} {
		r := httptest.NewRequest(tt.method, "/api/contacts/1/profile", strings.NewReader(tt.body))
		w := httptest.NewRecorder()
		s.hEditProfile(w, r, tt.id)
		if w.Code != tt.status {
			t.Fatalf("body=%s got=%d want=%d response=%s", tt.body, w.Code, tt.status, w.Body.String())
		}
	}
}
