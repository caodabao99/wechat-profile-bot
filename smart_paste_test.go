package main

// ═══════════════════════════════════════════════════════════════════════════
// Phase 4 验收：Smart Paste 2.0（§12）。钉死三条硬约束，而非只测 happy path：
//  1. §12.1 粘贴闭环四指标：恒等式「输入 = 新增 + 重复 + 异常」，异常单列可聚合。
//  2. §12.2 去重身份优先哈希、含 contact 维度——相同内容、不同联系人**不得**误去重
//     （证明不是「只依赖内容」）；相同 (contact, hash) 才去重。
//  3. §12.3 AI 触发分级：冷启动照常；已有画像时普通无信号粘贴只入库（不触发），
//     有重要变化信号才触发；累积到 间隔×2 强制安全刷新（绝不永久停更）。
//     信号检测纯词法、确定性、可复现。
// ═══════════════════════════════════════════════════════════════════════════

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestComputePasteAuditClosedLoop(t *testing.T) {
	// 蓝图样例：输入 500、新增 37、其余重复、无异常。
	a := ComputePasteAudit(500, 500, 37)
	if a.Input != 500 || a.New != 37 || a.Dup != 463 || a.Anomaly != 0 {
		t.Fatalf("样例不符: %+v", a)
	}
	if a.Input != a.New+a.Dup+a.Anomaly {
		t.Fatalf("§12.1 恒等式破口: %d != %d+%d+%d", a.Input, a.New, a.Dup, a.Anomaly)
	}
	// 异常：解析出 10 条、其中 3 条空被丢弃、有效 7 条里新增 2。
	b := ComputePasteAudit(10, 7, 2)
	if b.Input != 10 || b.Anomaly != 3 || b.Dup != 5 || b.New != 2 {
		t.Fatalf("异常分解不符: %+v", b)
	}
	if b.Input != b.New+b.Dup+b.Anomaly {
		t.Fatalf("恒等式破口: %+v", b)
	}
	// 钳制：新增不得超过可用，可用不得超过输入 → 绝不伪造。
	c := ComputePasteAudit(3, 5, 9)
	if c.New > c.Dup+c.New || c.Input < c.New || c.New != 5 || c.Dup != 0 || c.Anomaly != 0 {
		t.Fatalf("非法入参应被钳制自洽, 实得 %+v", c)
	}
}

func TestDedupIdentityIsContactScopedNotContentOnly(t *testing.T) {
	db := regressionDB(t)
	a := regressionContact(t, db, "甲")
	b := regressionContact(t, db, "乙")
	// 完全相同的一条内容（同 sender、同内容、无时间戳）分属两个联系人。
	msg := []Message{{Sender: "other", Content: "好的"}}
	n1, err := SaveMessages(db, a, msg)
	if err != nil || n1 != 1 {
		t.Fatalf("甲首条应新增 1，实得 n=%d err=%v", n1, err)
	}
	// §12.2 铁律：身份含 contact 维度 → 乙存同样内容也必须新增（不被甲去重）。
	n2, err := SaveMessages(db, b, msg)
	if err != nil || n2 != 1 {
		t.Fatalf("§12.2 相同内容不同联系人不得误去重，乙应新增 1，实得 n=%d err=%v", n2, err)
	}
	// 同联系人重复粘贴 → 命中 UNIQUE(contact_id, msg_hash) → 去重为 0。
	n3, err := SaveMessages(db, a, msg)
	if err != nil || n3 != 0 {
		t.Fatalf("同联系人重复应去重为 0，实得 n=%d err=%v", n3, err)
	}
	var cnt int
	db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&cnt)
	if cnt != 2 {
		t.Fatalf("库里应恰有 2 行（甲、乙各一），实得 %d", cnt)
	}
}

func TestDetectChangeSignalsDeterministic(t *testing.T) {
	// 普通寒暄：无信号。
	plain := []Message{{Sender: "other", Content: "在吗"}, {Sender: "me", Content: "在"}}
	if s := DetectChangeSignals(plain); s != nil {
		t.Fatalf("普通消息应无信号，实得 %v", s)
	}
	// 对方「我是新入职的律师，打算创业」→ new_fact + project + intent。
	facts := []Message{{Sender: "other", Content: "我是新入职的律师，打算创业做个法律项目"}}
	got := DetectChangeSignals(facts)
	want := map[PasteSignal]bool{SigNewFact: true, SigIntent: true, SigProject: true}
	if len(got) != 3 {
		t.Fatalf("应检出 3 类信号，实得 %v", got)
	}
	for _, s := range got {
		if !want[s] {
			t.Fatalf("意外信号 %q", s)
		}
	}
	// me 的话不算对方事实来源：只含 me 的信号词 → 无信号。
	onlyMe := []Message{{Sender: "me", Content: "我打算结婚"}}
	if s := DetectChangeSignals(onlyMe); s != nil {
		t.Fatalf("me 的话不应触发对方信号，实得 %v", s)
	}
}

func TestDecideAITriggerTiering(t *testing.T) {
	db := regressionDB(t)
	old := config
	defer func() { config = old }()
	config = &Config{}
	config.Profile.ColdStartCount = 20
	config.Profile.UpdateInterval = 10

	// 1) 冷启动：无画像 + 达阈值 → 触发（不受信号门影响）。
	cold := regressionContact(t, db, "冷启动")
	db.Exec(`UPDATE contacts SET other_msg_count=20, profile_json='' WHERE id=?`, cold)
	d := DecideAITrigger(db, cold, nil)
	if !d.Trigger || d.Path != "cold_start" {
		t.Fatalf("冷启动达阈值应触发 cold_start，实得 %+v", d)
	}

	// 2) 已有画像、恰好到间隔、普通无信号 → 只入库，不触发（§12.3 成本治理）。
	quiet := regressionContact(t, db, "闲聊")
	db.Exec(`UPDATE contacts SET other_msg_count=30, profile_msg_count=20, profile_json='{"x":1}' WHERE id=?`, quiet)
	d = DecideAITrigger(db, quiet, []Message{{Sender: "other", Content: "今天天气不错"}})
	if d.Trigger || d.Path != "none" {
		t.Fatalf("普通无信号粘贴(gap=10)应只入库，实得 %+v", d)
	}

	// 3) 已有画像、到间隔、有重要变化信号 → 触发 signal。
	sig := regressionContact(t, db, "有变化")
	db.Exec(`UPDATE contacts SET other_msg_count=30, profile_msg_count=20, profile_json='{"x":1}' WHERE id=?`, sig)
	d = DecideAITrigger(db, sig, []Message{{Sender: "other", Content: "我刚辞职，打算创业"}})
	if !d.Trigger || d.Path != "signal" {
		t.Fatalf("到间隔且有信号应触发 signal，实得 %+v", d)
	}

	// 4) 已有画像、无信号但累积到 间隔×2 → 触发 periodic_refresh（保证不永久停更）。
	stale := regressionContact(t, db, "久未更")
	db.Exec(`UPDATE contacts SET other_msg_count=50, profile_msg_count=29, profile_json='{"x":1}' WHERE id=?`, stale)
	d = DecideAITrigger(db, stale, []Message{{Sender: "other", Content: "嗯嗯"}})
	if !d.Trigger || d.Path != "periodic_refresh" {
		t.Fatalf("gap=21≥间隔×2 应安全刷新，实得 %+v", d)
	}
}

func TestIngestStatsAnomalyColumnEndToEnd(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()
	id := regressionContact(t, db, "异常统计")
	// 一次：可用 10 全新增、异常 0；一次：可用 8 新增 3(重复 5)、异常 2。
	RecordIngestStatAudit(db, id, 10, 10, 0, now.Add(-time.Hour))
	RecordIngestStatAudit(db, id, 8, 3, 2, now)

	st, err := GetIngestStats(db, now, 90)
	if err != nil {
		t.Fatal(err)
	}
	if st.TotalParsed != 18 || st.TotalNew != 13 || st.TotalDup != 5 || st.TotalAnomaly != 2 {
		t.Fatalf("四指标聚合不符: %+v", st)
	}
	// 兼容旧签名：anomaly 记 0。
	RecordIngestStat(db, id, 4, 1, now.Add(-2*time.Hour))
	st2, _ := GetIngestStats(db, now, 90)
	if st2.TotalAnomaly != 2 {
		t.Fatalf("旧签名不应引入异常计数，实得 %d", st2.TotalAnomaly)
	}
}

func TestSmartPasteIngestAPIExposesAuditAndTrigger(t *testing.T) {
	db := regressionDB(t)
	old := config
	defer func() { config = old }()
	config = &Config{}
	config.Profile.ColdStartCount = 20
	config.Profile.UpdateInterval = 10

	cfg := &Config{APIToken: "test-rel-token", MyName: "我"}
	cfg.Profile.ColdStartCount = 20
	cfg.Profile.UpdateInterval = 10
	s := &apiServer{db: db, cfg: cfg, llm: NewLLMClient(cfg), sessions: &webSessionStore{sessions: map[string]time.Time{}}, ingestRL: newRateLimiter(100, time.Minute)}

	text := "粘贴人 2025/6/10 10:23:45\n最近怎么样\n我 2025/6/10 10:25:00\n挺好的"
	w := callAPI(s, http.MethodPost, "/api/ingest", `{"text":"`+escapeJSON(text)+`","analyze":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("ingest 应 200，实得 %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Audit struct {
			Input   int `json:"input"`
			New     int `json:"new"`
			Dup     int `json:"dup"`
			Anomaly int `json:"anomaly"`
		} `json:"audit"`
		AITrigger struct {
			Path   string `json:"path"`
			Reason string `json:"reason"`
		} `json:"aiTrigger"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Audit.Input != resp.Audit.New+resp.Audit.Dup+resp.Audit.Anomaly {
		t.Fatalf("API 四指标恒等式破口: %+v", resp.Audit)
	}
	if resp.AITrigger.Path == "" {
		t.Fatal("aiTrigger.path 应非空（可解释触发决策）")
	}
}

// escapeJSON 仅转义测试里用到的最小字符集（引号/反斜杠/换行/制表）。
func escapeJSON(s string) string {
	b := make([]byte, 0, len(s)+8)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\n':
			b = append(b, '\\', 'n')
		case '\t':
			b = append(b, '\\', 't')
		case '\r':
			b = append(b, '\\', 'r')
		default:
			b = append(b, s[i])
		}
	}
	return string(b)
}
