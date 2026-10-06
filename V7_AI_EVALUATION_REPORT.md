# V7 AI Evaluation Report — AI Evaluation Lab（蓝图 §18 / §19）

> 生成时间：2026-10-06（Phase 16 Documentation）
> 定位：Server v7.0.0 的**离线、确定性、零真实用户数据** AI 质量评测底座
> 代码：`eval.go`（评分器/指标/对比/回归）+ `tests/evaldata/golden_cases.json`（黄金集）+ `eval_test.go`（钉）

---

## 1. 设计约束（为什么这样做）

- **离线**：评测器不联网、不调用任何真实模型，只吃「已录制的样本输出」，可重复、可进 CI、零成本。
- **确定性**：同一黄金集 + 同一评分器 → 每次结果完全一致（无随机、无时钟依赖）。
- **零真实用户数据**：黄金集 22 例**全部为虚拟联系人**，绝不包含任何真实聊天（§32 红线）。
- **单一评分真相**：JSON 抽取**复用 `llm.go` 的 `ExtractJSON`**，评测与生产解析同一条代码路径——不会出现「评测过了生产却解析失败」的口径漂移。
- **可切真实模型**：runner 只需用真实模型输出替换 `model_outputs` 里的样本，同一套指标/对比/回归逻辑原样复用。

---

## 2. 黄金集规模

| 维度 | 值 |
|------|----|
| 数据集版本 | `eval-lab-v1` |
| 用例总数 | 22（≥ §18 要求的 ≥20） |
| 覆盖任务类型 | **11** 类（≥ 要求的 ≥11） |
| 对比模型档位 | 3：`model-a`=理想 / `model-b`=一般 / `model-c`=坏输出(非法 JSON) |
| 每例附带元数据 | `latency_ms` / `tokens`（供成本与延迟维度） |

**11 类任务**：`fact_extraction` · `attach_evidence` · `detect_conflict` ·
`memory_consolidation` · `memory_review_queue` · `profile_summary` ·
`relationship_session_brief` · `coaching_suggestion` · `simulate_action` ·
`decision_rank` · `qa_answer`

---

## 3. §18.2 指标定义（5 质量维 + 2 聚合）

| 指标 | 含义 | 方向 |
|------|------|------|
| `json_validity` | 输出能否被 `ExtractJSON` 解出合法 JSON | 越高越好 |
| `required_field_coverage` | 契约要求的字段是否齐全 | 越高越好 |
| `snippet_recall` | 应召回的证据片段命中率 | 越高越好 |
| `evidence_attribution` | 每条结论是否挂上证据（可溯源） | 越高越好 |
| `hallucination` | 命中禁止臆造片段的比例 | **越低越好** |
| `accuracy` | 综合准确率（各 case 判定为「通过」的占比） | 越高越好 |
| `failure_rate` | 无法解析/违约的失败率 | 越低越好 |

`RunEvaluation` 聚合出 `EvalReport`；空分母一律记 0（`evalRatio` 除零安全），绝不 NaN。

---

## 4. §18.3 模型对比 & §18.4 回归

- **CompareModels 排序键**（确定性、无歧义）：`accuracy↓ → failure_rate↑ → tokens↑ → 模型名↑`。
- **DetectRegression**：同任务两版对比，关键指标劣化超过 `tolerance` → 标 `REGRESSION`（如理想→坏，`json_validity` 必被标出）；同模型自比 → 判不回归（基线稳定）。

---

## 5. 实测结果（全绿，`go test` 摘录）

| 测试 | 断言（= 真值） | 结果 |
|------|----------------|------|
| `TestGoldenSetCoversAllTaskTypes` | ≥20 例 / ≥11 类 / 三档样本齐 | ✅ PASS |
| `TestIdealModelScoresPerfect` | model-a `accuracy==1.0` · `json_validity==1.0` · `hallucination==0`，五指标名俱在 | ✅ PASS |
| `TestBadModelFailsJSON` | model-c `json_validity==0` · `failure_rate==1.0` | ✅ PASS |
| `TestMidModelSitsInBetween` | model-b `accuracy∈(0,1)`（严格居中，证评分有区分度） | ✅ PASS |
| `TestCompareModelsRanking` | model-a 居首、整体降序 | ✅ PASS |
| `TestDetectRegressionFlagsDegradation` | 理想→坏判回归(含 json_validity)；自比不回归 | ✅ PASS |

**解读**：三档模型得到「完美 / 中间 / 归零」的单调可分评分，证明评分器既有绝对判据又有相对区分度——真实模型替换样本后即可据此选型/验收。

---

## 6. §19 缓存 / Prompt / Model 治理（同 Phase 交付）

切模型或改 Prompt 时「缓存绝不能串」的结构性保证：

- **缓存键五分量**：`aiCacheKey{ContactID, Task, ContextVersion, Model, PromptVersion}`；
  `valid()` 要求**五者齐全**才启用缓存，任一为空 → 不写不读（宁可重算不可污染）。
- **主键即五列**：`ai_response_cache` 的 `PRIMARY KEY (contact_id, task, context_version, model, prompt_version)`。
  → 换模型 / 改 Prompt / Context 版本演进时键自然变化，旧缓存**天然失效、不可能命中错值**。

| 测试 | 断言 | 结果 |
|------|------|------|
| `TestCacheKeyRequiresAllFiveComponents` | 逐个削掉任一分量 → `valid()==false` | ✅ PASS |
| `TestCachePrimaryKeyIsFiveComponents` | 从 `SQLITE_MASTER` DDL 实取主键恰含这五列 | ✅ PASS |
| `TestCacheHitLoggedSeparatelyFromRealCalls` | 缓存命中不计入真实 API 消耗口径 | ✅ PASS |

---

## 7. 诚实边界（PARTIAL 说明）

- 黄金集是**录制的静态样本**，非实时真实模型跑分；本 Phase 交付的是**评测底座与治理钉**，
  真实模型基准由运维在切换模型时用 runner 现录 `model_outputs` 再跑同一套逻辑。
- 评测为库/测试形态（`go test`），**未暴露 `/api/eval` HTTP 端点**——刻意如此：
  评测不应成为可被外部触发的运行时接口（§32 最小暴露面）。
