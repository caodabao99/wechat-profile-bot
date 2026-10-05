# ARCHITECTURE_AUDIT.md — PERSONAL RELATIONSHIP OS 2.0 · Phase 0 架构审计

> 本文件是 OS 2.0 升级**动手写代码之前**的强制产物（规格第四节）。所有结论均来自对 `main` 分支当前代码的实际扫描，非假设旧版本。
> 审计基线：`main @ d9fc7b0`（tag `v5.5.2`），Go `1.25.1`，157 个 `.go`（约 30,064 行非测试代码），81 条 `/api/` 路由，58 个测试文件。
> 测试基线：`go test -race -cover ./...` → **ok，81.5s，coverage 69.9%**（升级前全绿）。

## 0. 真实基线确认

| 仓库 | 角色 | 最新正式 Release |
|---|---|---|
| caodabao99/wechat-profile | Windows Desktop | v3.0.0 |
| caodabao99/wechat-profile-bot | Go Server | v5.5.2（本次审计对象） |

版本单一事实来源：[sysinfo.go](file:///home/caodabao/文档/qodercn/wechat-profile-bot/sysinfo.go) 的 `const appVersion`。当前 = `v5.5.2`。

技术栈关键事实（约束后续 Phase）：
- SQLite（`github.com/glebarez/go-sqlite` 纯 Go 驱动 → `CGO_ENABLED=0` 交叉编译成立）。直接第三方依赖仅此三件：go-sqlite、`go-resty/resty/v2`、`skip2/go-qrcode`。
- **不引 Redis / Elasticsearch / 向量库**（规格第十二节铁律，OS 2.0 全程遵守）。
- 单连接池 + 分层锁 `dbMu`（不可重入）：LLM 调用与 `RenderPrompt` 一律在锁外，绝不嵌套。
- 前端 Vue3 + `go:embed` + 纯 CSS，零重型图表库（不引 D3/ECharts/React）。
- 真实发布目标平台：linux/amd64、linux/arm64、windows/amd64（darwin 非目标，sysinfo 无 darwin 实现）。

---

## 1–6. 数据库表登记表（唯一事实来源 = registry.go）

规格明确：**Data Layer Registry 是表语义唯一事实来源，不得另造注册表**。当前 [registry.go](file:///home/caodabao/文档/qodercn/wechat-profile-bot/registry.go) 已登记 **40 张表**，四个正交轴（Type / Backup / Rebuild / HasContactID）逐表如实记录。下表即 item 1–6 的完整答案（Owner 列即「表 owner」，Type 列即 core/derived/cache/audit/config，Backup 列即「是否进入 backup」，Rebuild 列即「是否可 rebuild」，HasContactID 列即「是否含单一 contact_id」）。

### 核心业务数据（Backup=true，不可重建）
| 表 | Type | Owner | Backup | Rebuild | ContactID |
|---|---|---|---|---|---|
| contacts | core | contact | ✔ | ✘ | ✘（本体） |
| contact_aliases | core | contact | ✔ | ✘ | ✔ |
| messages | core | message | ✔ | ✘ | ✔ |
| messages_archive | core | archive | ✔ | ✘ | ✔ |
| profile_history | core | profile | ✔ | ✘ | ✔ |
| merge_log | audit | merge | ✔ | ✘ | ✘（source/target_id） |
| contact_tag_links | core | tag | ✔ | ✘ | ✔ |
| contact_tags | config | tag | ✔ | ✘ | ✘（全局字典） |
| followup_items | core | followup | ✔ | ✘ | ✔ |
| relationship_goals | core | goals | ✔ | ✘ | ✔ |
| contact_events | audit | timeline | ✔ | ✘ | ✔ |

### 派生状态（含 contact_id，删联系人级联清理；可重建但仍入备份）
| 表 | Type | Owner | Backup | Rebuild | ContactID |
|---|---|---|---|---|---|
| weekly_challenges | derived | gamification | ✔ | ✔ | ✔ |
| contact_achievements | derived | achievements | ✔ | ✔ | ✔ |
| contact_quality_history | derived | quality | ✔ | ✔ | ✔ |
| assistant_emotions | derived | assistant | ✔ | ✔ | ✔ |
| gamification_state | derived | gamification | ✔ | ✔ | ✘ |

### 派生 / 缓存（Backup=false，restoreSkipTables，恢复末尾清空、缺则自愈重建）
| 表 | Type | Owner | Rebuild | ContactID |
|---|---|---|---|---|
| profile_facts | derived | facts | ✔ | ✔ |
| profile_fact_evidence | derived | facts | ✔ | ✔（FK→profile_facts） |
| relationship_daily_metrics | derived | metrics | ✔ | ✔ |
| relationship_action_suggestions | derived | relationship | ✔ | ✔ |
| suggestion_outcomes | derived | relationship | ✔ | ✔（FK→suggestions） |
| contact_connections | derived | network | ✔ | ✘（contact_a/b 双列，专用 DELETE） |
| contact_topic_history | derived | topics | ✔（仍入备份） | ✔ |
| topic_evolution_state | derived | topics | ✔（仍入备份） | ✘ |
| insight_trend_history | derived | trend | ✔ | ✘ |
| weekly_plan_cache | cache | weekly_plan | ✔ | ✘ |
| life_state_cache | cache | life_state | ✔ | ✘ |
| life_projection_cache | cache | life_state | ✔ | ✘ |
| network_insight_cache | cache | network | ✔ | ✘ |
| self_portrait_cache | cache | portrait | ✔ | ✘ |
| intervention_cache | cache | intervention | ✔ | ✘ |
| briefing_cache | cache | briefing | ✔ | ✘ |

### 全局配置（Backup=true，按主库现状保留）
| 表 | Type | Owner |
|---|---|---|
| assistant_settings | config | assistant |
| archive_settings | config | archive |
| mode_presets | config | assistant |
| prompt_templates | config | prompts |

### 审计日志
| 表 | Type | Owner | Backup |
|---|---|---|---|
| backup_log | audit | backup | ✘（恢复时须保留自身） |
| assistant_runs | audit | assistant | ✔ |
| assistant_notified | audit | assistant | ✔ |
| email_send_log | audit | legacy | ✔（邮件推送已移除，遗留表） |

> 消费方：`cleanup.go` 用 `GetTableMeta(name)!=nil` 作「已纳管」门槛；`datareport.go` 的 `GET /api/system/table-registry` 导出本清单。新增表**必须**在此登记（规格第二十一节）。

---

## 7. messages / messages_archive 直接 SQL 查询位置

全库 **60 处** 直接命中，分布在 **17 个文件**（`FROM/INTO/UPDATE messages[_archive]`）：

| 文件 | 命中数 | 归档感知 |
|---|---|---|
| archive.go | 14 | ✔（归档本体，读写双表） |
| merge.go | 9 | ✔（recompute/undo 并计双表） |
| storage.go | 7 | ✔（分页取消息、GetAllMessages） |
| relationship.go | 8 | ✔（趋势 UNION ALL 双表） |
| assistant.go | 4 | ✘（仅活跃表） |
| timeline.go | 4 | ✘（仅活跃表） |
| health.go | 1 | ✘（仅活跃表，msg_unix 计数） |
| heatmap.go | 1 | ✘（仅活跃表） |
| insights.go | 1 | ✘（仅活跃表） |
| life_narrative.go | 1 | ✘（仅活跃表） |
| mirror.go | 2 | ✘（仅活跃表，近 N 条 me 消息） |
| topics.go | 1 | ✘（仅活跃表） |
| metrics.go | 1 | ✔（daily metrics UNION ALL 双表） |
| contact.go | 1 | ✘（仅活跃表） |
| followup.go | 1 | ✘（仅活跃表） |
| achievements.go | 2 | 需逐条核对 |
| search.go | 2 | ✔（含 archive FTS） |

**审计结论（Phase 1 / Phase 9 复核清单）**：9 个文件仅查活跃 `messages` 而未并 `messages_archive`。其中多数是「近期窗口」语义（mirror 取最近 me 消息、followup/assistant 近 N 天提取），归档后数据本就应缺席 → 属**设计正确**；但凡属**历史累计统计**者，若只算活跃表即违反规格第 12.1 节「不得出现某模块把历史归档后数据变成 0」。v5.5.2 已修复 `RecomputeOtherMsgCount` 一例。Phase 1 须逐个复核上述文件、区分「近期窗口 vs 历史累计」，并对历史累计类补齐 archive-aware 回归测试。

## 8. LLM 调用入口

**网络出口唯一**：[llm.go](file:///home/caodabao/文档/qodercn/wechat-profile-bot/llm.go) 的 `LLMClient`，`Call`/`CallContext` 内部 POST `baseURL + "/chat/completions"`（OpenAI 兼容）。`configured()` 为降级闸门——未配置即短路返回，所有上层功能据此安全降级、绝不 503。

其余全部是 **`*LLMClient` 的消费方**（非独立出口），共约 24 处业务函数：`AskContactHistory`/`extractAskKeywords`(ask.go)、`RewriteReply`/`ReviewDraft`(assistance.go)、`analyzeContactEmotion`/`runDailyCheck`/`checkAssistantSchedule`(assistant.go)、`GenerateBlessings`/`attachBlessings`(calendar.go)、`extractFollowups`/`RefreshFollowups`(followup.go)、`GenerateNarrative`(narrative.go)、`GenerateOrUpdateProfile`/`summarizeProfileChange`/`SupplementProfile`/`AnalyzeIntent`(profile.go)、`RehearsalReply`/`ReviewRehearsal`(rehearsal.go)、`GenerateActionSuggestions`/`fillDrafts`(relationship.go)、`SimulateReply`(simulate.go)、`SummarizeContact`(summary.go)、`analyzeContactTopics`/`ComputeTopicEvolution`(topics.go)、`GenerateWeeklyPlan`/`generateOutreachDraft`(weekly_plan.go)。
> OS 2.0 含义：新增 AI 能力（Context Engine / Decision / Replay）**只需复用这一出口**，无需再造 HTTP 层。

## 9. Prompt 使用入口

**模板渲染唯一入口**：`RenderPrompt(db, key, vars)`（[prompts.go](file:///home/caodabao/文档/qodercn/wechat-profile-bot/prompts.go)）；DB 覆盖优先（`prompt_templates` 表，懒建 `ensurePromptTemplates`，`getPromptOverride`/`validateOverride`），否则内嵌 `PromptSpec.Default`。当前在用模板键：`ask_answer`、`ask_keywords`、`emotion_analyze`、`calendar_blessing`、`followup_extract`、`relationship_narrative`、`profile_update`、`profile_change_summary`、`profile_supplement`、`intent_analysis`；另有 `assistance.go:assistancePrompt` 局部构造。
> 新 AI 功能应新增 `PromptSpec` 键接入本系统，不旁路硬编码 prompt。

## 10. 联系人关系状态计算入口（P2/P3 必须复用，禁第二套）

- **健康度融合**：[health.go](file:///home/caodabao/文档/qodercn/wechat-profile-bot/health.go) `ComputeHealth` → `fuseHealth(intimacy, trendState, emoAvg, daysSinceLast, meRatio)`、`bandOfHealth`、`projectCooling`。
- **亲密度**：[assistant.go](file:///home/caodabao/文档/qodercn/wechat-profile-bot/assistant.go) `computeIntimacy`；降温收集 `collectCoolingContacts`。
- **趋势分类**：[relationship.go](file:///home/caodabao/文档/qodercn/wechat-profile-bot/relationship.go) `GetRelationshipTrend`/`getTrendLocked` → `classifyTrend`/`classifyTrendWith`。
- **趋势快照序列**：[trend.go](file:///home/caodabao/文档/qodercn/wechat-profile-bot/trend.go) `appendTrendSnapshot`/`GetTrendSeries`（写 `insight_trend_history`）。
- **统一指标源**：[metrics.go](file:///home/caodabao/文档/qodercn/wechat-profile-bot/metrics.go) `GetAggregatedMetrics` / `relationship_daily_metrics`（UNION ALL 双表，归档感知）。
> Relationship State Machine（Phase 3）与 Decision Engine（Phase 4）的输入**必须**来自以上既有函数 + Metrics Layer，不得重算消息统计。

## 11. 分页接口

- **消息分页**：`api.go` 已支持 **keyset**（`?beforeId=`，[api.go:594](file:///home/caodabao/文档/qodercn/wechat-profile-bot/api.go)），底层 `GetMessagesPageBefore`（[storage.go:792](file:///home/caodabao/文档/qodercn/wechat-profile-bot/storage.go)）；未带 beforeId 时保留旧 offset 语义向后兼容。
- **联系人分页**：`getContactsPage`（`storage.go:966`）仍用 `LIMIT ? OFFSET ?` + `strftime('%s', c.last_updated)` 排序。
- **搜索分页**：[search.go](file:///home/caodabao/文档/qodercn/wechat-profile-bot/search.go) 用 `LIMIT ? OFFSET ?`（:232、:351），`searchMaxOffset = 10000` 封顶防穿。

## 12. OFFSET 查询（全部，Phase 1 keyset 化候选）

| 位置 | 说明 |
|---|---|
| search.go:232 | FTS/关键词搜索分页 |
| search.go:351 | 搜索（归档/回退分支）分页 |
| storage.go:770 | 消息旧 offset 分页（兼容路径） |
| storage.go:966 | 联系人分页，且排序用 `strftime`（违 11.2 msg_unix 优先） |

> 关键：`api.go`/`storage.go` 的 keyset 骨架**已存在且经验证**（`beforeId` + `GetMessagesPageBefore`）。Phase 1 是把 **search** 与 **contacts** 也 keyset 化，并按 11.3 返回 `nextCursor/hasMore`（cursor 含 msg_unix/id/archived，安全编码不直传 SQL 字段）。

## 13. profile_json 直接解析位置

全库 **44 处**引用，跨 **14 个文件**：`assistance.go`、`assistant.go`、`backup.go`、`calendar.go`、`datareport.go`、`export_api.go`、`facts.go`、`merge.go`、`profile.go`、`quality.go`、`relationship_api.go`、`social.go`、`storage.go`、`tagsuggest.go`。
> `profile_json` 是画像原始 JSON（`contacts` 表列 + `profile_history` 快照）。事实派生的**上游**即解析它（`deriveFacts(profileJSON)`）。OS 2.0 的 Context Engine 需从 profile_json 取 Identity/说话风格等，应统一经 `facts.go`/`profile.go` 的既有解析，勿在各 AI 模块各自 `json.Unmarshal`。

## 14. facts 生成位置（P1 Temporal Memory 改造核心）

管线全在 [facts.go](file:///home/caodabao/文档/qodercn/wechat-profile-bot/facts.go)：
- `deriveFacts(profileJSON) []profileFact`（:32）——从画像 JSON 拆结构化事实。
- `ExtractProfileFacts`→`extractProfileFactsLocked`（:77/:84）——旧行 `UPDATE ... SET status='retired'`（:93），再 `INSERT INTO profile_facts(...)`（:99）。
- 置信度更新：`UPDATE profile_facts SET confidence=?`（:204，当前基于证据量）。
- `RebuildFactsAndEvidence`（:212）/`refreshFactsQuietly`（:231）/`GetFacts`（:275）。
- 建表：`storage.go:346`（v10，`profile_facts`，status CHECK **仅** active/retired）。

**现状与 OS 2.0 P1 的差距（须升级 FACT LIFECYCLE）**：
- 现 `status` 只有 `active/retired`；规格要 `inferred/confirmed/verified/stale/conflict/rejected/superseded`。
- 现缺列：`source_type`(user/message/ai/system)、`valid_from`、`valid_until`、`last_confirmed_at`、`superseded_by`、`confidence_type`、`evidence_strength`。现仅 `source`(默认'profile')+`confidence`(REAL)。
- 现 `deriveFacts`→字符串检索=唯一证据来源；规格要 Evidence Assessment（match_type/support_strength/is_direct_support），不能仅凭「同词即直接证据」。
> 迁移铁律：`profile_facts` 是 **derived/可重建/不入备份**，改 CHECK 约束需按 SQLite 惯例（新表重建 or 版本化 v19），Phase 2 严格遵循「不破坏旧字段、可重建、懒/迁移择一」并同步 registry.go 与 backup.go。

## 15. evidence 生成位置

同在 facts.go：
- `AttachFactEvidence`→`attachFactEvidenceLocked`（:121/:127）——按 fact_value 在该联系人消息中检索支撑，`INSERT OR IGNORE INTO profile_fact_evidence(fact_id, contact_id, message_id, archived, snippet, msg_time)`（:192）；先 `DELETE FROM profile_fact_evidence WHERE contact_id=?`（:152）。
- 读回：`SELECT ... FROM profile_fact_evidence WHERE fact_id IN (...)`（:316）。
- 建表：`storage.go:377`（v11，FK `ON DELETE CASCADE`，UNIQUE(fact_id,message_id,archived)）。

**现状 vs OS 2.0 P1**：evidence 已有 `message_id`/`msg_time`/`archived`/`snippet`（满足 5.7「点击跳消息、支持双表、归档仍可打开」的**数据基础**）；缺 `match_type`(exact/semantic_candidate/same_topic/contextual)、`support_strength`、`quote`、`is_direct_support`（5.2）。Phase 2 用「FTS5 + 联系人 + 时间窗口 + 关键词 + 上下文消息」做候选召回（**不引 Embedding/向量库**），规则判定 direct/weak/uncertain。

---

## 附 A. 迁移与建表策略现状（Phase 2/5 落表依据）

- **版本化迁移链**：`storage.go` `PRAGMA user_version` 1→**18**（当前最高档；`if version < N` 逐段升级，任一步失败在写版本号前返回，下次整体重跑，不留半升级）。核心/审计/配置表走此链。
- **懒建表 `ensure*`（不入迁移链、不 bump user_version）**：`ensureAchievements`、`ensureArchiveTables`、``ensureAssistantTables`、`ensureGamification`、`ensureFollowupTables`、`ensureGoals`、`ensurePromptTemplates`、`ensureQualityHistory`、`ensureTagTables`、`ensureTimelineTables`、`ensureTopicHistory`、`ensureDailyMetricsSeededLocked`、`ensureFTSFor`。
> 遵循既有铁律：**新增派生/缓存表用懒 `ensure*` + 幂等 `CREATE TABLE IF NOT EXISTS`，不 bump user_version、不入 backup.go 的 derivedTables**；新增用户真实数据表才考虑版本化迁移并登记 registry + 评估 backup。Phase 5 `relationship_projects`（用户创建、类 relationship_goals）应走「懒 ensure + registry 登记 + Backup=true + contact 级联」路线，与 `relationship_goals` 对齐。

## 附 B. 明确禁止重复实现清单（已核实全部存在于当前代码）

FTS5(fts.go)、msg_unix(storage/backfill)、relationship_daily_metrics(metrics.go)、Data Layer Registry(registry.go)、Unified Metrics Layer(metrics.go)、Ask TA(ask.go)、Relationship Health(health.go)、Cooling Alert(assistant.go)、Weekly Plan(weekly_plan.go)、Coach(relationship.go)、Goals(goals.go)、Challenges(challenges.go)、Topics(topics.go)、Narrative(narrative.go/life_narrative.go)、Network(connections.go/network)、Self Portrait、Intervention Learning(intervention)、Weekly Briefing(briefing)、Timeline(timeline.go)、Conversation Simulation(simulate.go/rehearsal.go)、Backup/Restore(backup.go)、Data Export/Forget(export_api.go)、Prompt Plugin System(prompts.go)。
> 新功能必须复用上述模块；不得再造第二套 health / metrics / relationship score / weekly plan / facts / briefing。

## 附 C. Phase 0 结论

1. 架构分层清晰、登记表/指标层/AI 出口/提示词系统均已收敛为单点，OS 2.0 的整合有稳固底座，**无需推倒重来**。
2. 升级的真实工作集中在：facts/evidence 的**生命周期扩展**（P1）、在其上建**统一状态层**（P2，复用 health/trend/metrics）、**确定性决策打分**（P3，复用建议/目标/待办）、**项目表**（P4）、**Context Engine**（P5，复用单 LLM 出口 + RenderPrompt）、**Replay**（P6）、**搜索 keyset**（P1，骨架已在）。
3. 需持续守住的正确性红线：归档感知（附第 7 节 9 文件复核）、删/合并/撤销一致性、AI 永不 panic 且降级、版本三源同步、宁可 Unknown 也不猜成事实（规格第二十八节）。
4. 基线全绿（coverage 69.9%），每个后续 Phase 完成后必须回归，不得使既有测试下降。

*Phase 0 完成，进入 Phase 1（搜索/DB 正确性）。*
