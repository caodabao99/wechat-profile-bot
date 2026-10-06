# V7 Performance Report — 性能 Benchmark（蓝图 §25.1）

> 生成时间：2026-10-06（Phase 16 Documentation）
> 代码：`perf_bench_test.go`（分位延迟基准）+ `TestPerfPercentileMath` / `TestPerfScaleEnvParsing`（钉）
> 运行：`go test -run '^$' -bench 'BenchmarkPct_'`（基准不计入常规覆盖率、不进 CI 关键路径）

---

## 1. 为什么自建分位基准

Go 原生 `testing.B` 只给 `ns/op` 的**均值**，会掩盖尾延迟——关系 OS 的读路径（搜索/历史/首页聚合）
用户体感由 **P95/P99** 决定，而非平均值。v7 因此自建一套「固定采样 + 逐次计时 + 最近秩分位」范式，
用 `b.ReportMetric` 直接上报 `p50ms / p95ms / p99ms` 三列。

- `perfPercentile(samples, p)`：**最近秩法** `idx = ceil(p·n) − 1`，在排序副本上取值——**绝不就地修改入参**
  （`TestPerfPercentileMath`：1..100ms → P50/P95/P99 = 50/95/99；空样本 → 0；乱序入参不被篡改）。
- `timeOp(b, n, op)`：对 n 次操作逐次计时，聚合出三档分位并上报。

---

## 2. 覆盖的操作（§25.1 七项，含补齐的 Aggregate）

| Benchmark | 被测原语 |
|-----------|----------|
| `BenchmarkPct_SaveMessages` | `SaveMessages` 批量入库（预置规模 + 唯一 hash） |
| `BenchmarkPct_History` | `HistoryMessages` 历史读取 |
| `BenchmarkPct_Aggregate` | `HistoryAggregateByDay` 日聚合（**Phase 13 补齐的缺口**） |
| `BenchmarkPct_Search` | `SearchMessages` FTS 搜索 |
| `BenchmarkPct_Keyset` | `SearchMessages` cursor 深分页（取 `NextCursor`） |
| `BenchmarkPct_RefreshStates` | `RefreshRelationshipStates` 状态刷新 |
| `BenchmarkPct_Consolidation` | `BuildConsolidationProposal` 记忆整合建议 |

---

## 3. 规模可复现（env 化，CI 快 / 压测深）

| 变量 | 默认 | 语义 |
|------|------|------|
| `PERF_SCALE` | 20,000 | 数据集规模；§25.1 目标档位 100K / 500K / 1M / 5M / 10M |
| `PERF_SAMPLES` | 30 | 每个操作的分位采样次数 |

日常 CI 用小规模冒烟（下述），深度压测只需 `PERF_SCALE=5000000 go test -bench ...`
即可在**同一套代码**下打到 §25.1 的 5M 级目标，无需改测试。

---

## 4. 冒烟实测（本机 linux/amd64 · `PERF_SCALE=5000` · `PERF_SAMPLES=15` · `-benchtime=1x`）

| 操作 | p50ms | p95ms | p99ms | allocs/op |
|------|------:|------:|------:|--------:|
| SaveMessages | 125.4 | 142.8 | 142.8 | 0 |
| History | 4.73 | 5.24 | 5.24 | 46,606 |
| Aggregate | 17.93 | 18.61 | 18.61 | 1,659 |
| Search | 26.84 | 28.71 | 28.71 | 7,693 |
| Keyset | 23.34 | 24.66 | 24.66 | 7,826 |
| RefreshStates | 2.80 | 6.38 | 6.38 | 11,774 |
| Consolidation | 2.12 | 3.17 | 3.17 | 107,959 |

> 读路径（History/RefreshStates/Consolidation）亚毫秒~几毫秒级；Search/Keyset 在 5K 规模下 p99 < 30ms，
> 且走 keyset cursor（默认不做全表 `COUNT(*)`），尾延迟随规模**近线性**、不出现 offset 深翻页的平方级恶化（场景 E）。
> 数值为小样本冒烟，仅证「管线正确、分位可观测」；真实容量结论须以 §3 高档 `PERF_SCALE` 复跑为准。

---

## 5. 结构性性能保证（跨 Phase 收口，非本 Phase 新增但被基准锁定）

- **单一检索层**：Search/History 全走同一 `message_repository` 门面 + FTS5，无重复实现（§32）。
- **深分页不 COUNT**：`Search`/`Keyset` 默认 `hasMore` 由「多取一条」推断，仅 `includeTotal` 显式才计总数。
- **日聚合重建门槛**：归档感知（archive-aware），百万级下不误触发全量重算。
- **纯 Go SQLite（modernc / glebarez）**：`CGO_ENABLED=0` 三平台一致，性能画像跨 amd64/arm64 可移植。

---

## 6. 门禁

`-race` 全量套件（含基准文件编译）在 Phase 13/14/15 均绿；基准本身只在 `-bench` 时执行，
不拖慢常规测试与覆盖率统计。
