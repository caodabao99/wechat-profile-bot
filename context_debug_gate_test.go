package main

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// contextDebugProbe 以指定 debug 配置调用 routeContactContext，返回解析后的响应 map。
func contextDebugProbe(t *testing.T, db *sql.DB, id int64, debug bool) map[string]any {
	t.Helper()
	s := &apiServer{db: db, cfg: &Config{ContextDebug: debug}}
	r := httptest.NewRequest("GET", "/api/contacts/1/context?task=profile", nil)
	w := httptest.NewRecorder()
	s.routeContactContext(w, r, id)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	return out
}

// TestContactContextPrivacyGate 校验 §4.3 隐私门：默认不返回 rendered/完整上下文，仅结构化摘要；
// 开启 contextDebug 才返回 rendered。
func TestContactContextPrivacyGate(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "隐私门测试")
	regressionMessages(t, db, id, "我最近准备换公司", "我是律师")

	// 默认（debug=false）：无 rendered、无 context 正文、有 summary。
	off := contextDebugProbe(t, db, id, false)
	if off["debug"] != false {
		t.Fatalf("expected debug=false, got %v", off["debug"])
	}
	if _, ok := off["rendered"]; ok {
		t.Fatal("rendered must NOT be present when ContextDebug=false (privacy)")
	}
	if _, ok := off["context"]; ok {
		t.Fatal("full context must NOT be present when ContextDebug=false (privacy)")
	}
	sum, ok := off["summary"].(map[string]any)
	if !ok || sum["recent_messages"] == nil {
		t.Fatalf("expected structured summary, got %v", off["summary"])
	}

	// 开启 debug：返回完整 context + rendered。
	on := contextDebugProbe(t, db, id, true)
	if on["debug"] != true {
		t.Fatalf("expected debug=true, got %v", on["debug"])
	}
	if rendered, ok := on["rendered"].(string); !ok || rendered == "" {
		t.Fatalf("expected non-empty rendered when ContextDebug=true, got %v", on["rendered"])
	}
	if _, ok := on["context"]; !ok {
		t.Fatal("expected full context when ContextDebug=true")
	}
}
