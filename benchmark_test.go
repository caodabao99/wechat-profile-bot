package main

// 关键路径基准测试（非缺陷补强，仅提供性能可观测性）。
// 全部只读/幂等、独立建库，不污染其它用例；用 `go test -bench=. -benchmem` 运行。

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// benchDB 为基准单独建一个跑过完整迁移的库，随 b.Cleanup 释放。
func benchDB(b *testing.B) *sql.DB {
	b.Helper()
	if config == nil {
		config = &Config{}
	}
	db, err := InitDB(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	return db
}

// benchSeedGraph 建 n 个联系人串成链状，返回 db 供 buildNetwork 消费。
// 节点名用 bench-%04d 避免按名去重导致的规模不足（历史教训）。
func benchSeedGraph(b *testing.B, n int) *sql.DB {
	b.Helper()
	db := benchDB(b)
	base := time.Now().Format("2006-01-02 15:04:05")
	for i := 0; i < n; i++ {
		id, err := GetOrCreateContact(db, fmt.Sprintf("bench-%04d", i))
		if err != nil {
			b.Fatal(err)
		}
		if i > 0 {
			prev, err := GetOrCreateContact(db, fmt.Sprintf("bench-%04d", i-1))
			if err != nil {
				b.Fatal(err)
			}
			if _, err := db.Exec(`INSERT OR IGNORE INTO contact_connections
				(contact_a, contact_b, connection_type, detail, confidence, created_at)
				VALUES (?,?,?,?,?,?)`, prev, id, "shared_interest", "b", 0.9, base); err != nil {
				b.Fatal(err)
			}
		}
	}
	return db
}

func BenchmarkBuildNetwork(b *testing.B) {
	db := benchSeedGraph(b, 200)
	now := time.Now()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := buildNetwork(db, now); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkComputeSocialStats(b *testing.B) {
	db := benchDB(b)
	cid, err := GetOrCreateContact(db, "bench-social")
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now()
	for i := 0; i < 2000; i++ {
		sender := "me"
		if i%2 == 0 {
			sender = "other"
		}
		if _, err := db.Exec(`INSERT INTO messages (contact_id, sender, content, msg_hash, msg_time)
			VALUES (?,?,?,?,?)`, cid, sender, fmt.Sprintf("消息 %d", i), fmt.Sprintf("h%d", i),
			now.Add(-time.Duration(i)*time.Minute).Format(time.RFC3339)); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ComputeSocialStats(db, 30); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDeriveFacts(b *testing.B) {
	pj := `{
		"basic_info":{"occupation":"教师","location":"北京","important_dates":["生日: 5月20日","结婚纪念日"]},
		"personality":["开朗","细致"],
		"interests":["篮球","阅读","旅行"],
		"important_facts":["养了一只猫"],
		"communication_style":{"frequent_phrases":["么么哒","好的呢"]},
		"relationship":{"closeness":"很亲密"}
	}`
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = deriveFacts(pj)
	}
}

// ---------- P15: 大体量消息性能基准 ----------

// benchSeedMessages 给联系人灌入 n 条消息（真实时间戳分布，跨度 90 天）。
func benchSeedMessages(b *testing.B, db *sql.DB, cid int64, n int) {
	b.Helper()
	now := time.Now()
	batch := make([]Message, 0, 500)
	for i := 0; i < n; i++ {
		sender := "other"
		if i%3 == 0 {
			sender = "me"
		}
		batch = append(batch, Message{
			Sender:    sender,
			Content:   fmt.Sprintf("bench-msg-%06d", i),
			Timestamp: now.Add(-time.Duration(n-i) * time.Minute),
		})
		if len(batch) == 500 || i == n-1 {
			if _, err := SaveMessages(db, cid, batch); err != nil {
				b.Fatal(err)
			}
			batch = batch[:0]
		}
	}
}

func BenchmarkSaveMessages1000(b *testing.B) {
	db := benchDB(b)
	cid, _ := GetOrCreateContact(db, "bench-write-1k")
	msgs := make([]Message, 1000)
	now := time.Now()
	for i := range msgs {
		msgs[i] = Message{Sender: "other", Content: fmt.Sprintf("w-%d-%d", b.N, i), Timestamp: now.Add(time.Duration(i) * time.Second)}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 每次用不同 hash 避免被去重成 0
		for j := range msgs {
			msgs[j].Content = fmt.Sprintf("bench-%d-%d", i, j)
		}
		if _, err := SaveMessages(db, cid, msgs); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHistoryMessagesLarge(b *testing.B) {
	db := benchDB(b)
	cid, _ := GetOrCreateContact(db, "bench-read-5k")
	benchSeedMessages(b, db, cid, 5000)
	f := HistoryFilter{ContactID: cid, SinceUnix: time.Now().AddDate(0, 0, -90).Unix(), WithTimeOnly: true}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := HistoryMessages(db, f, 200); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHistoryCountLarge(b *testing.B) {
	db := benchDB(b)
	cid, _ := GetOrCreateContact(db, "bench-count-5k")
	benchSeedMessages(b, db, cid, 5000)
	f := HistoryFilter{ContactID: cid, SinceUnix: time.Now().AddDate(0, 0, -90).Unix()}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := HistoryCount(db, f); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRefreshStates50Contacts(b *testing.B) {
	db := benchDB(b)
	for i := 0; i < 50; i++ {
		cid, _ := GetOrCreateContact(db, fmt.Sprintf("bench-state-%02d", i))
		db.Exec(`UPDATE contacts SET profile_json='{"summary":"s"}' WHERE id=?`, cid)
		benchSeedMessages(b, db, cid, 100)
	}
	now := time.Now()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := RefreshRelationshipStates(db, now, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkConsolidationProposal(b *testing.B) {
	db := benchDB(b)
	// 20 个联系人，每人 10 条 active 事实
	now := time.Now().Format(time.RFC3339)
	for i := 0; i < 20; i++ {
		cid, _ := GetOrCreateContact(db, fmt.Sprintf("bench-consol-%02d", i))
		dbMu.Lock()
		for j := 0; j < 10; j++ {
			db.Exec(`INSERT OR IGNORE INTO profile_facts (contact_id, fact_type, fact_key, fact_value, status, confidence, source_type, last_seen, first_seen, updated_at, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
				cid, "interest", "", fmt.Sprintf("fact-%d-%d", i, j), "active", 0.6, "ai", now, now, now, now)
		}
		dbMu.Unlock()
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := BuildConsolidationProposal(db, time.Now()); err != nil {
			b.Fatal(err)
		}
	}
}
