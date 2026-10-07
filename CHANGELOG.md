# 更新日志

### v7.3.2（2026-10-07）— 修掉 v7.3.1 留下的 2 个自伤面 + 1 个测试环境脆弱，并加一道"文档↔实现"契约钉

第四轮投产前审计（浅克隆 GitHub HEAD，在克隆上跑 `-race` 全量）确认 v7.3.1 的修复有效（0 数据竞争），但抓出 3 项：

- **F1（v7.3.1 引入）文档超前于实现**：README/CHANGELOG 写"快照的 `cooldown_seconds` 供前端置灰按钮"，实际 `static/` 对它 **0 引用** —— 冷却在界面上毫无体现，用户只会看到一次报错。现已真正实现：按钮在冷却期置灰并显示「冷却中，N 秒后可重试」，失败后自动刷新状态。
- **F2（v7.3 引入）游标与凭据不再配对**：v7.3 把长轮询游标落盘成 `ilink_syncbuf.json`，但备份 sidecar 白名单没收录它 —— 用旧备份恢复会得到「旧会话令牌 + 新会话游标」的错配（服务端可能对该 buf 报错，或从头重放）。现在：白名单收敛为单一来源 `sidecarCredentialFiles`（备份打包 / 恢复前自动备份 / 恢复落盘三处共用），游标与凭据**同进同出**；若备份里没有游标而带凭据，恢复时自动清除磁盘上不匹配的游标，并通过 `ResetCursor()` 同步清内存（当前进程不会再拿旧游标去请求）。
- **F3（既有）测试依赖运行环境**：`TestRestoreRollsBackSidecarsAndDB` 在 `umask 077` 下**必然失败** —— 夹具用 `WriteFile(..., 0640)` 建文件被 umask 削成 0600，而断言写死 0640。**产品代码本身是对的**（恢复路径有显式 `os.Chmod` 保留原权限），坏的是测试；已补显式 `Chmod`，实测在 0077 / 0022 下都通过。systemd `UMask=0077`、Docker 严格 umask 或将来加 bot CI 都会被这种假红误导。
- **新增机制化防护**：`uiFieldContract` + `TestDocumentedUIFieldsAreActuallyUsedByFrontend` —— 凡"文档声称由网页使用"的响应字段，必须**同时**在 `static/` 有引用、在 README 有说明，缺一即红（本次立刻抓出 `qr_image`/`accuracy`/`verdict` 三个未写进 README 的字段，已补）。

新增测试 5 个（备份-恢复游标配对端到端、缺游标旧备份恢复后清除陈旧游标、`ResetCursor` 清内存+磁盘且幂等、UI 字段契约、umask 加固下的恢复回归）。README 同步：备份内容与加密范围说明、`/api/wechat/bind` 字段清单、能力测试字段清单。

无 API 破坏性变更；`user_version` 仍为 **28**。注意：**旧备份 zip 里没有游标**属正常，恢复时程序会清掉本地游标并从头拉取（不会重复入库）。


### v7.3.1（2026-10-07）— 修掉 v7.3.0 自己带进来的 5 个问题（投产前第三轮审计）

第三轮审计（克隆 GitHub HEAD 实读 + 在克隆上跑测试 + 抽验线上产物）确认：v7.3.0 的 9 项修复都已生效，**但其中 4 项引入了新的自伤面**，另发现 1 个既有长跑隐患。本版全部修掉，每条配回归测试。

- **N1 账本被"重建派生"误清**（v7.3.0 引入）：`ingest_ledger` 被放进 `derivedTables`，于是「仅重建派生」和管理员恢复末尾清空都会 `DELETE` 它 —— 防重放凭据被抹掉后，未提交/待重放的消息会**重新跑一遍 LLM**。新增独立的 `runtimeTables`（不入备份、但也不受重建/清空影响），注册表 `Rebuild:false`。
- **N2 一次误点毁掉可用会话**（v7.3.0 引入）：`runRebind` 一进来就 `ClearCredentials()`，而 `-14` 可能只是服务端瞬时抛错；扫码没成旧凭据也没了。现改为**暂存 + 失败原样回滚**（成功才丢弃），并加 **5 分钟冷却**（该协议已实测存在 rate limit，反复点等于自锁），冷却通过 `/api/wechat/rebind` 返回 429 + `Retry-After`，快照新增 `cooldown_seconds` 供前端置灰。
- **N3 重绑后游标归零的重放窗口**（v7.3.0 引入）：账本 TTL 由 7 天提到 **30 天**，覆盖重绑后空游标可能带来的历史重放（内容层有 `msg_hash` 兜底不重复入库，但白花额度）。
- **N4 游标文件未忽略**（v7.3.0 引入）：`.gitignore` 补 `ilink_syncbuf.json`。
- **N5 日志无轮转**（既有）：`bot.log` 与 `security.log` 此前只 `O_APPEND`、无上限；磁盘满后备份 / 游标落盘 / `VACUUM INTO` 会一起失败。新增 copy-truncate 式自转（**20MB × 保留 3 份**，不换 fd 故对已打开的 handler 安全），启动时 + 每 6 小时检查。

验证：`go test ./...` 通过；新增 5 个回归用例（重建派生保账本 / 重绑失败回滚 / 重绑冷却 / 日志轮转与截断 / 既有用例随 TTL 调整）。

无 API 破坏性变更；`user_version` 仍为 **28**（不再新增迁移）。


### v7.3.0（2026-10-07）— 投产前审计 9 项缺陷全部修复

第二轮上线前审计（读代码 + 联网核实 iLink 协议约束）确认 9 项问题，本版全部收口，每条都带针对性回归测试。

**消息链路可靠性（最高优先）**
- **C9 先去重后处理导致丢消息**：旧实现 `GetUpdates` 一拿到消息就标记已见并当场推进游标，处理失败/崩溃即**永久丢失**；且游标只在内存，重启后空游标按协议**可能重放整段历史**。现改为 **at-least-once**：游标「后置提交」（整批处理成功才 `CommitCursor` 并落盘 `ilink_syncbuf.json`）+ 新增持久幂等账本 `ingest_ledger`（migration **v28**，done 跳过、失败重试、3 次判毒丸放弃并告警、7 天自动清理）。`recentIDs` 退化为「已成功处理」的 5 分钟快路径，绝不前置标记。
- **C1 会话过期后成死端**：`-14` 置粘性位后主循环只 `continue`、**永不再收消息**，于是提示用户发的「重登」命令本身也收不到；`ResetSession` 不删凭据，重启仍跳过扫码 → 唯一出路是手工删文件。现改为：退避探测（60s→×3→上限 60min，对齐官方插件 Session Guard）+ `AllowProbe` 让循环真能再发一次请求 + **网页端「重新扫码绑定」**（`POST /api/wechat/rebind`、`GET /api/wechat/bind`，服务端渲染二维码 PNG，扫成功即自动恢复轮询，无需重启）。
- **C2 核心链路无 panic 隔离**：`recover` 此前只在归档/助手等旁路。新增 `SafeHandleMessage` / `SafeFlushPending`：panic 被隔离成兜底回复 + 日志，并且**不提交游标**（交给重放重试），单条坏消息不再带走整个进程。

**数据正确性**
- **C5「对方的口头禅」生产恒为空**：落库是 snake_case（`communication_style`/`frequent_phrases`），读取侧却解 camelCase；旧测试 fixture 也用 camelCase 所以自洽测不出。现两种命名都解并按画像内去重，fixture 改为真实 snake_case。
- **C6 作息小时/星期分布整体偏 8 小时**：`strftime('%H'/'%w')` 对带偏移的 RFC3339 先转 UTC。加 `'localtime'`（全仓扫描确认这是唯一漏写的一处，其余 6 处均已有）。
- **C3 迁移表重建非原子**：v19 / v26 的 CREATE→INSERT→DROP→RENAME 逐条自动提交，断电会留下 `*_new` 残表或已 DROP 的主表，下次启动**永久失败需手工修库**。现各包成一个事务；测试把 `user_version` 回卷到 v18 重放迁移，验证收敛、无残表、主表可写。
- **C4 `MergeFacts` 撞 UNIQUE**：预删只防「loser 与 keep 冲突」，两个 loser 共享同一支撑消息时迁移证据必撞 `UNIQUE(fact_id,message_id,archived)`，且三步无事务会留下半合并。现先在 loser 之间按 `(message_id, archived)` 去重，四步全部包进一个事务。

**安全**
- **C8 认证前慢 body DoS**：`MaxBytesReader` 只限量不限时，全局 `ReadTimeout` 又设不得（会掐断 200MB 备份上传）。实测 Go 的 `TimeoutHandler` 要等内层 handler 返回才发得出 503，**单独靠它防不住**，因此加的是**并发上限** `authMaxConcurrent=32`：打满立即 429（带 `Retry-After`）并记安全日志，把资源占用变成有界。
- **C7 明文 HTTP 且程序主动推 `http://公网IP`**：自动探测到公网明文地址时输出 `slog.Warn`，微信「网址」命令的提示改为讲清真实后果（apiToken/TOTP/聊天内容明文过网，拿到 token 即可绕过 2FA 并导出含密钥的备份），并明确要求正式使用先配 `webBaseURL` 走 HTTPS 反代。

- 附带修好两个既有问题：① `TestFacadeAggregateMatchesDetail` 的夹具写无偏移裸格式时间，被 `strftime(...,'localtime')` 日桶整体 +8h，导致本地 16 点后必然失败（已发布的时钟依赖坏测试），改为与生产一致的 RFC3339；② `RebindSnapshot` 读会话字段未持 `c.mu` 的真数据竞争，`-race` 抓到后已修。
**版本与迁移**：`appVersion` v7.2.0 → **v7.3.0**；SQLite `user_version` **27 → 28**（新增 `ingest_ledger`，登记进表注册表、不入备份、可整体清理）。


门禁：`node --check` / gofmt / vet / 三平台 `CGO=0` / U+FFFD=0 / `-race` 覆盖率不低于上一版；新增 `ilink_reliable_test.go` + `audit_fixes_test.go` 覆盖上述每一条。

### v7.2.0（2026-10-07）— 网页端接入三个 v7 运营面板 + API 一致性双向钉

投产前审计发现：v7 的指挥中心 / 策略学习 / 个性化校准等端点「后端全有、网页零消费」。本版把它们真正接进界面。

- **洞察页新增 3 个子标签**：**指挥中心**（`GET /api/command-center` 六栏 Top3 只读聚合）、**策略学习**（`GET /api/strategy/history` 回暖率 + Wilson 95% 置信区间 + 样本少标记）、**个性化校准**（`/api/calibration` 列表 / 记录 / 按对象撤销 / 清空；`feedback` 用后端封闭枚举做下拉，杜绝填了未知值被拒）。
- **`static_api_pin_test.go` 双向钉**：前端每个 `/api/` 调用必须有后端路由；后端每个 API 域必须「被前端消费」或「登记 README」。首跑即抓出 `/api/stats/ingest` 自 v6.3.0 起前端零引用、文档零登记的孤儿端点。
- **`bot_dispatch_test.go`**：补齐 `bot.go`（1058 行）此前为零的 `NewBot`/`HandleMessage` 分发测试，含「已注册命令必须出现在帮助文本」一致性钉。
- **文档归因纠正**：`V7_IMPLEMENTATION_REPORT.md` 的 UI(Web) 行由虚标 ✅ 改为据实描述；README 截图标注由「首页·指挥中心」改为真实装配来源；不存在的 `data/health` 路径订正为 `system/data-health`。
- 覆盖率 72.4% → **73.2%**；真实 HTTP 并发压测（16 路读 20 端点 + 3 路并发触发最长持锁重建）412 请求，无死锁、无 5xx、响应体全部合法 JSON。
- 如实标注：`/api/llm/router`（策略本身由后端每次调用生效，但**无管理界面**）与 `/api/system/data-health` 仍为 API-only，尚未接进网页。

无破坏性 API 变更；SQLite `user_version` 不变（27）。

### v7.1.1（2026-10-07）— 补丁：发布包内补齐 LICENSE 与新 README

纯打包内容修正，无代码行为变更：v7.1.0 的 tag 早于「MIT LICENSE 入库」与「README 首屏重构 + 截图 + CHANGELOG 拆分」两个提交，导致下载包内缺 LICENSE、README 为旧版。本补丁把二者纳入发布包。

- appVersion v7.1.0 → v7.1.1；下载物/镜像标签/徽章同步。

### v7.1.0（2026-10-07）— 模型能力可验证 + 调用参数可自定义

本版本把 AI 能力从「声称」变成「可验证」，并让模型调用参数适配多厂商差异：

- **网页端「验证模型能力」面板**（`POST /api/llm/capability`）：一键用当前活动模型跑 7 条能力探针（情绪含过度报警陷阱 / 跟进含过度抽取陷阱 / 事实幻觉 / 缺失信息拒答 / 冲突 / 时间推理），返回准确率 / JSON合法率 / 幻觉率 / 逐条结果与结论。走既有鉴权、single-flight、未配置优雅降级不 500；与生产同一条 `ExtractJSON`/评分路径。
- **模型调用参数可自定义**（档案 `extraBody`）：每个模型档案可填一段 JSON，逐键合并进每次请求，适配各家对思考/推理模式的不同命名（`enable_thinking` / `thinking:{type}` / `reasoning_effort` …），无需改代码；结构性键 `messages` 受保护，坏 JSON 保存即拒。
- **AI 评测器修复**（`eval.go`/`golden_cases.json`）：期望片段支持「|」同义备选、禁用片段否定语保护、确定性任务从大模型评测分母剔除、补齐缺上下文的用例——真实模型跑分不再被同义换写/限流/超时误判（GLM 基线 accuracy 0.36→0.82）。
- **真实模型验证**：GLM-4.7-flash / glm-5.3-flash 端到端实测通过。

无破坏性 API 变更；`appVersion` 由 v7.0.0 升至 **v7.1.0**；`-race` 覆盖率 **72.4%**。

### v7.0.0（2026-10-06）— Personal Relationship OS 3.0

本版是 **v7.0 蓝图「收口 + 连接 + 提升智能层级」的完整交付**，把既有系统收拢成一台会自我学习的个人关系智能机，而非「更多 AI 功能」。全 17 个 Phase（P1–P17）一次性落地，接通 `PERCEIVE→REMEMBER→VERIFY→UNDERSTAND→DECIDE→REHEARSE→ACT→OBSERVE→LEARN→UPDATE MEMORY` 的闭环。桌面端配套升至 **v4.0.0**（独立仓库）。SQLite `user_version` 25 → **27**（新增 `personal_calibration_profile` 唯一真相表 + `relationship_action_log` 结果列），向后兼容、幂等迁移。

- **P1 Context Engine 唯一入口化收口**：所有 AI 任务上下文一律经 `BuildContactContext`（全仓仅此一份）+ Context Task Registry（22 任务登记预算/所需块/cacheable/requiresLLM 为单一来源）；杜绝第二套 Context Engine（§32 审计 0 命中）。
- **P2 AI Model Router + Cost Governance**：`llm_router.go` 按任务能力/成本选模；`llm_budget.go` 任务级归因 + 日预算护栏（只拦后台批量、不拦用户即时操作，超限降级不 503）。
- **P3 Memory / Evidence 3.0**：`memory_temporal.go` + `evidence_chain.go`——FACT/INFERENCE 分层、每条结论可溯源到消息证据、事实冲突进待确认队列**绝不直接覆盖**（场景 B/C）。
- **P4 Smart Paste 2.0**：`smart_paste.go` 增量去重，同段二次粘贴 0 新增、绝不重复调用 AI（场景 D），四指标审计闭环。
- **P5 Relationship Session**：`relationship_session.go` 编排 before-brief→strategy→rehearsal→observation，落统一 Action Ledger。API `/api/contacts/{id}/session`。
- **P6 Strategy Learning / Calibration**：`strategy_learning.go` 聚合历史表现（Wilson 置信下界、有界软加分、措辞只谈相关不谈因果）；`calibration.go` 用户反馈写独立软调整表、限幅 + 时间衰减。学习让**下一次推荐因这一次结果而改变**（场景 A 闭环，见 `regression_v7_test.go`）。
- **P7 Command Center / Contact Brief**：`command_center.go` 首页六栏 Top3 只读编排（模型全失效时仍显示 State/Risk/Today/Memory/Portfolio，仅 AI 文案降级、绝不 500，场景 G）；`contact_brief.go` 纯确定性联系前简报。API `/api/command-center` · `/api/contacts/{id}/brief`。
- **P8 Network Opportunity Discovery**：`network_opportunity.go`「谁可能帮我」只读检索，零新真相层。API `/api/opportunity`。
- **P9 Historical Repository 收口**：`message_repository.go`/`history_source.go` 命名门面 Recent/Historical/Aggregate，深分页 keyset 不 `COUNT(*)`（场景 E）。
- **P10 Data Health / Backup / Merge / Delete**：`datahealth.go` Data Health 2.0 聚合器（仅重建 derived）；删除级联无孤儿不变量；带口令备份密钥加密、API Key 不回传明文。
- **P11 AI Evaluation Lab（§18/§19）**：`eval.go` 离线/确定性/零真实数据评测底座（22 例覆盖 11 任务类型、五质量指标 + 模型对比 + Prompt 回归钉）；缓存键五分量治理（切模型/改 Prompt 天然失效、绝不串）。
- **P13 性能 Benchmark（§25.1）**：`perf_bench_test.go` 七操作上报 P50/P95/P99，规模经 `PERF_SCALE` env 化（100K–10M 可复现）。
- **P14 安全回归（§27）**：`security_v7_test.go` 钉十攻击面（SQLi/FTS 注入/SSRF/密钥泄漏/认证/限流/路径穿越/备份密漏/CORS/代理滥用）。
- **门禁**：全增量过同一门禁——`gofmt`/`build`/`vet=0`、linux/{amd64,arm64}·windows/amd64（`CGO_ENABLED=0`）交叉编译、U+FFFD=0；`-race -cover` 全绿，覆盖率 71.2%（v6.3.3 基线）→ **72.3%**（棘轮只升不降）。652 测试/基准函数全通过。
- **交付报告**：`V7_ARCHITECTURE_BASELINE.md` · `V7_IMPLEMENTATION_REPORT.md` · `V7_AI_EVALUATION_REPORT.md` · `V7_PERFORMANCE_REPORT.md` · `V7_SECURITY_REPORT.md`（§33 二十六项逐项 IMPLEMENTED/PARTIAL 标注）。

### v6.3.3（2026-10-06）

本版是 **v6.3 蓝图“成本控制 + 数据完整性 + 智能化”三大主题的收口版**，一次性落地 P2b/P2c/P9/P5/P11/P12/P15 共 7 个增量。无破坏性 API 变更，桌面端维持 v3.2.0。

- **P2a/P2b 任务级成本归因**：`llm_call_log` 新增 `Label` 字段，`taskLabelFor()` 确定性映射任务中文名；网页“按模型”下方新增“按任务”表，每笔调用可追溯到“为哪个 AI 任务花的”。
- **P2c 日预算护栏**：新建 `llm_budget.go`，ctx flag 区分后台批量 vs 用户即时。超限只拦后台任务（返回普通 error，不 503）、不拦用户操作；先查缓存再查预算。`LLMSettings` 新增 `DailyTokenBudget` 字段，前端加预算 UI。
- **P9 历史消息统一访问层**：新建 `history_source.go`（messages ∪ messages_archive 唯一实现）+ `historyTimeExpr` 统一时间口径。迁移 6 文件 9 处归档盲区 SQL。审计棘轮 `TestNoGrowthOfRawMessagesQueries`——基线 15 文件 36 处残留只允许递减。
- **P5 Memory Consolidation**：新建 `consolidation.go`，确定性检测冲突/陈旧/近重复（bigram Jaccard ≥ 0.6），产出 `ConsolidationProposal`。API: GET/POST `/api/memory/consolidation`。
- **P11 Action Center 2.0**：新建 `today_aggregate.go`，四源（决策/待办/记忆审核/风险）按联系人折叠为一张卡、每日上限 7、紧急度分组、Snooze 机制。API: GET `/api/today` + POST `/api/today/snooze`。
- **P12 Smart Paste 去重统计**：新建 `ingest_stats.go`，每次 ingest 后记录 (parsed, new, dup)，GET `/api/stats/ingest` 返回去重率与 Top5。90 天自动清理。
- **P15 性能基准**：新增 5 个 `Benchmark*` 函数（SaveMessages 1k、HistoryMessages 5k、HistoryCount、RefreshStates 50 人、ConsolidationProposal），为后续版本提供性能回归基线。
- **门禁**：全增量过同一门禁——`gofmt`/`build`/`vet`、linux/{amd64,arm64}·windows/amd64（`CGO_ENABLED=0`）交叉编译、U+FFFD=0；`-race -cover` 全绿，覆盖率 71.1% → **71.2%**（不降反升）。

### v6.3.2（2026-10-06）

本版补齐 P1 骨干的最后一块——**§5.4 Context Debug 可观测层**：把「某个 AI 任务在这个联系人身上到底取了什么上下文、渲染后值多少 token、缓存里有没有、由哪个模型服务」从各模块的隐式行为变成**可查询的 API 事实**。无数据库 schema 变更、无破坏性 API 变更（仅向既有端点新增字段），桌面端维持 v3.2.0。

- **块计划与实取对照（`block_plan` / `missing_required`）**：以 Context Task Registry（§5.3）为「计划」、以渲染前的上下文为「实取」，逐块输出该任务是否声明需要（required/optional）、实取条数与是否非空；单独列出「**声明必需却为空**」的块——这是排障时信号量最高的一项（如叙事任务 required `recent_messages` 却一条未取到）。
- **token 估算（`token_estimate`）**：输出渲染后文本的 token 估算、预算上限、占比与是否被截断。项目内无分词器，故字段显式标 `estimate_only`——**不冒充模型侧真实用量**（CJK 逐字计 1、拉丁每 4 个非空白字符计 1）。
- **缓存快照状态（`cache`）**：按 `(contact, task, context_version, model)` 分层返回已缓存快照计数与最新写入时间，并给出活动模型与 `llm_configured`。刻意不谎称命中：缓存主键含「最终 prompt 哈希」，而本端点不构建 prompt，故 `hit_can_be_asserted` **恒为 `false`** 并附原因，只报存在性、不宣称必然命中。
- **任务登记告警（`registered`）**：未登记进注册表的任务会回落默认规格，此字段为 `false`，作为新 AI 任务忘记登记的迁移探针。
- **隐私同纪律（§4.3）**：可观测层 `obs` 全部为**计数 / 指纹 / 布尔 / 估算**，不含任何消息正文；正文与 `rendered` 仍只由 `config.contextDebug` 闸门决定。验收测试不空转：先断言同一上下文渲染后**确实**含指定私密串，再断言 `obs` 序列化后完全不含它。订正 README 对本端点的旧描述（原文未提隐私门，默认并不返回正文）。
- **不新增包袱**：不建表、不引入第二套 Context 引擎，全部消费既有 registry + `ContactContext` + `ai_response_cache`。
- **门禁**：`gofmt`/`build`/`vet`、linux/{amd64,arm64}·windows/amd64（`CGO_ENABLED=0`）交叉编译、U+FFFD=0；`-race -cover` 全量绿，覆盖率 70.6% → **70.7%**（不降反升）。

### v6.3.1（2026-10-06）

本版是 v6.3.0 骨干加固的**接管收口补丁**——延续 §5.2「谁在什么条件下走哪条 AI 出口」的治理纪律，把剩余联系人作用域的 LLM 调用点接入统一缓存原语，并首次厘清「刻意不接管」的边界。纯增量成本/去重治理，无 schema、无 API、无用户可见行为变更（桌面端维持 v3.2.0）。

- **§5.2 对话类接管第二批**：`simulate.go`（对话模拟，复用已登记 `TaskSimulation`）、`rehearsal.go`（彩排回一句 `RehearsalReply` / 预演复盘 `ReviewRehearsal`）迁至 `callLLMCached`——同输入同 prompt 二次调用命中缓存、跳过重复模型消耗。重跑幂等的对话模拟/彩排适合接管。
- **§5.2 联系人类后台派生接管第三批**：`followup.extractFollowups`（待跟进抽取）、`weekly_plan.generateOutreachDraft`（每周维护开场白）、`assistant.analyzeContactEmotion`（联系人情绪分析）迁至 `callLLMCached`——均为联系人作用域、同一消息快照重跑幂等的后台派生任务，未变化的联系人在周期扫描中命中缓存、省重复消耗。
- **接管边界厘清（关键决策）**：`assistance.go` 的 `RewriteReply`（草稿改写）/`ReviewDraft`（草稿检查）**刻意保持裸 `llm.CallContext`** 并在 `context_registry.go` 标 `Cacheable:false`——交互式操作期望每次新鲜多样，且 `callLLMCached` 先查缓存会绕过 `ctx`、使「请求取消须真实中断模型」的语义失效。判断准则固化为：幂等内容生成/派生 → 接管；即时交互改写/检查 → 不接管。
- **Context Task Registry 同步登记**：新增 `TaskRehearsal`/`TaskRehearsalReview`/`TaskFollowup`/`TaskOutreach`/`TaskEmotion`（接管）与 `TaskRewrite`/`TaskDraftReview`（`Cacheable:false`，如实记录不接管决策）。
- **门禁**：全增量过同一门禁——`gofmt`/`build`/`vet`、linux/{amd64,arm64}·windows/amd64（`CGO_ENABLED=0`）交叉编译、U+FFFD=0；`-race -cover` 全绿，覆盖率维持 **70.6%**（不降）。

### v6.3.0（2026-10-06）

本版是 **Personal Relationship OS 从「功能堆叠」走向「统一治理」的骨干加固版**：先把「谁在什么条件下取什么上下文、走哪条 AI 出口」这套底层纪律做扎实（可观测、可缓存、可校验），并对当前 `main` 代码做了一次以实测为准的架构审计。**遵循蓝图「骨干优先、不许假装完成」的纪律——本版只交付并验证下列能力，其余规划项见文末「路线图（尚未在本版本实施）」，未实现即未实现。**

- **架构审计与发布一致性治理（`ARCHITECTURE_AUDIT_V6_2.md`）**：以 `main` 实测（非 README 声称）钉死版本基线——schema `user_version=25`、49 张登记表、逐模块 Context/LLM 出口接管矩阵、消息 SQL 分布、AI Cache 覆盖、Secret/SSRF 现状与 P0–P13 能力状态矩阵。并修复 GitHub `releases/latest` 因按 `created_at` 排序而仍指向旧版的问题（以 release-id + 布尔 `make_latest` 显式声明最新）。
- **Context Task Registry 单一事实来源（`context_registry.go`，§5.3）**：把原先散落在 `budgetFor` switch 里的「每类任务取多少条消息/证据/事件/主题/周、预算多少 token、需要哪些上下文块、可否缓存」收敛为一张代码注册表；`budgetFor(task)` 改为委托 `taskSpec(task).Budget`。补齐 `summary`/`intent`/`memory_review`/`topic`/`experiment` 等任务登记。专测 `TestContextRegistryBudgetNoDrift` 逐一比对预算零漂移，未知任务安全回落。
- **AI 响应缓存接管扩展（§5.2）**：5 个联系人内容生成任务（画像生成/补充/意图分析、关系叙事、往来摘要、主题演化）由裸 `llm.CallContext` 统一迁至 `callLLMCached` 单一原语——相同认知输入 + 相同 prompt 的二次调用命中缓存、跳过重复模型消耗，跨模型结果天然隔离，未配置时沿用各模块既有确定性降级。验收测 `TestNarrativeTakeoverHitsCache` 以「模型 HTTP 端点仅被触达一次」这一代码事实锁定接管（非 README 声称）。
- **模型/代理端点入参校验（`llm_settings.go`，安全）**：唯一写库入口 `saveLLMSettings` 前置护栏——BaseURL 仅 `http/https` 且必须含主机名、禁止 `user:pass@host` 内嵌账号；代理仅 `http/https/socks5`；空值放行（回落 `config.json` 属合法语义）。非法值直接拒绝、不落库、不改活动配置，堵住协议处理器/SSRF 类误配。`TestSaveLLMSettingsRejectsBadURLEndToEnd` 断言非法值不污染库。
- **治理与测试**：全增量过同一门禁——`gofmt`/`build`/`vet`、linux/{amd64,arm64}·windows/amd64（`CGO_ENABLED=0`）交叉编译、U+FFFD=0；`-race -cover` 含治理回归全绿，覆盖率 70.5% → **70.6%**（不降反升）。

**路线图（尚未在本版本实施，据实标注、不充数）**：AI Model Router / Task Policy / 成本面板（P2）、Relationship Session 编排（P4）、Memory Consolidation（P5）、AI Evaluation Lab（P6）、Personal Calibration（P7）、Network Opportunity Discovery（P8）、Historical Message Repository 统一访问层全量收敛（P9，本版仅接管 5 个内容任务，其余 ~10 个裸调点与全局任务待后续）、Relationship Portfolio 2.0（P10）、Action Center 2.0（P11）、Smart Paste 增量去重统计（P12）、100K~10M 消息 Benchmark（P15）。桌面端本版无新增入口，维持 v3.2.0（不强升版本号）。

### v6.2.0（2026-10-06）

本版聚焦**模型与代理的运行时自助管理**：把原先只能在 `config.json` 里写死的对话模型，升级为网页可视化的一等公民——可在「模型与代理」页切换模型、适配国内外预设、开关推理/思考模式、添加自定义模型，统计真实调用量，并为一位国外模型配置**仅作用于模型调用**的网络代理与一键连通性自检。全部遵循：运行时解析、config.json 兜底（老部署零感知）；代理绝不外溢到微信同步/邮件等其他功能；密钥对外一律打码、绝不明文外泄；探测/调用失败当「数据」呈现、绝不 503。数据库新增两张表（`llm_settings` 配置、`llm_call_log` 用量），均为 `Backup:true`、恢复缺表则保留主库。

- **模型档案与运行时切换（`llm_settings.go`/`llm.go`/`llm_api.go`）**：懒建 `llm_settings` 单行 JSON 配置表，多档案（label/provider/baseURL/apiKey/model/region/disableThinking/useProxy）；`LLMClient` 每次调用经 `resolveSpec` 读活动档案，未配置或无库时回落 `config.json`，缓存键含 model 故切模型天然不串用。`/api/llm/{settings,active,profile,profile/delete}` 全套端点，密钥 GET 一律打码、PUT 回传掩码按 ID 沿用旧值。
- **国内外预设 + 自定义模型（`llm_presets.go`）**：内置国内（DeepSeek/通义/智谱/火山豆包/Kimi/硅基流动）、国外（OpenAI/Gemini/Groq/Mistral/xAI，默认走代理）与本地（Ollama，无需密钥）预设目录，`/api/llm/presets` 供前端一键带出接口与常用模型；亦可完全自定义。
- **模型调用量统计（`llm_usage.go`）**：追加式 `llm_call_log`（真实 API 调用才记、缓存命中不计，口径=实际消耗），解析响应 `usage` 的 prompt/completion/total token 与延迟/是否经代理；`ComputeLLMUsage` 聚合今日/7 天/30 天三窗口 + 按模型分解，`/api/llm/usage`。
- **仅模型调用的网络代理 + 连通性自检（`llm_proxytest.go`/`llm_api.go`）**：代理经 `clientFor` 注入 LLMClient 的 resty 客户端（`proxyMu` 保护、仅 URL 变化重建，绝不在调用中改传输层），只影响模型 HTTP 出口；`POST /api/llm/proxy/test` 测直连/代理出口 IP 与到代表性站点（Google 204/OpenAI/Cloudflare）的延迟，`POST /api/llm/model/test` 对活动模型发一次最小调用验可达。探测目标可注入（测试用 httptest，绝不依赖真实外网），结果落 `settings.Proxy` 供状态页展示。
- **状态页概览 + 前端「模型与代理」页（`api.go`/`static/`）**：`/api/status` 增加 `llm` 块（活动模型/代理态/最近测试/用量摘要，全程降级不 500）；新增「模型与代理」导航页，覆盖档案增删改切换、推理开关、国内外预设、自定义、代理配置与连通性/模型可达测试、调用量图表。
- **网页文案中文化**：关系状态（基态·动态态）、趋势、预警、事实类型等原先直出的英文枚举统一映射为中文，未知值保留原样不致空白。
- **治理与测试**：全 Phase 过同一门禁——`gofmt`/`build`/`vet`、linux/amd64·windows/amd64（`CGO_ENABLED=0`）交叉编译、U+FFFD=0、`node --check`；`-race -cover` 含治理回归（登记表/备份恢复/级联清理/FeatureSweep）全绿，覆盖率 70.2% → **70.5%**。新增模型设置/预设/用量/状态/代理测试（httptest 端到端，零真实外网依赖）。

### v6.1.0（2026-10-06）

本版把 **Personal Relationship OS** 从「能算出洞察」推进到「能闭环行动」——在 v6.0 已有的 `记忆 → 可信事实 → 关系状态 → 决策` 链路之后，补齐 `决策 → 行动 → 结果 → 学习 → 记忆更新` 的**执行与学习**半环，并把首页主入口从分散的洞察模块收敛为**行动中心（Action Center）**。全部遵循增量复用、不重写既有能力；确定性优先、LLM 缺席也能完整运行；AI 结论一律分层 FACT/INFERENCE，估算标 estimate、不声称因果，宁可 Unknown 也不臆断。数据库支持版本升至 **v25**。

- **P0 · AI Context Engine 全面接管（`context.go`/`context_adapters.go`）**：`ContactContext` 新增确定性内容指纹 `context_version`（同输入同版本，可缓存/可观测）；建立 AI 响应缓存基础设施与 `prompt_version`，渐进影子接管——首个模块改造为经统一 Context Adapter + `callLLMCached` 取数，命中缓存不重算、LLM 缺席降级不 503。新 prompt 一律走 `RenderPrompt`。
- **P1 · Action Ledger 行动账本（`action_log.go`/`action_api.go`/`decision_ledger.go`，`relationship_action_log` v21）**：为每条被采纳的决策建立可追溯行动记录，八态生命周期（generated/viewed/accepted/deferred/dismissed/acted/completed/expired）；**行动与结果分离**——§5.4 自动结果观察在 7/14/30 天窗口回填互动变化，`estimated` 口径只描述观测、不声称因果；CRUD + 治理三处同步（登记表/备份/级联清理）。账本状态接入 AI Context 的 `PreviousActions`，让「为什么现在做」看得见历史。
- **P2/P3 · Decision→Action→Outcome 闭环（`decision.go`/`decision_ledger.go`）**：决策候选带 `action_log_id`/`ledger_status`/`decision_fingerprint`，指纹防重复开行动；决策被接受即落账本、观测周期到自动记结果、结果回流影响后续优先级——闭环真正打通。
- **P3 · Evidence Provenance 2.0（`evidence_provenance.go`）**：证据分六档 `evidence_type`，§7.4 关键词命中不再自动抬升置信度；§7.5 冲突证据召回——第一人称反转且不含值的判为 `conflict`、视图级 `HasConflict`（不改事实生命周期）。
- **P4 · Memory Review 待确认记忆（`memory_review.go`）**：§8 用确定性打分构建「需要确认的记忆」Top 队列（含冲突/低置信/久未确认），支持确认/否定/暂不三种处置；只读端点 `GET /api/memory/review?limit=N`。
- **P5 · Relationship Experiment 关系实验（`experiment.go`/`experiment_measure.go`/`experiment_api.go`，`relationship_experiment` v24）**：把「我做了某改变，关系是否更好」变成可对照的实验——§9.2/§9.3 前后对照日均互动、给出四类结论（明确标非因果），建/列/start/abandon/cancel/measure 全套端点。
- **P6 · Archive-aware 一致性审计（`audit`）**：修复 4 处日聚合重建门槛只数活跃表的问题——已整体归档的联系人在读路径误得 0；新增共享门槛 `hasMessagesForRebuildLocked`，并以 §10.3 归档前/后/恢复三阶段回归锁定。
- **P7 · Search / Contacts 深分页二阶段（`search`/`contact.go`）**：§11.1 Search 默认不再执行全表 `COUNT(*)`，`hasMore` 由多取一条推断、总数改 `includeTotal` 显式 opt-in；§11.3 Contacts 引入 keyset 游标（v25 `last_updated_unix` 普通列 + 触发器、`idx_contacts_updated_unix`、`GetContactsPageCursor`），深页不再随页深退化；旧 offset 版标 `Deprecated` 保留（§11.4），前端改走 cursor。
- **P8 · Relationship Risk Center 关系风险中心（`risk.go`/`risk_api.go`）**：§13 **不新建分数**，把 Health / State + 日聚合 + projects/followups 逾期 + §7.5 事实冲突 + 维护日历重要日子聚合为七类风险；§13.2 每条带数据来源/最近变化/涉及联系人/建议行动；降温/失衡/错过/冲突标 INFERENCE、不臆断因果；`severity` 分类排序；只读聚合端点 `GET /api/risks`（支持 type/severity 过滤），增值表缺失优雅跳过不 500。
- **P9 · Relationship Portfolio 关系投资组合（`portfolio.go`/`portfolio_api.go`）**：§12 用户设定每周关系时间预算，按既有信号（类别权重 × 亲密度 × 风险/机会/健康调节）建议本周投入分配；§12.2 只给建议——预算/类别权重/手指定均可覆盖，手指定优先占预算、余量以最大余数法整数分配；已使用为近 7 天行动名义分钟**估算**（`UsedIsEstimate`，不谎称精确）；`GET /api/portfolio`、`GET/PUT/POST /api/portfolio/settings`。
- **P10 · Action Center UI（`static/`）**：§14 首页主入口从「十几个洞察模块」改为**行动中心**——Today Top3 卡片（谁 / 为什么现在 / 做什么 / 最佳时机 / 相关目标与待跟进）配 [接受][稍后][已完成][忽略] 四操作，另加三条摘要 strip：待确认记忆（§14.1）/ 关系风险（§14.2）/ 本周时间建议（§14.3）。纯复用既有只读端点，静默降级，零第三方依赖。
- **桌面端 v3.2.0（独立仓库 caodabao99/wechat-profile）**：§15 在已有 Relationship State、Today Action 之外补齐第三入口 **Memory Review**（`remote.go`/`ui_relationship.go`）；远程模式调服务器 API，本地模式缺派生能力时明确提示「此功能需要连接 Relationship OS Server」、绝不静默失败。
- **治理与测试**：全 Phase 过同一门禁——`gofmt`/`build`/`vet`、linux/amd64·windows/amd64（`CGO_ENABLED=0`）交叉编译、U+FFFD=0；`-race -cover` 全绿，覆盖率 69.9% → **70.2%**；新增 §22 回归（context 版本/接管/缓存/行动账本/决策生命周期/指纹/outcome/证据溯源/事实冲突/记忆复核/实验/归档感知/游标分页/组合排序/风险聚合/合并撤销/删除级联/备份恢复）与 §23 深分页 Benchmark（场景5：keyset 深页 0.67ms vs OFFSET 14.29ms，约 21×，且 `includeTotal` 成本量化）。

### v6.0.0（2026-10-05）

本版为 **Personal Relationship OS 2.0**——把此前分散的 Profile / Facts / Evidence / Timeline / Metrics / Goals / Suggestions / Health / Topics 等能力整合为一条闭环链路：`原始消息 → 记忆 → 可信事实 → 关系状态 → 目标/项目 → 风险/机会 → 决策 → 行动 → 结果 → 学习 → 记忆更新`。全部遵循增量复用、不重写既有模块；确定性优先、LLM 缺席也能完整运行。

- **Phase 3 · Relationship State Machine（`statemachine.go`）**：为每个联系人派生「基态 / 动态态 / 亲密度 / 趋势 / 预警」的关系状态快照，状态跃迁写历史（`relationship_state`/`relationship_state_history`，均为派生表：不 bump `user_version`、入 `derivedTables`、访问时缺则自愈重建）。只读端点 `GET /api/relationships/state`、`GET /api/contacts/{id}/state`。
- **Phase 4 · Decision Engine（`decision.go`，本次核心）**：统一判断层，汇聚 State + Health + 待跟进 + 目标 + 重要日子 + 行动建议，以**确定性打分**（非 LLM 排序，`scoreDecision` 纯函数）产出「今天谁最值得投入时间、为什么、做什么」的 Top-N。端点 `GET /api/relationships/decisions/today`（别名 `GET /api/decision/today`）、`POST /api/decision/refresh`。每个候选带 `reason_codes`/`why_now`/`best_time`，可回答「为什么」。
- **Phase 5 · Relationship Projects（`projects.go`/`projects_api.go`）**：目标之上的高层关系经营单元（阶段 discovery/building/…/closing、状态 active/paused/completed/cancelled、下一步行动与到期）。懒建持久表 `relationship_projects`（用户创建的 core 表：**入备份、不入 `derivedTables`、含 `contact_id` 级联清理**）。CRUD 端点 `GET/POST /api/relationships/projects`、`.../projects/{pid}`（PATCH/PUT/DELETE），联系人作用域别名 `GET/POST /api/contacts/{id}/projects`。
- **Phase 6 · AI Context Engine（`context.go`）**：统一分层上下文构造器 `BuildContactContext`，按任务（Ask/Coach/Simulation/Decision…）施加差异化 token/消息预算，逐块自锁取数、任一缺失优雅降级、绝不全量历史。只读端点 `GET /api/contacts/{id}/context?task=&q=`（含 `rendered` 提示词文本，供可观测调试）。
- **Phase 7 · Memory Replay（`replay.go`，「重新认识 TA」）**：确定性拼装 12 段关系回放（首遇 / 阶段 / 转折 / 兴趣 / 职业 / 升温降温 / 共享事件 / 主题 / 当前状态 / 当前焦点 / 目标 / 未完成），**每条结论都带来源证据**、不依赖 LLM。端点 `GET /api/contacts/{id}/replay`（返回结构化 `replay` + `rendered`）。
- **Phase 8 · Desktop/Web 集成（`static/`）**：联系人详情头部展示关系状态 chip 与「重新认识 TA」回放弹层；联系人列表顶部新增「今天值得做」决策卡（确定性 Top-3，点名字直达）。纯 Vue3 + `go:embed` + CSS，零第三方依赖。
- **Phase 9 · 全量回归（`phase9_regression_test.go`）**：锁定删除/合并/备份恢复对新表的正确性——合并须把用户创建的 `relationship_goals`/`relationship_projects` 一并迁至目标且撤销精确还原（`merge.go` 补齐）、删除级联清理新表、核心用户表随备份往返而派生 `relationship_state` 不入备份、registry 元数据防漂移。
- **Phase 10 · 性能基准（`pagination_bench_test.go`）**：深分页 OFFSET vs keyset 游标对比 Benchmark + 确定性结果等价验收，验证 keyset 不随页深退化。
- **治理与测试**：数据层登记表 / 备份派生清单 / 联系人级联清理三处对每张新表保持同步；LLM 双重降级永不 503；`-race -cover` 全绿，覆盖率保持 ~69%，linux/amd64、linux/arm64、windows/amd64（`CGO_ENABLED=0`）交叉编译通过。配套桌面端 Web 对接轻量升级为 **Desktop v3.1.0**（独立仓库 caodabao99/wechat-profile）。

### v5.5.2（2026-10-05）

本版为**全库深度验证 + 缺陷修复**（无新增用户可见功能）：对整个代码库做系统性体检——用密码学已知答案向量与独立实现交叉校验安全核心、活体跑通全部变更类端点、并修复一处真实计数缺陷：

- **修复：`RecomputeOtherMsgCount` 非归档感知**（`merge.go`）。公共版此前只 `COUNT` 活跃表 `messages`、漏算 `messages_archive`，一旦对已归档的联系人调用就会把多年积累的对方消息数清零（事务版 `recomputeOtherMsgCountTx` 早已归档感知，两口径漂移）。现改为委派事务版（单趟事务、归档感知），彻底消除双份口径；新增用例：归档 3 条后重算仍为 3（旧实现会得到 0）。
- **备份加密安全测试**（`backup_crypto_test.go`，此前 0% 覆盖）：口令派生用一份独立的教科书 PBKDF2-HMAC-SHA256 实现交叉校验生产实现（120k 迭代逐字节相等）；AES-256-GCM 往返、错口令/篡改/截断/魔数不符一律拒绝、随机盐使同明文两次密文不同、空明文往返、`BackupNeedsPassword` 真实 zip 判定。
- **TOTP 安全测试**（`twofa_crypto_test.go`，此前 0%）：`totpCodeAt` 用 **RFC 6238 附录 B 已知答案向量**校验 HMAC-SHA1 动态截断（最易出 bug 处）；窗口容差、`totpNewSecret`、`otpauth` URI、常量时间 `tokenMatches`、网页会话生命周期（创建/校验/未知/过期清理/活动续期/吊销）。
- **核心纯函数测试**（`helpers_pure_test.go`）：`coachKindAction`/`clampSummaryDays`/`sortQualityDims`/`firstID`/`cleanList`/`headerSafe`（邮件头注入防护）/`extractQRPayload`/`orUnknown`/`ifEmpty`/`isNoSuchTable` 全 100% 覆盖。
- **DB 逻辑 + 变更端点活体扫描**（`db_logic_test.go`、`mutating_sweep_test.go`）：分页/筛选「总数与分页一致、limit 夹取、offset 归一、合并排除」；并把真实 mux 跑起来逐个走「联系人 CRUD」「合并→撤销」「标签建/改名/批量贴/删」「待跟进建/改状态/删」「归档 run→status→restore→settings」「备份导出」「日历密钥 rotate→clear」全链路，每步用直连 SQL 断言状态真的变了。
- 覆盖率 67.2% → **69.9%**（安全核心/纯函数升至 75–100%，多个写端点从 0% 到 45–75%）；`-race` 全绿，linux/windows/arm64 交叉编译（`CGO_ENABLED=0`）通过。
- 备注：核对 `go.mod` 发现后端实际使用 `glebarez/go-sqlite`、`go-resty/resty`、`skip2/go-qrcode` 等第三方依赖且 `go 1.25.1`；`backup.go` 中「为避开 go1.24 而手搓 PBKDF2」的注释系历史遗留（现 stdlib 已可用），但手搓实现经交叉校验正确，出于不改动已验证密码学的原则本版不动它，仅记录。

### v5.5.1（2026-10-05）

本版为**内部底座 + 缺陷修复**（无新增用户可见功能）：落地 Personal Relationship OS 路线图中优先级最高的两项数据底座，并修复若干真实 bug：

- **数据层登记表（Data Layer Registry，`registry.go` 新建）**：`TableMeta`/`TableRegistry()`/`GetTableMeta()`/`IsContactScoped()` 把全库各表的语义（core/derived/cache/audit/config）与备份/重建/是否含 `contact_id` 三轴收敛为单一事实来源。新增运维只读端点 `GET /api/system/table-registry`。
- **统一指标层（Unified Metrics Layer，`metrics.go` 新建）**：`GetAggregatedMetrics()` 把「按联系人、归档感知、窗口内」的小时直方与回复延迟中位收敛到一处（单趟锁、本地时区、缺归档表自动降级、纯读无副作用），`coach.go` 的时机/节奏/作息签名改从它取数，不再各自直查 `messages`。`medianSeconds` 为可脱库单测纯函数。
- **修复：删联系人回滚**。`contact_connections` 用 `contact_a/contact_b` 双列、无 `contact_id`，若误入级联清理清单会生成 `WHERE contact_id=?` 报 *no such column* 而让删除事务整体失败；已从 `contactCleanupTables` 移除（该表由 `contact.go` 专用 DELETE 处理），并加回归护栏单测。
- **修复：跨面板消息计数口径不一致**。`/api/status` 与 `/api/system/data-report` 改为从 `relationship_daily_metrics` 聚合计数后，刚升级、指标尚未建立时会误报 0 条；现与 health/heatmap 同语义在读路径内自愈重建（`ensureDailyMetricsSeededLocked`），并有真实 HTTP 活体用例验证。
- **修复/清理**：`coach_test.go` 峰值时段用例原插 `me` 消息与 `OtherHourHist`（「对方活跃时段」）语义矛盾，改为 `other`；删除重构后已无调用方的死函数 `hourWeekdayHist`。新增 `datalayer_test.go` 覆盖登记表与指标层（含清理表必须已登记且含 contact_id 的一致性护栏）。覆盖率 67.2%，`-race` 全绿，四目标交叉编译通过。

### v5.5.0（2026-10-05）

LLM 叙事 + 关系目标追踪（均为增量复用，不重写既有模块）：

- **关系叙事生成**（`narrative.go`，新建；`prompts/relationship_narrative.txt`）：`GenerateNarrative` 复用 summary 管道（`computeAchMetrics` 取相识年数/累计条数/首互动、`loadSummaryInputs` 取展示名/近期原文、`topNarrativeTopics` 取最新主题快照、`RenderPrompt`+`llm.CallContext`）写一段关系故事；**双重降级**——`!llm.configured()`/原文不足 `summaryMinMsgs`/渲染或调用失败/`cleanNarrativeText` 产物短于阈值时，回落为 `buildDeterministicNarrative`（纯函数、依事实拼成、不编造），**永不返回 ErrLLMNotConfigured/503**。`clampNarrativeDays`/`topNarrativeTopics`/`buildDeterministicNarrative`/`cleanNarrativeText` 均为可脱库单测纯函数。新增 `POST /api/contacts/{id}/narrative`（`routeContact` 加 `case "narrative"`）。提示词注册表 14→15（新增 `relationship_narrative`），计数连锁同步 `prompts_test.go`/`prompts_api_test.go`/`feature_sweep_test.go` 与本文档。前端驾驶舱新增「关系故事」卡（叙事正文 + 来源徽标 + 主题 chips + note）与一键 canvas 手绘分享图（`downloadNarrativeCard`/`wrapCanvasText` 逐字 measureText 换行、`toDataURL` 下载，纯前端零库）。
- **关系目标追踪**（`goals.go`/`goals_api.go`，新建）：持久表 `relationship_goals` 懒 `ensureGoals`（`CREATE TABLE IF NOT EXISTS`、不 bump user_version、不入 `derivedTables`→自动纳入备份/恢复）。`goalProgressPct`/`goalDateWindow`/`normalizeGoalMetric` 纯函数；`autoDetectGoalCompletions` 按窗口读 `relationship_daily_metrics` 的 `me_count` 自动判定达标，首次达成置 done、+XP、升级（复用 `gamification_state`/`challengeXPPerDone`/`levelForXP`）并锁外 `RecordContactEvent(...,"milestone",...)`；`CreateGoal`/`CompleteGoal`/`DeleteGoal`/`BuildGoals` 完整 CRUD、均幂等。单连接池三趟锁（读候选后 `rows.Close()` 再补算进度/写库/锁外补事件）绝不嵌套。新增 `GET/POST /api/assistant/goals`、`POST /api/assistant/goals/{id}/complete`、`DELETE /api/assistant/goals/{id}`（`routeAssistant` 加 `case "goals"` 子分发）。与「维护挑战」语义划清（挑战=系统建议、目标=用户自设）。前端助手页新增「关系目标」面板（进度条 + 完成勾选 + 新建表单），联系人详情新增「TA 的目标」子列表。

### v5.4.0（2026-10-05）

七项零 LLM 确定性新功能（均为增量复用、计算即读不落库、可脱库单测、前端手写 SVG 零第三方库）：

- **关系断点预警**（`health.go`）：`projectCooling`/`alertRank` 纯函数由近30天/前30天斜率 + 沉默天数前瞻预测「预计多少天进入沉寂」（`healthDormantHorizon=30`），分级 `none/watching/urgent`。`HealthItem` 新增 `alert`/`etaDays`，`HealthSummary` 新增 `alertCount`，`HealthDashboard` 新增 `alerts` 子集（按紧急度排序）；`GET /api/relationships/health` 响应向后兼容纯增量。前端健康面板新增「断点预警」子视图。
- **消息密度热力图**（`heatmap.go`，新建）：`buildHeatmap` 一趟锁内自愈重建 + 全年按日聚合 `relationship_daily_metrics`。新增 `GET /api/contacts/{id}/heatmap?year=YYYY`（`routeContact` 加 `case "heatmap"`）。前端驾驶舱新增 GitHub 贡献图式 53×7 SVG 网格（`heatmapGrid` computed 纯前端算格位）。
- **互动节奏分析**（`coach.go`）：`latencyStats`（回复间隔中位数+秒回率）/`timeSignature`（深夜聊友/早安伙伴/日间型，固定阈值）/`collectReplyLatencies`/`computeContactRhythm`。新增 `GET /api/contacts/{id}/rhythm`。前端驾驶舱新增「互动节奏」卡。
- **标签冲突检测**（`tagsuggest.go`/`tagsuggest_api.go`）：`tagConflictPairs` 互斥组常量 + `matchTagGroup`/`detectTagConflicts`（确定性、按 contactId 升序）/`collectAppliedTagsByContact`。新增 `GET /api/assistant/tags/conflicts`。前端标签管理弹层新增冲突面板。
- **对话风格镜像**（`mirror.go`，新建）：`lexicalRichness`/`avgLen`/`markerShare`/`emojiShare`/`mirrorDelta`/`mirrorTraits`（均按 rune 迭代、零依赖）/`loadMeTexts`/`buildMirror`。新增 `GET /api/contacts/{id}/mirror`。前端驾驶舱新增「风格镜像」卡（维度条 + 对比 delta）。
- **关系能量流桑基图**（纯前端、零后端改动）：`app.js` 新增 `flowSankey` computed（手绘 SVG 桑基，确定性布局：「我」→四层圈带→每层 Top-N 联系人，连线宽度∝score）与洞察页「能量流」子标签（复用 `loadCircles`）。
- **数据健康自检报告**（`datareport.go`，新建）：`buildDataReport` 一趟锁聚合 DB 体积/消息增速/画像覆盖率/各派生缓存表行数/最近备份；`api.go` 顶层新增 `routeSystem`→`GET /api/system/data-report`。前端状态页新增「数据健康自检」面板。
- **测试与门禁**：新增 `v54_features_test.go`（projectCooling/latencyStats/timeSignature/detectTagConflicts/lexicalRichness/avgLen/markerShare/emojiShare/mirrorDelta/mirrorTraits 纯函数边界与确定性）；`feature_sweep_test.go` 新增 5 个确定性端点用例（heatmap/rhythm/mirror/tags-conflicts/data-report）+ 响应键完整性断言。`go test -race -cover` 全绿。

### v5.3.0（2026-10-05）

- **关系健康度综合仪表盘**（`health.go`，洞察页新子标签）：新增只读端点 `GET /api/relationships/health?window=90`，`fuseHealth`（可脱库单测纯函数）为每位联系人融合亲密度基座（复用 `computeIntimacy`）+ 趋势因子（复用 `relationship_daily_metrics` 近30/前30 互动比）+ 情绪因子（`assistant_emotions`）+ 沉默衰减 + 单向惩罚，夹取 0-100 并分优秀/良好/一般/需关注/危险五档。`ComputeHealth` 批量一趟算全部、附全局均值/band 直方/最需关注榜；联系人上限护栏 300。计算即读、不落库、不新增表、不调模型。
- **智能关系圈层管理**（`circles.go`，洞察页新子标签）：新增只读端点 `GET /api/relationships/circles`，`assignTier`（可脱库单测纯函数）按互动频率（近90天活跃天）+ 亲密度确定性分核心/亲密/社交/弱联系四层（阈值 75/40/15、活跃天≥12），每层配差异化维护打法与节奏（7/14/30/90 天）；可选从 `GetCachedNetwork` 贴圈簇归属标签（无缓存则省略、不触发重算）。不重复 v5.0.0 打标签，是「分层视图 + 每层打法」的独立视角。
- **主动关系教练**（`coach.go`，关系助手页新面板）：新增只读端点 `GET /api/assistant/coach` 与 `GET /api/contacts/{id}/timing`。`computeContactTiming`+`hourWeekdayHist`（纯函数）从对方历史发言 `messages(∪archive)` 的 `msg_time` 聚成小时×周几直方（用 `datetime(msg_unix,'unixepoch','localtime')` 保时区正确），产最佳时段/星期，样本不足 8 条诚实降级；coach 只读装配周计划 Top 目标 + 行动/理由/话术（复用已有 draft、零新模型调用）/时机。前端拼接均在 JS 侧、规避插值陷阱。
- **关系维护挑战 / 游戏化**（`challenges.go`，关系助手页新面板）：新增只读端点 `GET /api/assistant/challenges` 与两张持久表 `weekly_challenges`/`gamification_state`（懒 `ensure`、不 bump `user_version`、不入 `derivedTables` → 依 `listRestoreTables` 动态纳入备份）。每周从「久未联系/待回复/核心圈」确定性挑最多 3 条（按 ISO 周 `INSERT OR IGNORE` 去重、跨周不重刷），读时按本周新增互动自动判定 `achieved`、首次达成累加 XP（等级 = 1 + ⌊xp/100⌋）并写一条 `kind="milestone"` 事件（复用成就机制）。`levelForXP` 为纯函数。
- **消息主题演化追踪**（`topics.go`，洞察页新子标签，唯一新增 LLM 功能）：新增 `GET /api/contacts/{id}/topics`（只读历史）/`POST /api/contacts/{id}/topics/analyze`（手动触发），并追加分支入助手周一调度（不塞进 LLM-free 的 `ComputeAdvancedInsights`）。对活跃联系人（上限 30、至少 12 条）按周让模型聚类聊天主题，产 emergent/persistent/fading 写入持久表 `contact_topic_history`（懒 `ensureTopicHistory`、每联系人裁剪近 104 周，仿 `quality_history.go`）。双重降级门控：仅当 `llm.configured()` 且 `AdvancedInsightsEnabled` 才跑，否则 `GET` 返回空 + note、不 500、绝不编造；坏 JSON 回退上周快照；`topic_evolution_state` 周锁防并发/坏模型重跑。新增第 14 个提示词模板 `prompts/topic_evolution.txt`（提示词注册表 13→14，同步更新 `prompts_test.go`/`prompts_api_test.go`/`feature_sweep_test.go` 与本文档）。
- **零依赖 / 单连接池纪律**：五个新功能只组装配接既有已上线原语（`computeIntimacy`/`relationship_daily_metrics`/`assistant_emotions`/`GetCachedWeeklyPlan`/`GetCachedNetwork`/`achievements`），不重写这些模块、规避 golden/`feature_sweep` 回归；#7/#6/#5/#8 全程零 LLM、可离线单测、结果可复现。全程分层取 `dbMu`、绝不嵌套锁（`computeIntimacy`/`RenderPrompt`/LLM 调用/`RecordContactEvent` 均按契约在未持锁时发起）。无任何第三方依赖，`go:embed static/` 覆盖前端、`//go:embed prompts` 自动纳入新提示词。
- **测试与门禁**：新增 `health_test.go`/`circles_test.go`/`coach_test.go`/`challenges_test.go`/`topics_test.go`（纯函数各因子与夹取、边界阈值、小样本诚实降级、跨周幂等、XP/事件不重复、未配模型降级、坏 JSON 回退、历史裁剪）；`feature_sweep_test.go` 新增 6 个确定性端点 + 1 个 LLM 端点用例。`go test -race -cover` 全绿、覆盖率 66.2%（不低于 v5.2.1 基线 65.8%）。

### v5.2.1（2026-10-05）

- **对话质量评分可视化 → 历史趋势追踪**（联系人驾驶舱）：既有四维雷达图（v4.8.0）已上线，本次补上「追踪历史趋势」这一维度——新增派生表 `contact_quality_history`，每次访问 `GET /api/contacts/{id}/quality` 时按 ISO 周幂等留存当周综合分快照（复用 `weekStartOf`，仅留近约 104 周），响应新增 `history` 字段；前端复用既有零依赖 `sparkPoints` 内联 SVG 折线呈现综合分走势与较上期环比。全程本地确定性计算、惰性采集不新增后台任务、best-effort 写失败不影响评分响应。
- **关系成就 / 里程碑系统**（联系人驾驶舱）：新增 `achievements.go` 与只读端点 `GET /api/contacts/{id}/achievements`，从聊天记录确定性自动派生三类里程碑——累计消息（100/500/1000/5000 条）、连续互动天数（messages ∪ archive 活跃自然日最长连续段，7/30/100 天）、相识周年（起点=首条消息时间，1/3/5 年）。首次跨越某档位即幂等写 `contact_achievements` 去重表 + 一条 `kind="milestone"` 的 `contact_events`，于是「时间线」标签自动呈现解锁历史；前端用成就卡片网格展示（达成高亮+解锁日期、未达成进度条）。`evaluateAchievements`/`longestStreak`/`wholeYearsSince` 均为纯函数、可脱离 DB 离线单测。
- **零依赖 / 单连接池纪律**：两张新表按持久化用户数据纳入备份恢复（不入 `derivedTables`，避免恢复清空去重表导致里程碑重复）；全程分层取 `dbMu`、绝不嵌套锁（`RecordContactEvent` 在未持锁时调用）。无任何第三方依赖，`go:embed static/` 覆盖前端。
- **核心模块补齐独立单测**（质量补强、无功能变更）：新增 `security_test.go`/`merge_test.go`/`ilink_test.go` 覆盖登录封禁与限流、联系人合并与撤销、iLink 客户端原子写与凭据往返。
- **发布链路去漂移**：`deploy/package_release.sh` 改为从主仓实时交叉编译、版本自动解析、`config.json` 模板自动生成；新增 `deploy/sync_bot_docker.sh` 把 Docker 构建上下文精确镜像主仓受版本控制源。
- **测试与门禁**：`quality_history_test.go`（按周幂等、升序、裁剪）+ `achievements_test.go`（streak 连续性、周年整年、成就检测幂等）。`go test -race -cover` 全绿、覆盖率 65.8%（不低于 v5.2.0）。

### v5.2.0（2026-10-05）

- **新增网页端「分析插件（提示词）」管理界面**（关系助手页底部）：基于 v5.1.0 落地的提示词引擎与 `/api/assistant/prompts*` 端点，提供可视化编辑体验：
  - 模板列表：13 个模板按注册表顺序展示，每项带标题、所属功能、「已自定义/内置默认」徽标、变量数与最近更新时间。
  - 行内展开编辑：以当前生效正文打底编辑，点变量 chips 在光标处插入 `{{key}}`，实时字节计数与 8KB 上限提示（超限/空时禁用保存），可折叠查看内置默认做对照。
  - 预览与回滚：一键预览用样本变量字面渲染（只读、不调模型）；「保存自定义」写入覆盖并即时刷新徽标，「恢复默认」一键删除覆盖回内置默认。
  - 竞态安全：展开详情用序号守卫，快速切换模板时丢弃旧慢回包；沿用全局 `api()` 的 401 会话失效处理与 `toast` 反馈。
- **纯前端、零新依赖**：`static/app.js`（`prompts` reactive + `loadPrompts/openPrompt/insertPromptVar/savePrompt/resetPrompt/previewPrompt`）、`static/index.html`（关系助手区新增 `pmt-*` 面板）、`static/style.css`（`.pmt-*` 样式、响应式）；无新增后端端点、不引任何第三方 JS 库。字面占位符在模板里统一经 JS `pmtToken()` 拼接（避免 Vue 插值解析器在字符串内第一个 `}}` 处提前闭合）。
- **测试**：`prompts_api_test.go` 补齐界面依赖端点的 HTTP 契约断言（鉴权、列表 13 项与字段、详情、合法覆盖保存、缺必填/未知变量/未知 key 拒绝、预览替换无遗留占位符、重置回默认、方法守卫 405）。`go test -race -cover` 全绿、覆盖率不低于 v5.1.0；`node --check static/app.js` 通过。

### v5.1.0（2026-10-05）

- **插件化分析引擎（第一阶）：LLM 提示词模板外置**。将全部 13 个 LLM 提示词模板（画像更新/补充/变化摘要、意图分析、提问关键词/作答、回顾摘要、情绪分析、待跟进抽取、关系开场白、周计划开场白、回复推演、节日祝福）从硬编码 `fmt.Sprintf` 外置为可编辑「分析插件」：
  - 内置默认逐字迁移至 `prompts/*.txt`，经 `//go:embed prompts` 内嵌；覆盖项存 SQLite 新表 `prompt_templates`（懒建、不进版本化 migrate、不 bump user_version）。
  - 渲染入口 `RenderPrompt(db, key, vars)`：读取优先级 DB 覆盖 → 内置默认；变量用字面占位符 `{{key}}`，以单趟 `strings.NewReplacer` 精确替换（非 text/template），杜绝模板注入、结果确定、且值内 `{{x}}` 不会被二次扫描。
  - 安全护栏：保存时校验必填变量齐备、拒绝未声明/游离 `{{}}`、空内容与 8KB 超限；渲染期遇非法覆盖记日志并回退内置默认，绝不因坏模板 500 阻断业务；未配模型时各站点原有 `ErrLLMNotConfigured`→503 降级不变。
  - 热加载：不做进程内缓存（LLM 调用稀少、多库场景下全局缓存不安全），每次渲染读一行主键即反映最新覆盖，改完即生效、无需重启。
- **新增 API**：`GET /api/assistant/prompts`、`GET /api/assistant/prompts/{key}`、`PUT /api/assistant/prompts/{key}`、`DELETE /api/assistant/prompts/{key}`、`POST /api/assistant/prompts/{key}/preview`（挂在 `routeAssistant` 下，复用既有认证）。
- **备份兼容**：`prompt_templates` 被 `listRestoreTables` 动态纳入（自动随备份）；`restorePreserveWhenAbsent` 新增该表，恢复旧备份（缺该表）时保留本机自定义、不清空。
- **行为无损**：`prompts_test.go` golden 逐字节对比（位置参数 `fmt.Sprintf` vs 命名变量 `RenderPrompt`）锁死 13 个模板默认输出与迁移前一致；另覆盖生命周期/校验拒绝/坏覆盖回退/防级联注入/注册表自洽。`feature_sweep_test.go` 新增提示词模板列表用例（63/63 全绿）。`go test -race -cover` 全绿、覆盖率 63.9%。零第三方库、`CGO_ENABLED=0` 四目标交叉编译不变。

### v5.0.0（2026-10-05）

**新增功能**
- **智能联系人自动分组**（`GET /api/assistant/tags/suggest?ids=1,2,3`）：基于画像（地域/职业/兴趣）与互动指标（亲密度分层、情绪告警、往来待跟进）用确定性规则为联系人产标签建议，每条带命中理由与置信度（亲密度≥70→核心关系 90、有 open money/promise→有往来待跟进 90、情绪 alert→近期需关心 85、地域→省市 80、职业关键词→行业 70、兴趣前 2 → 兴趣标签 60）。
- **一键/批量采纳面板**（标签管理）：建议面板列出联系人/建议标签/置信度条/理由，支持单条采纳与勾选全部采纳；采纳经 `POST /api/assistant/tags/apply` 走 `CreateTag`（幂等）+ `INSERT OR IGNORE contact_tag_links`，零新增写路径，采纳后即时刷新标签计数。

**工程与铁律**
- 纯确定性、可离线复现（同数据同参跑两次逐字段相等）、不调模型：`SuggestTags` 分层单锁读——contacts 一次锁、computeIntimacy 全量一次、TagsForContacts 已挂标签一次、assistant_emotions 一次、followup_items 一次，每段各自 `dbMu` 取锁不嵌套；已挂同名标签去重 + 批内去重，按 (contactId asc, confidence desc, tagName asc) 稳定排序。
- 护栏：ids 缺省全量截 `suggestMaxContacts=200`、每联系人 `perContactMax`（默认 4、硬上限 8）、apply 单次上限 500、请求体 64KB。`assistant_emotions`/`followup_items` 缺失则静默跳过对应规则不 500。不新增配置开关、不落新表。

**测试覆盖**
- `tagsuggest_test.go` 6 用例：确定性双跑 `reflect.DeepEqual` + 全规则命中 + `perContactMax` 截断 / 已挂同名标签去重 / 缺表静默跳过不阻塞其它规则 / apply 幂等（首次 1、重复 0、link 唯一、批量混合新旧只新增未挂部分、联系人不存在报错）/ apply 超限拒绝 / ids 缺省全量护栏。`feature_sweep_test.go` 新增标签智能建议用例（62/62 全绿）。`go test -race -cover` 全绿、覆盖率 63.9%（>上一版本 63.6%）。

### v4.9.0（2026-10-05）

**新增功能**
- **智能回顾摘要**（`POST /api/contacts/{id}/summary` body `{days}`）：选定时间窗口后基于窗口内真实聊天原文，让模型产一份结构化回顾——`overview` 一段话总结 + `topics` 话题 chips + `todos` 待办列表（每条带 `[n]` 出处、可跳转定位原文、可一键转跟进）。
- **cockpit 「回顾摘要」卡片**（联系人驾驶舱）：时间窗选择 7/30/90/180 天 + 生成/重新生成 + 展示 overview 高亮块 + topics chips + todos 列表 + 出处内联卡（复用 ask 出处跳转 与 待跟进写入路径），零新增写路径。

**工程与铁律**
- 复用 ask.go 两段式框架与降级：`!llm.configured()` → `ErrLLMNotConfigured` → HTTP 503，绝不编造；窗口内原文不足 4 条时如实返回空摘要 + note；模型返回坏 JSON 回退为原文摘录，不新增信息。
- 单连接池、分层取锁、不嵌套：GetContactByID 一次锁 + 消息查询一次锁；消息扫描上限 400 条护栏、prompt 上限 120 条、单条原文截断 200 字、topics≤ 8、todos≤ 12、ref 越界自动清空。一次性返回不落库、不新增开关。

**测试覆盖**
- `summary_test.go` 5 用例：未配模型 503 / 桩 LLM 全链 200 验证 overview+topics+todos+sources+note / 消息不足空摘要不硬编 / 桩坏 JSON 回退不 panic / 服务层 days clamp 与联系人不存在。`go test -race -cover` 全绿、覆盖率 63.6%（>上一版本 63.4%）。

### v4.8.0（2026-10-05）

给单个联系人一个量化的“对话质量”视图：从回复及时性、对话深度、话题多样性、情绪正向度四个维度各打 0-100 分并加权出综合分，前端用手写内联 SVG 雷达图 + 维度条呈现。严格延续本仓铁律——go:embed 单文件 Vue3、无构建工具、不引入任何重型第三方 JS（无 Chart.js/ECharts）、纯确定性。新增一个只读端点，不新增数据库表、不依赖模型。

新增功能

- **对话质量评分端点**：`GET /api/contacts/{id}/quality?days=90`（默认 90 天、上限 3650、非法/越界自动夹回），返回 `{contactId,name,score,dims:[{key,label,value,detail}],windowDays,generatedAt}`。四维均在本地按 `messages` 时间线与画像确定性计算：回复及时性取「对方→我」回复间隔中位数（口径同社交大盘，超 6 小时不计为回复，样本 <3 向中性值收敛并标注仅供参考）；对话深度按活跃天数/消息量/对方均长加权（沿用亲密度 clamp 范式）；话题多样性取画像里兴趣/性格/重要事实/口头禅去重概念数；情绪正向度取 `assistant_emotions` 窗口内均分
- **情绪为增值表、缺失优雅降级**：`assistant_emotions` 表不存在或窗口内无记录时，情绪正向度记 `value=-1` 并在 `detail` 诚实标注“未计入综合分”，综合分只由其余三维按固定权重加权后再按比例归一；主流程绝不因缺表而 500
- **自研雷达图**：联系人“驾驶舱”页顶部新增“对话质量”卡片。四轴（及时/深度/多样/正向）手写内联 SVG：25/50/75/100% 网格环 + 数据多边形 + 轴标签，大号综合分 + 四维进度条（按分数 good/mid/poor 着色）+ 维度明细说明；可切窗口（30/90/180/365 天）刷新，无数据维度半径归 0 塌向圆心诚实反映“缺项”

工程与铁律

- 严格单连接池：联系人+消息时间线一趟锁取尽、情绪一趟锁独立读取、画像在锁外解析，绝不嵌套锁；`dbMu` 外不做长计算
- 确定性：同输入两次调用逐字段相等（`Score` 与 `Dims`）；不依赖 map 迭代顺序、不用随机数
- 新增单测：四维确定性与逐字段可复现、取值落 0-100（情绪缺失记 -1）、缺 `assistant_emotions` 表静默跳过且综合分仍由三维归一、窗口 clamp、空数据联系人不 panic 且 dims 恒为 4 个、空切片非 nil；活体扫描新增 `/api/contacts/{id}/quality`（端点总数 60→61）

### v4.7.0（2026-10-05）

把已有日期数据（生日/纪念日、手动大事记、待跟进）聚合成一个统一的“关系维护日历”视图。严格延续本仓铁律——go:embed 单文件 Vue3、无构建工具、不引入任何重型第三方 JS（无 FullCalendar）、纯确定性。新增一个只读端点、`followup_items` 走懒建表的幂等 ALTER（不 bump user_version、不改 backup.go）。

新增功能

- **关系维护日历端点**：`GET /api/assistant/calendar/events?from=&to=`（默认当前自然月，Bearer+IP 白名单、只读）把三类已带日期事件聚合为统一的 `[]CalendarEvent{date,kind,title,contactId,name,meta}`，按 date 升序、tie 按 contactId 确定排序。生日/纪念日取自画像 ImportantDates、在窗口内逐年展开（2/29 在平年回退 2/28）；大事记取 `contact_events`（按 event_time 前缀日期）；跟进取 `followup_items` 中 `status='open'` 且 `due_date` 落窗口内。三个数据源各自取一次连接锁、绝不嵌套，value-added 表缺失时该源静默跳过
- **待跟进截止日期**：`followup_items` 新增 `due_date` 列（懒建表幂等 ALTER：先 `PRAGMA table_info` 判列、缺则 `ADD COLUMN`、容忍 duplicate column）；`AddFollowup`/`ListFollowups` 读写该列，POST body 接可选 `dueDate`（严格 YYYY-MM-DD 校验）；前端待跟进表单新增 `<input type="date">`、列表项展示截止徽章
- **自研月历网格**：关系助手页“日历订阅”下方新增“关系维护日历”卡片。固定 7 列网格、补齐邻月整周（置灰不可点）、月切换/回到本月、每日格内按 kind 着色事件 chip、点 chip 跳转联系人详情。高亮今日、本月事件计数与图例

内部改进

- 日历聚合严格单连接池：生日源锁内取尽 contacts 再解锁逐年展开，大事/跟进源各自取锁顺序完成；窗口护栏最多 730 天防逐年展开循环被越界参数拉大
- 新增单测：due_date 往返/格式校验/幂等 ALTER 补列且重复 ensure 不报错；calendar/events 三源聚合、窗口边界外不出现、kind/date 正确、确定性排序与逐字段可复现、2/29 平年回退 2/28、已完成跟进不入历；活体扫描新增 `/api/assistant/calendar/events`（端点总数 59→60）

### v4.6.0（2026-10-05）

把 v4.5.0 已算好的社交网络图从“文字洞察”升级为“视觉洞察”：将图算法内部已有的逐节点中间态（圈簇归属、割点、介数重要度、风险）与连线拓扑完整导出，前端用自研内联 SVG 力导向图交互式呈现。严格延续本仓铁律——go:embed 单文件 Vue3、无构建工具、不引入任何重型第三方 JS（无 D3/Cytoscape）、纯确定性。只改 `GET /api/insight/network` 响应体（加性字段 nodes/edges），不新增端点、不改数据库结构。

新增功能

- **关系知识图谱可视化**：`NetworkInsight` 新增 `nodes`/`edges` 两个加性字段（逐节点 contactId/姓名/圈簇/风险/归一化介数/是否割点/是否脆弱/度数，逐边端点/权重）；buildNetwork 在算完圈簇/割点/介数后加一个确定性第二遍，按联系人 id 升序输出节点、按 (A,B) 升序输出无向边（与读取/遍历顺序无关，可复现）；边权重取 `contact_connections.confidence` 作互动强度代理
- **自研力导向图**：前端在洞察页“社交网络”子页的文本洞察上方新增“关系网络图”卡片。确定性初值（节点按 index 均布圆周、不用 Math.random）+ 固定 300 步退火迭代（库仑斥力 O(n²) + 弹簧 + 中心引力）；支持滚轮缩放、拖背景平移、拖节点微调、悬停 tooltip（姓名/圈子/风险/重要度/连接数）、点击节点跳转联系人详情；节点半径∝度数、连线粗细∝权重、颜色按圈簇取调色板、割点红环、脆弱（割点且高风险）红填充

内部改进

- 现有文本消费方（圈簇/桥梁/撮合列表）不受影响（nodes/edges 为加性字段）；空图/未成圈时 nodes/edges 为非 nil 空数组（前端 `.length` 直读不崩）
- 新增拓扑导出单测：节点数=`nodeCount`、逐边两端均在节点集且 A<B、degree 与邻接一致（sum=2×边数）、betweenness∈[0,100]、cluster 索引合法、桥节点为割点、Fragile 蕴含 Articulation；确定性（同输入两次逐字段全等）与缓存往返一致；活体扫描断言 `/api/insight/network` 响应含 nodes/edges 键（端点总数仍 59）

### v4.5.0（2026-10-05）

给 v4.4.0 的高阶洞察四件套长出“时间维度 + 自我校准”：把瞬时快照变长走势、让结果数据反向校准优先级、为敏感关系数据补上可携与遗忘能力。全部确定性、可解释、默认不调大模型、不产生意外费用。升级只需替换二进制并重启，数据库结构向后兼容（`user_version` 升到 18，新增一张趋势历史表启动时自愈创建）。趋势与闭环沿用现有高阶洞察开关，不新增开关。

新增功能

- **洞察趋势与周环比**：新增 append-only 周快照表 `insight_trend_history`，每周一随四件套追加一行聚合标量（只留近 52 周）；各子页头部指标旁渲染自包含内联 SVG sparkline 走势；简报顶部新增“本周关键变化”块（如“高风险关系 3 段→5 段”“你先开口率 71%→64%↓（你更被动了）”）——从“看当下”升级到“看走势”，首周无基准时优雅跳过
- **跨层闭环自校准**：复用已有干预回测表（`suggestion_outcomes`，零新表），取每联系人近 90 天最近一条已回测结论，让“上次建议是否奏效”反向重排简报 Top 行动（已回暖沉底不重复催、无改善浮顶）并在行动/桥梁描述里附“上次建议已见效/仍无改善”标注；无回测数据时严格等同 v4.4.0 行为
- **图计算规模护栏**：网络图算法进 Brandes/Tarjan 前对节点数封顶（`netMaxNodes=250`），超出则按亲密度取最亲密的 250 人核心圈（tie-break 按联系人 id 升序保确定性）、过滤悬边并重建图，置 `truncated`/`nodeCap` 字段并在洞察里追加“已聚焦核心圈”说明，防联系人增长后周一重算拖慢
- **数据可携与遗忘**：新增 `GET /api/data/export` 全量开放 JSON 导出（流式逐表直写避免全库驻留内存；`redact=1` 可选脱敏：`contacts.name/remark`→`联系人#<id>`、消息正文抹除为 `***`、保留结构/计数/时间戳）；并把“彻底删除联系人”的级联补齐隐私残留（`relationship_daily_metrics`、`profile_facts`、`profile_fact_evidence`、`contact_connections`、`suggestion_outcomes`、`relationship_action_suggestions`）

内部改进

- 新增趋势历史表 `insight_trend_history`（`week_start` 主键、一周一行幂等覆盖），属派生便利层：不纳入备份、恢复末尾清空、历史重启可接受；DB `user_version` 升到 18，`backupCurrentDBVer` 同步
- 趋势追加挂在 `ComputeAdvancedInsights` 流水线末尾（简报之后），不新增调度器、复用现有周一高阶洞察块；读缓存/读历史均在各写锁之前顺序完成，单连接池下绝不嵌套取锁
- 新增 2 个 API（`/api/insight/trend`、`/api/data/export`），网页端高阶洞察子页加 sparkline 与“本周关键变化”块、设置区加“导出全部数据”按钮与脱敏复选框
- 趋势/周环比/闭环排序/图截断/导出脱敏均附确定性、边界与幂等单测，活体扫描端点从 57 增至 59

### v4.4.0（2026-10-05）

在“人生模拟器”基础上新增**高阶洞察四件套**：社交网络智能 / 自我关系画像 / 证据化干预学习 / 主动人生简报，均入现有“洞察”页的四个新子页（进入默认落地到“本周简报”）。全部由系统主动算好、人只看结果，纯本地确定性计算、默认不调大模型、不产生意外费用。升级只需替换二进制并重启，数据库结构向后兼容（新增四张派生表启动时自愈创建，旧库无需迁移脚本）。高阶洞察默认开启，可在关系助手设置里关闭。

新增功能

- **社交网络智能**：把“谁和谁可能认识”的两两关联升级成一张完整网络，跑纯确定性图算法（BFS 连通分量、固定轮次标签传播 LPA 圈簇、迭代式 Tarjan 割点、Brandes 介数中心性）——自动识别各个圈子、找出删掉就会让网络碎片化的关键桥梁人物、给出网络脆弱度评分与跨圈撮合引荐，每条附可解释依据
- **自我关系画像**：从全体关系里反照出你自己——谁总在先开口（全局 + 按类别）、近 30 天精力正从哪个类别迁向哪个类别、有多少关系几乎只有你一厢情愿（高失衡且你付出显著更多），以及整体社交风格标签（主动/被动、深耕/广撒网、偏向哪类人）。零新增采数
- **证据化干预学习**：用已有的隐式反馈回测数据学“对哪类人做什么真有效”——按干预方式（主动破冰问候/久未联系后重启/回复未接消息/重日送祝福/兑现待跟进承诺）分组算回暖率，附 **Wilson 95% 置信区间**；样本不足（<10）时诚实降级只报基线率不假装精确，按置信下界×覆盖人数排出“被验证有效的动作”
- **本周简报**（默认落地页）：把人生模拟器 + 社交网络 + 自我画像 + 干预学习四层揉成一页“打开即行动”的每周摘要——Top-3 本周最该做的事（每条指向具体人、附来源层与依据）、全局健康度一行、最强信号；纯确定性模板文案，每周一自动随四层刷新

内部改进

- 新增四张派生缓存表（`network_insight_cache`、`self_portrait_cache`、`intervention_cache`、`briefing_cache`，DB `user_version` 升到 17），不纳入备份、恢复末尾清空、访问时缺则自愈重建，沿用周计划/人生模拟器的骨架
- 四层能力 + 简报编排均严格“一次取尽进内存再聚合”、缓存在锁外取，单连接池下绝不边 rows.Next() 边写、绝不嵌套取锁；简报编排层只读各层缓存、缺层优雅跳过，任一子页首次打开都能自愈拿到一致且完整的一批数据
- 四层并入既有周一触发点（零新增调度器），用 `IsBriefingStale` 天然周锁隔离，不占用周计划的 `lastWeekly`，互不影响幂等语义
- 新增 5 个 API（`/api/insight/{network,self,intervention,briefing}` 只读 + `/api/insight/recompute` 异步重算）；图算法/画像/Wilson/编排均附确定性、边界与幂等单测

### v4.3.0（2026-10-04）

把产品从“关系提醒工具”升级为“人生模拟器”：新增**人生总览 / 未来推演 / 人生年表**三个洞察子页，全部由系统自动算好、打开即看。升级只需替换二进制并重启，数据库结构向后兼容（新增两张派生表在启动时自愈创建，旧库无需迁移脚本）。

新增功能

- **人生总览**：把每段关系折算成一份“人际资产账本”——余额（关系深度）、现金流（近 30 天互动相对历史基线的增值/缩水）、折旧（断联天数 + 近 90 天冷却斜率）、风险分（陈旧 + 负趋势 + 主动性失衡合成）与象限分类（挚友/好友/熟人/弱关系/事务型）；并汇总人际总财富、本季涨跌、依赖集中度（HHI）、高风险将断清单，以及时间精力回流的类别占比偏差洞察（如“家人占比从 20% 降到 6%”）。纯 SQL 聚合现有数据，零模型开销
- **未来推演**：按当前互动趋势把每段关系确定性地快进到 90 天后（可解释、可复现、每条附“为什么”），算出“什么都不做→N 段关系将自然断掉”；系统自动生成三条 what-if 对比（维持现状 / 每周联系高风险清单一分钟 / 把时间往家人身上移），每条给出“可多保住 M 段 / 总财富 +X%”。人只看结果、不拨旋钮；数据不足（<30 天历史）的联系人标注“暂不预测”而非乱推
- **人生年表**：从最早一条对话、按年活跃度、跨年最长情的关系、最近在意的人与你手动记下的大事，自动聚合出里程碑年表；可选一句模型文案润色，未配模型时只出结构化里程碑、不报错

内部改进

- 新增两张派生缓存表（`life_state_cache`、`life_projection_cache`，DB `user_version` 升到 16），不纳入备份、恢复末尾清空、访问时缺则自愈重建，完全沿用周计划的骨架
- 周计划与人生模拟器共用周一触发点（零新增调度器），但各自独立加锁：周计划用 `lastWeekly` 周锁，人生模拟器用缓存新鲜度作天然周锁，互不影响幂等语义
- 人生状态/推演聚合在单连接池下均“先取尽结果再写库”，并在锁外取缓存，避免边读边写与嵌套取锁死锁
- 前端并入现有“洞察”页新增 3 个子页（不新建一级导航），设置卡加 `lifeSimEnabled` 开关（默认开，零配置）

> 提醒：人生模拟器全部能力纯本地规则计算、不调大模型、不产生意外费用；如不需要可在关系助手设置里关闭“启用人生模拟器自动计算”。

### v4.2.0（2026-10-04）

三个“系统主动做、人只看结果”的能力落地：关系助手从“给建议”升级为“每周排行 + 自动回测效果 + 看见人脉关联”。升级只需替换二进制并重启，数据库结构向后兼容（新增表在启动时自愈创建，旧库无需迁移脚本）。

新增功能

- **每周维护计划**：每周一调度器自动从活跃建议里算出“本周最该联系的 5 个人”（按优先级 + 亲密度排序），可用时由大模型草拟一句自然开场白，结果缓存在看板“本周维护计划”卡片直接展示。只在看板呈现、不发邮件；也可点“立即重算”手动刷新
- **隐式反馈闭环**：粘贴聊天记录入库时，若检测到你已经发过消息（sender=me），系统自动把对应建议标为“已执行”并记下执行前的互动趋势基线；14 天后自动回测互动是否回暖，得出改善/持平/恶化结论，看板底部展示“近 90 天建议采纳率与回暖率”。全程无需你点任何按钮
- **关系图谱**：从各联系人画像里交叉比对共同城市、同类职业、共同兴趣、以及“画像提到过彼此”，纯 SQL 推导出人际关联边（不额外调用模型）。洞察页新增“关系图谱”子页，展示谁和谁可能认识、可信度多高；结果表首次访问时缺则自愈重建

内部改进

- 新增两处派生表（周计划缓存、建议回测结果、关系连线）均不纳入备份，升级/恢复后自动重建，旧库平滑过渡
- 关系连线与周计划排行在单连接池下改为“先取尽结果再写库”，避免边读边写死锁

> 提醒：每周维护计划的开场白会调用大模型、按量产生费用；如不需要可在关系助手设置里关闭“启用每周维护计划”。

### v4.1.0（2026-10-04）

聚焦“关系助手到底怎么用”的一次体验收敛：去掉冗余的选择项，让助手开箱即是“主动帮你维护关系”的高灵敏行为。升级只需替换二进制并重启，数据库结构向后兼容。

功能调整（精简）

- **下线「运行模式」多预设选择器**：家人/好友/同事/客户/静音专注五套预设对大多数人是冗余负担。关系助手的定位本就是主动维护关系，因此直接将该功能整体移除（前后端与预设表），不再需要“先选模式”这一步
- **助手默认改为“真·客户档”高灵敏**：开箱即为沉寂 14 天即判降温、生日提前 5 天、情绪预警与待跟进 AI 自动抽取默认开启。总开关仍需你在配好 SMTP 后主动启用，避免未配置邮箱就后台空跑/产生模型费用
- **移除网页端「命令说明」页**：微信命令说明由微信端（发「帮助」）单一来源提供，网页端不再复制一份，避免两处维护不一致
- **关系助手页新增「三步上手」引导**：配邮箱→启用开关→看结果/收邮件的使用主线就地说明，功能不再“看不出怎么用”

> 提醒：情绪预警与待跟进自动抽取现在默认开启，会调用大模型、按量产生费用；不需要时可在关系助手页随时关闭。
> 旧版已创建的 `mode_presets` 表升级后保留不动，不影响备份/恢复。

> 详细变更与部署步骤见上文各功能章节。旧版本说明见下方 v4.0.0 及更早条目。

### v4.0.0（2026-10-04）

大版本更新：新增多项网页端能力、云端部署体验与全面的响应式适配，并修复一批跨端缺陷。升级只需替换二进制并重启，数据库结构向后兼容（新增表/列在首次访问时自愈创建，旧库无需迁移脚本）。

新增功能

- **运行模式预设**：把当前助手表单/阈值存成「客户 / 家人 / 朋友」等命名预设，一键切换不同关系的提醒风格；预设只存阈值不存密码/日历密钥，切换不会覆盖你的 SMTP 凭据
- **关系趋势与驾驶舱**：联系人维度的互动趋势、久未联系/冷却回暖判定与下一步行动建议集中呈现
- **「问 TA」**：基于已生成画像与真实聊天证据回答你关于某个人的具体问题，并附来源消息可点回看
- **周期性洞察报告**：支持按周/月/年多周期聚合生成可分享长页报告（年度/社交大盘延续增强）
- **云端公网访问地址自动探测**：未配置 `webBaseURL` 时自动探测服务器公网出口 IP 生成可点开的网页地址（`网址` 命令）；探测并发化、失败回退局域网 IP

稳定性与安全修复

- 模式预设应用会保留真实 SMTP 密码与日历密钥，不再被内置空凭据覆盖
- 网页助手设置保存时不再重置关系趋势阈值（静默天数/冷却/回暖），掩码凭据正确回填
- 分页查询补回消息 `id`，修复按 id 去重/定位偏差
- 网页登录响应/预设列表对密码与日历密钥一律打码，杜绝凭据外泄
- 助手调度器每周期加异常隔离，单次任务 panic 不再中断后续提醒
- 备份/恢复增强：缺表时不清空全局配置表

网页端全面响应式适配（PC / 平板 / 手机）

- 登录页大按钮修复（一处 CSS 类名撞车曾把按钮压塌）
- 日志/归档/可信设备等宽表格在窄屏改为卡内横向滚动，操作按钮不再被裁切
- 手机端触控目标放大、刘海/圆角屏安全区避让

> 详细变更与部署步骤见上文各功能章节。旧版本说明见下方 v3.2.0 及更早条目。

### v3.2.0（2026-10-04）

新增网页端功能：**对话预演**。升级只需替换二进制并重启，数据库结构无变化。

- 联系人详情页新增 **预演** 子页。描述一个要跟对方开口的场景（谈涨薪、拒绝借钱、表白……），AI 按这个人的画像与真实聊天语气扮演他跟你对练，可选「我先说」或「对方先说」
- 每条回应都带一个情绪标签，让你看到自己这句话把对方推到了什么状态；模型被明确要求不要为了配合你而软化立场
- 随时点 **结束并复盘**，AI 以沟通顾问身份退出角色，给出做得好 / 有问题 / 真去谈可能踩的坑 / 可以这么说，以及 0~10 分的达成可能评分；整场对话可一键复制
- 扮演依据来自画像（性格、沟通风格、口头禅、最近的事、雷区等 17 项）加上最近 30 条真实消息里挑出的说话样例，会在页面上原样展示出来供你核对
- **完全无状态**：预演过程不写数据库、不发消息，刷新页面或切换联系人即清空；后端对请求体做了 512KB、单条 2000 字、最多 60 轮的上限约束
- 新增接口 `GET /api/contacts/{id}/rehearsal/context`、`POST /api/contacts/{id}/rehearsal/turn`、`POST /api/contacts/{id}/rehearsal/review`（均要求已认证且配置了 LLM）

### v3.1.1（2026-10-04）

维护版本，不含新功能，集中修复 v2.4 以来累积的缺陷。升级只需替换二进制并重启，数据库自动迁移（`user_version` 升到 7，兼容旧库）。

**修复：数据一致性**

- 撤销合并联系人时，被合并方的**标签、日历事件、待跟进事项**之前不会一起回滚，导致撤销后留下孤儿数据；现在合并前把这三类记录快照进 `merge_log.undo_extra`，撤销时原样恢复
- 归档消息现在保留原始 `id`，恢复（`/api/archive/restore`）后与归档前完全一致，不再出现 id 漂移
- 归档调度器支持随进程退出而停止，并加了 `recover`，单次归档 panic 不再拖垮整个后台循环

**修复：时区与排序**

- 周报统计里按 SQL `CURRENT_TIMESTAMP` 写入的时间没有加 `'localtime'` 换算，跨时区部署时统计区间会偏移
- 归档判定和消息排序统一改用 `strftime('%s', msg_time)` 比较。`messages.msg_time` 是带时区偏移的 RFC3339 字符串，直接按字符串比较在混合偏移的库里会排错
- 社交活跃度面板的日期现在按时间先后输出，不再依赖 map 遍历顺序

**修复：安全**

- 邮件发送加了 SMTP 超时（拨号 / 握手 / 数据阶段），发件服务器无响应时不再无限期阻塞提醒任务
- 邮件头字段做 CRLF 过滤，主题和收件人里的换行不能再注入额外邮件头
- 日历订阅链接的 key 改用定长比较（`subtle.ConstantTimeCompare`）
- 可信设备令牌文件改为临时文件 + `rename` 原子写入，写入中途崩溃不再留下半截 JSON；解析失败的文件重命名为 `.corrupt` 保留现场，而不是直接丢弃全部可信设备
- 归档接口收到非法请求体时返回 400 并说明原因，不再当成「使用默认配置」静默执行

**修复：网页端**

- 看板加载时若某个统计接口返回空，整页会白屏（读 `undefined.length`）；现已加守卫，单块数据缺失只影响那一块
- 退出登录后改为整页刷新，避免上一个账号的联系人详情、草稿等状态残留到下一次登录
- 切换联系人时补齐弹窗和忙碌态的重置，之前快速连点两个联系人会看到上一个人的弹窗内容
- 联系人详情、聊天历史、统计、标签、归档等加载函数加了请求序号，快速切换时慢返回的旧请求不会再覆盖新数据
- 关系助手和归档设置里的数字输入框：清空后保存会送出空串，后端只回一句笼统的「请求体解析失败」；现在发请求前就拦下，提示具体是哪个字段、合法范围是多少，且上下界与后端校验一致
- 「立即归档」的确认框显示的是输入框里的天数，但实际发的是空请求体、后端按**已保存**的配置执行，两者可能不一致；现在把确认的天数一起提交
- 会话过期后停止后台轮询，不再反复弹出登录失效提示；只有 401/403 才清除本地令牌，网络故障不会把用户登出
- 之前静默失败的几处请求改为可见提示，归档和标签加载失败后页面上直接给出重试按钮
- 保存联系人画像成功后的收尾刷新若失败，错误信息会写进已经关闭的弹窗并残留到下次打开；现在刷新失败不再污染错误位
- 样式表补齐 `.hint`、`.hint.block`、`.list-head` 三条一直在被引用却没有定义的规则（提示文字此前和正文一样大、联系人页与合并记录页的标题行按钮会掉到下一行），并修正 6 处引用了不存在的 CSS 变量导致的边框丢失
- 登录页的「幽灵按钮」样式此前没有作用域前缀，`width:100%` 泄漏到全站所有 `.btn.ghost`；已限定在登录卡片内

### v3.1.0（2026-10-03）

**新增：待跟进事项（承诺 / 借钱 / 待回复）**

- 网页端「关系助手」页新增待跟进卡片，可手动记一笔（类型分承诺、金钱往来、待回复），带金额、截止说明
- 也可让 AI 从最近聊天里扫出「我答应过的事 / 借钱还钱 / 该回复没回复」，自动抽取默认关闭（有模型开销），开启后每天最多扫 8 个联系人、只看最近 30 天消息（均可配）
- 未完成项每天随提醒邮件一起推送，网页端可标记完成 / 忽略 / 重新打开，也可直接删除

**新增：联系人标签分组**

- 自定义标签（可改名、可删除），联系人详情页可勾选标签，列表页显示标签 chips 并支持按标签筛选
- 列表页支持勾选多人批量打标 / 批量去标（单次上限 2000 人）

**新增：聊天记录全文搜索**

- 网页端新增「洞察」页，第一个子面板就是全文搜索：跨全部联系人按关键词搜消息，可限定联系人、起止日期
- 可选一并搜已归档消息（默认只搜活动消息），支持翻页加载更多

**新增：联系人时间线**

- 联系人详情页新增「时间线」子页，按时间倒序汇总首次联系、画像更新、合并、手动记录的大事
- 可手动补记事件（标题 + 详情 + 时间），可删除；派生节点自动生成，不需维护

**新增：重要日子日历订阅（.ics）**

- 「关系助手 → 日历订阅」一键生成独立密钥，得到一条 `.ics` 订阅地址，手机/电脑日历客户端直接订阅
- 联系人的生日、纪念日按 RFC5545 导出为全天、每年重复的事件；订阅地址不走 Bearer，靠 URL 密钥鉴权（仍受 IP 白名单约束），可随时重置或关闭
- 密钥只显示一次（后续查看是打码值），泄露后重置即可

**新增：重要日子 AI 祝福语草稿**

- 重要日子临近时按联系人画像和你的说话风格生成 3 条草稿，网页端一键复制，**不代发**
- 提醒邮件里是否附草稿由开关控制，默认关闭

**新增：疑似重复联系人推荐**

- 「洞察」页按昵称/备注相似度扫出可能是同一个人的联系人对，给出建议保留项和判定理由，一键跳去合并页

**新增：年度关系报告**

- 选年份一键生成：消息量、最活跃的一天、月度走势、情绪曲线、亲密度变化、高频关键词、大事记
- 可导出为独立 HTML 长页另存分享（内联样式，不依赖服务端）

**新增：我的社交大盘**

- 全量联系人的互动统计：24 小时活跃分布、周几分布、双方平均回复速度、谁先开口的比例、你的高频用词
- 统计窗口可选（默认 30 天，最长 3650 天），纯本地计算、不调用模型

**修复**

- 修复备注为空（NULL）的联系人被静默跳过的问题：会导致生日提醒邮件和日历订阅对绝大多数联系人生效不了
- 修复年度关系报告在无数据时把情绪曲线、亲密度变化、大事记返回成 `null`（前端读不到 `.length`）的问题，统一返回 `[]`

**升级说明**

- 全部为网页端增值功能，默认不改变现有行为：AI 抽取待跟进、AI 祝福语草稿默认关闭，日历订阅不生成密钥就不生效；`config.json` 格式不变，直接替换二进制/镜像升级即可
- 新增的 4 张表（`contact_tags`、`contact_tag_links`、`contact_events`、`followup_items`）在启动时按需创建，随主库一起备份；即使这些表缺失，核心记录/查询/提醒功能也照常工作
- 升级后请强制刷新一次网页（Ctrl/Cmd + Shift + R）

### v3.0.0（2026-10-03）

**新增：消息归档（默认关闭，对现有功能零侵入）**

- 把超过保留期（默认 730 天 ≈ 2 年，可配 30 ~ 36500 天）的旧消息从 `messages` 表移入同库的 `messages_archive` 归档表，缩小活动数据、加快日常查询
- 网页端「备份」页新增「消息归档」卡片：开关每日自动归档（每 24 小时跑一次）、设置保留天数、立即归档、查看活动/已归档/可归档统计与按联系人明细
- 随时可恢复：按单个联系人或一键恢复全部；联系人已删除的归档消息不会写回（避免悬空），原地保留并单独标注
- 安全性：归档/恢复都在单事务里先复制、确认入档后再删除，`msg_hash` 撞车也不丢消息；`msg_time` 为空或非法的历史数据原地保留，宁少归不错归
- 归档表与主库同一个 SQLite 文件，备份（VACUUM INTO）天然覆盖，恢复时随表一起搬回

**新增：可信设备（免重复登录）**

- 登录页新增「信任此设备」勾选（默认勾选）：完整通过 Token + 2FA 登录后签发一枚 90 天有效的长效令牌存在浏览器，之后打开网页免输 Token 和动态验证码，每次使用自动滑动续期
- 网页端「备份」页新增「可信设备」卡片：查看全部可信设备（名称、打码令牌、添加/最近使用/到期时间、最近 IP）、单独吊销、一键吊销全部
- 安全边界：免登录换取会话仍受 IP 白名单和封禁名单约束；关闭 2FA 时自动吊销全部可信设备；令牌文件 `trusted_clients.json` 权限 0600、不进备份；设备数上限 20 台，超出自动淘汰最久未使用的
- 浏览器端点「退出登录」只对本标签页生效（不会立刻又自动登进去），重新打开页面仍免登录；要彻底退出请在「可信设备」里吊销

**升级说明**

- 两项功能默认均不改变现有行为：归档默认不启用（不开启就只是多建两张空表），可信设备只在登录时勾选才生效；`config.json` 格式不变，直接替换二进制/镜像升级即可
- 升级后请强制刷新一次网页（Ctrl/Cmd + Shift + R）

### v2.4.0（2026-10-03）

**新增：关系助手（网页端增值功能，默认关闭，对现有功能零侵入）**

- **重要日子提醒**：自动扫描联系人画像里的重要日子（生日、纪念日），支持「5月1日」「1995-05-01」「12/25」等常见写法，提前 N 天（可配）进入提醒；无法识别的原文（如农历）单独列在「未识别」区，不会误报
- **久未联系提醒**：超过 N 天（默认 7 天）无任何互动的联系人进入冷却名单，附带最后一条消息方便找话题；从没聊过的不参与
- **亲密度评分**：纯本地统计近 30 天互动（活跃度 40 分 + 双方均衡度 30 分 + 对方投入 30 分），Top10 看板展示，不调用模型、零成本
- **AI 情绪预警**：对近 3 天有消息的联系人做情绪分析（复用已配置的 LLM），情绪低落等给出摘要与建议；每日分析数量可配（默认上限 10 个）控制花费
- **每日提醒邮件**：每天固定时间（默认 08:00）把以上三类聚合成一封 HTML 邮件发到你的邮箱；无提醒事项时不发；同一事项窗口期内不重复提醒，发送失败下次自动补发
- **每周报告邮件**：每周固定时间（默认周日 20:00）发送本周消息量、互动最多、亲密度 Top5、最久没联系、下周重要日子、情绪速览
- **SMTP 配置**：网页端「关系助手」页填写邮箱 SMTP（465/SSL 或 587/STARTTLS 均支持），可发测试邮件验证；密码保存后打码显示；任务运行记录与邮件发送日志可查
- **手动运行**：网页端可立即触发每日提醒 / 每周报告，不用等定时
- 实现上全部逻辑在新文件（`assistant.go` / `assistant_api.go` / `mailer.go`），配置存 SQLite 新表，不改 `config.json` 格式；不开启时定时任务只读一行配置即返回，无任何副作用

### v2.3.3（2026-10-03）

**修复**

- **手机端网页顶栏菜单显示不全**：小屏顶栏改为两行布局——第一行「品牌 + 退出」，第二行导航独占整行、放不下时可横向滑动，联系人 / 合并记录 / 备份 / 状态 / 命令说明五个入口全部可达；此前单行布局里导航被品牌和退出按钮挤成一条缝，只能看到前一两个入口且不易发现可以滑动

### v2.3.2（2026-10-03）

**新增**

- **Docker 镜像随 Release 发布**：Release 附带 `wechat-profile-bot-docker-v2.3.2.tar.gz`，`docker load` 即可导入 `wechat-profile-bot:v2.3.2` 镜像，不再必须 git clone 源码构建；发布包内也新增 `Dockerfile` / `docker-compose.yml` / `docker-entrypoint.sh`，可自行构建

**变更**

- **完全移除 2FA 防重放限制**：不再记录已使用的时间步（`totp_secret.json` 的 `LastUsedStep` 字段废弃），同一验证码在其 30 秒有效窗口（含前后各 1 步容差）内可重复使用；此前多设备同时登录或重复提交会被误拒为「验证码已被使用」，现已不会。登录仍需密码 + 2FA 验证码两要素，安全性不受影响

**测试**

- 新增 TOTP 验证单测（正常步、窗口边界、错误码）；删除联系人回归测试同步更新

### v2.3.1（2026-10-03）

体验修复与小增强，全部改动向后兼容，不影响既有数据和配置。**网页端更新后请强制刷新一次（Ctrl/Cmd + Shift + R）**，避免浏览器继续使用旧缓存。

**优化**

- **编辑画像与画像查看格式完全对齐**：网页端编辑弹窗改为与画像查看一致的 9 节结构（概要 / 基本信息 / 性格特征 / 沟通风格 / 兴趣爱好 / 情绪模式 / 关系 / 典型意图 / 重要事实），字段名与画像里逐字一致（职业、所在地、口头禅、表情使用、压力源、安慰有效话题、近期共同事件等），回填顺序固定，不再出现两套字段名和概要排到最后的问题
- **建议回复固定给四种风格**：意图分析每次返回「稳妥得体 / 简洁直接 / 亲切热情 / 委婉留余地」各一条，风格由用户自己挑，不再由模型只选 3 种
- **微信「状态」命令增加服务器资源**：显示磁盘、内存、系统负载和程序占用；数据盘使用 ≥70% 给出【提醒】、≥90% 给出【紧急】并提示尽快备份迁移，赶在写满前发现风险
- **全角竖线分隔符**：`改写`、`草稿检查` 命令的竖线 `｜` 和 `|` 都认，中文输入法下不用切换

**测试**

- 新增分隔符解析、建议回复数量上限、服务器资源块单测；画像编辑保存 round-trip 经浏览器实测

### v2.3（2026-10-03）

体验与部署改进，全部改动向后兼容，不影响既有数据和配置（使用反向代理需新增一项配置，见下）。

**新增**

- **网页「状态」页**：展示微信连接、服务运行时长、服务器资源（CPU / 内存 / 磁盘 / 系统负载）、本程序资源占用、数据量，每 5 秒自动刷新；同时显示服务端识别到的访问 IP 与白名单/可信代理状态，方便排查反代问题
- **联系人分页与服务端搜索**：列表默认每页 30 条、底部「加载更多」，不再首屏全量加载；搜索改为「查询」按钮/回车触发，由后端匹配昵称、备注、别名，未加载的联系人也能搜到。桌面端远程模式仍用全量接口，不受影响
- **反向代理真实 IP 支持**：新增 `trustedProxies` 配置项。只有 TCP 直连来源在可信代理列表内时才采纳 `X-Forwarded-For`，并从代理链最右侧逐跳校验，白名单 / 封禁 / 限流全部按真实访客 IP 判定；直连客户端伪造 XFF 无法绕过。反代后 `apiWhitelist` 继续填访客真实公网 IP 即可
- **微信端建议回复方便复制**：意图分析的建议回复改为逐条单独发送，长按单条即可复制；风格标签放在消息末尾（如 `内容【稳妥得体】`），粘贴后从末尾一删即可

**优化**

- 网页「发送前检查 + 候选回复 + 画像变化」拆为联系人详情的独立「辅助」页签，不再混在画像页；画像变化可收起
- 候选回复风格体系统一：意图分析自动按语境挑选 3 种风格并保证差异可感知，修复旧版模型返回字符串数组时风格错位的问题；微信 / 网页 / 桌面三端展示一致
- 网页手机端适配（≤640px）：顶栏导航与详情子页签横向滚动、弹窗收窄限高、状态页单列、联系人摘要两行截断、表格收紧
- 网页「命令说明」与微信端帮助文案逐字同步

**测试**

- 新增真实 IP 解析安全单测 14 例（直连、单级/多级反代、XFF 伪造、IPv6、CIDR 白名单）；分页与搜索经浏览器实测

### v2.2（2026-10-02）

功能增强与逻辑修复，全部改动向后兼容，不影响既有数据和部署方式。

**新增**

- **意图分析给多条候选回复**：一次分析结合语境从「稳妥得体 / 简洁直接 / 亲切热情 / 委婉留余地」四种固定风格中挑最合适的 3 种，每条带风格标签；网页/桌面端每条可单独复制。模型只回一条时按实际数量展示，不凑数
- **编辑画像**：网页/桌面端联系人详情新增画像编辑，可修改内容、删除列表条目、清空字段，直接保存不经过 AI 改写，改动记入画像历史；画像已被其他操作更新时会拒绝覆盖，避免丢失新内容
- **一键换个说法**：候选回复旁点选目标风格（稳妥得体 / 简洁直接 / 亲切热情 / 委婉留余地）即可改写该条，只改写该条、保留原意、不捏造事实。微信命令：`改写 昵称｜风格｜内容`（竖线 `｜`、`|` 都可以，不用切换输入法）
- **回复前帮我看看**：输入准备发送的草稿，结合画像和近期聊天指出可能的歧义并给出修改建议，不自动发送。微信命令：`草稿检查 昵称｜内容`（竖线中英文输入法都可以）
- **画像变化高亮**：对比最近两次画像，展示新增 / 修改 / 删除及前后内容，不调用模型。微信命令：`画像变化 昵称`

**修复（11 项逻辑问题）**

- 恢复备份：修复特定并发下的数据库死锁
- 撤销合并：新合并保存必要快照，撤销时恢复源联系人的消息与原画像（旧版本合并时已删除且无快照的消息无法找回）
- 后台画像任务：合并、撤销或恢复前启动的过期结果不再写回，避免画像污染
- 2FA：绑定检查与写入原子化，已绑定时拒绝覆盖，并发首次绑定仅一次成功
- 备份恢复：报失败时回滚数据库并补偿文件，不再出现"已替换成功却报失败"
- 删除确认：恢复备份后清除旧确认，不再出现"确认删 A 实际删到 B"
- 连字符日期（如 `2025-6-10`）正确解析，不再回退当前时间导致去重失效
- 意图分析失败后重置去重标记，同一文本可直接重试
- 网页刷新 `#/contact/ID` 链接后正常加载详情，不再空白
- 桌面端备份密码对话框关闭前不再丢失输入；删除最后一个联系人后清空右侧面板

**说明**

- 新功能（改写 / 草稿检查 / 画像变化）复用现有模型接口，不新增外部依赖，配置无需改动
- 画像编辑针对当前画像，旧历史仍保留；后续重新分析聊天记录时模型可能再次提取已删除的信息，界面已有提示

### v2.1（2026-10-02）

安全加固专项，全部改动向后兼容，不影响既有数据和用法。

**新增**

- **登录失败永久封禁**：同一 IP 网页登录失败累计 10 次即永久封禁，之后所有请求 403，重启不解除。名单持久化在 `banned_ips.json`（0600，原子写入，损坏时自动备份为 `.corrupt` 并以空名单启动）
- **独立安全日志 `security.log`**（0600）：记录每次登录失败的 IP 与原因、封禁/解封事件、限流拒绝、白名单拒绝；与业务日志 `bot.log` 分开，便于审计
- **封禁自救命令**：`--list-bans`、`--unban <ip>`、`--unban-all`、`--help`（改完名单需重启服务生效）
- **备份加密（可选）**：导出时可设口令，把 zip 内的密钥文件（`config.json`、`ilink_credentials.json`、`totp_secret.json`）用 AES-256-GCM 加密为 `.enc`；口令不设则完全等同旧行为。聊天记录 `data.db` 与 `MANIFEST.json` 始终明文，zip 仍是标准格式。导入自动识别，口令错误直接中止、不动现有数据
- **`/api/ingest` 限流**：每 IP 每分钟 120 次，超出返回 429 + `Retry-After`，防止 token 泄露后被刷爆大模型账单
- **安全响应头**：所有响应带 `X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: no-referrer`、`Content-Security-Policy`
- **HTTP 超时**：`ReadHeaderTimeout 10s`、`IdleTimeout 120s`，防慢速连接（Slowloris）占满连接数
- **Docker 容器不再以 root 运行**：新增 `docker-entrypoint.sh`，先对齐 `/config` 属主并收紧到 700，再用 `su-exec` 降权到 `PUID:PGID`（默认 1000:1000）；`PUID=0` 可退回 root
- **部署脚本入库**：`scripts/linux/wechat-profile-bot.service`（systemd 模板）、`scripts/windows/{start,install-service,uninstall-service}.bat`（nssm 服务）
- README 补全「常驻运行与开机自启」（systemd / nohup / rc.local / Windows 服务 / 任务计划）和「Docker 日常运维」章节

**说明**

- 客户端 IP 默认取自 TCP `RemoteAddr`；配置 `trustedProxies` 后仅采纳可信代理链上的 `X-Forwarded-For`（配置方法见「反向代理部署」），伪造头无法绕过封禁与白名单
- 桌面端用 `apiToken` 直连，token 配错只记审计日志、**不计入封禁计数**，不会把自己锁死
- 备份口令程序不保存在任何地方，忘了就解不开 `.enc`（聊天记录不受影响）

### v2.0（2026-10-02）

首个服务端版本：微信消息收发、AI 人物画像与意图分析、联系人管理（备注/合并/撤销）、网页管理界面（2FA）、REST API 供桌面端远程调用、数据备份/恢复与操作日志、Docker 部署支持。

