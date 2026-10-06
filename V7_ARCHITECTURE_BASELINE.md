# V7 Architecture Baseline — Server v6.3.3 / Desktop v3.2.0

> 生成时间：2026-10-06  
> 方法：源码实测（非 README 推断）

---

## 1. 版本与运行时

| 项目 | 值 |
|------|----|
| Server 版本 | v6.3.3 (`sysinfo.go:13`) |
| Desktop 版本 | v3.2.0 (`main.go:20`) |
| Go 版本 | 1.25.1 (`go.mod:3`) |
| SQLite user_version | 25 (`backup.go:42 backupCurrentDBVer`) |
| 非测试 .go 文件 | 124 |
| 非测试代码行数 | ~61,729 |
| Desktop .go 文件 | 23 / ~8,375 行 |
| API handler 函数总数 | 177 |
| Registry 登记表数 | 45 |

---

## 2. 数据表分类 (registry.go)

| 类型 | 数量 | 代表表 |
|------|------|--------|
| core | 11 | contacts, messages, messages_archive, profile_history, contact_aliases, contact_tag_links, followup_items, relationship_goals, relationship_projects, contact_events |
| derived | 14 | profile_facts, profile_fact_evidence, relationship_daily_metrics, relationship_state, relationship_state_history, relationship_action_suggestions, suggestion_outcomes, contact_connections, contact_topic_history, topic_evolution_state, insight_trend_history, weekly_challenges, contact_achievements, contact_quality_history, assistant_emotions, gamification_state |
| cache | 10 | life_state_cache, life_projection_cache, network_insight_cache, self_portrait_cache, intervention_cache, briefing_cache, weekly_plan_cache, today_snooze, ingest_stats, ai_response_cache |
| config | 6 | assistant_settings, portfolio_settings, archive_settings, mode_presets, prompt_templates, llm_settings, contact_tags |
| audit | 8 | merge_log, backup_log, assistant_runs, assistant_notified, email_send_log, llm_call_log, relationship_action_log, relationship_experiment |

**Backup 范围**：core + config + audit (Backup:true) + 部分 derived (weekly_challenges/achievements/quality/emotions/gamification/contact_topic_history/topic_evolution_state)  
**不入备份**（restoreSkipTables）：profile_facts, profile_fact_evidence, relationship_daily_metrics, relationship_state, relationship_state_history, relationship_action_suggestions, suggestion_outcomes, contact_connections, 所有 cache, insight_trend_history

---

## 3. AI / LLM 入口全清单

### 3.1 低层调用核

| 函数 | 文件 | 作用 |
|------|------|------|
| `LLMClient.CallContext(ctx, prompt)` | llm.go:119 | 唯一 HTTP 出口（OpenAI-compatible） |
| `LLMClient.Call(prompt)` | llm.go:100 | 无 context 的快捷路径 |
| `callLLMCached(ctx, db, llm, contactID, task, contextVersion, prompt)` | ai_cache.go:106 | 带缓存 + 预算熔断 + task 归因 |

### 3.2 业务入口（使用 callLLMCached — 经缓存/预算/归因）

| Task | 调用方文件 | Context 接管？ |
|------|-----------|---------------|
| profile | profile.go:266, 349 | 是（buildProfileContext） |
| intent | profile.go:396 | 否（自建 prompt） |
| ask | ask.go:99 | 部分（buildAskContext 取 context，但 retrieve 独立查消息） |
| coach | — | 否（直接 llm.CallContext 不走 task） |
| narrative | narrative.go:267 | 是（对应 TaskNarrative 在 registry） |
| simulation | simulate.go:82 | 是（buildSimulationContext） |
| decision | decision_ledger.go:202 | 是（BuildContactContext） |
| briefing | — | 否（确定性构建，不调 LLM） |
| replay | — | 是（TaskReplay registry，但调 LLM 路径不在此文件） |
| summary | summary.go:129 | 是 |
| emotion | assistant.go:634 | 否（自建 prompt） |
| followup | followup.go:371 | 否（自建 prompt） |
| topic | topics.go:269 | 否（自建 prompt） |
| outreach | weekly_plan.go:357 | 否（自建 prompt） |
| rehearsal | rehearsal.go:269, 335 | 否（自建 prompt） |

### 3.3 裸调 `llm.CallContext` 绕过缓存/预算

| 文件 | 行号 | 场景 |
|------|------|------|
| ask.go | 116 | 关键词提取（子步骤，非终态输出） |
| assistance.go | 92, 117 | 改写/检查（交互式，设计决策 §5.2 刻意不接管） |
| calendar.go | 346 | 日程解析 |
| profile.go | 302 | summarizeProfileChange（辅助） |
| relationship.go | 360 | fillDrafts（建议草稿生成） |
| llm_api.go | 317 | model test (ping) |

### 3.4 Context Engine 当前状态

- `BuildContactContext` 统一构造已实现 (context.go:132)
- `contextTaskRegistry` 已注册 **14 个 task** (context_registry.go)
- `RenderContextText` 已实现 (context.go:489)
- `context_version` / `computeContextVersion` 已实现 (context.go:361)
- **但实际只有 4 个 adapter 被调用方使用** (profile/ask/coach/simulation)
- 多数 task 虽注册了 spec，调用方仍自建 prompt，不走 BuildContactContext
- **Context 接管率**：4/14 业务入口 = **~29%**（仅 profile/ask/simulation/decision 真正经 Context Engine）

### 3.5 预算/归因当前状态

| 能力 | 状态 | 文件 |
|------|------|------|
| llm_call_log（每次调用一行） | IMPLEMENTED | llm_usage.go |
| ComputeLLMUsage（日/周/月 + 分模型 + 分任务） | IMPLEMENTED | llm_usage.go |
| llmBudgetStatus（日 token 上限 + 今日已用） | IMPLEMENTED | llm_budget.go |
| llmBudgetAllows（熔断） | IMPLEMENTED | llm_budget.go |
| 月/周预算 | **MISSING** | 仅 daily |
| per-contact 归因 | **MISSING** | llm_call_log 无 contact_id 列 |
| 80% 预警 | **MISSING** | 仅 100% 硬熔断 |
| Model Router (per-task 模型策略) | **MISSING** | 全局唯一 active profile |

---

## 4. 数据访问 — messages / messages_archive

### 4.1 统一历史层 (P9)

- `history_source.go` 已实现：`HistoryMessagesLocked` / `HistoryCountLocked` / `HistoryMessages` / `HistoryCount` / `HistoryMessagesPlain`
- `historyTimeExpr = COALESCE(msg_unix, CAST(strftime('%s', msg_time) AS INTEGER))`
- 已迁移文件（使用 History* 系列函数）：insights.go, life_state.go, period_report.go, quality.go, report.go, summary.go

### 4.2 仍直接 `FROM messages` 的文件（棘轮基线 36 处 / 15 文件）

| 文件 | 出现次数 | 场景判定 |
|------|----------|----------|
| storage.go | 7 | SaveMessages/GetRecent/GetAll/GetPage/GetPageBefore — Recent Window 语义 |
| relationship.go | 5 | 互动聚合/趋势/建议草稿 — Historical 需补 archive |
| merge.go | 5 | 合并时迁移/统计 — Recent + Historical 混合 |
| assistant.go | 4 | 调度器情绪/摘要 — Recent Window |
| archive.go | 4 | 归档操作本身 (INSERT INTO archive SELECT FROM messages) — 特殊 |
| metrics.go | 3 | 日指标重算 — Historical |
| life_narrative.go | 3 | 人生叙事 — Historical |
| timeline.go | 2 | 时间线事件 — Historical |
| social.go | 2 | 社交图谱边权 — Historical |
| mirror.go | 2 | 自我画像 — Historical |
| achievements.go | 1 | 成就计数 — Historical |
| ask.go | 1 | 相关消息召回 — Historical (via SearchMessages) |
| contact.go | 1 | 联系人列表消息数 — Historical |
| followup.go | 1 | 跟进建议上下文 — Recent Window |
| search.go | 1 | FTS/LIKE 搜索主表 — Recent Window（归档走另一臂） |

### 4.3 Search / Pagination

| 能力 | 文件 | 状态 |
|------|------|------|
| FTS5 (messages_fts / messages_archive_fts) | fts.go, search.go | IMPLEMENTED |
| SearchMessages (FTS→LIKE 降级) | search.go:184 | IMPLEMENTED |
| Keyset pagination (contacts) | storage.go:1298 GetContactsPageCursor | IMPLEMENTED |
| Offset pagination (messages) | storage.go:1048 GetMessagesPage | IMPLEMENTED |
| Before-cursor (messages) | storage.go:1082 GetMessagesPageBefore | IMPLEMENTED |

---

## 5. 业务模块状态评估

| 模块 | 状态 | 说明 |
|------|------|------|
| Smart Paste (ingest) | IMPLEMENTED | parser.go + bot.go + hIngest (api.go:1043) + RecordIngestStat |
| FTS5 搜索 | IMPLEMENTED | search.go + fts.go |
| 历史统一访问层 | PARTIAL | history_source.go 仅 6 文件已迁移，36 处仍直查 |
| msg_unix 时间戳 | IMPLEMENTED | migrate v22/v23 backfill |
| Unified Metrics | IMPLEMENTED | metrics.go + relationship_daily_metrics |
| Data Layer Registry | IMPLEMENTED | registry.go 45 表 |
| Temporal Facts (生命周期) | IMPLEMENTED | facts.go: status/valid_from/valid_until/superseded_by |
| Evidence Provenance | IMPLEMENTED | evidence_provenance.go + profile_fact_evidence |
| Memory Review | IMPLEMENTED | memory_review.go: BuildMemoryReviewQueue |
| Memory Consolidation | IMPLEMENTED | consolidation.go: conflict/stale/duplicate |
| Relationship State | IMPLEMENTED | statemachine.go: base×dynamic |
| Relationship Health | IMPLEMENTED | health.go: ComputeHealth |
| Decision Engine | IMPLEMENTED | decision.go: BuildDecisionCandidates + scoreDecision |
| Action Ledger | IMPLEMENTED | action_log.go: 8 态生命周期 |
| Decision→Action→Outcome | IMPLEMENTED | decision_ledger.go: 候选幂等落 action_log |
| Relationship Experiment | IMPLEMENTED | experiment.go + experiment_measure.go |
| Risk Center | IMPLEMENTED | risk.go: BuildRisks (5 维度) |
| Relationship Portfolio | IMPLEMENTED | portfolio.go: ComputePortfolio + tier allocation |
| Action Center 2.0 (Today) | IMPLEMENTED | today_aggregate.go: 四源聚合 |
| Relationship Projects | IMPLEMENTED | projects.go |
| Memory Replay | IMPLEMENTED | replay.go: BuildRelationshipReplay |
| AI Context Engine | PARTIAL | 框架完备但接管率仅 29% |
| AI Cache | IMPLEMENTED | ai_cache.go: (contact_id,task,context_version,model,prompt_version) |
| Prompt Registry | IMPLEMENTED | prompts.go: 模板 + override |
| LLM Model Management | IMPLEMENTED | llm_settings.go: 多 profile 增删改查 |
| LLM Model Presets | IMPLEMENTED | llm_presets.go |
| LLM Usage | IMPLEMENTED | llm_usage.go: ComputeLLMUsage |
| LLM Cost Attribution | PARTIAL | 有 task/model/window，缺 contact 维度 |
| LLM Budget | PARTIAL | 仅 daily token 上限 |
| Proxy / Connectivity Test | IMPLEMENTED | llm_proxytest.go + llm_api.go:269,311 |
| Ask TA | IMPLEMENTED | ask.go: AskContactHistory |
| Coach | IMPLEMENTED | coach.go: BuildCoach |
| Conversation Simulation | IMPLEMENTED | simulate.go: SimulateReply |
| Narrative | IMPLEMENTED | narrative.go: GenerateNarrative |
| Briefing | IMPLEMENTED | briefing.go: GenerateBriefing |
| Network / Circles | IMPLEMENTED | network.go + circles.go |
| Life State / Forecast / Timeline | IMPLEMENTED | life_state.go + life_projection.go + life_narrative.go |
| Backup / Restore | IMPLEMENTED | backup.go: 全链路含 FTS rebuild |
| Merge / Undo / Delete | IMPLEMENTED | merge.go + cleanup.go |
| Desktop Local/Remote | IMPLEMENTED | data_adapter.go: isRemoteMode 切换 |

### 缺失模块 (v7.0 新需求)

| 需求 | 状态 |
|------|------|
| AI Model Router (per-task 策略) | **MISSING** |
| Relationship Session (编排闭环) | **MISSING** |
| Strategy Learning (per-type 有效性统计) | **MISSING** (intervention.go 有雏形) |
| Personal Calibration | **MISSING** |
| Contact Brief (联系人级简报) | **MISSING** |
| Network Opportunity Discovery | **MISSING** |
| Data Health 统一面板 | **MISSING** |
| AI Evaluation Lab | **MISSING** |
| Token/Cost per-contact | **MISSING** |
| Weekly/Monthly budget | **MISSING** |
| Cache observability (hit/miss/invalidate) | **MISSING** |
| State reason (解释为何 cooling) | PARTIAL (statemachine.go:stateReason 有基础) |
| Hysteresis (防状态抖动) | **MISSING** |
| Data Sufficiency gate (insufficient_data) | **MISSING** |
| Desktop: Contact Brief / Session / Memory | **MISSING** |

---

## 6. Context Block 全清单

已注册 15 个 ContextBlock：
- identity, facts, evidence, relationship_state, timeline, goals, projects, followups, topics, metrics, recent_messages, relevant_messages, previous_actions, previous_outcomes, action_log

已注册 14 个 ContextTask（context_registry.go）：
profile, ask, coach, narrative, simulation, decision, briefing, replay, memory_review, topic, experiment, summary, emotion, intent

> 注：followup/outreach/rehearsal/rehearsal_review 有独立 callLLMCached 调用但**未在 registry 注册 spec**。

---

## 7. Desktop 架构

| 项目 | 值 |
|------|----|
| 版本 | v3.2.0 |
| Go 文件 | 23 个 (含测试 2) |
| 行数 | ~8,375 |
| 函数数 | 238 |
| 核心模式 | data_adapter.go 根据 isRemoteMode 切换本地 SQLite / 远程 API |
| 远程调用 | remote.go → RemoteClient (HTTP + Token) |
| UI | ui.go (Fyne 框架) + ui_*.go 分功能页 |
| 本地能力 | contacts/messages/profile/merge/backup/assistance |
| 缺失 | Contact Brief / Relationship Session / Memory Maintenance / Today 首页 |

**local/remote 分叉点** (data_adapter.go)：GetContacts / GetContact / GetMessages / GetHistory / GetStats / GetContactProfile / GetMergeLogs / MergeContacts / UndoMerge / EditProfile / SupplementProfile / RegenerateProfile / SetRemark / DeleteContact / RewriteReply / ReviewDraft / ProfileChanges / AnalyzeIntent / ResolveContact

---

## 8. 安全与认证

| 能力 | 文件 | 状态 |
|------|------|------|
| API Token 鉴权 | auth_api.go + security.go | IMPLEMENTED |
| Rate Limiter | security.go:319 | IMPLEMENTED |
| IP Ban | security.go:61 | IMPLEMENTED |
| 2FA (TOTP) | twofa.go | IMPLEMENTED |
| CORS | api.go | IMPLEMENTED (configurable) |
| Path Traversal 防护 | api.go route() | IMPLEMENTED |
| API Key 掩码 | llm_api.go:19 maskLLMSettings | IMPLEMENTED |
| Backup 排除敏感文件 | backup.go | IMPLEMENTED |
| SQL Injection 防护 | 全参数化查询 | IMPLEMENTED |
| FTS Query Injection | search.go:escapeLike | IMPLEMENTED |
| SSRF 防护 | llm_proxytest.go | PARTIAL (仅校验 URL scheme) |

---

## 9. 测试与覆盖率

| 指标 | 值 |
|------|----|
| 测试文件 | ~55 个 _test.go |
| 覆盖率 | 71.2% |
| Race 检测 | -race 全绿 |
| Benchmark | 5 个 (benchmark_test.go) |
| 棘轮审计 | TestNoGrowthOfRawMessagesQueries (history_source_test.go) |
| 门禁 | gofmt / vet / build / 三平台交叉编译 / U+FFFD=0 |

---

## 10. 改造风险点

| 风险 | 等级 | 说明 |
|------|------|------|
| Context 接管率仅 29% | P0 | 10/14 业务入口仍自建 prompt，绕过 Context Engine |
| 6 处裸 llm.CallContext | P0 | 不走缓存/预算/归因（assistance/calendar/relationship 刻意设计除外） |
| 15 文件 36 处直查 FROM messages | P1 | 部分需迁移到 HistorySource（Historical 语义） |
| 无 per-task model routing | P0 | 所有 task 共用同一 active profile |
| 无 Relationship Session 概念 | P1 | v7 核心流程缺失 |
| llm_call_log 缺 contact_id | P2 | 无法做 per-contact 成本归因 |
| 仅 daily budget | P2 | 周/月预算缺失 |
| statemachine 无 hysteresis | P2 | 短期抖动可致状态反复 |
| 无 Data Health 统一面板 | P2 | 各表自诊断散落在 registry API |
| Desktop 缺远程新 API 调用 | P1 | Remote mode 无法使用 today/session 等新端点 |

---

## 11. 文件清单（按功能域）

### 核心存储/迁移
- storage.go (migrate, SaveMessages, GetRecent/All/Page/Cursor)
- archive.go (ensureArchiveTables, 归档/反归档)
- fts.go (ensureFTSFor, rebuild)
- history_source.go (统一历史查询)
- registry.go (表元数据注册)

### 联系人/消息/画像
- contact.go, parser.go, bot.go (微信 bot 主循环)
- profile.go (画像生成/意图分析)
- ingest_stats.go (Smart Paste 统计)

### AI / Context / Cache / Prompt
- llm.go, llm_settings.go, llm_presets.go, llm_usage.go, llm_budget.go, llm_task_ctx.go
- context.go, context_registry.go, context_adapters.go, context_debug.go
- ai_cache.go, prompts.go

### 记忆/事实/证据
- facts.go, evidence_provenance.go, memory_review.go, consolidation.go

### 关系状态/健康/决策/行动
- statemachine.go, health.go, metrics.go, relationship.go
- decision.go, decision_ledger.go, action_log.go, action_observe.go
- risk.go, portfolio.go, today_aggregate.go
- followup.go, goals.go, projects.go

### 网络/主题/生活
- network.go, social.go, circles.go, connections.go
- topics.go, timeline.go, life_state.go, life_projection.go, life_narrative.go

### AI 对话功能
- ask.go, coach.go, simulate.go, narrative.go, briefing.go, rehearsal.go
- summary.go, replay.go, assistance.go, weekly_plan.go

### 实验/干预学习
- experiment.go, experiment_measure.go, experiment_api.go
- intervention.go

### 运维/安全/备份
- security.go, auth_api.go, twofa.go
- backup.go, backup_api.go, merge.go, cleanup.go
- export_api.go, config.go, logger.go

### API/路由
- api.go, relationship_api.go, assistant_api.go, life_api.go
- tags_api.go, projects_api.go, insights_api.go, insight_api.go
- goals_api.go, followup_api.go, rehearsal_api.go, prompts_api.go
- portfolio_api.go, risk_api.go, connections_api.go, action_api.go
- tagsuggest_api.go, timeline_api.go, assistance_api.go, calendar.go

### 前端
- static.go, webpanel.go (嵌入式 HTML)
- qrcode.go, pollhealth.go, ilink.go (微信互联)

### 系统信息/其他
- sysinfo.go, sysinfo_linux.go, sysinfo_windows.go
- achievements.go, challenges.go, quality.go, quality_history.go
- mirror.go, self_portrait.go, heatmap.go, datareport.go
- mailer.go (遗留), mode_presets
- report.go, period_report.go, trend.go, insight_trend_history (in trend.go)

---

## 12. 总结

v6.3.3 已建立了完整的「分析关系」基础设施，拥有 45 张数据表、124 个源文件、177 个 API handler、14 个注册 AI task。Context Engine 框架设计到位但落地不足（29%），Model Router 完全缺失，Relationship Session 闭环尚未建立。v7.0 的核心工作是 **收口（Context/History/Budget）+ 连接（Session/Strategy/Brief）+ 提升（Router/Eval/Calibration）**。
