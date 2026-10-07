package main

// 投产前审计修复的针对性回归测试（批次 B/C/D）。
// 每条都对应一个已被证实的缺陷：先能复现原问题，再钉住修复后的正确行为。

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// C3：v19/v26 的表重建现在包在事务里，因此**可以安全重放**。
// 旧实现逐条自动提交：崩在中途会留下 *_new 残表或已被 DROP 的主表，下次启动永久失败。
// 做法：把 user_version 回卷到 18 再 migrate()，要求一路收敛到当前版本、无残表、主表可写。
func TestMigrationTableRebuildIsReplaySafe(t *testing.T) {
	db := regressionDB(t)
	if _, err := db.Exec(`PRAGMA user_version = 18`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("回卷到 v18 后重放迁移应成功（重建已包事务），实得: %v", err)
	}
	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil || ver != backupCurrentDBVer {
		t.Fatalf("重放后应回到当前版本 %d，实得 %d (err=%v)", backupCurrentDBVer, ver, err)
	}
	// 残表是「迁移被中断」的指纹，重放后不得存在
	for _, leftover := range []string{"profile_facts_new", "relationship_action_log_new"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, leftover).Scan(&n); err != nil || n != 0 {
			t.Fatalf("不应残留 %s 表，实得 n=%d err=%v", leftover, n, err)
		}
	}
	// 主表必须还在且可写（重建没把表弄丢；FK 在 DSN 层未开，故 contact_id=0 可行）
	if _, err := db.Exec(`INSERT INTO profile_facts(contact_id, fact_type, fact_value) VALUES(0,'occupation','probe')`); err != nil {
		t.Fatalf("重建后 profile_facts 应可写: %v", err)
	}
}

// insertFact 建一条 active 事实并返回其 id。
func insertFact(t *testing.T, db *sql.DB, cid int64, value string) int64 {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO profile_facts(contact_id, fact_type, fact_value, status)
		VALUES(?, 'occupation', ?, 'active')`, cid, value); err != nil {
		t.Fatalf("建事实 %q 失败: %v", value, err)
	}
	var id int64
	if err := db.QueryRow(`SELECT id FROM profile_facts WHERE contact_id=? AND fact_value=?`, cid, value).Scan(&id); err != nil {
		t.Fatalf("回读事实 id 失败: %v", err)
	}
	return id
}

// C4：两个 loser 共享同一条支撑消息时，旧实现迁移证据必撞 UNIQUE(fact_id,message_id,archived)。
func TestMergeFactsWithSharedEvidenceAcrossLosers(t *testing.T) {
	db := regressionDB(t)
	cid, err := GetOrCreateContact(db, "合并证据测试")
	if err != nil {
		t.Fatal(err)
	}
	keep := insertFact(t, db, cid, "职业:教师")
	l1 := insertFact(t, db, cid, "职业 老师")
	l2 := insertFact(t, db, cid, "当老师")

	// keep/l1/l2 都引用同一条消息；l1/l2 还共享第二条 —— 这就是撞约束的触发条件
	link := func(fid int64, msg int) {
		if _, err := db.Exec(`INSERT INTO profile_fact_evidence(fact_id, contact_id, message_id, archived)
			VALUES(?,?,?,0)`, fid, cid, msg); err != nil {
			t.Fatalf("插入证据失败 fact=%d msg=%d: %v", fid, msg, err)
		}
	}
	for _, fid := range []int64{keep, l1, l2} {
		link(fid, 99001)
	}
	link(l1, 99002)
	link(l2, 99002)

	if err := MergeFacts(db, keep, []int64{l1, l2}); err != nil {
		t.Fatalf("loser 之间共享支撑消息时合并不应报错（旧实现撞 UNIQUE），实得: %v", err)
	}

	var dup int
	if err := db.QueryRow(`SELECT COUNT(*) FROM (SELECT message_id, archived FROM profile_fact_evidence
		WHERE fact_id=? GROUP BY message_id, archived HAVING COUNT(*)>1)`, keep).Scan(&dup); err != nil || dup != 0 {
		t.Fatalf("keep 的证据不应有重复组，实得 dup=%d err=%v", dup, err)
	}
	var stranded int
	if err := db.QueryRow(`SELECT COUNT(*) FROM profile_fact_evidence WHERE fact_id IN (?,?)`, l1, l2).Scan(&stranded); err != nil || stranded != 0 {
		t.Fatalf("loser 上不应残留证据，实得 %d err=%v", stranded, err)
	}
	var superseded int
	if err := db.QueryRow(`SELECT COUNT(*) FROM profile_facts WHERE id IN (?,?) AND status='superseded'`, l1, l2).Scan(&superseded); err != nil || superseded != 2 {
		t.Fatalf("两个 loser 都应标 superseded，实得 %d err=%v", superseded, err)
	}
	// keep 的两条支撑消息都应保留（去重只删重复，不得把证据删光）
	var keptEvidences int
	if err := db.QueryRow(`SELECT COUNT(*) FROM profile_fact_evidence WHERE fact_id=?`, keep).Scan(&keptEvidences); err != nil || keptEvidences != 2 {
		t.Fatalf("keep 应有 2 条去重后的证据，实得 %d err=%v", keptEvidences, err)
	}
}

// C6：SQLite 的 strftime 对带偏移的 RFC3339 会先转 UTC，不加 'localtime' 就偏 8 小时
// （life_state 的小时/星期分布曾踩此坑）。这条把语义钉死。
func TestStrftimeLocalHourSemantics(t *testing.T) {
	db := regressionDB(t)
	rfc := "2026-03-05T08:05:00+08:00" // 北京时间 08:05，UTC 为 00:05
	var naive, local int
	if err := db.QueryRow(`SELECT CAST(strftime('%H', ?) AS INT)`, rfc).Scan(&naive); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT CAST(strftime('%H', ?, 'localtime') AS INT)`, rfc).Scan(&local); err != nil {
		t.Fatal(err)
	}
	if naive == 8 {
		t.Skipf("本机时区让不加 localtime 也恰好得 8，无法体现差异（TZ=%v）", time.Local)
	}
	if local != 8 {
		t.Fatalf("加 'localtime' 应取到本地小时 8，实得 %d（不加则为 %d，即曾经偏移的小时数）", local, naive)
	}
}

// C8：认证前通道必须「同时占用的资源有界」。实测 go 的 TimeoutHandler 要等内层 handler 返回才能发出
// 503（单独靠它防不住滴漏占住 goroutine），所以真正的防护是并发上限：打满即立即 429。
func TestAuthGuardCapsConcurrency(t *testing.T) {
	s := &apiServer{cfg: &Config{}, guard: newSecurityGuard(), authSem: make(chan struct{}, 1)}
	occupied := make(chan struct{}, 1)
	release := make(chan struct{})
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		occupied <- struct{}{}
		<-release // 模拟被慢速 body 长期占住的 handler goroutine
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	})
	h := s.authGuard(inner)

	// 第一个请求占满唯一槽位（不等待其完成，故意让它一直挂着）
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/auth/login", nil))
	}()
	select {
	case <-occupied:
	case <-time.After(2 * time.Second):
		t.Fatal("第一个请求未进入 handler，用例无法验证并发上限")
	}

	// 第二个请求必须立即被 429 拒掉，而不是排队并再多占一个 goroutine
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, httptest.NewRequest(http.MethodPost, "/api/auth/login", nil))
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("通道打满时应立即 429，实得 %d body=%s", w2.Code, w2.Body.String())
	}
	if w2.Header().Get("Retry-After") == "" {
		t.Error("429 必须带 Retry-After，让前端能退避重试")
	}

	close(release)

	// authSem 为 nil（老测试/CLI 构造体）时绝不能永久阻塞：这是 nil channel 发送的陷阱
	s2 := &apiServer{cfg: &Config{}}
	done := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		s2.authGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })).
			ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/auth/login", nil))
		done <- w.Code
	}()
	select {
	case code := <-done:
		if code != http.StatusTeapot {
			t.Fatalf("nil 信号量应退化为不限流，实得 %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nil authSem 导致永久阻塞（向 nil channel 发送的陷阱）")
	}
}
