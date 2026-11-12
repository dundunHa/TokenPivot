# Proposal

## Why

当前请求速率把请求总 token 累加后换算成 tokens/min，既不代表模型在生成阶段的输出速度，也没有首 token 延迟（TTFT，从请求开始到首个有效输出的耗时）。用户因而无法区分生成慢、首响应慢和仅用量较高的请求。

## What Changes

- 为普通请求与 compaction 请求增加客户端观测的生成速度和 TTFT：速度采用 `(N-1)/(最后有效输出时间-首个有效输出时间)` tokens/s，TTFT 采用 `首个有效输出时间-请求开始时间` ms；明确这是基于流式 chunk 的估算，不是逐 token 精确测量。
- 在 DSH 宿主暴露请求级可选计时元数据，TokenPivot `plugins/dsh` 将其随最终 provider `outputTokens` 一并上报；重试 attempt 分开计时，只采用最终成功 attempt 的计时和最终事件顺序，禁止跨 attempt 拼接。
- 无最终用量但有有效计时的请求仍上报，未知用量标为 missing；中断请求保留既有用量但不纳入成功请求指标。采集接口与 PostgreSQL 明细以可空字段保存新指标；旧记录、缺失计时及无效样本保持未知，不回填、不按 0 处理，也不改变既有分钟聚合口径。
- 请求明细展示每请求速度与 TTFT；顶部滚动近 60 秒分别展示有效请求的算术平均速度和平均 TTFT，以及各自独立有效样本数。
- 普通流与绕过 Agent 流的 compaction 汇总分别接入同一数据形状，但不引入通用计时框架，不修改 Session 持久化格式或无关统计。
- 宿主改动仅在后续获得显式 `/root/work/deepseek-harness` 跨仓执行上下文后实施；本 change 的 repo-local 权限不自动扩展到宿主。

## Capabilities

### New Capabilities

- `request-generation-metrics`: 定义请求级生成速度与 TTFT 的采集、有效性、持久化、滚动窗口汇总及展示行为。

### Modified Capabilities

无。

## Impact

- DSH 宿主：普通 AssistantStream/Agent 重试关联、compaction summarizer 的可选计时元数据，以及向插件公开的最终事件边界。
- TokenPivot 插件：`plugins/dsh/lib/mapper.js`、`plugins/dsh/lib/collector.js` 及相应测试。
- TokenPivot 后端：采集接口模型、规范化与 PostgreSQL 请求明细、滚动 60 秒查询和服务层汇总。
- TokenPivot 前端：请求明细类型、请求日志列与顶部指标展示。
- 兼容性：新增字段可空，旧客户端、旧记录及新旧版本混合上线均保留“缺失”语义；不改变事件身份、既有用量和分钟聚合。
