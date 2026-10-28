# T-043 / T-044 / T-045：DSH 平台核实、标识规则与采集插件

关联：T-043（只做 DSH，PI 留给 T-047）、T-044、T-045。

## 用户决策（2026-10-28）

- T-043 只核实 DSH；PI 能力核实与 PI 插件都留给 T-047。
- 插件代码放本仓库 `plugins/dsh/`，**不安装**到任何 DSH profile（服务端 T-011 采集接口还没实现）。
- 项目归属：插件配置写「本地目录 → project_id」映射，没配上的事件发 `project_id: null`（未归属）；不改服务端、不改 OpenAPI。
- 采集范围：`assistant/message` 与 `compaction/summary` 两类事件，只采集插件启用之后的新事件，不扫描历史日志；断网时本地排队、恢复后补报。

## 交付物

| 任务 | 文件 | 内容 |
|---|---|---|
| T-043 | `docs/research/2026-10-28-dsh-platform-capabilities.md` | DSH 0.2.1-rc.3 的用量字段、时间、稳定 ID、事件种类与缺口，附本机样本统计 |
| T-044 | `docs/decisions/0005-plugin-instance-and-project-identity.md` | 插件实例（source）与项目标识规则 |
| T-045 | `plugins/dsh/`（`index.js` 接线、`lib/mapper.js` 映射、`lib/queue.js` 本地队列、`lib/uploader.js` 上报、`test/*.test.js`） | DSH host 插件；`node --test` 跑测试，用本地模拟服务端 |

## 设计要点

1. 监听 host 事件 `session/event`，只处理 `assistant/message`（`data.usage` 存在）与 `compaction/summary`（`data.usage` 存在）。
2. `source_event_id`：`dsh:msg:<message.id>`、`dsh:compaction:<session.id>:<compactionId>`。fork/seed 出的会话复制父会话事件时 message.id 不变，服务端按同一身份判 duplicate。
3. 用量映射：DSH 的 `inputTokens` 不含缓存，所以 `input_tokens = inputTokens + cacheReadTokens + cacheWriteTokens`；`output_tokens = outputTokens`；`total = input + output`，与 DSH `totalTokens` 不一致时标缺失；缓存字段没出现标缺失；`reasoning_tokens` 有字段才报，否则缺失。`status`、`duration_ms` 取不到，填 null。
4. 不上报 prompt、回复内容、路径、cwd、会话标题；cwd 只在本地用于匹配项目映射。
5. 本地队列：JSON 文件原子写（写临时文件再 rename），默认目录 `~/.tokenpivot/dsh/`；按结果出队：accepted/duplicate/conflict/expired/rejected 出队（后三种追加到 `dead-letter.jsonl` 留证），retryable、5xx、网络错误保留并指数退避（遵守 `Retry-After`）；401/403 暂停上报直至配置变化或重启；413 把批次对半拆。队列有上限，超出丢最旧并计数日志。
6. 重传沿用同一 `source_event_id` 与相同内容（入队时就冻结事件体）。
7. 令牌只从 volatile 配置读，不写日志、不落盘。

## 验收

- `node --test plugins/dsh/test/` 全绿：映射（含缺失、total 不一致、无缓存字段、compaction）、队列持久化与重启恢复、上报结果分类、503 + Retry-After 退避、断网后补报且事件 ID 不变、413 拆批、401 暂停、令牌不出现在日志与队列文件。
- 不安装插件、不改服务端代码。

## 非目标

PI 插件与 PI 核实（T-047）；服务端采集接口（T-011）；source/project 创建与管理 UI（T-021/T-041）；历史日志回填。
