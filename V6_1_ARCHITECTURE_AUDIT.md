# V6.1 Architecture Audit —— Relationship Intelligence Loop

> 本审计基于 **GitHub main 真实代码**（`wechat-profile-bot HEAD=9080e35 / tag v6.0.0 / appVersion=v6.0.0`；`wechat-profile tag v3.1.0`）逐文件读取得出，**不沿用旧版本假设**。
> 目标：把 v6.0 已存在的节点连成闭环 `Memory → State → Decision → Action → Outcome → Learning → Memory`。

## 0. 基线核实（真实代码，非蓝图假设）

| 事实 | 核实结果 |
|---|---|
| Server Release | v6.0.0（`sysinfo.go:13 appVersion="v6.0.0"`） |
| Desktop Release | v3.1.0（`wechat-profile` tag，含关系状态/重新认识TA/今天值得做三面板，**仅远程模式**） |
| origin/main vs HEAD | 0/0（已同步，已发布） |
| AI Context Engine | `context.go` **已存在且分层完备**（8 类 task + 全认知层 + 分任务预算）——但**仅被 debug 端点调用** |
| 事实证据 | `facts.go` **已有** `match_type/support_strength/quote/is_direct_support/confidence_type/source_type` 与用户确认保护——比蓝图假设更先进 |
| 行动结果 | `suggestion_outcomes` **已有** improved/stable/worsened 14 天回测 + `intervention_cache`——比蓝图假设更先进 |

**结论**：v6.0 把「节点」都建好了（State/Decision/Projects/Context/Facts/Replay/Outcomes），但**没有连成环**：Decision 只出建议、不落到 Action；Action 不记 Outcome；Outcome 不反哺 Memory/Context；各 AI 模块各拼各的上下文。v6.1 的价值是**收口与连接**，不是新建平行系统。

## 1. 十项检查结论速览

| # | 检查项 | 结论 | 证据 |
|---|---|---|---|
| 1 | v6.0 实现了什么 | 节点齐全：状态机/决策/项目/上下文/事实证据/回放/回测 | registry.go、context.go |
| 2 | 只有 API 没闭环 | **Decision→Action→Outcome 断链**；`/context` 只读不接线 | decision.go 仅产出、无生命周期 |
| 3 | 旧模块自行组装 context | **是**：profile/ask/coach/narrative/simulate/briefing 全绕过 BuildContactContext | 见 §2 |
| 4 | 统计未 archive-aware | **是**：metrics.go / relationship.go 累计统计 `FROM messages` 漏归档 | 见 §5 |
| 5 | 分页仍 OFFSET | **是**：search 双路径仍 OFFSET 兼容；getContactsPage 用 `strftime+OFFSET` | 见 §6 |
| 6 | SQL 存在 COUNT(*) 全量 | **部分**：datareport/achievements/relationship 全表 COUNT 在热路径 | 见 §6 |
| 7 | 缓存无失效定义 | **是**：intervention_cache/derived 表有自愈，但**无 context_version/AI cache** | context.go 无版本字段 |
| 8 | AI 输出无来源 | **部分**：facts 有 source_type，但 coach/ask/narrative 文本层未强制 FACT/INFERENCE 标签 | 见 §2 |
| 9 | AI facts 证据不足 | **是**：核心仍 `content LIKE` 命中即升 confidence；缺 strong/weak/topic 分级 | facts.go:188 |
| 10 | action 无 outcome | **是**：接受/稍后/忽略/完成无记录点；回测只覆盖历史 suggestions | 见 §3 |

---

## 2. P0 —— AI Context Engine 全面接管（Phase 1）

`BuildContactContext()` 已具备 Profile/Ask/Coach/Narrative/Simulation/Decision/Briefing/Replay 全部分层，但调用方**只有** `relationship_api.go:319`（debug 端点）。真实 AI 入口全部自行拼上下文：

| file | function | problem | severity | proposed fix |
|---|---|---|---|---|
| profile.go | `GenerateOrUpdateProfile` (211) | 直接读 `profile_json` + 传入 `messages[]`，自行拼 `profile_update` prompt，绕过统一认知快照 | High | 加 `buildProfileContext()` 适配器→`BuildContactContext(TaskProfile)`；RenderPrompt 变量由快照渲染，保持输出结构不变 |
| ask.go | `AskContactHistory` (49) | `ask.go:179` 直接 `SELECT ... FROM messages` 取相关消息自建上下文 | High | `buildAskContext()`→`BuildContactContext(TaskAsk, query)` |
| coach.go | `BuildCoach` (156) | 全局教练自行组装，未走 context | High | `buildCoachContext()`→`BuildContactContext(TaskCoach)`（联系人维度）|
| narrative.go | `GenerateNarrative` (193) | 自行拉 topics/messages | Medium | `buildSimulationContext/narrative` 复用快照 |
| simulate.go | `SimulateReply` (37) | 接收外部 `recent[]` 自行拼 | Medium | 走 `BuildContactContext(TaskSimulation)` |
| briefing.go | `GenerateBriefing` (81) | 自行聚合 | Medium | 走 `BuildContactContext(TaskBriefing)` |
| context.go | `ContactContext` (72) | **无 `context_version`、无 AI cache、无 prompt_version** | **Blocker**（缓存复用前置） | 增 `ContextVersion int64`（由 profile/fact/state/goal/project/followup/new-msg 变更驱动）；新增 `context_ai_cache` 派生表，key=(contact_id,task,context_version,model,prompt_version) 命中才复用 |
| relationship_api.go | `routeContactContext` (328) | **无条件返回 `rendered`**，生产暴露大量私人聊天+推理上下文 | **High（隐私）** | 加 `debug` 开关（config，默认 false）：关→仅返回结构化摘要（去 rendered），开→才回 rendered |

**铁律**：只加适配器，**不重写 Prompt、不改旧 API 输出结构**（§4.1）。新 prompt 一律 `RenderPrompt`，禁 `fmt.Sprintf`（§16）。

---

## 3. P1/P2 —— Action Ledger 与 Decision→Action→Outcome 闭环（Phase 2、3）

现状：`relationship_action_suggestions` + `suggestion_outcomes`（improved/stable/worsened，14 天回测）+ `intervention.go` 学习已存在。缺的是**统一生命周期**与**用户操作落点**。

| file | function/table | problem | severity | proposed fix |
|---|---|---|---|---|
| decision.go | `DecisionCandidate` (103) / `TodayDecisions` | 只出「谁/为什么/做什么」，**无 id、无 fingerprint、无状态机**，无法被接受/稍后/忽略 | High | 决策项产出 `decision_id` + `decision_fingerprint(contact+action_type+reason_codes+context_version+time_window)`；相同 fingerprint 不重复生成（§6.1）|
| schema | `suggestion_outcomes` | 可承载 acted/completed，但**无 generated/viewed/accepted/deferred/dismissed/expired 生命周期列** | High | **优先扩展现有表**（加 `lifecycle`/`deferred_until`/`dismiss_reason`/`source`/`acted_at`），**不新建第二套**（§5.1）；确实无法承载才建 `relationship_action_log` |
| intervention.go | outcomes | outcome 值域 improved/stable/worsened，**未区分「用户确认 confirmed」vs「系统估算 estimated」** | High | 结果模型显式分 `confirmed`（用户）/`estimated`（系统观察 7/14/30 天，标 estimated，**绝不声称因果**）（§5.3/5.4）|
| api | 决策 today 路由 | 无 `[接受][稍后][忽略][已完成]` 写端点 | Medium | 新增 `POST /api/decision/{id}/lifecycle {action}`；deferred 存 `deferred_until` 到期重回候选（§6.2）；dismiss 记 reason 但**不永久屏蔽**（§6.3）|

**复用铁律**：suggestions/outcomes/intervention **不重做**（§5）。

---

## 4. P3 —— Evidence Provenance 2.0（Phase 4）

`facts.go` 已有证据评估（match_type/support_strength/is_direct_support）与 `source_type='user'` 权威保护——**但核心检索仍是 `content LIKE`**，命中即影响 confidence，且证据类型未细化到蓝图要求的六档。

| file | function | problem | severity | proposed fix |
|---|---|---|---|---|
| facts.go | 证据匹配 (188) | `WHERE content LIKE ?` 参数化关键词命中即作证据；**纯关键词命中可抬高 confidence** | High | 引入显式 `evidence_type ∈ {direct, strong_context, weak_context, topic_related, conflict, insufficient}`；**纯关键词只允许 topic_related，禁止提升 fact confidence**（§7.4）|
| facts.go | `deriveFacts` | 「我准备换公司」这类只能 strong_context，却可能直接建「职业=某公司」 | High | direct 仅限明确第一人称陈述（§7.2）；语境推断降级为 strong_context，不建确定事实 |
| facts.go | 冲突 (127) | 已有 superseded/retired，但**时间关系不明时未落 conflict** | Medium | 新事实与旧值冲突且时序不可判 → `status=conflict`（§7.5）|

---

## 5. P6 —— Archive-aware 一致性（Phase 7）

蓝图 §10 要求逐个判断 Recent vs Historical，**不机械全改**。真实漏点：

| file | function/位置 | problem | severity | proposed fix |
|---|---|---|---|---|
| metrics.go:55 | `src := SELECT ... FROM messages` | 日粒度聚合仅 active，漏 `messages_archive` | **High** | Historical 聚合改 `messages UNION ALL messages_archive` |
| relationship.go:106 | `COUNT(*) FROM messages WHERE contact_id=?` | **累计互动/总消息数漏归档** | **High** | 累计类走 UNION archive |
| relationship.go:292/296/307/310 | recent/mine 计数 | 多为 Recent window（30/90 天）——**允许只看 active** | Low | 逐个判定；确为历史累计才并入 archive |
| facts.go:188 | 证据检索 `FROM ... messages` | 事实证据属 Historical（历史原话支撑），需含归档消息 | Medium | 证据检索 UNION archive（并保留 archived 标记）|

**每个历史统计补三阶段一致性测试**：archive 前 / archive 后 / restore 后（§10.3）。

---

## 6. P7 —— Search / Contacts 深分页第二阶段（Phase 8）

v6.0 已有 keyset 基础（search 支持 Cursor，Phase 10 已证明 keyset 不随页深退化）。遗留：

| file | function | problem | severity | proposed fix |
|---|---|---|---|---|
| storage.go:1040 | `getContactsPage` | `ORDER BY strftime('%s',c.last_updated) DESC ... LIMIT ? OFFSET ?`——**正是 §11.3 点名反模式** | High | 改 `ORDER BY last_updated_unix, id` cursor/beforeID；禁 strftime |
| search.go:283,440 | 分页 SQL | 仍保留 `LIMIT ? OFFSET ?` 兼容路径 | Medium | 保留但 **deprecated**；新 Web UI 禁调 offset（§11.4）|
| search.go | `searchMaxOffset=10000` + 结果总数 | 默认是否执行 `COUNT(*)` 待核 | Medium | 默认 `includeTotal=false`，返回 `hasMore/nextCursor`；仅显式要求才 COUNT（§11.1）|
| search.go | Cursor | cursor 仅内部用，客户端可传 offset | Low | cursor 结构固定 `(msg_unix,id,archived)`，**禁客户端传 SQL order fields**（§11.2）|

---

## 7. 纯新增能力（P4/P5/P8/P9 —— 无现有实现，Phase 5/6/9/10）

grep 确认：**memory-review / risk / portfolio / experiment 端点与表均不存在**，属净新增，但必须**聚合现有信号**、**不新建平行 score**、**核心用户数据进 registry**。

| 能力 | 复用来源 | 新增 | registry/cleanup |
|---|---|---|---|
| P4 Memory Review | facts（confidence/证据弱/冲突/未确认/过时）| `memory_review_queue`（或复用 facts 视图 + Top3/系统≤10）；确认→`source_type=user,confidence=1`；否定→`status=rejected`；确认后 AI 不得自动降级只可提 conflict | 派生入 registry；contact scoped |
| P5 Experiment | intervention.go 学习 | `relationship_experiments`（contact/goal/duration/strategy/metrics/status）；结论只输出 observed improvement/no clear change/negative/insufficient，**禁因果表述** | 用户创建→core、Backup=true、HasContactID、进 cleanup+merge |
| P8 Portfolio | state/health/goals/projects/risk/centrality/outcome | 周时间预算 + 建议分配；**只建议不代决定** | config（预算）+ 派生看板 |
| P9 Risk Center | health/state/cooling/followup/project/goal/date/fact conflict | 聚合视图（**不新建 score**）+ 风险类型枚举 + 解释（数据来源/最近变化/涉及人/建议）| 派生、可重建 |

---

## 8. 横切铁律（每 Phase 遵守）

- **单一版本源**：Server `sysinfo.go appVersion`→v6.1.0；Desktop `main.go appVersion`→**仅在桌面真正加 §15 三入口后**才 v3.1.0→v3.2.0；README/Release/Docker/UI/API 自动同步。
- **派生/懒建表治理三处同步**：registry.go（Type/Owner/Backup/Rebuild/HasContactID）+ backup.derivedTables（仅纯派生）+ cleanup.contactCleanupTables（含 contact_id 的用户数据）。删级联 / 合迁移 / 撤销还原。
- **性能**：SQLite/WAL/单连接池；**禁 Redis/ES/VectorDB**；不为 AI 加新库。
- **AI 成本**：确定性候选→Context Engine→Top N→LLM；Decision 默认不需 LLM；**绝不刷新首页=给每人调 LLM**。
- **自锁 reader**：顺序调用绝不嵌套 dbMu。
- **输出分层**：FACT/INFERENCE/SUGGESTION/SIMULATION/OBSERVATION 显式标签，AI 推测不得显示成事实。
- **确定性优先、LLM 双重降级永不 503、宁可 Unknown 不猜成事实**。

---

## 9. Phase 实施顺序与验收映射

| Phase | 内容 | 关键验收 |
|---|---|---|
| 0 | 本审计 | V6_1_ARCHITECTURE_AUDIT.md ✅ |
| 1 | Context 接管 + context_version + cache + debug 门 | context adoption/version/invalidation 测试 |
| 2 | Action Ledger（扩展 outcomes 承载生命周期）| action ledger 测试 |
| 3 | Decision lifecycle + fingerprint 防重 + deferred/dismissed | lifecycle/fingerprint 测试 |
| 4 | Evidence Provenance 2.0（evidence_type 六档）| provenance/conflict 测试 |
| 5 | Memory Review 队列 | memory review 测试 |
| 6 | Experiment Learning（观察性）| experiment 测试 |
| 7 | Archive correctness（逐个 + 三阶段）| archive before/after/restore 测试 |
| 8 | Search/Contact cursor + includeTotal | search/contact cursor 测试 |
| 9 | Risk Center（聚合）| risk aggregation 测试 |
| 10 | Portfolio 时间预算 | portfolio ranking 测试 |
| 11 | Action Center UI（Today/Memory/Risk/Action/Portfolio）| — |
| 12 | Desktop v3.2 三入口（State/Today Action/Memory Review）| 仅真加才升版本 |
| 13-15 | 回归 / Benchmark(100K–10M,P50/95/99) / 文档发布 v6.1.0 | go test -race -cover 不降 |

> 备注：多处代码（context 分层、facts 证据列、suggestion_outcomes 回测）**比蓝图假设更完备**，实施时以「扩展复用」为准，严禁重复造平行系统。
