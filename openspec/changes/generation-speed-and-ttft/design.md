# Design

## Context

本设计实现 `proposal.md` 与 `specs/request-generation-metrics/spec.md`，不改变其中的指标口径。

已核实的现状与约束：

- DSH 普通请求在 `/root/work/deepseek-harness/packages/core/agent-loop/src/agent.ts` 中为每次 attempt 新建 `AssistantStreamAttempt`。`assistant-stream.ts` 已用 `Date.now()` 给 chunk 打毫秒时间，并在 Session append 提交后发出带 `committed.seq` 的 end 帧；`packages/llm/llm/src/assistant-stream.ts` 已导出符合本需求的 `isTokenDelta`。
- DSH compaction 在 `/root/work/deepseek-harness/packages/compaction/compaction-basic/src/summarizer.ts` 直接消费 `ctx.llm.stream`，绕过普通 Agent 流；`region.ts` 才将成功结果追加为 `compaction/summary`。因此两条路径必须分别采集，再在同一个公开通知处汇合。
- `Session.append` 先提交并同步发出 `session/event`，普通流随后才发 end 帧。新插件不能假设计时通知先于最终 Session 事件。
- TokenPivot `internal/ingestion/service.go` 当前只写 `tokenpivot-usage-event/v1` 的七元素摘要，但 `docs/openapi/openapi.yaml` 已规定新增字段版本必须能从留存列重建共同版本。直接把两个字段追加到 v1 会让旧重传误判，故需要最小的 v1/v2 比较，而不是改原 v1 数组。
- `usage_events` 不可变，服务角色只有表级 SELECT/INSERT；下一迁移编号为 `00008`。现有 `(workspace_id, occurred_at, id)` 索引已覆盖滚动窗口，无需新索引。
- 现有 `session-stats` 使用 step/start 边界且 decodeTokens 未减 1，也不覆盖直接 compaction 流，本方案不复用它。
- 本 change 的 `allowedEditRoots` 只有 `/root/work/TokenPivot`。宿主文件仅是后续实施路径，执行宿主块前主代理必须取得显式 `/root/work/deepseek-harness` 跨仓权限。

## Goals / Non-Goals

**Goals:**

- 用一个非持久化、按 Agent 实例与 Session seq 关联的公开通知，把最终成功请求的三个原始时间交给插件。
- 只传输并保存整数原始计时，由后端用最终 known `output_tokens` 派生速度，避免浮点值进入规范化摘要。
- 保持 Session 持久化格式、事件身份、既有用量、分钟聚合和旧客户端重传行为不变。
- 让后端、宿主、插件和前端有明确的混合版本行为与上线顺序。

**Non-Goals:**

- 不追求真实逐 token 时间，不把 provider 请求结束或 finish 帧当作最后输出。
- 不新增通用计时框架、消息队列、配置开关或外部依赖。
- 不改 Session JSONL/持久化事件，不回填历史记录，不从 `duration_ms` 推算 decode 时长。
- 不修改无关宿主 UI、session-stats 或 TokenPivot 分钟用量统计。

## Decisions

### 1. 原始计时类型与普通流采集

在宿主 `packages/core/agent/src/runtime-types.ts` 定义唯一的公开数据形状：

```ts
export interface GenerationTiming {
  readonly requestStartedAt: number
  readonly firstOutputAt: number | null
  readonly lastOutputAt: number | null
}

export interface GenerationTimingNotice {
  readonly seq: SessionSeq
  readonly timing: GenerationTiming
}
```

在现有 Cordis 事件声明中加入：

```ts
'agent/generation-timing'(this: Scoped<Agent>, payload: { agent: Agent; notice: GenerationTimingNotice }): void
```

`AssistantStreamAttempt` 在 `start()` 记录 `requestStartedAt=Date.now()`；该位置在 `agent.ts` 消费惰性 generate 之前，代表本次 provider 请求开始。`push(chunk)` 以 `isTokenDelta(chunk)` 为基础，但 tool-call 额外要求 argumentsDelta 非空或 name 为非空字符串；不修改共享判定函数以免改变无关消费者。首个有效 chunk 固定 `firstOutputAt`，每个有效 chunk 更新 `lastOutputAt`，两者取该 push 的同一个 `Date.now()`。usage、finish、空文本和空 reasoning 不更新边界。

end 帧增加可选 `generationTiming?: GenerationTiming`。普通流仅在正常完成路径携带可发布计时，取消路径不携带；Agent 的帧发布处只在 `frame.outcome.kind === 'committed'` 且最终事件为正常完成的 `assistant/message` 时，通过 `dispatch.emit` 以单 payload `{ agent, notice: { seq: frame.outcome.seq, timing } }` 发出 `agent/generation-timing`。`interrupted: true` 消息只保留既有用量，不发布计时，不进入成功请求指标样本。每个 attempt 的字段属于其 `AssistantStreamAttempt` 实例；失败 attempt 不生成最终通知，重试的新实例重新计时。不同 Agent 不靠局部 attemptId 关联，而靠事件中的 Agent 实例与 Session 内唯一 seq 关联。

备选方案是复用 step/start 和最终 stream 重新扫描；它会混入 attempt 前等待，且要新增 last-token 扫描并处理重试，故不采用。

### 2. compaction 复用同一通知，不复用普通流状态

`SummaryResult` 增加可选 `generationTiming`。`summarizeWithLlm` 在进入 `for await` 前记录本次请求开始，在循环内使用与普通流完全相同的有效输出判定（排除空 tool name 且空 argumentsDelta）和每次迭代的 `Date.now()` 更新首末边界。recover 重新调用 summarizer 时创建全新局部计时，只有最终成功返回的 `SummaryResult` 进入提交。

`region.ts` 的 `commitCompactionBody` 改为接收 `agent`（由 `agent.session` 完成原 append），在 `compaction/summary` 的 `session/event` 之后、继续 append replacement message 之前，通过现有 `emitAgentEvent` 同步发出同一个单 payload 的 `agent/generation-timing`，seq 取刚返回的 `summaryEvent.seq`。非 LLM 构造的 SummaryResult 可不带计时，此时不发通知，插件按缺失处理。

普通与 compaction 必须遵守同一顺序：最终 `session/event` 先发生，`agent/generation-timing` 后在同一 JavaScript 调用栈发生。该顺序是公开事件约定，并用测试固定；不把计时写入 Session。

备选方案是扩大 `Session.append` 以传第三份非持久化 metadata；这会改动所有 Session 实现与调用边界，而现有 end.seq 和 Agent context 已足够，故不采用。

### 3. 插件用一次微任务兼容通知顺序与旧宿主

`plugins/dsh/index.js` 注册全局 `agent/generation-timing` 监听器，将单 payload 转交 collector；collector 不直接依赖宿主 context。`plugins/dsh/lib/collector.js` 对 `assistant/message` 和 `compaction/summary` 暂存 `{session,event,timing}`，按实际 Session 对象与 event.seq 分组（可用 WeakMap），并安排一次 `queueMicrotask` 收口。通知必须满足 `payload.agent.session === pending.session`，不只比较字符串 ID。新宿主会在同一调用栈补发计时通知；监听器只把通知填入仍等待的相同 Agent/session/seq 项。微任务执行时：

- 有匹配通知则把它传给 mapper；
- 旧宿主没有通知则立即以无计时上报，不等待定时器、不重试关联；
- 迟到或找不到 pending 项的通知被忽略，避免附着到后续请求。

`mapSessionEvent(session,event,options,timing?)` 新增纯函数映射：

- `generation_duration_ms = lastOutputAt-firstOutputAt`；任一边界缺失、差值为负或非安全整数时为 null；0 保留。
- `ttft_ms = firstOutputAt-requestStartedAt`；任一边界缺失、差值为负或非安全整数时为 null；0 保留。
- 两个值都缺失时省略两个上传字段，使旧宿主上的新插件仍发送原 v1 形状；至少一个有效时两个键都出现，另一个可为 null。
- 不计算或上传速度，不把 chunk 数当 outputTokens，不改变 `occurred_at=event.time`。原本无 usage 会被跳过的事件，若有有效计时也必须上报：未知用量字段标为 missing，不填 0，保留可用用量；两项计时均无效且原 usage 无效时仍沿用跳过行为。普通与 compaction 均测试无 usage 时 TTFT 经 mapper、队列、上传到独立样本统计的链路。

新宿主配旧插件时，旧插件忽略新增帧字段和事件；旧宿主配新插件时，微任务按缺失继续原上报路径。

### 4. 采集只增加两个整数，使用最小 v1/v2 规范化

POST ingestion event 新增：

```yaml
generation_duration_ms: { type: [integer, 'null'], minimum: 0, maximum: 9007199254740991 }
ttft_ms: { type: [integer, 'null'], minimum: 0, maximum: 9007199254740991 }
```

两个字段均可缺省、可为 null，负数和超过安全整数拒绝；缺省按 null 落列。派生速度不进入请求或摘要。

服务端按请求形状选择算法版本（客户端仍不提交版本字段）：

- 两个属性都未出现：按 `tokenpivot-usage-event/v1`，摘要仍为原七元素数组。
- 任一属性出现：按 `tokenpivot-usage-event/v2`，缺少的另一个属性归一为 null；v2 摘要为原七元素后依次追加 `generation_duration_ms`、`ttft_ms` 的九元素数组。

同版本仍直接比较 SHA-256。已知 v1/v2 跨版本时共同版本固定为 v1：v2 是纯新增字段，所有 v1 比较列仍保留；从 v2 旧行的留存列重建 v1 摘要，从本次请求投影 v1 摘要，再判 duplicate/conflict。旧行本身是 v1 时复用其 v1 hash。未知旧版本仍返回 `NORMALIZATION_INCOMPATIBLE`，不建立通用版本注册表。

因此只新增或缺失计时、原七字段相同的跨版本重传是 duplicate，首行不被更新；同 v2 重传改变任一原始计时则是 conflict。`internal/ingestion/postgres.go` 的 ExistingEvent 查询需读取重建 v1 所需的全部留存列，而不是只读 version/hash。此行为与现有 OpenAPI 第 6.5 节一致，也保证旧记录不会因补带计时而回填。

备选方案是把计时排除在摘要之外并永远写 v1；这会让同一 v2 内容的计时修正无法判 conflict，也绕开已冻结的跨版本规则，故不采用。

### 5. PostgreSQL 保存原始时长，查询时派生十进制速度

新增 `db/migrations/00008_generation_metrics.sql`：

- `ALTER TABLE usage_events ADD COLUMN generation_duration_ms bigint;`
- `ALTER TABLE usage_events ADD COLUMN ttft_ms bigint;`
- 两列均可空、无默认值、不回填；遵循现有约定，不增加 CHECK/FK/RLS。表级 SELECT/INSERT 授权继续适用，不新增索引。
- Down 仅用于明确接受删除新数据的环境；正常回滚保留列，见 Migration Plan。

`internal/ingestion` 保存两个原始字段。`internal/requestdetails/postgres.go` 用以下同一过滤条件识别有效速度：

```sql
e.output_tokens_quality = 'known'
AND e.output_tokens > 1
AND e.generation_duration_ms > 0
```

每请求速度表达式为：

```sql
round(
  ((e.output_tokens - 1)::numeric * 1000) / e.generation_duration_ms,
  6
)
```

`generation_duration_ms=0` 被保留为原始观测，但速度为 null；estimated/missing output、N≤1、null 或非正 decode 时长均不进入速度。TTFT 只要求 `ttft_ms IS NOT NULL`，与速度样本独立。

滚动窗口仍用同一 SQL 语句的 `statement_timestamp()` 和 `occurred_at ∈ [as_of-60s,as_of)`。平均速度对未舍入的每请求 numeric 表达式做 `avg` 后再 `round(...,6)`；平均 TTFT 对整数毫秒做 `avg` 后同样处理。两个 `count FILTER` 分别使用各自有效条件。平均值以十进制字符串返回，避免 JSON 浮点与精度差异；显示层再格式化为 49.5 tokens/s、8000 ms 等。

### 6. 读取接口采用新增字段，保留旧响应便于混合上线

`RequestDetail` 新增可选响应字段；新后端始终返回，旧后端可能省略：

```json
{
  "generation_speed_tps": "49.500000",
  "ttft_ms": 8000
}
```

`usage-rate` 新增两个可选对象，各自带单位和独立样本数：

```json
{
  "generation_speed": {"unit":"tokens/s","value":"49.500000","sample_count":1},
  "ttft": {"unit":"ms","value":"8000.000000","sample_count":1}
}
```

现有 `unit=tokens/min`、`total_tokens` 与窗口元数据暂时保留为旧前端兼容字段；新前端不再展示旧 tokens/min 速率。这样旧前端可读取新后端。新前端解析旧后端时，将缺少的新响应字段视为 null、样本数 0，而不是报错或显示 0。

`docs/openapi/openapi.yaml` 在基础块一次性冻结 ingestion v1/v2、两个存储字段及以上读取形状；`docs/data-model.md` 同步说明可空列与不可变行为。无新增 API 路径。

### 7. 前端只展示后端口径

`web/src/features/requests/api.ts` 增加可空十进制字符串解析和上述可选字段兼容；`model.ts` 只传递后端值，不重算速度或平均值。`RequestLogPanel.tsx`：

- 每行增加“生成速度 (tokens/s)”与“TTFT (ms)”；
- 顶部把旧“近60秒速率(tokens/min)”替换为“近60秒平均生成速度”和“近60秒平均TTFT”，分别显示有效样本数；
- 缺失显示既有未知样式（如 `—`），样本数为 0 时平均值同样为未知；
- 用短说明标明“客户端观测的 chunk 估算，非逐 token 精确值”和滚动 60 秒窗口。

不增加前端定时计算、token 加权或 UI 自动化验收；行为由解析与组件测试覆盖。

## Risks / Trade-offs

- [毫秒 wall clock 可能回拨，且一个 chunk 可含多个 token] → 负差值按缺失，0 decode 不计算速度；界面明确 chunk 估算，绝不伪造逐 token 精度。
- [计时通知与 Session 事件顺序接错会串请求] → 公开约定固定“Session 事件先、计时通知后、同调用栈”，用 Agent 实例+seq 关联；插件一次微任务收口并测试并发、迟到通知和旧宿主。
- [新字段直接改变 v1 摘要会破坏重传] → 请求形状选择 v1/v2，跨版本双向重建 v1；增加旧 v1→新 v2、旧 v2→旧 v1、同 v2 计时冲突测试。
- [新插件向旧后端发送新字段会被逐项拒绝] → 后端先于插件上线；旧宿主无计时时插件省略字段。回滚先停新插件并处理含新字段的队列。
- [numeric 逐请求除法比整数求和成本高] → 仅作用于当前工作空间近 60 秒原始行，沿用现有 occurred_at 索引，不新增通用聚合层。
- [保留旧 tokens/min 响应字段会增加少量查询与文档复杂度] → 这是旧前端兼容所需的临时成本；新 UI 完全不消费旧速率，后续另开破坏性版本再移除，本 change 不扩张清理范围。

## Migration Plan

1. 在任何新写入前应用 `00008`；旧后端忽略新增可空列，可继续运行。
2. 部署支持 v1/v2 与新读取字段的 TokenPivot 后端，再部署包含公开计时通知的 DSH 宿主。两者实现可并行，但插件发布必须等两者都就绪。
3. 发布 TokenPivot 插件。无计时或旧宿主仍发 v1；有原始计时才发 v2。
4. 最后发布前端。旧前端配新后端仍可读；新前端配旧后端仅显示未知。
5. 验证旧插件/新宿主、新插件/旧宿主、新插件/新后端及历史记录，再恢复正式流量。

回滚顺序为前端 → 插件 → 宿主；插件回滚前先停止接收含新字段的队列或确保这些项目仍由新后端处理。数据库列正常保留，不执行 Down。后端优先向前修复：原始旧二进制不认识已落库的 v2 行，若直接回退，相关身份重传会得到 `NORMALIZATION_INCOMPATIBLE`；完整后端回退必须使用仍能识别 v2 的兼容构建，而不是假装 v2 从未写入。

## 依据

- vLLM 固定提交 [`serve.py` L654-L663](https://github.com/vllm-project/vllm/blob/20ad1db3fe900ce07c2ee2ccdac077ed9826530b/vllm/benchmarks/serve.py#L654-L663)，2026-11-12 已核实：TPOT 使用 `(latency-ttft)/(N-1)`。采用其“排除首个输出、N 减 1”原则；不采用其请求结束终点，本项目终点明确为最后有效输出。
- GenAI-Perf [`Metrics Output Token Throughput Per User`](https://raw.githubusercontent.com/triton-inference-server/perf_analyzer/main/genai-perf/README.md)，2026-11-12 已核实：每用户输出吞吐排除 first token、关注生成阶段。采用算术公式旁证，不引入其工具或请求结束语义。
- vLLM [`issue #6531 comment`](https://github.com/vllm-project/vllm/issues/6531#issuecomment-2350902657)，2024-09-14 已核实：一个 chunk 可包含多个 token。采用“单 chunk 多 token 时 decode 时长可为 0、速度缺失”，拒绝把 chunk 数当 N。
- 以上均为社区口径依据；实际关联、事件顺序与存储方案只依据本仓库和宿主已核实代码，未引入新依赖。

## 执行切分

- **F 宿主基础块**：拥有宿主 `runtime-types.ts`、普通流、compaction 与对应测试；是插件的串行前置。执行前需显式宿主跨仓权限。
- **B TokenPivot 采集基础块**：拥有 `00008`、ingestion、`docs/openapi/openapi.yaml`、`docs/data-model.md` 及采集测试；与 F 可并行，是读取、插件的串行前置。OpenAPI 本 change 的全部修改只归 B，避免共享文件冲突。
- **C 读取块**：拥有 `internal/requestdetails` 及读取测试，依赖 B。
- **D 插件块**：拥有 `plugins/dsh` 与 Node 测试，依赖 F+B。
- **E 前端块**：拥有 `web/src/features/requests` 及前端测试，依赖 C，可在 D 同时并行。
- **V 联调验证块**：依赖全部块，只执行测试、迁移验证与上线检查；发现修改退回所属块。

本方案无新依赖、无 lockfile/package.json 改动、无 migration 编号竞争；`00008` 仅 B 使用，宿主公共类型仅 F 修改，父代理无需在合并后重新生成共享产物。
