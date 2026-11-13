# 生成计时 v2 一次性转换手册（T-070）

本手册用于把本地环境里尚未转换的旧格式数据升级到只认 `tokenpivot-usage-event/v2` 的运行时：服务端 PostgreSQL 的存量事件行、DSH 插件队列（`queue.json`）、PI 插件队列（`queue.sqlite`）。

运行时不再提供旧版本兼容：落库一律写 v2，查重只比较已存的 v2 摘要，遇到版本不是 v2 的行返回 `NORMALIZATION_INCOMPATIBLE`。所以**先停写、转换、复核，再放开写入**；顺序反过来会得到一串被拒绝的请求，而不是自动兼容。

## 0. 顺序与前置

固定顺序，不要调换：

1. 停掉所有写入方：服务进程、DSH 插件、PI 插件。转换期间不得有并发写入。
2. 每个组件先跑只读 check。check 报错就停，不要带 `--apply` 重试：报错说明数据本身不满足转换条件，重试不会变好。
3. 只有 check 报告存在待转换项（PostgreSQL `pending_conversion > 0`，两个队列 `convertible > 0` / `state: "needs_conversion"`）时，才做备份再 `--apply`。
4. 没有待转换项（PostgreSQL `v1_rows: 0`，队列 `state: "converted"`）就在 check 后结束：不备份、不 apply、不创建任何文件。PostgreSQL 在这种情况下 `--apply` 也是 no-op。

PostgreSQL 的备份在停写之后、apply 之前自行做一次逻辑备份；备份方式与保留位置 TODO(待确认)：本手册的验证只在一次性测试 schema 内进行，未在真实部署库演练恢复。

三个组件的转换互相独立，可以按任意顺序做，但每个组件内部都要「check → （需要时）备份 → apply → 再 check」。

## 1. 服务端 PostgreSQL

先构建二进制：

```bash
go build -o bin/tokenpivot-convert-generation-v2 ./cmd/tokenpivot-convert-generation-v2
```

**必须用二进制判断退出码**：`go run` 把程序的非零退出统一报成它自己的退出码 1（实测程序退出 2 时 `go run` 退出 1，只在 stderr 多一行 `exit status 2`），用它看「提交结果未知」会误判成普通失败。

命令只读环境变量 `TOKENPIVOT_MIGRATION_DATABASE_URL`，用迁移角色连接；服务角色 `tokenpivot_app` 在 `usage_events` 上没有 UPDATE 权限，用它跑 `--apply` 会失败。

```bash
# 只读 check（默认行为）
TOKENPIVOT_MIGRATION_DATABASE_URL="postgres://…/tokenpivot" \
  bin/tokenpivot-convert-generation-v2

# 显式转换：单事务，只重写 normalization_version 与 payload_hash
TOKENPIVOT_MIGRATION_DATABASE_URL="postgres://…/tokenpivot" \
  bin/tokenpivot-convert-generation-v2 --apply
```

check 与 apply 都向 stderr 打一行 JSON。在一次性测试 schema 上实测（2026-11-13，2 条 v1 + 1 条 v2；每行实际还带一个 `time` 字段，这里略去）：

```json
{"level":"INFO","msg":"report","mode":"check","v1_rows":2,"v2_rows":1,"rows":3,"pending_conversion":2,"state":"needs_conversion"}
{"level":"INFO","msg":"report","mode":"apply","converted":2,"v1_rows":0,"v2_rows":3,"rows":3,"verified":true,"state":"converted"}
```

- `pending_conversion` 是当前仍为 v1 的行数；`state` 为 `needs_conversion` 或 `converted`。
- 没有 v1 行时 check 报 `pending_conversion: 0`、`state: "converted"`；此时 `--apply` 也是 no-op，报 `converted: 0`。
- 转换完成后重跑 check，期望 `v1_rows: 0`、`pending_conversion: 0`、`state: "converted"`。

退出码（四种情况都用构建后的二进制实测过）：

| 退出码 | 含义 |
|---|---|
| 0 | check / apply / no-op 成功；`--help` 与 `-h` 也返回 0（usage 打到 stderr） |
| 1 | 参数非法、缺少 `TOKENPIVOT_MIGRATION_DATABASE_URL`、预检失败、执行失败（事务已回滚，数据未变） |
| 2 | COMMIT 结果未知，见下方处置；只有直接运行二进制才看得到 2 |

apply 之前会做整体预检，任一条不满足即失败且不改任何行：所需表与列存在；版本分组只有 v1 与 v2；v1 行的两个计时列全为 NULL；`payload_hash` 非空且恰好 32 字节；六个 token 指标的 value/quality 组合合法（`known`/`estimated` 必须带 `[0, 2^53-1]` 的整数，`missing` 必须不带值）；v1 行数不超过 100000。

check 用只读事务，读的却是同一批行：它同样加载全部 v1 行、逐条校验 value/quality 并重算 `DigestV2`，所以 check 通过意味着 apply 的这批业务校验也会通过。未知 quality、缺值、负值、超过 2^53-1 或读取异常会让 check 与 apply 一起失败，零写入。

apply 是单事务，只写 `normalization_version` 与 `payload_hash`；提交前逐行核对：该行的完整快照（`to_jsonb(row)` 去掉这两列）前后一致、已有 v2 行及其摘要不变、`usage_minute_stats` 不变、v1 计数为 0、写入的新摘要等于 `DigestV2` 的结果。完整快照覆盖 `received_at`、workspace/source/event 身份等规范化输入里没有的列。任一步不符就回滚。已验证两种场景整体回滚：第一行更新成功后第二行失败；触发器同时篡改 `received_at`、`source_event_id` 与 `workspace_id`。

### 退出码 2：COMMIT 结果未知

在测试 schema 上用延迟唯一约束制造 COMMIT 冲突，实测输出与退出码：

```bash
# <t070_schema> 是本次测试用的临时 schema；真实库不需要 options 参数
TOKENPIVOT_MIGRATION_DATABASE_URL="postgres://…/tokenpivot?options=-csearch_path%3D<t070_schema>" \
  bin/tokenpivot-convert-generation-v2 --apply
# 进程退出码 2
# {"level":"ERROR","msg":"commit result unknown: keep the writers stopped and verify with a fresh read-only check","stage":"conversion","status":"commit_unknown","err":"commit: database operation failed"}
```

同一 schema 用 `go run ./cmd/tokenpivot-convert-generation-v2 --apply` 执行同样的失败：进程退出码 1，stderr 多一行 `exit status 2`。

处置固定为三步，不要自行加戏：

1. 继续停写，不把它当成功，也不启动服务。
2. 换一个新连接执行只读 check，确认事务到底有没有提交。同一个 schema 提交失败后实测报告 `{"level":"INFO","msg":"report","mode":"check","v1_rows":2,"v2_rows":0,"rows":2,"pending_conversion":2,"state":"needs_conversion"}`；若报告 `v1_rows: 0`、`state: "converted"`，说明 COMMIT 实际已生效。
3. 按实际状态人工决定是否再跑 apply。不要自动重试，也不要把备份当成「未提交」的依据去恢复——COMMIT 可能已经生效。

提交结果未知的两种现场都验证过：命令实际已提交时，新连接的只读 check 报 `converted`；未提交时报 `needs_conversion`。两种情况下命令都不打印成功报告，也不自动重试或恢复。

## 2. DSH 插件队列（`queue.json`）

```bash
node plugins/dsh/scripts/convert-generation-v2.js           # 只读 check
node plugins/dsh/scripts/convert-generation-v2.js --apply   # 补缺失键并原子写回
```

状态目录默认取 `$TOKENPIVOT_DSH_STATE_DIR`，否则 `~/.tokenpivot/dsh`；用 `--dir <目录>` 指定其他位置。`--help` 打印用法。

脚本直接读 `queue.json` 文本，不经过插件的队列加载逻辑（加载会顺手修 aside、去重、裁剪，这些都不该在转换里发生）。它只给缺失的 `generation_duration_ms`、`ttft_ms` 补 `null`：

- 已有 `null` 或非负安全整数：原样保留。
- 负数、小数、超过 2^53 的整数、字符串、对象：整体中止，不写任何文件。
- 事件本身不合法（未知事件字段、缺 `provider`/`model`/`occurred_at`/`usage`、文本长度或首尾空白越界、日期或偏移非法、`usage` 结构或 value/quality 配对非法、项目字段同时为有效值、可选字段类型非法）也在补键之前整体中止。项目字段中的显式 `null` 合法，两个项目字段同时为有效值才冲突。
- `dead-letter.jsonl`、其他文件和队列包装对象的未知顶层键不动；事件内部的未知键则整体报错。
- check 和 apply 做同一套校验：check 失败就是 apply 会失败，两者都不写文件。
- apply 用 `JSON.stringify` 重写整个文件：字段值与取值、item 顺序、队列包装对象的未知顶层字段都保留，但文件原来的空白与缩进不保留。没有待补键时脚本完全不重写文件，字节原样不变（缩进也原样）。

实测输出（临时目录 fixture，2026-11-13）：

```json
{"mode":"check","items":1,"convertible":1,"converted":0,"state":"needs_conversion"}
{"mode":"apply","items":1,"convertible":1,"converted":1,"backup":"queue.json.bak-2026-11-13T03-42-56-778Z","state":"converted"}
```

- apply 前把原文件复制成 `queue.json.bak-<UTC 时间戳>`（内容与原文逐字节相同），然后同目录临时文件（0600）+ fsync + rename + 目录 fsync 写回。
- 再跑一次 apply：`converted: 0`，不再生成备份，文件字节不变。
- `queue.json` 不存在：`{"mode":"check","items":0,"convertible":0,"converted":0,"state":"no_queue"}`，退出码 0，不创建目录。
- 文件损坏（非 JSON、`format` 不是 1、`items` 不是数组、事件缺 `source_event_id`、ID 重复）、事件结构非法或计时值非法：check 与 apply 都退出码 1，stderr 一行 `{"state":"failed","error":"…"}`，零写入、无备份。

## 3. PI 插件队列（`queue.sqlite`）

```bash
node plugins/pi/scripts/convert-generation-v2.js           # 只读 check
node plugins/pi/scripts/convert-generation-v2.js --apply   # 单事务写回
```

状态目录默认取 `$TOKENPIVOT_PI_STATE_DIR`，否则 `~/.pi/agent/tokenpivot`；用 `--dir <目录>` 指定其他位置。

打开数据库之前先检查恢复单元，任何异常都保留现场、退出码 1、不对数据库做任何写入：

- 同目录存在 `queue.sqlite-journal` 就停：`state: "journal_present"`，交人工。**零字节的 journal 也算**——它同样说明上一次写入没有正常结束，脚本不触发 SQLite 恢复，也不删 journal。
- 同目录存在 `queue.sqlite-wal` 或 `queue.sqlite-shm`：`state: "unexpected_wal"`，交人工。
- 用独立只读连接预检：`journal_mode` 必须是 `delete`、`user_version` 必须是 1、`meta` 与 `events` 表都存在、`events` 具备 `id/payload/status/created_at/updated_at` 列。不符即失败（例如 `journal_mode` 是 WAL 时报人工处理，`user_version` 是 2 时报 `legacy_state`）。

只处理 `status = 'pending'` 的行，按 `created_at, rowid` 顺序；每条 `payload` 必须能解析成对象，且 `payload.source_event_id` 与行 `id` 一致。事件字段集合、文本、日期与偏移、usage 配对、项目互斥和可选字段类型均按第 2 节同样的规则校验；任一条结构非法，check 与 apply 都整体中止且不写库。只给缺失的两个计时键补 `null`。

实测输出（fixture 库，2026-11-13）：

```json
{"mode":"check","pending":1,"convertible":1,"converted":0,"state":"needs_conversion"}
{"mode":"apply","pending":1,"convertible":1,"converted":1,"backup":"queue.sqlite.bak-2026-11-13T03-43-03-879Z","state":"converted"}
```

- apply 先复制整个数据库文件为 `queue.sqlite.bak-<UTC 时间戳>`，再用 `BEGIN IMMEDIATE` 在一个事务里 `UPDATE events SET payload = ? WHERE id = ? AND status = 'pending'`；某行 `changes` 不是 1 时回滚并报 `concurrent_change`。
- 不动 `updated_at`、`meta`、`user_version`、`journal_mode` 等 PRAGMA 设置；`dead`、`overflow` 行的 payload 逐字节不变。
- 再跑一次 apply：`converted: 0`，只有一份备份，库文件字节不变。
- 库文件不存在：`state: "no_queue"`，退出码 0，不创建目录。

## 4. 恢复与收尾

- DSH：停插件，把 `queue.json.bak-<时间戳>` 覆盖回 `queue.json`（备份就是完整原文，包括排版）。
- PI：停插件，把 `queue.sqlite.bak-<时间戳>` 覆盖回 `queue.sqlite`，并确认同目录没有残留的 `-journal`、`-wal`、`-shm`。
- PostgreSQL：只在确认提交状态并决定恢复后，使用停写后、apply 前制作的备份；提交结果未知时先按第 1 节用新连接只读检查，不自动恢复。本条未在真实部署库演练，属 TODO(待确认)。
- 三个组件都报告转换完成（服务端 `v1_rows: 0`，两个队列 `state: "converted"`、`convertible: 0`）之后，再启动服务与插件。

## 5. 已核实范围

验证日期 2026-11-13，Go 1.26.4、Node 26.13.0、PostgreSQL 18.6（本地测试容器）。本手册中的命令、输出与退出码均在一次性测试 schema 与临时状态目录里实际执行过，未对真实业务数据运行：三个组件的 apply 与 no-op、未知版本、参数非法、COMMIT 结果未知（含已提交/未提交两种现场的只读复核）、DSH/PI 的事件结构损坏、空 journal、非法计时都在测试环境复现过。PostgreSQL 备份与恢复的具体命令未验证，标为待确认。
