# TokenPivot DSH 用量插件

DeepSeek Harness（DSH）的 host 插件：把每次模型请求的最终用量上报到 TokenPivot。

- 采集：`assistant/message`、`compaction/summary` 两类会话事件里的 `usage`；只采集插件启用之后的新事件，不扫描历史日志。
- 上报字段：事件 ID、provider、model、完成时间、token 数（缺失的标 `missing`，不填 0）、`duration_ms`、`generation_duration_ms`、`ttft_ms`、`project_id`（或自动归属时的 `project_key`，只有仓库目录名）。两个计时键**每次上报都出现**：host 没提供时值是 `null`，不从 `duration_ms` 推导。`duration_ms` 是一次模型调用的耗时：同一 turn、step 的 `step/start` 到最终 `assistant/message` 的事件时间差（含首 token 之前的等待；失败重试后从最后一次尝试结束重新计时）。开始事件没见到（插件在 step 中途启用）、`compaction/summary`、差值为负或超过 1 小时时上报 null，不猜；开始时间只放内存，不写队列。**不上报** prompt、回复内容、路径、工作目录、会话标题、凭证（自动归属只上报仓库目录名这一段，不含路径）。
- 本地队列：`~/.tokenpivot/dsh/queue.json`（写临时文件、fsync 后改名）；断网、服务端 5xx、限流、`FUTURE_EVENT`（本机时钟比服务端快）时保留，按指数退避（遵守 `Retry-After`）重发同一事件 ID 与同一内容；DSH 重启后自动补报。服务端最终拒绝的事件（conflict / expired / 其他 rejected，含带 `project_key` 被拒的事件、单条超过 1 MiB）连同事件体写入同目录 `dead-letter.jsonl`，可人工修正后重报；上传器不会为了让请求通过去改写队列里的事件体。
- 整批 400、401、403、415 时暂停上报（这些是凭证或插件/服务端版本问题，拆批重试只会丢数据），期间事件继续排队；改了 token 后自动恢复，否则重启 DSH 后恢复。

规则依据：[DSH 平台能力核实](../../docs/research/2026-10-28-dsh-platform-capabilities.md)、[ADR-0005](../../docs/decisions/0005-plugin-instance-and-project-identity.md)、[OpenAPI](../../docs/openapi/openapi.yaml)。

## 配置

| 键 | 说明 |
|---|---|
| `serverUrl` | TokenPivot 地址，只允许 https；`localhost`/`127.0.0.1` 可用 http |
| `token` | 接入凭证（`tpk_` 开头），volatile 保存，不落入队列文件与日志 |
| `projects` | `[{ path, projectId }]`，本地目录 → 项目 UUID，最长匹配，优先于自动归属 |
| `autoProject` | 默认开。没匹配上映射时，按会话所在 git 仓库（worktree 归主仓库）的**目录名**作 `project_key` 上报，服务端首次见到时自动建项目；不在 git 仓库里为未归属。关到 `false` 则没匹配上的一律未归属 |
| `stateDir` | 队列目录，默认 `~/.tokenpivot/dsh` |
| `maxQueueItems` | 队列上限，默认 10000，超出丢弃最旧并记日志 |

## 旧队列升级

服务端只接受 `tokenpivot-usage-event/v2`，两个计时键必填（值可以是 `null`）。旧版本插件写进队列的事件没有这两个键，直接上报会得到 `INVALID_ARGUMENT` 并进死信。升级做法：**先停掉 DSH 的写入**，再跑一次性转换脚本。

```sh
# 在仓库根目录执行；脚本本身在 plugins/dsh/scripts/
node plugins/dsh/scripts/convert-generation-v2.js            # 只读检查，报告有多少条需要补
node plugins/dsh/scripts/convert-generation-v2.js --apply    # 补缺失键并原子写回
```

- 状态目录默认取 `$TOKENPIVOT_DSH_STATE_DIR`，否则 `~/.tokenpivot/dsh`；可用 `--dir <目录>` 指定。
- `--apply` 先把原文件备份成 `queue.json.bak-<UTC 时间戳>`，再写临时文件、fsync、改名。
- 只给缺失的 `generation_duration_ms`、`ttft_ms` 补 `null`；已有 `null` 或非负安全整数原样保留；`dead-letter.jsonl` 与其他文件不动。负数、小数、超过 2^53、字符串等非法值会整体中止，零写入。
- 再跑一次 `--apply` 报 `converted: 0`，不产生新备份。退出码 0 表示成功（含 `no_queue`），1 表示失败。

## 测试

```sh
make test-plugin-dsh   # 等价于 cd plugins/dsh && node --test test/
```

测试用本地模拟服务端，不依赖 TokenPivot 服务端与 DSH。

## 安装

当前**不安装**：服务端采集接口（T-011）尚未实现。接口可用后，把本目录作为本地 bundle 装入 DSH profile（需先 `npm install` 取得 `@deepseek-ai/schemastery`），再在插件页填写配置。

升级 DSH 或新增 provider 路由后，应重新核对：① `inputTokens` 含不含缓存、是否给 totalTokens（核实文档“结论速览”第 2 行，插件靠 totalTokens 判定，没有时 input 只能记 estimated）；② 是否出现 `reasoningTokens`、outputTokens 是否含推理。
