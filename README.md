# wechat-profile-bot

微信 ClawBot（iLink Bot API）服务端 —— 把微信聊天记录自动存到数据库，生成人物画像和意图分析。

配套 Windows 桌面端：[github.com/caodabao99/wechat-profile](https://github.com/caodabao99/wechat-profile)

## 功能

- **微信消息收发**：通过腾讯官方 iLink Bot API 长轮询收发消息，无需公网 IP/Webhook/内网穿透
- **聊天记录识别**：直接粘贴微信多选复制的聊天记录，自动解析并存入 SQLite
- **人物画像**：AI 自动生成/更新联系人画像（性格、兴趣、沟通风格、关系等）
- **意图分析**：分析对方最新消息的潜在意图、情绪、建议回复
- **联系人管理**：备注、合并（换昵称后关联）、撤销合并、删除
- **REST API**：内置 HTTP 接口（默认端口 17965），Windows 桌面版可远程复用同一份数据与模型分析
- **关系助手**（网页端，默认关闭）：重要日子提醒、久未联系提醒、亲密度评分、AI 情绪预警，每日提醒 / 每周报告通过 SMTP 邮件发送
- **每周维护计划**（网页看板）：每周一自动排行“本周最该联系的 5 个人”并草拟开场白，只在看板展示不发邮件，可手动立即重算
- **隐式反馈闭环**：粘贴记录入库时自动识别你已联系→标建议已执行，14 天后自动回测互动是否回暖，看板展示近 90 天采纳率/回暖率，全程零手动操作
- **关系图谱**（洞察页）：从画像交叉比对共同城市/同类职业/共同兴趣/提到彼此，纯 SQL 推导谁和谁可能认识及可信度
- **人生总览**（洞察页，人生模拟器）：把每段关系算成一份“人际资产账本”——余额（关系深度）、现金流（近期互动相对历史的增值/缩水）、折旧（断联天数 + 冷却斜率）、风险分与象限分类，并汇总人际总财富、依赖集中度、高风险将断清单与时间精力回流偏差。纯 SQL 零模型开销，每周一自动算好，打开即看
- **未来推演**（洞察页，人生模拟器）：按当前互动趋势把每段关系确定性地快进到 90 天后，算出“什么都不做会断掉几段”；系统自动生成三条 what-if 对比（维持现状 / 每周联系高风险清单一分钟 / 把时间往家人身上移），每条附“可多保住几段 / 总财富变化”，人只看结果不拨旋钮
- **人生年表**（洞察页，人生模拟器）：从最早对话、按年活跃度、跨年最长情的关系、最近在意的人与你手动记下的大事，自动聚合出里程碑年表；数据稀疏时优雅降级为空，不强推不乱预测
- **社交网络智能**（洞察页，高阶洞察）：把两两关联升级成一张网络，跑纯确定性图算法（连通分量 + 标签传播圈簇、Tarjan 割点、Brandes 介数中心性）——自动识别你的各个圈子、找出“删掉就会让网络碎片化”的关键桥梁人物、给出网络脆弱度评分与跨圈撮合引荐（“你可以把 X 介绍给 Y”），每条附可解释依据，零模型开销
- **自我关系画像**（洞察页，高阶洞察）：从全体关系里反照出你自己——谁总在先开口（全局 + 按类别）、精力正从哪个类别迁向哪个类别、有多少关系几乎只有你一厢情愿、以及你的整体社交风格标签（主动/被动、深耕/广撒网、偏向哪类人），工具从“看别人”变成“照见自己”，零新增采数
- **证据化干预学习**（洞察页，高阶洞察）：用你自己的历史回测数据学“对哪类人做什么真有效”——按干预方式分组算回暖率并给出 Wilson 95% 置信区间，样本不足时诚实降级只报基线率、绝不假装精确；按置信下界 × 覆盖人数排出“被验证有效的动作”
- **本周简报**（洞察页，高阶洞察，进入洞察页默认落地）：把人生模拟器 + 社交网络 + 自我画像 + 干预学习四层揉成一页“打开即行动”的每周摘要——Top-3 本周最该做的事（每条指向具体人、附来源层与依据）、全局健康度一行、最强信号；纯确定性模板文案，每周一自动随四层刷新
- **洞察趋势与周环比**（v4.5.0）：每周把四层的关键标量追加成一条 append-only 历史快照（只留近 52 周），各子页头部指标旁渲染 sparkline 走势，简报顶部新增“本周关键变化”块（如“高风险关系 3→5”“你先开口率 71%→64%”）——把产品从“看当下”升级为“看走势”，纯确定性、零新增采数
- **跨层闭环自校准**（v4.5.0）：复用已有干预回测表（零新表），让“上次建议是否奏效”反向重排简报 Top 行动——已回暖的关系自动沉底不重复催、仍无改善的浮顶并附“上次建议后仍无改善”标注，四层互相咬合；无回测数据时严格等同旧行为
- **图计算规模护栏**（v4.5.0）：联系人增长后，网络图算法进 Brandes/Tarjan 前对节点数封顶（按亲密度取最亲密的 250 人核心圈，tie-break 按 id 保确定性），防周一重算拖慢单连接池，并在洞察里诚实说明“已聚焦核心圈”
- **数据可携与遗忘**（v4.5.0）：新增一键全量开放 JSON 导出（可选脱敏：姓名→伪名、正文抹除、保留结构与计数），并把“彻底删除联系人”的级联补齐隐私残留（互动指标/画像事实与证据/关系连线/建议回测）——纯本地、只读、零费用，给你的敏感关系数据真正的掌控感
- **关系知识图谱可视化**（v4.6.0，洞察页社交网络）：把已有图算法算出的完整拓扑（节点/连线及权重、圈簇归属、割点、介数重要度）导出到前端，用自研内联 SVG 力导向图交互呈现——节点=联系人、连线粗细=互动强度、颜色=所属圈子、红环=桥梁人物、红填充=桥梁且高风险，支持缩放/平移/拖拽微调/悬停详情/点击跳转联系人，把数据洞察升级为视觉洞察。零第三方库（不引 D3/Cytoscape）、确定性布局（不用随机数）、不新增端点
- **关系维护日历**（v4.7.0，关系助手页）：新增只读聚合端点 `GET /api/assistant/calendar/events`，把生日/纪念日（年度重复逐年展开、含 2/29 平年回退 2/28）、手动大事记、带截止日的待跟进聚合成统一事件流，前端用自研 7 列月历网格呈现（按 kind 着色、点事件直达联系人）。待跟进新增可选 `due_date` 列（懒建表走幂等 ALTER、不 bump user_version、不改 backup.go）。零第三方库（不引 FullCalendar）、确定性排序、只读端点
- **对话质量评分**（v4.8.0，联系人驾驶舱）：新增只读端点 `GET /api/contacts/{id}/quality?days=90`，对单个联系人从**回复及时性 / 对话深度 / 话题多样性 / 情绪正向度**四维各给 0-100 分并加权出综合分，前端用手写内联 SVG 雷达图 + 维度条呈现。全部在本地按消息与画像确定性计算、不调模型（情绪为增值表，缺失则该维不计入综合、按比例归一，诚实标注“无数据”）；可随时刷新、结果稳定可复现。零第三方库（不引 Chart.js/ECharts）、只读、不新增表
- **智能回顾摘要**（v4.9.0，联系人驾驶舱）：新增 `POST /api/contacts/{id}/summary` body `{days}`，选定时间窗口后基于窗口内真实聊天原文调模型产一份结构化回顾——overview一段话总结 + topics 话题 chips + todos 待办列表（每条标 [n] 出处可跳转原文、可一键转跟进）。复用 ask.go 两段式检索与降级：未配模型 503、不编造；窗口内原文不足 4 条时如实返回空摘要 + note；模型返回坏 JSON 回退为原文摘录。摘要一次性返回不落库。
- **插件化分析引擎·提示词模板外置**（v5.1.0，关系助手）：把全部 13 个 LLM 提示词模板从硬编码外置为可编辑「分析插件」——内置默认经 `go:embed` 逐字内嵌、可被 SQLite `prompt_templates` 覆盖并**运行时热加载**（改完即生效、无需重启）。变量用**字面占位符 `{{key}}`**、渲染为单趟精确字符串替换（非 text/template，杜绝模板注入、结果确定）；保存时强制校验必填变量齐备、拒绝未知/游离占位符、限 8KB；坏覆盖或读取失败一律**回退内置默认、绝不因模板问题 500 阻断业务**。默认渲染与迁移前 `fmt.Sprintf` **逐字节一致**（golden 测试锁死，纯重构零行为变化）。新增 `/api/assistant/prompts*` 一组端点支持列表/取详情/保存/重置/试渲染，覆盖表自动纳入备份恢复。
- **分析插件（提示词）网页管理界面**（v5.2.0，关系助手页）：把上述提示词端点做成可视化面板「🧩 分析插件（提示词）」——列出 15 个模板（标题/所属功能/是否已自定义徽标/更新时间），行内展开即可编辑当前生效正文、点变量 chips 在光标处插入 `{{key}}`、实时字节计数（8KB 上限提示）、与内置默认对照、一键预览（用样本变量字面渲染、不调模型）、保存自定义 / 恢复默认。纯前端复用既有 Vue + `api()` 范式，**零第三方依赖**、无新增后端端点。
- **对话质量历史趋势**（v5.2.1，联系人驾驶舱）：在既有四维雷达图旁新增一条「综合分历史趋势」折线——每次查看质量评分时按 ISO 周幂等留存综合分快照到新派生表 `contact_quality_history`（每联系人仅留近约 104 周），前端复用既有零依赖 `sparkPoints` 内联 SVG 折线呈现、附最新值与较上期环比。全程本地确定性计算、不调模型、惰性采集不新增后台任务；best-effort 写历史失败不影响评分响应。零第三方库，`GET /api/contacts/{id}/quality` 响应新增 `history` 字段。
- **关系成就 / 里程碑系统**（v5.2.1，联系人驾驶舱）：新增只读端点 `GET /api/contacts/{id}/achievements`，从聊天记录**确定性自动派生**关系里程碑——累计消息（100/500/1000/5000 条）、连续互动天数（7/30/100 天，messages ∪ archive 活跃自然日最长连续段）、相识周年（1/3/5 年，起点=首条消息时间）。前端用成就卡片网格呈现（达成亮徽章+解锁日期、未达成进度条）。首次跨越某档位即写 `contact_achievements` 去重表 + 一条 `kind="milestone"` 的 `contact_events`，于是「时间线」标签自动留下解锁历史；重复检测幂等、不重复写。全程本地计算、不调模型、可离线单测；去重表按持久化用户数据纳入备份恢复。零第三方库。
- **关系健康度综合仪表盘**（v5.3.0，洞察页）：新增只读端点 `GET /api/relationships/health?window=90`，为每位联系人融合亲密度基座 + 趋势（升温/持平/降温/沉睡）+ 情绪均值 + 沉默衰减 + 单向惩罚，算出一个 0-100 的**关系活力分**（与驾驶舱「对话质量分」不同口径），按优秀/良好/一般/需关注/危险五档分布。前端洞察页新子标签展示汇总均值 + band 直方条 + 全局热力图网格（绿→红着色、点格直达联系人）+ 最需关注榜。全程纯本地确定性、计算即读不落库、不调模型；`fuseHealth` 为可脱库单测纯函数，联系人上限护栏 300。零第三方库。
- **智能关系圈层管理**（v5.3.0，洞察页）：新增只读端点 `GET /api/relationships/circles`，按互动频率（近90天活跃天数）+ 亲密度确定性分入**核心/亲密/社交/弱联系**四层（阈值 75/40/15），每层配一套差异化维护打法与建议节奏（周/两周一月/季），并可选贴上社交网络圈簇归属标签。前端洞察页新子标签四层堆叠分区 + 成员 chips（点直达）。不重复 v5.0.0 打标签——圈层是「分层视图 + 每层打法」的独立视角；纯本地、计算即读、不调模型；`assignTier` 为可脱库单测纯函数。零第三方库。
- **主动关系教练**（v5.3.0，关系助手页）：新增只读端点 `GET /api/assistant/coach` 与 `GET /api/contacts/{id}/timing`。教练卡片只读装配既有周计划的 Top 目标，每条附「为何联系（理由）+ 说什么（复用已有 draft 话术、可复制）+ 什么时候发（时机建议）」；时机为**净新增维度**——从对方历史发言的 `messages(∪archive)` 时间戳聚成小时×周几直方，算出最佳时段/星期，样本不足 8 条时诚实返回「数据不足、建议常规时段」。零新模型调用（draft 有则用、无则留空 + note）；`hourWeekdayHist` 为可脱库单测纯函数。前端助手页新面板。零第三方库。
- **关系维护挑战 / 游戏化**（v5.3.0，关系助手页）：新增只读端点 `GET /api/assistant/challenges`，每周从「久未联系 / 待回复 / 核心圈」确定性挑最多 3 条小目标（按 ISO 周幂等入库、跨周不重复刷），你照常发消息、系统按本周新增互动自动判定达成并累加 XP（等级 = 1 + ⌊xp/100⌋），首次达成写一条 `kind="milestone"` 事件与 v5.2.1 成就徽章联动。两张新持久表 `weekly_challenges`/`gamification_state`（懒建表、不 bump user_version）作为用户数据自动纳入备份恢复。前端助手页新面板（周挑战清单 + 本周进度条 + XP 经验条 + 等级）。纯本地确定性、不调模型。零第三方库。
- **消息主题演化追踪**（v5.3.0，洞察页）：新增 `GET /api/contacts/{id}/topics`（只读历史）与 `POST /api/contacts/{id}/topics/analyze`（手动触发），并挂入助手周一调度自动刷新。对活跃联系人按周让模型聚类聊天主题，产 `{topics:[{name,weight,status:emergent/persistent/fading}]}` 写入持久表 `contact_topic_history`（每联系人裁剪近 104 周）；前端洞察页新子标签展示联系人×主题演化表（chips 带 ↑涌现/→持续/↓消退）。为唯一新增 LLM 功能，仅当已配模型且开启高阶洞察时才跑（否则返回空 + note、不 500、绝不编造）；坏 JSON 回退上周快照；本周周锁防并发/重跑。新增第 14 个提示词模板 `topic_evolution`（13→14）。
- **关系断点预警**（v5.4.0，洞察页健康仪表盘）：复用 `ComputeHealth` 已算好的近30天/前30天互动量与沉默天数，`projectCooling`（可脱库单测纯函数）由衰减斜率前瞻预测「预计多少天进入沉寂」，分级 `none/watching/urgent`。`GET /api/relationships/health` 响应**向后兼容**新增 `items[].alert`/`etaDays` + `summary.alertCount` + `alerts`（仅正在降温的关系子集，按紧急度排序）；前端新子视图抢在断联前提醒你主动联系。零新端点、零模型、不落库。
- **消息密度热力图**（v5.4.0，联系人驾驶舱）：新增只读端点 `GET /api/contacts/{id}/heatmap?year=YYYY`，按自然日聚合 `relationship_daily_metrics` 的 `me_count+other_count`（该联系人指标为空但有消息时沿用自愈重建），返回 `{year,days,max,total}`；前端以 GitHub 贡献图式 53×7 SVG 网格按日着色、hover 显日期+条数，一眼看穿关系的季节变化。纯 SQL、计算即读、不新增表。
- **互动节奏分析**（v5.4.0，联系人驾驶舱）：新增只读端点 `GET /api/contacts/{id}/rhythm`，`latencyStats`（回复间隔中位数+秒回率）与 `timeSignature`（深夜聊友/早安伙伴/日间型）为可脱库单测纯函数，复用 `computeContactTiming` 的小时/周一直方口径，返回 `{replyMedianMin,fastRatio,signature,bestHours,bestWeekdays,sample,note}`。前端「互动节奏」卡呈现节奏画像 + 时段签名。纯本地统计、零模型。
- **标签冲突检测**（v5.4.0，标签管理）：新增只读端点 `GET /api/assistant/tags/conflicts`，`detectTagConflicts`（可脱库单测纯函数）按内置互斥标签组（亲密×疏远、家人×职场）检测同一联系人身上的矛盾标签，返回冲突清单（空则 note「标签体系自洽」）；前端标签管理弹层新增冲突面板、一键跳联系人消歧。一趟锁读已挂标签、只读不写库、零模型。
- **对话风格镜像**（v5.4.0，联系人驾驶舱）：新增只读端点 `GET /api/contacts/{id}/mirror`，`lexicalRichness`/`avgLen`/`markerShare`/`emojiShare` 等纯函数按 rune 统计你发给某联系人的消息，并与你的全局均值对比，产「你在 TA 面前的样子」标签（话痨型/简洁型/表情党/问询型/热情型…）；前端「风格镜像」卡以维度条 + delta 徽标呈现。零第三方依赖、纯本地文本统计、不新增表。
- **关系能量流桑基图**（v5.4.0，洞察页新子标签「能量流」）：纯前端手绘 SVG 桑基——「我」→ 四层圈带 → 每层 Top-N 联系人，连线宽度∝亲密度 score、节点按圈层/score 确定性排序（无随机数），数据源直接复用 `GET /api/relationships/circles`。零后端改动、零第三方库、点叶子直达联系人。
- **数据健康自检报告**（v5.4.0，状态页）：新增只读端点 `GET /api/system/data-report`，一趟锁纯 SQL 聚合 DB 体积 / 消息总数与近30天增速 / 联系人·画像·标签·时间线计数与覆盖率 / 各派生缓存表行数与存在性 / 最近成功备份，前端「数据健康自检」面板以指标卡 + 覆盖率条呈现。纯本地只读、不新增表、不调模型。
- **关系叙事生成**（v5.5.0，联系人驾驶舱）：新增 `POST /api/contacts/{id}/narrative`（`body {days}`），复用 summary 管道（`computeAchMetrics`/`loadSummaryInputs`/`numberMessagesForSummary`/`latestTopicsBefore`/`RenderPrompt`）把相识年数、首次互动、近期主题、往来条数 + 编号原文喂给模型，写一段温暖的关系故事；**双重降级**：无 LLM / 调用失败 / 产物空或短于阈值时回落为 `buildDeterministicNarrative` 纯事实拼成的确定性叙事，**永不 503、不编造**（`source` 标记 `llm`/`deterministic`）。新增第 15 个提示词模板 `relationship_narrative`（提示词注册表 14→15，同步更新 `prompts_test.go`/`prompts_api_test.go`/`feature_sweep_test.go` 与本文档）。不落库、计算即返回。前端「关系故事」卡展示叙事 + 来源徽标 + 近期主题 chips，并可一键 canvas 手绘导出为精美分享图（纯前端、零第三方库）。
- **关系目标追踪**（v5.5.0，关系助手页 + 联系人详情）：新增持久表 `relationship_goals`（懒 `ensureGoals`、不 bump user_version、不入 `derivedTables`、依动态 `listRestoreTables` 自动纳入备份/恢复）与 CRUD 端点 `GET/POST /api/assistant/goals`、`POST /api/assistant/goals/{id}/complete`、`DELETE /api/assistant/goals/{id}`。用户为某联系人自设可量化目标（如「本周主动发 5 条」），系统按窗口读 `relationship_daily_metrics` 的 `me_count` 自动判定达标：达成即置 `done`、`+XP`、升级（与 v5.3.0 挑战共用 `gamification_state`/`levelForXP`）并写一条 `kind="milestone"` 事件。与「维护挑战」语义划清（挑战=系统建议、目标=用户自设）；达标判定/手动完成均幂等（仅处理 `active` 行）。`goalProgressPct`/`goalDateWindow`/`normalizeGoalMetric` 为可脱库单测纯函数。单连接池三趟锁（读候选/写库/锁外补事件）绝不嵌套。前端助手页「关系目标」面板（进度条 + 完成勾选 + 新建表单）与联系人详情「TA 的目标」子列表。
- **智能联系人自动分组**（v5.0.0，标签管理）：新增只读端点 `GET /api/assistant/tags/suggest?ids=1,2,3`，基于联系人画像（地域/职业/兴趣）与互动指标（亲密度分层、情绪告警、往来待跟进）用**确定性规则**批量产标签建议（含命中理由与置信度），前端一键/勾选批量采纳经 `POST /api/assistant/tags/apply` 走 `CreateTag`（幂等）+ `INSERT OR IGNORE` 落库。全程本地计算、不调模型、结果可复现（同数据同参跑两次逐字段相等）；情绪/待跟进为增值表，缺失则静默跳过对应规则不报错。零第三方库、采纳幂等（重复采纳不产生重复链接）。
- **待跟进事项**：手动记一笔，或让 AI 从最近聊天里扫出「答应过的事 / 借钱还钱 / 待回复」，未完成项每天随提醒邮件一起推送
- **联系人标签**：给联系人打自定义标签分组，列表页可按标签筛选、勾选多人批量打标
- **聊天记录全文搜索**：跨全部联系人按关键词搜消息，可限定联系人、时间范围，可选一并搜归档消息
- **联系人时间线**：单个联系人的完整往来脉络（首次联系、画像变更、合并、手动记录的大事），可手动补记事件
- **对话预演**：把要跟某个人开口的重要对话（谈涨薪、拒绝借钱、表白等）先练一遍——AI 按这个人的画像和真实聊天语气扮演对方跟你对练，随时能看到自己话术带来的情绪变化，结束后以沟通顾问身份复盘打分。**演练内容不落库、也不会真的发出去**
- **重要日子日历订阅**：把联系人生日/纪念日导出成标准 `.ics` 订阅链接，手机或电脑日历客户端直接订阅，全年自动重复提醒
- **AI 祝福语草稿**：重要日子临近时按画像和你的说话风格生成 3 条草稿，自己复制粘贴发送（不代发）
- **疑似重复联系人推荐**：按昵称/备注相似度扫出可能是同一个人的联系人对，给出建议保留项，一键跳去合并
- **年度关系报告**：一键生成某一年的关系长页（消息量、最活跃的一天、月度走势、情绪曲线、亲密度变化、关键词、大事记），可分享
- **我的社交大盘**：全量联系人的互动统计——24 小时活跃分布、周几分布、双方回复速度、谁先开口、高频用词
- **消息归档**：超过保留期（默认 730 天 ≈ 2 年）的旧消息可一键或每日自动移入同库归档表，缩小活动数据、加快日常查询；随时可按联系人或全部恢复，随备份一起导出
- **可信设备（免登录）**：登录时勾选「信任此设备」，该浏览器 90 天内打开网页免输 Token 和动态验证码（使用即自动续期）；可在网页端查看、单独或全部吊销

## 快速开始

### 1. 获取程序

从 [Releases](https://github.com/caodabao99/wechat-profile-bot/releases) 下载 `wechat-profile-bot-v5.5.2.zip`，解压后得到：

```
wechat-profile-bot-linux-amd64            Linux 服务端（amd64）
wechat-profile-bot-windows-amd64.exe      Windows 服务端（amd64）
wechat-profile-bot.service                Linux systemd 服务模板
start.bat                                 Windows 前台运行
install-service.bat                       Windows 安装为服务（需 nssm.exe）
uninstall-service.bat                     Windows 卸载服务
Dockerfile                                Docker 镜像构建文件
docker-compose.yml                        Docker compose 配置
docker-entrypoint.sh                      Docker 容器入口脚本（降权启动）
config.json                               配置模板
README.md                                 本文档
```

- Linux 服务器用 `wechat-profile-bot-linux-amd64`，Windows 用 `.exe`
- Docker 部署见下方「Docker 部署」：直接加载 Release 附带的镜像 tar（`wechat-profile-bot-docker-v5.5.2.tar.gz`），或用包内 Dockerfile 本地构建，均不需要 git clone 源码

> 也可自行编译，需要 Go 1.25+：
> ```bash
> go build -ldflags="-s -w" -o wechat-profile-bot .                                  # 当前平台
> CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags="-s -w" -o wechat-profile-bot-linux-amd64 .
> CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o wechat-profile-bot-windows-amd64.exe .
> ```

### 2. 配置

首次运行会在程序目录生成 `config.json`，编辑以下内容：

```json
{
  "myName": "你的微信昵称",
  "apiPort": 17965,
  "apiToken": "",
  "apiWhitelist": [],
  "trustedProxies": [],
  "llm": {
    "apiKey": "sk-xxx",
    "baseURL": "https://api.deepseek.com",
    "model": "deepseek-chat",
    "disableThinking": true
  },
  "profile": {
    "coldStartCount": 20,
    "updateInterval": 10
  }
}
```

| 字段 | 含义 |
|------|------|
| `myName` | 你自己的微信昵称，必须与微信里显示的完全一致 |
| `apiPort` | REST API 端口，供 Windows 桌面版远程调用。默认 `17965`（非常用端口，降低被扫描风险）；填 `-1` 则完全禁用 API 服务 |
| `apiToken` | API 认证令牌（Bearer Token）。**留空则首次启动自动生成一个随机令牌并写回本文件**，之后保持不变 |
| `apiWhitelist` | IP 白名单，支持单 IP 与 CIDR，如 `["1.2.3.4", "192.168.1.0/24"]`。空数组 = 不限制。**反向代理后部署时这里仍填访客的真实公网 IP**，配合下面的 `trustedProxies` 使用 |
| `trustedProxies` | 可信反向代理的 IP/CIDR，如 `["127.0.0.1"]`、`["10.0.0.0/8"]`。仅当 TCP 直连来源命中此列表时，白名单/封禁/限流才按 `X-Forwarded-For` 里的真实访客 IP 判定；直连来源不命中时该头一律忽略（防伪造）。不经过反代请留空，切勿填写 `0.0.0.0/0` |
| `llm.apiKey` | 大模型 API Key，默认 DeepSeek，也可换成任意 OpenAI 兼容接口 |
| `llm.baseURL` | 接口地址，不带末尾斜杠 |
| `llm.model` | 模型名 |
| `llm.disableThinking` | 是否关闭模型的推理思考模式，默认 `true`。默认开启思考的模型（deepseek-v4-flash、qwen3.8-flash 等）必须保持 `true` |
| `profile.coldStartCount` | 累计多少条对方消息后首次生成画像，默认 20 |
| `profile.updateInterval` | 之后每新增多少条对方消息更新一次画像，默认 10 |

### 3. 运行

```bash
./wechat-profile-bot-linux-amd64
```

首次运行会打印二维码链接，用手机微信扫码并确认绑定。

### 4. 使用

在微信里向 ClawBot 发送：

- **粘贴聊天记录** → 自动识别并存入，返回分析结果
- **帮助** → 查看所有命令
- **画像 昵称** → 查看联系人画像
- **列表** → 查看所有联系人

### 5. 网页管理界面

浏览器打开 `http://服务器IP:17965/`，输入 `config.json` 里的 `apiToken` 登录。
首次登录需绑定 TOTP 验证器（Google/Microsoft Authenticator、微信、支付宝均可），以后每次登录输入动态码。

功能包括：联系人列表、画像查看/编辑、消息记录、历史版本、统计、合并/撤销、**备份导出/导入**、**关系助手**（重要日子 / 久未联系 / 亲密度 / 情绪预警看板与邮件提醒配置，默认关闭）。

## 常驻运行与开机自启

> **建议顺序**：先前台跑一次完成扫码绑定（二维码链接直接打印在终端，不会写进日志文件），拿到 `ilink_credentials.json` 之后再转成常驻服务。凭据会一直复用，之后重启都不用再扫码。

### Linux：systemd 服务（推荐）

**1. 放置程序并建专用用户**

```bash
sudo mkdir -p /opt/wechat-profile-bot
sudo cp wechat-profile-bot-linux-amd64 /opt/wechat-profile-bot/wechat-profile-bot
sudo chmod +x /opt/wechat-profile-bot/wechat-profile-bot

# 用低权限专用用户运行，不要用 root
sudo useradd -r -s /usr/sbin/nologin -d /opt/wechat-profile-bot wpbot
sudo chown -R wpbot:wpbot /opt/wechat-profile-bot
```

**2. 前台跑一次，完成配置和扫码**

```bash
cd /opt/wechat-profile-bot && sudo -u wpbot ./wechat-profile-bot
```

第一次会生成 `config.json` 模板并退出，填好 `llm.apiKey`、`myName` 后再执行一次，用手机微信扫描终端里的二维码链接，看到登录成功后 `Ctrl+C` 退出。

**3. 创建服务单元** `/etc/systemd/system/wechat-profile-bot.service`

仓库里带了现成模板（源码在 `scripts/linux/wechat-profile-bot.service`，发布包内为 `wechat-profile-bot.service`）：

```bash
sudo cp scripts/linux/wechat-profile-bot.service /etc/systemd/system/
```

内容如下，也可以自己手写一份：

```ini
[Unit]
Description=WeChat Profile Bot
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=wpbot
Group=wpbot
WorkingDirectory=/opt/wechat-profile-bot
ExecStart=/opt/wechat-profile-bot/wechat-profile-bot
Restart=always
RestartSec=10
LimitNOFILE=65536

# 安全加固：不给额外权限、独立 /tmp、系统目录只读
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=true

[Install]
WantedBy=multi-user.target
```

> `WorkingDirectory` 必须是程序所在目录：`config.json`、数据库、`bot.log`、`security.log`、`banned_ips.json` 全部按「与数据库同目录」存放，靠工作目录定位。
> `ProtectHome=true` 会让 `/home` 不可访问，所以**不要把程序装在 `/home` 下**；确实要放 `/home`，就把这一行删掉。

**4. 启用并设为开机自启**

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now wechat-profile-bot
sudo systemctl status wechat-profile-bot
```

**日常管理**

| 操作 | 命令 |
|------|------|
| 启动 / 停止 / 重启 | `sudo systemctl start\|stop\|restart wechat-profile-bot` |
| 开 / 关开机自启 | `sudo systemctl enable\|disable wechat-profile-bot` |
| 查看状态 | `sudo systemctl status wechat-profile-bot` |
| 实时看运行日志 | `sudo journalctl -u wechat-profile-bot -f` |
| 看最近 200 行 | `sudo journalctl -u wechat-profile-bot -n 200 --no-pager` |
| 看业务日志 | `tail -f /opt/wechat-profile-bot/bot.log` |
| 看安全日志（登录失败/封禁/限流） | `sudo tail -f /opt/wechat-profile-bot/security.log` |
| 查看被封 IP | `sudo -u wpbot /opt/wechat-profile-bot/wechat-profile-bot --list-bans` |
| 解封 IP | `sudo -u wpbot /opt/wechat-profile-bot/wechat-profile-bot --unban 1.2.3.4` 后 `sudo systemctl restart wechat-profile-bot` |

> **需要重新扫码时**（换了微信号、凭据被删）：`sudo systemctl stop wechat-profile-bot`，再按第 2 步前台跑一次，扫完码 `Ctrl+C`，然后 `sudo systemctl start wechat-profile-bot`。服务模式下二维码链接也会进 `journalctl`，属敏感信息，看完别外传。

### Linux：不用 systemd 的后台运行

```bash
cd /opt/wechat-profile-bot
nohup ./wechat-profile-bot > /dev/null 2>&1 &
```

程序自己会写 `bot.log` 和 `security.log`，所以标准输出可以直接丢弃。

```bash
pgrep -af wechat-profile-bot              # 查进程
kill $(pgrep -f wechat-profile-bot)       # 停止
```

开机自启（没有 systemd 的老系统）：把下面这行加到 `/etc/rc.local` 的 `exit 0` 之前，并 `chmod +x /etc/rc.local`

```bash
cd /opt/wechat-profile-bot && nohup ./wechat-profile-bot >/dev/null 2>&1 &
```

也可以用 `tmux` / `screen` 挂一个会话，方便随时 attach 回来看输出。

### Windows：安装为服务（nssm）

仓库 `scripts/windows/` 下带了三个脚本（发布包里直接和 exe 放在一起），Windows 本身不能把 exe 直接注册成服务，需要 [nssm](https://nssm.cc/download)：

1. 下载 nssm，解压后把 `win64/nssm.exe` 放到 exe 所在目录
2. **先双击 `start.bat` 前台完成首次扫码**（二维码链接只在这个窗口里，不写日志），登录成功后关掉窗口
3. 双击 `install-service.bat`，自动完成：服务名 `WeChatProfileBot`、开机自启、崩溃后 10 秒重启、`bot.log`/`bot-error.log` 单文件超 10MB 自动轮转
4. 日常管理：

```bat
net start WeChatProfileBot
net stop  WeChatProfileBot
sc query  WeChatProfileBot
```

卸载服务运行 `uninstall-service.bat`。

**不想装服务的话**：

```powershell
# 后台无窗口运行（PowerShell，在程序目录下执行）
Start-Process -FilePath ".\wechat-profile-bot-windows-amd64.exe" -WorkingDirectory (Get-Location) -WindowStyle Hidden

# 停止
Get-Process wechat-profile-bot-windows-amd64 | Stop-Process
```

开机自启（不装服务）：打开「任务计划程序」→ 创建任务 →
- 常规：勾选「不管用户是否登录都要运行」
- 触发器：新建 →「启动时」
- 操作：新建 → 程序填 exe 完整路径，**「起始于」必须填 exe 所在目录**（否则找不到 `config.json`）

## Docker 部署

镜像未发布到 Docker Hub，两种方式任选：

- **加载 Release 附带的镜像 tar**（推荐，无需 Go 环境）：下载 `wechat-profile-bot-docker-v5.5.2.tar.gz` 后 `docker load -i wechat-profile-bot-docker-v5.5.2.tar.gz`，得到 `wechat-profile-bot:v5.5.2` 镜像，再按下文 compose（删掉 `build:` 段）或 `docker run` 启动
- **本地构建**：需要源码或 Release 包内的 Dockerfile

### 方式一：docker compose（推荐）

在项目目录执行，compose 会自动本地构建镜像并启动：

```bash
docker compose up -d --build
```

### 方式二：手动构建并运行

```bash
docker build -t wechat-profile-bot:latest .

docker run -d --name wechat-profile-bot --restart unless-stopped \
  -v $(pwd)/config:/config \
  -e TZ=Asia/Shanghai \
  -p 17965:17965 \
  wechat-profile-bot:latest
```

配置和数据保存在 `./config` 目录。首次启动会在该目录生成 `config.json` 模板并退出，填好配置后再启动一次；启动日志里会打印二维码链接，用手机微信扫码绑定。

### 容器不再以 root 运行

镜像通过 `docker-entrypoint.sh` 启动：先在 root 权限下把 `/config` 的属主对齐到 `PUID:PGID` 并收紧到 `700`，再用 `su-exec` 降权运行业务进程。这样容器被攻破时也拿不到 root，同时宿主机上的 `./config` 仍然可读写。

`docker-compose.yml` 里可以调整：

```yaml
environment:
  - PUID=1000   # 改成宿主机上执行 docker 的用户的 id -u
  - PGID=1000   # 改成 id -g
```

- 默认 `1000:1000`，与大多数 Linux 首个普通用户一致；用 `id -u` / `id -g` 查自己的值
- 启动时会自动 `chown`，所以从旧版本（root 运行）升级过来不需要手工处理已有文件
- 设 `PUID=0` 可退回旧行为（以 root 运行），作为排查问题时的逃生通道
- 手动 `docker run` 时同理，加 `-e PUID=$(id -u) -e PGID=$(id -g)` 即可

### Docker 日常运维

| 操作 | 命令 |
|------|------|
| 启动（含构建） | `docker compose up -d --build` |
| 停止 / 重启 | `docker compose stop` / `docker compose restart` |
| 看容器输出 | `docker compose logs -f`（`--tail 200` 只看最近 200 行） |
| 看业务日志 | `tail -f ./config/bot.log` |
| 看安全日志 | `tail -f ./config/security.log` |
| 查看被封 IP | `docker exec wechat-profile-bot /app/wechat-profile-bot --list-bans` |
| 解封 IP | `docker exec wechat-profile-bot /app/wechat-profile-bot --unban 1.2.3.4` 然后 `docker compose restart` |
| 升级到新版 | 替换源码后 `docker compose up -d --build`（数据在 `./config`，不会丢） |
| 备份全部数据 | 直接打包宿主机 `./config` 目录，或用网页端「备份」导出 zip |

**开机自启**：`docker-compose.yml` 里的 `restart: unless-stopped` 已经保证容器随 Docker 启动，还需要让 Docker 自身开机自启：

```bash
sudo systemctl enable docker
```

**关于端口**：`docker-compose.yml` 用的是 `network_mode: host`（避免 bridge 网络访问外部大模型 API 的问题），此时 `-p` / `ports` 映射**不生效**，容器直接监听宿主机的 17965。改用 bridge 网络时才需要 `-p 17965:17965`。

**首次扫码**：`docker compose logs -f` 里会打印二维码链接，用手机微信打开绑定即可。二维码链接等同登录凭据，别截图外传。

## 命令列表

| 命令 | 说明 |
|------|------|
| 帮助 | 显示帮助 |
| 列表 | 查看所有联系人 |
| 画像 [昵称] | 查看画像（不填显示最近更新的） |
| 统计 昵称 | 查看消息统计 |
| 备注 昵称 内容 | 设置联系人备注 |
| 补充 昵称 信息 | 手动补充画像信息 |
| 备份 | 在服务器生成完整备份文件（下载/导入用网页管理界面） |
| 合并 新昵称 旧昵称 | 合并联系人 |
| 撤销合并 昵称 | 撤销最近一次合并 |
| 合并记录 | 查看合并历史 |
| 删除 昵称 | 删除联系人（需二次确认） |
| 状态 | 查看登录、运行状态和服务器磁盘/内存（磁盘将满会提醒备份迁移） |
| 重登 | 重置登录状态（需重启） |

## 网页管理界面（含双因素认证）

浏览器打开 `http://服务端IP:17965/` 即可使用网页版管理界面（功能与桌面端对齐）。登录采用双因素：

1. 输入 `config.json` 里的 `apiToken`
2. 首次登录自动弹出二维码，用 Google/Microsoft Authenticator、微信、支付宝等 TOTP 验证器扫码绑定并输入 6 位码确认；以后每次登录输入验证器上的动态码（30 秒变化）

说明：

- 浏览器登录后只保存有期限（7 天）的**网页会话令牌**，不长期保存 `apiToken`；退出即吊销
- Windows 桌面端远程模式继续直接用 `apiToken`，不受影响
- TOTP 密钥保存在数据目录的 `totp_secret.json`（Docker 下为 `/config/totp_secret.json`，权限 0600）。**换手机或验证器丢失时，在服务器删除该文件**，下次网页登录会重新走绑定流程
- 顶部导航的 **洞察** 页聚合四个子面板：聊天记录全文搜索、年度关系报告（可另存分享）、我的社交大盘、疑似重复联系人推荐
- 联系人列表支持按标签筛选、勾选多人批量打标；联系人详情页新增 **时间线** 子页，可手动补记大事
- 联系人详情页的 **预演** 子页可以「对话预演」：描述一个场景后 AI 照画像扮演对方跟你对练，可选择谁先开口，随时点「结束并复盘」拿到话术点评与达成可能评分；整场演练只存在浏览器内存里，刷新或切换联系人即清空
- **关系助手** 页除每日提醒外，还包含待跟进事项（手动记 / AI 扫描）、重要日子 AI 祝福语草稿、日历订阅（`.ics`）密钥管理

## 桌面端远程对接（REST API）

服务端启动后会在 `apiPort`（默认 17965）监听 `/api/`，Windows 桌面版 [wechat-profile](https://github.com/caodabao99/wechat-profile)
可以改用远程模式复用这份数据——桌面端界面不变，数据和 LLM 分析都在服务端完成。

桌面端 `config.json`：

```json
{
  "myName": "你的微信昵称",
  "remote": {
    "enabled": true,
    "apiURL": "http://服务端IP:17965",
    "apiToken": "服务端 config.json 里的 apiToken"
  }
}
```

远程模式下桌面端不需要 `llm.apiKey`。启动时会先请求 `/api/status` 探活，连不上直接弹窗退出。

### 安全模型

按顺序生效，任一层不通过立即返回：

1. **非常用端口**：默认 17965，降低被批量扫描的概率；不需要远程访问时把 `apiPort` 设为 `-1` 彻底关闭
2. **IP 黑名单**：登录失败累计 10 次的 IP 会被**永久封禁**，之后所有请求直接 403（详见下方「登录失败封禁」）
3. **IP 白名单**（`apiWhitelist`）：支持单 IP 和 CIDR，**优先于 Token 校验**，不在名单内一律 403；空数组表示不限制。反代后部署配合 `trustedProxies` 按真实访客 IP 判定（见下方「反向代理部署」）
4. **Bearer Token**（`apiToken`）：所有接口（含 `status`）都要求 `Authorization: Bearer <token>`，缺失或不匹配返回 401
5. **接口限流**：`/api/ingest` 每 IP 每分钟最多 120 次，防止 Token 泄露后被脚本刷爆大模型账单
6. **安全响应头**：所有响应自动带 `X-Content-Type-Options`、`X-Frame-Options: DENY`、`Referrer-Policy`、`Content-Security-Policy`，防点击劫持与 MIME 嗅探
7. **HTTP 超时**：`ReadHeaderTimeout 10s`、`IdleTimeout 120s`，避免慢速连接（Slowloris）长期占满连接数
8. **提示词编辑（v5.1.0）**：`/api/assistant/prompts*` 与上述接口同走同一认证（IP 黑白名单 + Bearer/会话）——编辑提示词等价于编辑本工具自己的模型指令，适用于单主自托管。缓解措施：仅**字面占位符**（非模板引擎、不执行任意逻辑）、保存时**必填变量校验 + 8KB 上限**、每个模板**可一键回滚内置默认**，且覆盖只改提示词文本、**不改代码路径与降级语义**。

> 服务端暴露在公网时务必同时配置 `apiToken` 和 `apiWhitelist`，或用防火墙/安全组限制来源。

### 登录失败封禁

网页登录（Token + TOTP）失败会按来源 IP 计数，**同一 IP 累计失败 10 次即永久封禁**，封禁后该 IP 访问任何接口都返回 403，重启服务也不会解除。

所有失败与封禁事件单独记在数据目录的 `security.log`（权限 0600，与业务日志 `bot.log` 分开），封禁名单持久化在 `banned_ips.json`（权限 0600，原子写入；文件损坏时自动备份为 `.corrupt` 并以空名单启动）。

登录成功会把该 IP 的失败计数清零，所以正常使用不会被误封。

服务端的运维命令（执行完立即退出，不会启动服务，服务在跑也可以直接执行）：

```bash
./wechat-profile-bot --list-bans      # 查看当前被封禁的 IP
./wechat-profile-bot --unban 1.2.3.4  # 解封指定 IP
./wechat-profile-bot --unban-all      # 解封全部
./wechat-profile-bot --help           # 查看全部命令
```

Docker 下把命令换成 `docker exec wechat-profile-bot /app/wechat-profile-bot --list-bans`。

> **解封后需要重启服务才生效**：这些命令只改 `banned_ips.json`，正在运行的进程把名单读在内存里，不会自动重新加载。`docker restart wechat-profile-bot` 或重跑一次二进制即可。

> 客户端 IP 默认只取自 TCP 连接的 `RemoteAddr`；配置了 `trustedProxies` 后才会采纳可信代理转发的 `X-Forwarded-For`，且从代理链最右端逐跳校验，伪造的左侧 IP 无法绕过封禁与白名单。

### 反向代理部署（Nginx / Caddy 等）

直接在反代后启用 `apiWhitelist` 会遇到两个问题：服务端看到的来源 IP 是反代服务器（导致所有人都被拦）；而把反代 IP 加进白名单又等于不设防（任何人都能访问反代）。正确做法是**白名单照填真实访客 IP，另把反代自身登记为可信代理**：

1. `config.json` 配置：
   ```json
   "apiWhitelist": ["你的公网IP"],
   "trustedProxies": ["127.0.0.1"]
   ```
   反代与本程序不在同一台机器时，`trustedProxies` 填反代的内网 IP 或网段（如 `["10.0.0.0/8"]`），不要填公网大网段。Docker 部署时容器看到的直连来源通常是网桥网关，同机反代可填 `["172.16.0.0/12", "127.0.0.1"]`；反代也在容器里则填它所在的 compose 网络网段。
2. Nginx 站点配置必须转发访客 IP（程序按 `X-Forwarded-For` 链逐跳校验，伪造头无效）：
   ```nginx
   location / {
       proxy_pass http://127.0.0.1:17965;
       proxy_set_header Host $host;
       proxy_set_header X-Real-IP $remote_addr;
       proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
       proxy_set_header X-Forwarded-Proto $scheme;
   }
   ```
3. 验证：登录网页管理界面，打开「状态」页，「服务运行」卡片里的「识别到的你的 IP」应显示你的公网 IP（而不是反代 IP），旁边显示「白名单已启用 / 反代可信」。
4. 安全建议：反代加 HTTPS；有条件时用防火墙让 17965 端口只允许反代服务器访问，避免访客绕过反代直连（直连时其 `X-Forwarded-For` 伪造头会被忽略，但多层防护更稳妥）。

### 接口一览

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/status` | 服务状态与联系人数（也需认证） |
| GET | `/api/contacts?includeMerged=1` | 联系人列表（全量，桌面端兼容） |
| GET | `/api/contacts?paged=1&offset=0&limit=30&q=关键词` | 联系人分页列表（返回 `{list,total,offset,limit}`） |
| POST | `/api/contacts` | 创建联系人，body `{"name"}` |
| POST | `/api/contacts/resolve` | 昵称解析为 ID，body `{"name"}` |
| GET / DELETE | `/api/contacts/{id}` | 联系人详情 / 删除联系人 |
| GET | `/api/contacts/{id}/messages?offset=&limit=` | 消息分页（默认 limit 50） |
| GET | `/api/contacts/{id}/stats` | 消息统计 |
| GET | `/api/contacts/{id}/history?limit=` | 画像历史 |
| POST | `/api/contacts/{id}/remark` | 设置备注，body `{"remark"}` |
| POST | `/api/contacts/{id}/name` | 改名，body `{"name"}` |
| POST | `/api/contacts/{id}/supplement` | 手动补充画像，body `{"note"}` |
| POST | `/api/contacts/{id}/regenerate` | 重新生成画像 |
| POST | `/api/contacts/{id}/analyze` | 意图分析，body `{"message"}` |
| POST | `/api/merge` | 合并联系人，body `{"sourceId","targetId","useSourceName","regenerate"}` |
| POST | `/api/merge/undo` | 撤销合并，body `{"logId"}` |
| GET | `/api/merge/logs?limit=&targetId=` | 合并记录 |
| POST | `/api/ingest` | 提交聊天记录识别，body `{"text","analyze"}` |
| GET | `/api/search/messages?q=&contactId=&from=&to=&archive=1&offset=&limit=&cursor=` | 全文搜索消息（`archive=1` 一并搜归档）；返回 `nextCursor`/`hasMore`，传 `cursor` 走 keyset 深分页（旧 `offset/limit` 仍兼容） |
| GET / POST | `/api/tags` | 标签列表 / 新建标签，body `{"name"}` |
| POST | `/api/tags/batch` | 批量打标，body `{"contactIds":[],"tagIds":[],"remove":false}` |
| PUT / DELETE | `/api/tags/{id}` | 重命名（body `{"name"}`）/ 删除标签 |
| GET / PUT | `/api/contacts/{id}/tags` | 读取 / 覆盖式设置某联系人的标签，body `{"tagIds":[]}` |
| GET / POST | `/api/contacts/{id}/timeline` | 联系人时间线 / 手动补记事件，body `{"title","detail","eventTime"}` |
| DELETE | `/api/contacts/{id}/timeline/{eventId}` | 删除一条手动记录的事件 |
| GET | `/api/contacts/{id}/rehearsal/context` | 对话预演的扮演依据（画像要点 + 真实聊天说话样例） |
| POST | `/api/contacts/{id}/rehearsal/turn` | 让 AI 以对方身份回一句，body `{"scene","turns":[{"role":"me\|other","text"}]}` |
| POST | `/api/contacts/{id}/rehearsal/review` | 结束预演并复盘，body 同上，返回 `{"summary","good","bad","risks","suggestions","score"}` |
| GET | `/api/insights/duplicates` | 疑似重复联系人推荐 |
| GET | `/api/insights/social?days=30` | 我的社交大盘 |
| GET | `/api/insights/report?year=2026&format=html` | 年度关系报告（`format=html` 返回可分享长页） |
| GET / POST | `/api/assistant/followups?status=open&limit=100` | 待跟进列表 / 手动新增，body `{"contactId","kind","content","amount"}` |
| POST | `/api/assistant/followups/scan` | 用 AI 从最近聊天里扫待跟进，body `{"contactId"}`（可选，缺省按每日上限自动挑） |
| PUT / DELETE | `/api/assistant/followups/{id}` | 改状态（body `{"status":"done\|ignored\|open"}`）/ 删除 |
| GET / POST | `/api/assistant/calendar/key` | 读取（打码）/ 重置 `.ics` 订阅密钥 |
| DELETE | `/api/assistant/calendar/key` | 清除订阅密钥（关闭日历订阅） |
| POST | `/api/assistant/blessing` | 生成 AI 祝福语草稿，body `{"contactId","kind","raw","month","day","dateStr","daysUntil"}`（除 `contactId` 外均可省略） |
| GET | `/api/calendar.ics?key=订阅密钥` | 日历订阅地址（不走 Bearer，仍受 IP 白名单约束） |
| GET / POST | `/api/assistant/weekly-plan` | 本周维护计划看板数据（排行+开场白+近90天反馈统计）/ 立即重算（调模型，后台执行） |
| GET | `/api/relationships/connections` | 关系图谱全量连线（表为空时自动重建，上限 500） |
| GET | `/api/contacts/{id}/connections` | 某联系人的关联（上限 100，按可信度降序） |
| GET | `/api/contacts/{id}/quality?days=90` | 对话质量四维评分（回复及时性/深度/话题多样性/情绪正向度，各 0-100 + 综合分；纯本地确定性、不调模型） |
| POST | `/api/contacts/{id}/summary` | 智能回顾摘要，body `{"days":30}`，返回 overview/topics/todos(带 [n] 出处)/sources；未配模型 503、不编造 |
| GET | `/api/relationships/health?window=90` | 关系健康度仪表盘（v5.3.0）：每联系人融合亲密度/趋势/情绪/沉默/单向信号算 0-100 健康分 + 全局 band 直方/均值/最需关注榜；返回 `{items,summary,generatedAt}`；纯本地确定性、计算即读不落库、不调模型 |
| GET | `/api/relationships/circles` | 智能关系圈层（v5.3.0）：按互动频率+亲密度确定性分入核心/亲密/社交/弱联系四层，附每层维护打法与节奏；返回 `{tiers,members}`；纯本地、计算即读、不调模型 |
| GET | `/api/assistant/coach` | 关系教练（v5.3.0）：组装周计划 Top 目标 + 每条附行动/理由/话术（复用已有 draft）/时机建议；返回 `{items,note}`；零新模型调用 |
| GET | `/api/contacts/{id}/timing` | 单联系人时机建议（v5.3.0）：按对方历史发言的小时×周几直方算最佳时段，小样本诚实降级“数据不足”；纯确定性、不调模型 |
| GET | `/api/assistant/challenges` | 维护挑战 / 游戏化（v5.3.0）：本周挑战清单 + 达成进度 + 累计 XP/等级 + 徽章数；首次生成幂等入库、读时按本周新增互动自动判定达成；纯确定性、不调模型 |
| GET | `/api/contacts/{id}/topics?weeks=26` | 消息主题演化历史（v5.3.0）：只读某联系人按周的主题快照（涌现/持续/消退），未配模型/无历史时返回空 + note、不 500 |
| POST | `/api/contacts/{id}/topics/analyze` | 手动触发某联系人本周主题演化聚类（v5.3.0，调模型）；未配模型 503；写入当周 `contact_topic_history`（幂等覆盖） |
| GET | `/api/contacts/{id}/heatmap?year=YYYY` | 消息密度热力图（v5.4.0）：按自然日聚合某联系人全年 `me_count+other_count`；返回 `{year,days,max,total}`；纯 SQL 计算即读、不调模型 |
| GET | `/api/contacts/{id}/rhythm` | 互动节奏分析（v5.4.0）：回复间隔中位数 + 秒回率 + 时段签名（深夜/早安/日间）+ 最佳时段/星期；返回 `{replyMedianMin,fastRatio,signature,bestHours,bestWeekdays,sample,note}`；纯本地、不调模型 |
| GET | `/api/contacts/{id}/mirror` | 对话风格镜像（v5.4.0）：统计你发给某联系人的句长/用词丰富度/表情/提问/感叹，与其全局均值对比；返回 `{dimensions,traits,note}`；纯本地文本统计、不调模型 |
| GET | `/api/assistant/tags/conflicts` | 标签冲突检测（v5.4.0）：按内置互斥标签组报出同一联系人身上的矛盾标签；返回 `{conflicts,total,note}`；只读不写库、零模型 |
| GET | `/api/system/data-report` | 数据健康自检（v5.4.0）：DB 体积/消息增速/画像·标签·时间线覆盖率/各派生缓存表行数/最近备份；返回 `{dbSizeBytes,messagesTotal,profileCoverage,cacheTables,...}`；纯本地只读、不调模型 |
| GET | `/api/system/table-registry` | 数据层登记表（v5.5.1）：返回全库表元数据 JSON 数组 `[{name,type,owner,backup,rebuild,hasContactId,note},...]`（core/derived/cache/audit/config 分类与备份·重建·contact_id 三轴）；运维诊断用，只读不调模型 |
| POST | `/api/contacts/{id}/narrative` | 关系叙事生成（v5.5.0，`body {days}` 缺省 90）：有 LLM 写一段关系故事，无 LLM/失败双重降级为确定性叙事，**永不 503**；返回 `{contactId,name,years,msgCount,firstDate,recentTopics,narrative,source:llm|deterministic,note,generatedAt}`；不落库 |
| GET | `/api/assistant/goals` | 关系目标列表（v5.5.0）：先跑一次达标检测再返回 `{generatedAt,weekStart,goals:[{id,contactId,name,title,metric,targetCount,periodStart,periodEnd,status,current,progressPct,createdAt,doneAt}],done,active,total,xp,level,nextLevelAt,note}` |
| POST | `/api/assistant/goals` | 新建目标（v5.5.0，body `{contactId,title,metric,targetCount,periodStart,periodEnd}`）：返回 `201 {createdId,goals}`；标题必填、达标数 1~999；联系人不存在 404、参数非法 400 |
| POST | `/api/assistant/goals/{id}/complete` | 手动达成目标（v5.5.0）：置 done、+XP、写里程碑；幂等（已达成/不存在返回 `{ok:false}`） |
| DELETE | `/api/assistant/goals/{id}` | 删除目标（v5.5.0）：返回 `{deleted,id}` |
| GET | `/api/assistant/tags/suggest?ids=1,2,3` | 智能标签建议（ids 缺省=全部活跃联系人，带上限护栏），返回 `{suggestions:[{contactId,name,tagName,reason,confidence}],total}`；纯本地确定性、不调模型 |
| POST | `/api/assistant/tags/apply` | 批量采纳标签建议，body `{"items":[{contactId,tagName}]}`，走 `CreateTag`幂等 + `INSERT OR IGNORE`，返回 `{affected}`；重复采纳不产生重复链接 |
| GET | `/api/assistant/prompts` | 提示词模板列表，返回 `{items:[{key,title,feature,vars,isCustom,updatedAt}],total}`（共 15 个） |
| GET | `/api/assistant/prompts/{key}` | 单个模板详情：`{default,effective,isCustom,override,vars,...}` |
| PUT | `/api/assistant/prompts/{key}` | 保存覆盖，body `{"content":"...含 {{key}} 占位符"}`；非法/缺必填变量/超 8KB → 400，未知 key → 404；下次 LLM 调用即热加载生效 |
| DELETE | `/api/assistant/prompts/{key}` | 重置为内置默认（删除覆盖行） |
| POST | `/api/assistant/prompts/{key}/preview` | 用样本变量试渲染（只读、不调模型），body `{"vars":{...}}`，返回 `{prompt}` |
| POST | `/api/relationships/connections/rebuild` | 手动全量重建关系连线（纯 SQL，不调模型） |
| GET | `/api/contacts/{id}/facts` | 可信画像事实 + 证据链（`?includeRetired=1` 含历史；空则自愈重建）；每条事实附 FACT 生命周期字段（status/sourceType/validFrom/validUntil/supersededBy/confidenceType/evidenceStrength）与逐条证据评估（matchType/isDirectSupport/supportStrength/quote） |
| POST | `/api/contacts/{id}/facts/rebuild` | 强制重建该联系人事实 + 证据 |
| POST | `/api/contacts/{id}/facts/confirm/{factId}` | 将一条事实提升为「用户确认」（最高可信来源，confidence=1.0、sourceType=user，后续派生不再自动降级它）；越权/不属于该联系人 → 403 |
| GET | `/api/contacts/{id}/state` | 关系状态机单联系人视图（`base_state`×`dynamic_state`+亲密度/趋势/预警/理由/转态时间；`?history=1` 附变迁历史）；存量缺失则自愈刷新整板 |
| GET | `/api/relationships/state` | 全局关系状态看板（按健康度升序/亲密度降序） |
| POST | `/api/relationships/state/recompute` | 强制重算全体关系状态（仅当状态真实跨越阈值才写变迁事件，返回 `{changed,total,states}`） |
| GET | `/api/relationships/decisions/today` | Decision Engine（Phase 4）「今天最值得做的关系行动」Top N（`?top=3`）：汇聚状态机+健康度+待办+目标+重要日子+行动建议，确定性打分（非 LLM 排序），每条含 `reason_codes`/为什么现在/建议行动/最佳时段/来源/置信度 |
| GET | `/api/relationships/projects` | Relationship Projects（Phase 5）列表（`?contactId=&status=`；`status=open` 取 active+paused） |
| POST | `/api/relationships/projects` | 新建关系项目（目标之上的高层经营单元：主题/阶段/下一步行动/截止） |
| GET/PUT/DELETE | `/api/relationships/projects/{id}` | 单条项目读/局部更新（PATCH 语义）/删除 |
| GET | `/api/contacts/{id}/context` | AI Context Engine（Phase 6）：分层构造联系人认知快照（`?task=profile/ask/coach/narrative/simulation/decision/briefing/replay`，`?q=` 附 FTS 相关消息），按任务预算返回结构化 `context` + `rendered` 提示词上下文块 |
| GET | `/api/life/state` | 人生总览快照（资产账本+组合聚合+时间回流；缓存缺失/过期则现算，纯 SQL） |
| GET | `/api/life/projection` | 未来推演（90 天走势 + 三条自动 what-if 策略） |
| GET | `/api/life/timeline` | 人生年表里程碑（每次现算，聚合很轻） |
| POST | `/api/life/recompute` | 后台异步重算人生状态+推演（立即返回，不阻塞） |
| GET | `/api/insight/network` | 社交网络洞察（连通分量/圈簇/割点桥梁/介数/脆弱度/跨圈撮合；缓存缺失或过期即自愈重算整条流水线，纯确定性图算法） |
| GET | `/api/insight/self` | 自我关系画像（先开口率/精力迁移/单向关系/风格标签） |
| GET | `/api/insight/intervention` | 证据化干预学习（按方式分组回暖率 + Wilson 95% 置信区间，样本不足诚实降级） |
| GET | `/api/insight/briefing` | 主动人生简报（编排层只读四层缓存合成 Top-3 行动 + 全局健康度 + 本周关键变化） |
| GET | `/api/insight/trend?weeks=12` | 高阶洞察趋势历史（升序周快照，供前端 sparkline；只读非自愈，无历史返回 `weeks:[]`） |
| GET | `/api/data/export?redact=0|1&include=archive,derived` | 全量开放 JSON 导出（`redact=1` 脱敏；默认含活动数据、不含归档/派生；附件下载） |
| POST | `/api/insight/recompute` | 后台异步重算四件套 + 简报（立即返回，不阻塞） |

验证示例：

```bash
curl -H "Authorization: Bearer <token>" http://127.0.0.1:17965/api/status
```

## 备份与恢复（换服务器/换电脑迁移）

在网页管理界面顶部点「备份」页：

- **导出备份**：下载一个 `wechat-profile-backup-日期.zip`，内含全部联系人、消息、画像历史、合并记录，以及 `config.json`、`ilink_credentials.json`、`totp_secret.json`（网页登录态 `web_sessions.json` 刻意不包含）
- **导入恢复**：选择之前导出的 zip，当前数据库会被整体替换；恢复前自动在数据目录留一份 `auto-backup-pre-restore-时间.zip`，恢复配置文件后需重启服务才生效
- **操作记录**：页面下方显示最近 50 次备份/恢复操作（时间、来源端、文件名、结果），失败操作标红

微信里发送「备份」命令可在服务器数据目录直接生成一份备份文件（适合无浏览器时先落盘，再用网页端下载）。桌面端连接本服务时，也可以直接用桌面端窗口左下角的「备份…/恢复…」按钮完成同样的操作（远程模式自动调用本服务接口）。

### 备份加密（可选）

导出时可以在「备份密码」框里填一个口令，填了就把 zip 里的**密钥文件**加密：

| | 说明 |
|---|---|
| 加密范围 | 只加密 `config.json`、`ilink_credentials.json`、`totp_secret.json`（模型 API Key、微信登录凭据、2FA 密钥） |
| 不加密 | `data.db`（聊天记录）和 `MANIFEST.json` 始终是明文，zip 也仍是标准格式，可用任意解压软件打开查看 |
| 加密算法 | PBKDF2-HMAC-SHA256 派生密钥（120000 次迭代）+ AES-256-GCM，文件在 zip 内改名为 `原名.enc` |
| 留空 | 和以前完全一样，明文保存，任何机器都能直接导入 |
| 导入 | 程序会自动识别备份是否加密；加密的必须填同一口令，口令不对会**直接中止恢复，现有数据不受影响** |

> **口令不会被程序保存在任何地方**，忘了就再也解不开那几个 `.enc` 文件（聊天记录不受影响，仍然可以正常恢复）。

对应接口：`POST /api/backup/export`，body `{"password":"..."}`；导入时在 multipart 里加 `password` 字段。`GET /api/backup/export` 仍然是明文备份（口令放查询串会进浏览器历史和访问日志，故不支持）。

> 备份 zip 包含模型 API Key、iLink 登录凭据和 TOTP 密钥，即使填了密码，聊天数据库仍是明文，请像保管密码一样妥善保管，不要通过不可信渠道传输。

## 注意事项

- 需要微信 8.0.70+，在「我 → 设置 → 插件」中开启 ClawBot
- 只支持私信（DM），不支持群聊
- 数据全部存在本地 SQLite，不上传到任何服务器
- 首次生成画像需要积累一定数量的对方消息（默认 20 条）

## 数据目录下的文件

均与数据库同目录（Docker 下为 `/config`）：

| 文件 | 说明 |
|------|------|
| `config.json` | 配置，含模型 API Key 和 `apiToken` |
| `wechat-profile-bot.db` | SQLite 数据库（联系人、消息、画像、备份记录） |
| `bot.log` | 运行日志 |
| `security.log` | **安全日志**：登录失败、IP 封禁、限流拒绝、白名单拒绝（权限 0600） |
| `banned_ips.json` | **永久封禁名单**，重启不丢失（权限 0600） |
| `ilink_credentials.json` | 微信登录凭据，删掉需重新扫码 |
| `totp_secret.json` | 网页登录的 2FA 密钥，删掉后下次登录重新绑定 |
| `web_sessions.json` | 网页会话令牌（7 天有效），刻意不进备份 |
| `trusted_clients.json` | 可信设备令牌（90 天免登录，使用即续期），刻意不进备份，可在网页端吊销 |

这些都是运行时生成的，`.gitignore` 已排除，不要提交到仓库。

## 更新日志

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

## 关联项目

- [wechat-profile](https://github.com/caodabao99/wechat-profile) — Windows 桌面版（walk GUI）
