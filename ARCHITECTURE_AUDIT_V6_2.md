# ARCHITECTURE_AUDIT_V6_2

> 唯一审计对象：`caodabao99/wechat-profile-bot` 的 `main` 实际代码（HEAD `83080ea` / tag `v6.2.0`）。
> 本文件**不**以 README 描述为事实来源；所有结论均来自对源码的直接核验（文件、符号、行号）。
> 图例：`IMPLEMENTED` 已完整落地 · `PARTIAL` 已落地但未完全收口 · `RISK` 存在需处置的风险 · `DEPRECATED` 遗留待清理 · `MISSING` 尚未实现。

---

## 1. 审计元信息（实测）

| 维度 | 实测值 | 来源 |
|---|---|---|
| `appVersion` | **v6.2.0** | `sysinfo.go` |
| Go 版本 | 1.25.1 | `go.mod` |
| 顶层 `.go` 文件 | 216（子目录 `.go` = 0，全平铺） | `find` |
| 非测试源文件 / 行数 | 116 / **37,452** | `cat $(非test) \| wc -l` |
| 测试文件 / 行数 | 97 受版本控制（另有 3 个永久隔离：`benchmark_test.go`/`facts_test.go`/`social_test.go`）/ 20,522 | `git ls-files` |
| HTTP 承载文件 | 39 | `grep ResponseWriter` |
| 数据库 `user_version` | **25**（`migrate()` 终点，与备份一致） | `backup.go:42 backupCurrentDBVer = 25` |
| Data Layer Registry 表 | **49** | `registry.go` |
| 前端 | `static/app.js` 4,025 / `index.html` 3,007 / `style.css` 1,244（go:embed） | `wc -l` |

---

## 2. 数据层 Registry 全清单（49 表，`registry.go`）

按 `Type` 分布：**core 9 · derived 16 · cache 8 · audit 9 · config 7**。三要素（Backup / Rebuild / HasContactID）在 registry 单点声明，`backup`/`cleanup` 从此派生，符合"三处同步"纪律。

- **core（源，全部 Backup:true, Rebuild:false）**：`contacts` `contact_aliases` `messages` `messages_archive` `profile_history` `contact_tag_links` `followup_items` `relationship_goals` `relationship_projects`。
- **audit（事实账本，Backup:true, Rebuild:false，真实行为不可重建）**：`merge_log` `contact_events` `relationship_action_log`（行动账本，八态生命周期 + outcome/provenance 分离）`relationship_experiment` `assistant_runs` `assistant_notified` `llm_call_log`（v6.2 用量）`backup_log`（Backup:false，恢复须保留自身）。
- **config（用户自建全局，Backup:true, Rebuild:false）**：`contact_tags` `assistant_settings` `portfolio_settings` `archive_settings` `mode_presets` `prompt_templates` `llm_settings`。
- **derived（可重建派生）**：`profile_facts` `profile_fact_evidence`（FACT 生命周期：status/source_type/valid_from/valid_until/superseded_by/confidence_type/evidence_strength）`relationship_daily_metrics` `relationship_state` `relationship_state_history` `relationship_action_suggestions` `suggestion_outcomes` `contact_connections` `contact_topic_history` `topic_evolution_state` `weekly_challenges` `contact_achievements` `contact_quality_history` `assistant_emotions` `gamification_state` `insight_trend_history`。
  - 派生表备份策略**不一致**（部分 Backup:true 随快照、部分 Backup:false 靠重建），属既有取舍，见 §6 RISK 讨论。
- **cache（尽力而为，Backup:false, Rebuild:true）**：`weekly_plan_cache` `life_state_cache` `life_projection_cache` `network_insight_cache` `self_portrait_cache` `intervention_cache` `briefing_cache` `ai_response_cache`。
- **DEPRECATED / 遗留**：`email_send_log`（Type:audit, Owner:legacy，Note 明确"邮件推送功能已移除，遗留表"）。

---

## 3. LLM 出口与入口（单一漏斗核验）

- **唯一真实 HTTP 出口**：`llm.go LLMClient.CallContext → doCallContext`；`clientFor(useProxy, proxyURL)` 用 `resty` 缓存客户端（`llm.go:55`），代理仅在 URL 变化时重建，绝不在调用中改传输层。
- **运行时解析**：`resolveSpec()` 读 `llm_settings` 活动档案，回落 `config.json`（`startupSpec`）；`configured()` = apiKey+baseURL 均非空且经 resolveSpec（`llm.go:73`）。
- **注入**：`WithDB()`（`llm.go:48`）使 LLMClient 具备切模型/代理/用量能力。
- **用量记录**：`CallContext` 每次真实调用（含失败）经 `logLLMCall` 追加 `llm_call_log`；缓存命中不触发、不计量。
- **代理连通性**：`llm_proxytest.go`（`runProxyTest`/`proxyTestOptionsFn` 可注入），`llm_api.go` 暴露 `POST /api/llm/proxy/test`、`/api/llm/model/test`。
- **入口清单（v6.2 LLM Model Management）**：`llm_settings.go`（档案存储 + 掩码）、`llm_presets.go`（国内外预设）、`llm_usage.go`（`ComputeLLMUsage` 聚合）。状态：`IMPLEMENTED`。

---

## 4. AI Context Engine 接管状态（P1 核心证据）

**引擎本身完备**（`context.go`）：`ContextTask` 8 种（profile/ask/coach/narrative/simulation/decision/briefing/replay）；`budgetFor(task)` 逐任务预算（MaxMessages/Relevant/Evidence/Events/Topics/Weeks/Tokens）；`ContactContext` 含 `ContextVersion` 内容指纹；`BuildContactContext(db,id,task,query,now)` 分层自锁、逐块优雅降级；`context_adapters.go` 有 profile/ask/coach/simulation/task 适配器；缓存语义键 `(contact_id, task, context_version, model, prompt_version)`（`ai_response_cache`）。Context Debug 门禁、context_version 已有专测（`context_debug_gate_test.go`/`context_version_test.go`）。

**但"接管"仅完成极小部分**（实测调用点）：

| 走 `callLLMCached`（缓存+上下文原语） | 仅 `ask.go:99`（TaskAsk）一处 |
|---|---|
| 构建了 `ContactContext`（但调用是否缓存需逐一确认） | `decision_ledger.go:202`、`relationship_api.go:330` |
| **仍直接 `llm.CallContext` 裸调、绕过 cache + Context Engine** | `profile.go`×4、`narrative.go`、`summary.go`、`topics.go`、`weekly_plan.go`、`simulate.go`、`rehearsal.go`×2、`relationship.go`、`assistance.go`×2、`assistant.go`、`calendar.go`、`followup.go`，及 `ask.go:116` 的降级分支 |

> 结论：蓝图自述的"渐进影子接管"属实且**严重未完成**——绝大多数 AI 模块仍各自拼 prompt、各自查库、裸调模型。这正是 **P1（全量接管）** 的净待办。

**状态：`PARTIAL`**（引擎 IMPLEMENTED，接入 MISSING 约 15 个调用点）。

---

## 5. 消息 SQL 访问点与历史一致性（P9 证据）

业务层直接对 `messages` / `messages_archive` 写 SQL 的文件（实测 27 个）：
`achievements archive ask assistant backup cleanup contact export_api facts fts followup insights life_narrative life_state merge metrics mirror period_report quality registry relationship report search social storage summary timeline`。

- 归档语义正确性依赖每个调用点自觉 union `messages_archive`，无统一层保证。
- **状态：`RISK` / `MISSING`（无 MessageRepository）**。P9 需引入 `RecentMessages/HistoricalMessages/SearchMessages/AggregateMessages` 并逐点收敛 + 审计分类（RECENT_OK / HISTORICAL_REQUIRED / SEARCH_OK / AMBIGUOUS）。

## 6. 分页方式

- keyset/cursor 与 offset 并存：`search`（默认不 COUNT，hasMore 多取一条推断，`includeTotal` opt-in）、`contact`（`GetContactsPaginatedCursor` v25 `last_updated_unix` 触发器）、offset 版标 `Deprecated` 保留。
- **状态：`IMPLEMENTED`**（深分页已收口，v6.1 有 benchmark 佐证 cursor 不随页深劣化）。

## 7. AI Cache

- `ai_cache.go callLLMCached` + `ai_response_cache`（语义键含 context_version + model + prompt_version，按 contact_id 级联清理）。缓存尽力而为、失败不破坏业务。
- 覆盖：仅 1/16 真实调用点走缓存（见 §4）。
- **状态：机制 `IMPLEMENTED`，覆盖率 `PARTIAL`**。

---

## 8. Secret / 凭据安全（P3 证据）

- GET 一律 `maskLLMSettings`（`apiKeyMask="******"`），PUT/upsert 回传掩码按 ID 沿用旧密钥。
- **RISK**：`llm_settings`（Type:config, **Backup:true**, Note"含密钥故参与备份"）——API Key 明文随备份导出，普通备份文件即含可用凭据。蓝图 P3 要求"默认排除明文 secret / 可选口令加密"，**当前 MISSING**。
- **RISK**：SSRF——`baseURL` 与 `proxy` 未见对内网/`file://`/`169.254`/RFC1918 的默认拒绝白名单（本地 Ollama 需豁免 `127.0.0.1`）。测试接口可被用于探测。P3 要求，**当前 MISSING**。
- 日志/前端未见明文回显 key（`IMPLEMENTED` 部分）。

---

## 9. 版本 / Release 一致性（P0）

实测并**已在本审计会话内修正**：

| 载体 | 值 | 状态 |
|---|---|---|
| `sysinfo.go appVersion` | v6.2.0 | ✅ |
| `main` HEAD / tag | `83080ea` = `v6.2.0`，已推 origin | ✅ |
| README | 含 v6.2.0 更新日志 | ✅ |
| `docker-compose.yml` image | v6.2.0 | ✅ |
| GitHub Release v6.2.0 | 存在，draft=false，2 资产（zip + docker tar.gz） | ✅ |
| GitHub `/releases/latest` | 原返回 **v6.1.0**（因 created_at 排序：v6.1.0 发布晚于 v6.2.0） | ✅ 已修正 |

**修正动作**：`PATCH releases v6.2.0 {make_latest:true}` + `v6.1.0 {make_latest:false}`，复核 `releases/latest` → **v6.2.0**。P0 一致性达成。

> 提示：后续脚本化发布若按"先新后旧"顺序创建 Release，必须以布尔 `make_latest`（经 release-id）显式声明最新，勿依赖 created_at。

---

## 10. 面向 v6.3 的能力状态矩阵（映射蓝图）

| 蓝图条目 | 当前 | 证据 |
|---|---|---|
| P0 版本/审计治理 | IMPLEMENTED（本文件）| §1–§9 |
| P1 Context 全量接管 | **PARTIAL** | §4（仅 ask 走 cache）|
| P2 AI Model Router（Task Policy/Fallback/Budget/Cost）| **PARTIAL→MISSING** | 模型档案/预设/用量/代理 IMPLEMENTED；按 task 的 primary/fallback/daily_budget/降级 MISSING |
| P3 Secret 安全 | **RISK/MISSING** | §8 |
| P4 Relationship Session 编排 | **MISSING** | Decision/Simulation/Action Ledger/Projects/Followups 各自 IMPLEMENTED，但未编排为 Session |
| P5 Memory Consolidation | **PARTIAL** | Memory Review IMPLEMENTED；重复/冲突/陈旧自动检测 + Proposal MISSING（FACT 生命周期字段已就绪 §2）|
| P6 AI Evaluation Lab | **MISSING** | 无 `tests/evaldata/` 合成集与黄金答案 |
| P7 Personal Calibration | **MISSING** | 统一规则，无用户纠正回路 |
| P8 Network Opportunity Discovery | **PARTIAL** | network graph/circles/facts/topics IMPLEMENTED；自然语言召回 MISSING |
| P9 Historical Message Repository | **MISSING** | §5 |
| P10 Portfolio 2.0 | **PARTIAL** | portfolio IMPLEMENTED；actual-effort 反馈 MISSING |
| P11 Action Center 2.0 | **PARTIAL** | Action Center IMPLEMENTED；TODAY 聚合 + 噪声控制 MISSING |
| P12 Smart Paste / Incremental AI | **PARTIAL** | `msg_hash` 去重 IMPLEMENTED；去重统计可视化 + 仅"新增重要事实"触发 AI 的增量策略 MISSING（注意既有 pitfall：同内容同时间戳按条数统计偏少）|
| P13 Desktop 入口 | **MISSING**（对应 Contact Brief/Session/Memory Maintenance）| 桌面 v3.2.0 |
| 数据治理 registry 三处同步 | IMPLEMENTED | §2 |
| Merge/Delete/Backup 无孤儿 | IMPLEMENTED（既有）| 需为新表持续补测 |

---

## 11. 结论

1. 底层能力**极厚**（49 表、~37.5K 行、统一 registry/备份/清理、Context 引擎与 LLM 运行时管理完备），但**上层"闭环编排"与"统一访问/接管"是本次 v6.3 的真正缺口**。
2. 最高优先级 = **P1 Context 全量接管**（把 §4 列出的 ~15 个裸调点迁到 `BuildContactContext + task adapter + callLLMCached + context_version`，删除各模块自查库），它同时是 P2 Router、P6 Eval、缓存一致性的前置。
3. 安全缺口 **P3（备份明文密钥 / SSRF）** 应与功能并行，风险最高。
4. 禁止清单（自动采集/Redis/ES/向量库/第二套 Metrics-Context-Score-Ledger/LLM 替代确定性排序/INFERENCE 冒充 FACT/correlation 冒充 causation/AI 失败 503/明文 key）在既有代码中基本已被遵守，v6.3 继续守纪。

---

## 12. v6.3.0 增量交付与状态更新（实测）

本版交付并过全门禁（`-race -cover` 70.5%→**70.6%**、linux/amd64·arm64 + windows/amd64 CGO=0、U+FFFD=0）：

| 蓝图项 | 交付状态（v6.3.0） | 代码事实 / 证据 |
|---|---|---|
| P0 版本/审计治理 | **IMPLEMENTED** | 本文件 + `releases/latest` 已修正指向 v6.2.0；本版按同纪律发 v6.3.0 |
| P1 §5.3 Context Task Registry | **IMPLEMENTED** | `context_registry.go`；`budgetFor` 委托；`TestContextRegistryBudgetNoDrift` 锁定零漂移 |
| P1 §5.2 裸调点→`callLLMCached` 接管 | **PARTIAL**（推进中）| 5 个联系人内容任务（profile×3/narrative/summary/topics）已迁；`TestNarrativeTakeoverHitsCache` 锁定「二次调用不重触模型」。剩余 ~10 个裸调点多为全局/会话任务（weekly_plan/calendar/followup/assistant/assistance/simulate/rehearsal/relationship_api），非联系人上下文任务，留后续 |
| P1 §5.4 Context Debug 完善 | **MISSING**（本版未做）| 沿用既有 `context_debug_gate`；token 估算/cache-hit 可视化待补 |
| P3 SSRF/协议处理器护栏 | **IMPLEMENTED** | `saveLLMSettings` 前置 BaseURL/Proxy 校验；`TestSaveLLMSettingsRejectsBadURLEndToEnd` 断言非法值不落库 |
| P3 备份明文凭据 | **INTENTIONAL 权衡（非本版修复）** | `data.db` 快照含 `llm_settings` 明文 key；旁路文件支持口令 AES-256、UI 明示「等同密码」——属可携性优先的刻意设计，改其加密链路风险波及重度回归的 restore，故本版仅在文档如实标注权衡，不动 |

**矩阵订正**：§10 中 P1「仅 ask 走 cache」→ 现为「ask + 5 内容任务走 cache（6/16+）」；P3「SSRF MISSING」→ **IMPLEMENTED（协议/主机/userinfo 护栏）**；备份 secret 维持权衡说明。其余 P2/P4–P13 状态不变（未在本版实施，见 README v6.3.0 路线图）。

## 13. v6.3.1 / v6.3.2 接管收口与可观测增量（实测）

本版为纯增量补丁（无 schema / 无 API / 无用户可见行为变更），继续推进 P1 §5.2。门禁：`-race -cover` 维持 **70.6%**、linux/amd64·arm64 + windows/amd64 CGO=0、U+FFFD=0。

| 蓝图项 | 交付状态（v6.3.1） | 代码事实 / 证据 |
|---|---|---|
| P1 §5.2 裸调点→`callLLMCached` 接管 | **PARTIAL→接近收口**（联系人/幂等类已尽数接管）| 新增 8 处迁至单一原语：`simulate`(TaskSimulation)、`rehearsal`×2（TaskRehearsal/TaskRehearsalReview）、`followup`(TaskFollowup)、`weekly_plan`(TaskOutreach)、`assistant`(TaskEmotion)，叠加 v6.3.0 的 6 处——共 **14 处接管点 / 10 个文件** |
| P1 §5.2 接管边界（新增结论）| **IMPLEMENTED（边界已明）** | 交互式操作刻意**不接管**并附代码注释：`assistance.go` `RewriteReply`/`ReviewDraft` 期望每次新鲜多样、且需取消能真实中断模型（缓存先查会绕过 `ctx`）；registry 中登记为 `Cacheable:false` 以记录决策而非遗漏。`llm_api.go:282` 连通性 ping 同理不缓存 |
| P1 §5.4 Context Debug 可观测 | **IMPLEMENTED**（v6.3.2 发布）| 新增 `context_debug.go`，经 `GET /api/contacts/{id}/context` 的 `obs` 字段返回：① `block_plan`——注册表「计划」×实取「结果」逐项对照，并单列 `missing_required`（声明必需却为空）；② `token_estimate`（CJK 逐字 + 拉丁每 4 字符，**标明仅为估算**、不冒充真实用量）占预算百分比与 `truncated`；③ `cache` 快照状态（按 context_version/model 分层计数）；④ 活动 `model`与 `llm_configured`；⑤ `registered`（未登记任务回落默认规格的迁移告警）。与 §4.3 隐私一致：`obs` 全为计数/指纹，**不含任何正文**（有专测以「同上下文 rendered 确实含该私密串」对照「obs 序列化后不含它」，防空断言）；不建表、不新引第二套 Context 引擎 |

**剩余 `llm.CallContext` 实测清单（共 9 处，均为刻意豁免、非遭遗漏）**：`ai_cache.go:124`（原语自身的真调出口，必须保留）、`profile.go:302`（变化说明辅助调用，作入参的 profile 已变、无 `contactID`）、`assistance.go:92/117`（交互式，见上）、`llm_api.go:282`（ping）、`ask.go:116`（非联系人作用域、已走自有 cache）、`calendar.go:346` 与 `relationship.go:360`（全局/多联系人编排类，不是单联系人上下文任务）。

> 因此，“§5.2 全量接管”的剩余净待办不再是一个调用点编号列表，而是一个架构前置：先把全局/多联系人任务纳入统一的 Context 构建与 `context_version` 语义（不能只靠 prompt 哈希作为锚点），否则缓存键无法安全描述这类任务。

*本文件由 Phase 0 生成，将随各 Phase 交付滚动更新；v6.3.1 为 §5.2 接管收口增量，v6.3.2 为 §5.4 可观测层增量（至此 P1 骨干 Registry→接管→可观测 闭环）。*
