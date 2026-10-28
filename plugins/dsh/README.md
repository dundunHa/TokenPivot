# TokenPivot DSH 用量插件

DeepSeek Harness（DSH）的 host 插件：把每次模型请求的最终用量上报到 TokenPivot。

- 采集：`assistant/message`、`compaction/summary` 两类会话事件里的 `usage`；只采集插件启用之后的新事件，不扫描历史日志。
- 上报字段：事件 ID、provider、model、完成时间、token 数（缺失的标 `missing`，不填 0）、`project_id`。**不上报** prompt、回复内容、路径、工作目录、会话标题、凭证。
- 本地队列：`~/.tokenpivot/dsh/queue.json`（写临时文件、fsync 后改名）；断网、服务端 5xx、限流、`FUTURE_EVENT`（本机时钟比服务端快）时保留，按指数退避（遵守 `Retry-After`）重发同一事件 ID 与同一内容；DSH 重启后自动补报。服务端最终拒绝的事件（conflict / expired / 其他 rejected、单条超过 1 MiB）连同事件体写入同目录 `dead-letter.jsonl`，可人工修正后重报。
- 整批 400、401、403、415 时暂停上报（这些是凭证或插件/服务端版本问题，拆批重试只会丢数据），期间事件继续排队；改了 token 后自动恢复，否则重启 DSH 后恢复。

规则依据：[DSH 平台能力核实](../../docs/research/2026-10-28-dsh-platform-capabilities.md)、[ADR-0005](../../docs/decisions/0005-plugin-instance-and-project-identity.md)、[OpenAPI](../../docs/openapi/openapi.yaml)。

## 配置

| 键 | 说明 |
|---|---|
| `serverUrl` | TokenPivot 地址，只允许 https；`localhost`/`127.0.0.1` 可用 http |
| `token` | 接入凭证（`tpk_` 开头），volatile 保存，不落入队列文件与日志 |
| `projects` | `[{ path, projectId }]`，本地目录 → 项目 UUID，最长匹配；没匹配上为未归属 |
| `stateDir` | 队列目录，默认 `~/.tokenpivot/dsh` |
| `maxQueueItems` | 队列上限，默认 10000，超出丢弃最旧并记日志 |

## 测试

```sh
make test-plugin-dsh   # 等价于 cd plugins/dsh && node --test test/
```

测试用本地模拟服务端，不依赖 TokenPivot 服务端与 DSH。

## 安装

当前**不安装**：服务端采集接口（T-011）尚未实现。接口可用后，把本目录作为本地 bundle 装入 DSH profile（需先 `npm install` 取得 `@deepseek-ai/schemastery`），再在插件页填写配置。

升级 DSH 或新增 provider 路由后，应重新核对：① `inputTokens` 含不含缓存、是否给 totalTokens（核实文档“结论速览”第 2 行，插件靠 totalTokens 判定，没有时 input 只能记 estimated）；② 是否出现 `reasoningTokens`、outputTokens 是否含推理。
