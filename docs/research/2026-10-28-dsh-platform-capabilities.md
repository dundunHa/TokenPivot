# DSH 平台能力核实（T-043）

日期：2026-10-28。对象：DeepSeek Harness（DSH）0.2.1-rc.3，会话格式 v4。PI 部分不在本次范围，留给 T-047。

证据来源：本机 `~/.dsh/sessions/<cwd-slug>/<id>/session.v4.jsonl.zstd` 日志统计（1102 个会话文件、23783 条 assistant 消息），以及 DSH host 的 inspect 接口（`Event.listEvents`、`Service.listService`）返回的事件签名与类型声明。

## 结论速览

| 问题 | 结论 | 证据 |
|---|---|---|
| 能否拿到请求级最终用量 | 能。`assistant/message` 的 `data.usage`、`compaction/summary` 的 `data.usage` | 类型 `TokenUsage { inputTokens; outputTokens; totalTokens?; cacheReadTokens?; cacheWriteTokens?; reasoningTokens? }` |
| `inputTokens` 是否含缓存 | **不含**。TokenPivot 的 input 要加回 cacheRead 与 cacheWrite | 23468 条 `totalTokens == input+output+cacheRead+cacheWrite`；其余 292 条没有缓存字段，`totalTokens == input+output` |
| 推理 token | 本机样本从未出现 `reasoningTokens`，推理计入 output；上报为缺失 | 全量统计 0 次出现 |
| 缓存字段 | 视 provider 而定，可能只有 read、只有 write、都有或都没有 | 键组合：{cacheRead} 20040、{cacheRead,cacheWrite} 3132、{cacheWrite} 401、都没有 187 |
| 请求完成时间 | 事件 `time`（毫秒 epoch，提交时刻）≈ 请求完成时刻 | 事件结构 `{type, seq, time, data}` |
| 稳定事件 ID | `data.message.id`（UUID）。fork/seed 出的会话复制父事件时 id 不变 | 跨文件重复 1758 个 id，时间、用量、模型全部一致；同文件内重复 0 |
| compaction 的 ID | `data.compactionId`，只在单个会话内唯一，需加会话 id 前缀 | 类型声明 |
| provider / model | `message.source.provider/model`；provider 是 DSH 里配置的**路由名**（如 nebius、deepseek-official、anthropic-bf），不是厂商名 | 样本取值 |
| 请求状态 / 耗时 | 拿不到。成功的 `assistant/message` 没有状态与耗时字段；上报 null | 类型声明 |
| 失败调用 | `assistant/attempt`（失败的尝试，如 TIMEOUT）没有 usage，失败请求的消耗无法得知 | 类型声明与样本 |
| 无用量的模型调用 | `session/title-llm-request`、`web/deepseek-search-llm-request` 没有 usage，无法采集 | 样本 |
| 无 usage 的 assistant 消息 | 极少（全量 23 条），多为中断；跳过，不当 0 上报 | 统计 |
| 插件拿事件的方式 | host 事件 `'session/event'(session, event)`：提交后触发、不等待监听者；监听者出错只记日志 | `Event.listEvents` |
| 会话工作目录 | `session.header.cwd`（可选） | `SessionHeader` 类型 |

## 对插件的约束

1. 只采集 `assistant/message` 与 `compaction/summary`；其余模型调用不可采集，看板上这部分用量缺失属已知限制。
2. 用 `message.id` 作事件 ID 可以天然消除 fork 带来的重复；compaction 用 `<session id>:<compactionId>`。
3. `session/event` 只对**实时追加**的事件触发；fork 时复制进新会话的父事件不经由它重新触发（种子事件随会话创建一次性载入）。即使触发，也因 message.id 相同被服务端判为 duplicate。
4. 插件不得往会话日志追加新事件类型；自身数据放插件自己的目录。
5. 调查样本统计只在本机做，结论随 DSH 版本可能变化；升级 DSH 后应重跑同样的统计（见 `plugins/dsh/README.md`）。

## 未核实

- 未在已安装状态下实测 `session/event` 对 fork 种子事件是否触发（插件按用户决定不安装）；第 3 条依据为类型说明，不是实测。
- `reasoningTokens` 在支持推理计数的 provider 上是否会出现：本机没有样本。
