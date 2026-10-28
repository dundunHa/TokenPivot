# TokenPivot DSH 用量插件

DeepSeek Harness（DSH）的 host 插件：把每次模型请求的最终用量上报到 TokenPivot。

- 采集：`assistant/message`、`compaction/summary` 两类会话事件里的 `usage`；只采集插件启用之后的新事件，不扫描历史日志。
- 上报字段：事件 ID、provider、model、完成时间、token 数（缺失的标 `missing`，不填 0）、`project_id`。**不上报** prompt、回复内容、路径、工作目录、会话标题、凭证。
- 本地队列：`~/.tokenpivot/dsh/queue.json`；断网、服务端 5xx、限流时保留，按指数退避（遵守 `Retry-After`）重发同一事件 ID；DSH 重启后自动补报。服务端最终拒绝的事件 ID 与原因写入同目录 `dead-letter.jsonl`。
- 凭证被拒（401/403）时暂停上报，修改配置或重启 DSH 后恢复，期间事件继续排队。

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

升级 DSH 后，应重新核对 `inputTokens` 是否仍不含缓存（核实文档“结论速览”第 2 行），口径变化会让 input 统计翻倍或偏少。
