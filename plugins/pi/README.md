# TokenPivot PI 用量插件

把 PI（`@earendil-works/pi-coding-agent`）的用量事件上报到 TokenPivot 的
`POST /api/v1/ingestion/events`。采集端只认「请求级最终用量」，不做估算式回填。

- 计划与验收依据：`docs/plans/2026-11-05-pi-usage-plugin.md`（T-047）。
- 接口格式：`docs/openapi/openapi.yaml` 的 `UsageEvent` / `UsageObject` / `ItemResult`。
- 无 npm 依赖：队列用 Node 内置 `node:sqlite`（需 Node ≥ 22.19）。

## 1. 安装

插件是目录型 PI 扩展，`plugins/pi/package.json` 里的 `pi.extensions: ["./index.js"]`
就是入口声明。安装只是在 PI 的全局扩展目录里建一个指向本目录的软链：

```sh
ln -s /path/to/TokenPivot/plugins/pi ~/.pi/agent/extensions/tokenpivot-pi
```

安装前先看目标是否已存在、是否软链：

```sh
ls -l ~/.pi/agent/extensions/tokenpivot-pi
```

PI 的扩展发现规则是「`extensions/*/package.json` 带 `pi.extensions` 字段」时按清单加载，
详见 PI 包内 `docs/extensions.md` 与 `dist/core/extensions/loader.js` 的
`resolveExtensionEntries()`。

## 2. 配置

默认配置 `~/.tokenpivot/pi/config.json`：

```json
{
  "serverUrl": "https://tokenpivot.example.com",
  "token": "tpk_PLACEHOLDER",
  "projects": [
    { "path": "/Users/me/code/project-a", "projectId": "0199a0b0-0000-7000-8000-00000000000a" }
  ],
  "stateDir": "~/.tokenpivot/pi",
  "maxQueueItems": 10000,
  "flushDelayMs": 2000,
  "requestTimeoutMs": 30000
}
```

| 键 | 含义 | 默认 |
|---|---|---|
| `serverUrl` | 服务地址。只允许 https；本机回环地址可用 http；拒绝 URL 用户信息与查询串；自动补成 `/api/v1/ingestion/events` | 无（空 = 已安装待配置，不采集） |
| `token` | 接入凭证（`tpk_` 开头）。也可以不放这里 | 无 |
| `projects` | 本地目录 → 项目 UUID；未匹配的用量上报为未归属 | `[]` |
| `stateDir` | 队列目录（`queue.sqlite`） | `~/.tokenpivot/pi` |
| `maxQueueItems` | 待上报事件上限，超出转 overflow | `10000` |
| `flushDelayMs` | 新事件后延迟多久开始上报 | `2000` |
| `requestTimeoutMs` | 单次 HTTP 请求超时 | `30000` |

环境变量（优先于配置文件，改了要重启 PI）：

- `TOKENPIVOT_PI_CONFIG`：配置文件的绝对路径。
- `TOKENPIVOT_PI_TOKEN`：接入凭证。用它可以不把 token 写进配置文件。

权限要求：配置文件里**只要写了非空 token**，文件必须 `0600`、所在目录必须 `0700`——
即使 `TOKENPIVOT_PI_TOKEN` 优先生效、实际用的是环境变量里的凭证，也一样检查。
不满足时插件**暂停启用**并输出固定原因码 `config_permissions`，不会替你去 chmod 别人的文件。
软链形式的配置文件同样判为不满足（`config_symlink`）。文件里没有凭证时不检查权限
（没有可泄露的东西）。

项目 ID 由服务端预先创建；本地目录只用于匹配，从不上报。

## 3. 采集什么、不采集什么

采集：`turn_end` 里**最终**的 assistant 消息，且同时具备

- 可核实的完成时间：`message.timestamp` + `message.durationMs`；
- 真实的模型归属：`message.provider` 与 `message.responseModel`（无效时回落到 `message.model`）；
- 请求级最终用量：`usage.input / output / cacheRead / cacheWrite / totalTokens`。

`occurred_at` 是 `timestamp + durationMs`，不用采集时间或开始时间替代。
`source_event_id` 是 `pi:msg:<sessionId>:<entryId>`：`entryId` 只在会话内唯一，
两个会话即使 `entryId`、时间、用量都相同也是两条事件。

跳过并计数的情形（`stderr` 里的 `count name=skip_*`）：

| 原因码 | 含义 |
|---|---|
| `skip_pending` / `skip_deferred` | 不是终态结果 |
| `skip_no_duration` | 没有 `durationMs`，完成时间不可证实 |
| `skip_not_assistant` | 不是 assistant 消息 |
| `skip_usage_missing` | 用量全缺，不是请求级最终用量 |
| `skip_no_provider` / `skip_no_model` | 没有可核实的归属 |
| `skip_invalid_id` | 会话 ID 或条目 ID 缺失/超长 |
| `skip_collect_disabled_no_token` | 只有服务地址、还没有凭证：已安装待配置，不开始新采集 |
| `compact_skipped` | 压缩可能合并两次摘要请求，首版只计数不映射 |

缺凭证或缺服务地址时**不开始新采集**，也不会把事件先塞进队列：已有队列原样保留，
补齐配置后由 30 秒配置检查恢复采集，不需要重启 PI。
不采集：Prompt、回答、工具内容、工作目录（只在本地用于项目匹配）、会话标题、供应商密钥。
不扫描历史会话；fork 复制出来的历史条目不会重传；续聊只采新通知。

用量口径（来源是 PI 的 `Usage`）：

- `cacheWrite1h` 是 `cacheWrite` 的子集，不再相加；`reasoning` 是 `output` 的子集，不加到 `output` 或 `total`。
- 服务端 `input` 含缓存。来源 `input` 的口径按 `total` 恒等关系判断：
  `total = input + output + cacheRead + cacheWrite` → 上报 `input + cacheRead + cacheWrite`；
  `total = input + output` → 原样上报 `input`；缓存全为已知零 → 两式等价，`input` 精确。
  证据不足时保留来源数值但标 `estimated`，不把未知缓存当作已知零。
- `total` 保留来源数值，不与独立数值取最大值；缺失就报 `missing`，让服务端按规则推导。
- 缺失指标保持 `missing`，不补 0；非法值单独计数（`count name=usage_invalid`）并按缺失处理。

## 4. 队列、容量与磁盘

- `stateDir/queue.sqlite`，`journal_mode=DELETE`、`synchronous=FULL`；目录 `0700`、数据库 `0600`。
- 事件入队即冻结完整事件体；同 ID 再次通知复用首次内容，不会因配置变化重算项目/模型/时间。
- 同 ID 不同内容视为冲突（`queue_conflict`），不覆盖首次内容。
- 只有 `COMMIT` 成功才算已采集。`SQLITE_BUSY` 不同步等待：入队按异步退避重试，耗尽记 `queue_failed`。
- `maxQueueItems` 只限制待上报事件（`pending`）。超额写入 `overflow` 状态并计数，不静默删除。
- 永久拒绝的事件转 `dead` 状态保留，便于人工排查；`dead` 与 `overflow` 都**不会自动清理**，
  长时间异常会让文件增长。清理前先停 PI，直接删 `queue.sqlite` 会连同未确认事件一起丢。
- 数据库或 rollback journal 损坏时，插件保留现场并暂停（`queue_unavailable`），不覆盖重建。
  发现非空旧格式 `queue.json` 时同样暂停（`legacy_state`），需要显式迁移。
- 队列目录是恢复单元。配置里的 `stateDir` 变了，插件会停旧库、按新目录重开（旧目录原样保留、
  不迁移不合并）；换服务地址请同时换新目录，旧队列不会自动转投。
- 队列目录打不开（软链、权限、临时 IO）时插件暂停，并在每次配置检查时重试同一目录；
  修好配置后自动重开，不要求重启 PI。损坏与旧格式不重试，保留现场等人处理。

## 5. 上报、暂停与恢复

- 每批最多 100 条、小于 1 MiB；逐项应用服务端结果，并按接口文档逐项校验：
  `accepted`/`duplicate`/`conflict`/`expired` 不得携带 `error` 字段；
  `rejected` 的错误码必须在白名单（`INVALID_ARGUMENT`、`FUTURE_EVENT`、`PROJECT_INVALID`、
  `USAGE_UNSUPPORTED`、`USAGE_OVERFLOW`、`NORMALIZATION_INCOMPATIBLE`）；
  `retryable` 的错误码必须是 `UNAVAILABLE`/`INTERNAL`/`DEADLINE_EXCEEDED`。
  任何一项不合格都按畸形整批保留退避——服务端的自由文本不会被写进队列。
- `accepted`/`duplicate` 出队，`conflict`/`expired`/永久 `rejected` 转死信，
  `rejected/FUTURE_EVENT`、`retryable`、未知状态、畸形 200 一律保留并退避。
- `429` / `5xx` / 网络错误按 `Retry-After`（秒数或 HTTP-date）或指数退避重试。
- `413` 对半拆批；单条仍超限转死信。
- `400` / `401` / `403` / `415` 暂停自动发送，队列保留。
- 超时覆盖到响应体读完为止：响应头已到、正文卡住同样按超时处理；`session_shutdown` 会打断它。
- 每批发车前重新读取地址与凭证；响应处理前再确认本批实际使用的地址/指纹/revision 仍是当前绑定，
  不一致就整批保留、结束本轮上报，后续批次绝不沿用旧凭证。
- 不跟随重定向；`Authorization` 只发往配置端点。日志只有固定错误码、计数与 HTTP status。

暂停的原因码（`stderr` 的 `upload_paused reason=...` 与 `config_state`）：

| 原因码 | 含义与恢复方式 |
|---|---|
| `http_400` / `http_401` / `http_403` / `http_415` | 检查凭证与插件/服务端版本；**配置实际变化**（地址或凭证）或用户确认同源后才恢复，配置没改时 30 秒检查不会自行解除暂停 |
| `token_changed` | 接入凭证变了，需要用户确认它仍属原 source（见第 6 节） |
| `server_changed` | 服务地址变了。必须换新的 `stateDir`，旧队列不会自动转投 |
| `binding_missing` | 库里已有事件但没有绑定元数据，需要人工处理 |
| `config_permissions` / `config_symlink` | 修文件/目录权限后自动恢复 |
| `config_invalid` / `config_unreadable` / `server_url_*` | 修配置后自动恢复 |

暂停恢复不依赖新事件：插件每 30 秒独立检查一次配置，配置修好就唤醒上报。
环境变量的变更需要重启 PI。

## 6. 更换凭证（同一 source）

凭证指纹（`SHA-256("TokenPivotPI/token/v1\0" + token)`）变了，插件会暂停新采集与发送、
保留队列，直到用户确认「新凭证仍属原 source」。顺序很重要：

1. 停掉所有共用同一 `stateDir` 的 PI 进程（已经发出的请求无法追回）。
2. 更新 token（配置文件或环境变量），启动**一个** PI。
3. 执行 `/tokenpivot-confirm-same-source`。
4. 确认后启动其余进程。

命令显示目的服务标识（仅 scheme/host/port）与「新凭证仍属原 source，继续发送旧事件」的含义，
按库中观察到的旧指纹 + `bindingRevision` 做条件更新（成功后 `bindingRevision + 1`）；
服务地址不匹配或条件已变化就保持暂停，不会出现后写覆盖。确认成功后立即唤醒上报。
换到 B 之后再换 C，仍需再次确认。

> PI 的扩展命令用 `pi.registerCommand()` 注册（PI 包 `docs/extensions.md` 的
> `pi.registerCommand(name, options)` 一节），所以本插件直接提供斜杠命令，无需额外导出接口。
> 命令在 headless / RPC 模式下没有可用的 `ui.notify` 时会静默，但确认动作本身照常执行；
> 可用 `stderr` 的 `binding_confirmed revision=N` 观察到结果。

换 source（不是同一 source 的轮换）请用新的 `stateDir`。source 只由服务端认证决定：
本地只能检测 token 字节是否变化，"仍属原 source" 是用户的归属断言，不是服务端的证明；
同一个 token 被服务端重绑定为别的 source，本地检测不到。

## 7. 验证状态与本机事实

- 已在本机核实：Node v26.11.1、darwin arm64、`node:sqlite` 的 `DatabaseSync` 同步 API 可用，
  `PRAGMA journal_mode=delete`、`PRAGMA synchronous=2`（FULL），
  `SQLITE_BUSY` 的错误形态是 `code=ERR_SQLITE_ERROR`、`errcode=5`、`errstr=database is locked`。
- 本地测试覆盖（80 个）：映射三分支与三态、跳过原因、会话命名空间、路径边界、绑定与 CAS 轮换、
  两进程 barrier 并发写、双 uploader 只删确认 ID、SIGSTOP 持锁仍 BUSY、提交前后 SIGKILL 恢复、
  数据库损坏保留、协议混合结果与严格逐项校验（禁止字段/白名单外错误码）、413/429/重定向、
  请求与响应体超时、在途凭证轮换、服务端已提交但响应丢失后的幂等重传、断网补报、
  换 stateDir 重开库、队列打开失败后的轮询重试、HTTP 暂停不自动恢复、退出等待入队并计数未采集、
  隐私三态（pending/dead/overflow 的 DB 字节与日志行）。
- **尚未验收**：真实 PI 进程加载、真实事件触发、真实服务端单事件与用量核对。
  这几项需要用户填好地址与凭证后在真实 PI 里完成；插件安装在 `~/.pi/agent/extensions` 之前，
  状态就是「代码已提交、未安装、未启用」。

跑测试：

```sh
make test-plugin-pi          # = cd plugins/pi && node --test test/*.test.js
```

`make check` 不包含插件测试（与 `test-plugin-dsh` 一致），需要单独跑上面的目标。

## 8. 撤回安装

```sh
mv ~/.pi/agent/extensions/tokenpivot-pi ~/.Trash/$(date +%F)/
```

只移动本次创建的软链；`~/.tokenpivot/pi/` 里的配置、队列和死信都保留，
需要彻底清理由用户自行处理。
