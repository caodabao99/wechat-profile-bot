package main

// 蓝图 §25.1 Benchmark —— 关键读写的 P50/P95/P99 分位延迟可观测性。
//
// 为什么单独成文：Go 原生 benchmark 只报 ns/op（均值），拿不到尾延迟分位；而 §25.1 明确要
// 记录 P50 / P95 / P99。这里用「固定次采样 + 逐次计时 + b.ReportMetric 上报分位列」的范式补齐，
// 覆盖 §25.1 点名的 7 类操作：Search / Keyset / History / Aggregate / SaveMessages /
// RefreshStates / Consolidation。
//
// 规模阶梯（§25.1：100K / 500K / 1M / 5M / 10M）经环境变量控制，默认取 CI 友好小值，避免拖慢
// 常规 `go test`（benchmark 本就不在普通测试里运行，只有 `-bench` 才跑）：
//
//	PERF_SCALE=100000  go test -run '^$' -bench 'BenchmarkPct' -benchtime=1x ./...
//	PERF_SCALE=1000000 ... 5000000 ... 10000000
//	PERF_SAMPLES=50    （每档采样次数，默认 30）
//
// 全部只读/幂等、独立建库（benchDB），不污染其它用例；复用 benchmark_test.go 的种子助手。

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"
)

// perfScale 返回种子消息规模（§25.1 阶梯），默认 20000（CI 友好），可经 PERF_SCALE 覆盖。
func perfScale() int {
	if v := os.Getenv("PERF_SCALE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 20000
}

// perfSamples 返回每档操作的计时采样次数，默认 30，可经 PERF_SAMPLES 覆盖。
func perfSamples() int {
	if v := os.Getenv("PERF_SAMPLES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 2 {
			return n
		}
	}
	return 30
}

// perfPercentile 从时长样本中取第 p 分位（0~1），返回毫秒。样本会先排序。
// 采用「最近秩」定义：idx = ceil(p*n)-1，稳定、单调、越界钳制。
func perfPercentile(samples []time.Duration, p float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	d := append([]time.Duration(nil), samples...)
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	idx := int(math.Ceil(p*float64(len(d)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(d) {
		idx = len(d) - 1
	}
	return float64(d[idx]) / float64(time.Millisecond)
}

// timeOp 运行 op 共 n 次、逐次计时，把 P50/P95/P99（ms）作为自定义指标列上报。
// 分位分布只取决于单次 op 的真实耗时，与 b.N 无关，故此处显式用固定采样数。
func timeOp(b *testing.B, n int, op func()) {
	b.Helper()
	b.ReportAllocs()
	samples := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		t0 := time.Now()
		op()
		samples = append(samples, time.Since(t0))
	}
	b.ReportMetric(perfPercentile(samples, 0.50), "p50ms")
	b.ReportMetric(perfPercentile(samples, 0.95), "p95ms")
	b.ReportMetric(perfPercentile(samples, 0.99), "p99ms")
}

// seedMessagesNoT 是非 b.* 版本的消息种子，便于在 StopTimer 外大批量灌数据。
func seedMessagesNoT(tb testing.TB, db *sql.DB, cid int64, n int) {
	tb.Helper()
	now := time.Now()
	batch := make([]Message, 0, 1000)
	for i := 0; i < n; i++ {
		sender := "other"
		if i%3 == 0 {
			sender = "me"
		}
		batch = append(batch, Message{
			Sender:    sender,
			Content:   fmt.Sprintf("pct-msg-%08d", i),
			Timestamp: now.Add(-time.Duration(n-i) * time.Minute),
		})
		if len(batch) == 1000 || i == n-1 {
			if _, err := SaveMessages(db, cid, batch); err != nil {
				tb.Fatal(err)
			}
			batch = batch[:0]
		}
	}
}

// ---------- §25.1 七类操作的分位基准 ----------

func BenchmarkPct_SaveMessages(b *testing.B) {
	db := benchDB(b)
	cid, _ := GetOrCreateContact(db, "pct-write")
	b.StopTimer()
	seedMessagesNoT(b, db, cid, perfScale()) // 预置规模，令写入命中已相当庞大的索引（§25.1 规模语义）
	samples := perfSamples()
	b.ResetTimer()
	seq := 0
	timeOp(b, samples, func() {
		seq++
		msgs := make([]Message, 1000)
		now := time.Now()
		for j := range msgs {
			msgs[j] = Message{
				Sender:    "other",
				Content:   fmt.Sprintf("pct-w-%d-%d", seq, j), // 唯一 hash，避免去重成 0
				Timestamp: now.Add(time.Duration(j) * time.Second),
			}
		}
		if _, err := SaveMessages(db, cid, msgs); err != nil {
			b.Fatal(err)
		}
	})
}

func BenchmarkPct_History(b *testing.B) {
	db := benchDB(b)
	cid, _ := GetOrCreateContact(db, "pct-hist")
	b.StopTimer()
	seedMessagesNoT(b, db, cid, perfScale())
	f := HistoryFilter{ContactID: cid, SinceUnix: time.Now().AddDate(-2, 0, 0).Unix(), WithTimeOnly: true}
	b.StartTimer()
	timeOp(b, perfSamples(), func() {
		if _, err := HistoryMessages(db, f, 200); err != nil {
			b.Fatal(err)
		}
	})
}

func BenchmarkPct_Aggregate(b *testing.B) {
	db := benchDB(b)
	cid, _ := GetOrCreateContact(db, "pct-agg")
	b.StopTimer()
	seedMessagesNoT(b, db, cid, perfScale())
	f := HistoryFilter{ContactID: cid, SinceUnix: time.Now().AddDate(-2, 0, 0).Unix()}
	b.StartTimer()
	timeOp(b, perfSamples(), func() {
		if _, err := HistoryAggregateByDay(db, f); err != nil {
			b.Fatal(err)
		}
	})
}

func BenchmarkPct_Search(b *testing.B) {
	db := benchDB(b)
	cid, _ := GetOrCreateContact(db, "pct-search")
	b.StopTimer()
	seedMessagesNoT(b, db, cid, perfScale())
	b.StartTimer()
	timeOp(b, perfSamples(), func() {
		if _, err := SearchMessages(db, SearchOptions{Query: "pct-msg", ContactID: cid, Limit: 20}); err != nil {
			b.Fatal(err)
		}
	})
}

// BenchmarkPct_Keyset 用游标翻页取深处一页（成本应恒定，不随页深下降）。
func BenchmarkPct_Keyset(b *testing.B) {
	db := benchDB(b)
	cid, _ := GetOrCreateContact(db, "pct-keyset")
	b.StopTimer()
	seedMessagesNoT(b, db, cid, perfScale())
	// 先取一页拿到 NextCursor，作为深处游标基准。
	res, err := SearchMessages(db, SearchOptions{Query: "pct-msg", ContactID: cid, Limit: 20})
	if err != nil {
		b.Fatal(err)
	}
	cursor := res.NextCursor
	b.StartTimer()
	timeOp(b, perfSamples(), func() {
		if _, err := SearchMessages(db, SearchOptions{Query: "pct-msg", ContactID: cid, Limit: 20, Cursor: cursor}); err != nil {
			b.Fatal(err)
		}
	})
}

func BenchmarkPct_RefreshStates(b *testing.B) {
	db := benchDB(b)
	b.StopTimer()
	// 联系人规模随 scale 联动（每 2000 条消息一名联系人，至少 10 名）。
	contacts := perfScale() / 2000
	if contacts < 10 {
		contacts = 10
	}
	if contacts > 2000 {
		contacts = 2000
	}
	now := time.Now()
	for i := 0; i < contacts; i++ {
		cid, _ := GetOrCreateContact(db, fmt.Sprintf("pct-state-%05d", i))
		db.Exec(`UPDATE contacts SET profile_json='{"summary":"s"}' WHERE id=?`, cid)
		seedMessagesNoT(b, db, cid, 50)
	}
	b.StartTimer()
	timeOp(b, perfSamples(), func() {
		if _, _, err := RefreshRelationshipStates(db, now, 0); err != nil {
			b.Fatal(err)
		}
	})
}

func BenchmarkPct_Consolidation(b *testing.B) {
	db := benchDB(b)
	b.StopTimer()
	rfc := time.Now().Format(time.RFC3339)
	contacts := perfScale() / 5000
	if contacts < 10 {
		contacts = 10
	}
	if contacts > 4000 {
		contacts = 4000
	}
	for i := 0; i < contacts; i++ {
		cid, _ := GetOrCreateContact(db, fmt.Sprintf("pct-consol-%05d", i))
		dbMu.Lock()
		for j := 0; j < 10; j++ {
			db.Exec(`INSERT OR IGNORE INTO profile_facts
				(contact_id, fact_type, fact_key, fact_value, status, confidence, source_type, last_seen, first_seen, updated_at, created_at)
				VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
				cid, "interest", "", fmt.Sprintf("fact-%d-%d", i, j), "active", 0.6, "ai", rfc, rfc, rfc, rfc)
		}
		dbMu.Unlock()
	}
	b.StartTimer()
	timeOp(b, perfSamples(), func() {
		if _, err := BuildConsolidationProposal(db, time.Now()); err != nil {
			b.Fatal(err)
		}
	})
}

// ---------- 分位数学的确定性单元校验（快速、与规模无关）----------

func TestPerfPercentileMath(t *testing.T) {
	// 1..100 ms 的样本。
	samples := make([]time.Duration, 100)
	for i := 0; i < 100; i++ {
		samples[i] = time.Duration(i+1) * time.Millisecond
	}
	if got := perfPercentile(samples, 0.50); got != 50 {
		t.Fatalf("P50 应为 50ms, got %g", got)
	}
	if got := perfPercentile(samples, 0.95); got != 95 {
		t.Fatalf("P95 应为 95ms, got %g", got)
	}
	if got := perfPercentile(samples, 0.99); got != 99 {
		t.Fatalf("P99 应为 99ms, got %g", got)
	}
	// 空样本 → 0，不 panic。
	if got := perfPercentile(nil, 0.95); got != 0 {
		t.Fatalf("空样本应返回 0, got %g", got)
	}
	// 乱序输入也应得到正确分位（内部排序、且不改动入参）。
	shuffled := []time.Duration{5 * time.Millisecond, 1 * time.Millisecond, 3 * time.Millisecond}
	if got := perfPercentile(shuffled, 0.50); got != 3 {
		t.Fatalf("乱序 P50 应为 3ms, got %g", got)
	}
	if shuffled[0] != 5*time.Millisecond {
		t.Fatal("perfPercentile 不应就地改动入参切片")
	}
}

// TestPerfScaleEnvParsing 锁住规模/采样环境变量解析（缺省与非法值都回退到安全默认）。
func TestPerfScaleEnvParsing(t *testing.T) {
	t.Setenv("PERF_SCALE", "1000000")
	if got := perfScale(); got != 1000000 {
		t.Fatalf("PERF_SCALE=1000000 应被采纳, got %d", got)
	}
	t.Setenv("PERF_SCALE", "not-a-number")
	if got := perfScale(); got != 20000 {
		t.Fatalf("非法 PERF_SCALE 应回退默认 20000, got %d", got)
	}
	t.Setenv("PERF_SAMPLES", "50")
	if got := perfSamples(); got != 50 {
		t.Fatalf("PERF_SAMPLES=50 应被采纳, got %d", got)
	}
	t.Setenv("PERF_SAMPLES", "1")
	if got := perfSamples(); got != 30 {
		t.Fatalf("过小 PERF_SAMPLES 应回退默认 30, got %d", got)
	}
}
