# T-070：移除请求计时 v1 兼容并显式转换 generation v2

- 状态：可实施；本文件只记录已批准方案，不代表代码、转换或文档已经落地。
- 基线：`main@5dabc40ce498e026d6ab9e562e6f139c7b880ead`。
- 执行切分：**单块，不拆**。规范化摘要、接口响应和队列格式由同一个 dev 串行修改，不并行写共享文件。
- 用户决策：本地开发，不保留历史协议兼容；HTTP API 继续沿用 `/api/v1`，持久化摘要只使用 `tokenpivot-usage-event/v2`。

## 1. 目标

1. 请求事件固定使用包含两个请求计时字段的九元素规范化文本和 SHA-256；`generation_duration_ms`、`ttft_ms` 均为必填、可空、非负安全整数，`null` 表示未知，`0` 表示真实零值。
2. 删除运行时按字段出现选择 v1/v2、跨版本投影比较、旧 tokens/min 聚合、前端缺字段回退和 DSH 删除 `project_key` 后重发等旧兼容路径。
3. 提供 PostgreSQL、DSH JSON 队列、PI SQLite 队列三种独立一次性转换工具；默认只读检查，只有显式 `--apply` 才修改旧数据。
4. 同步现行接口文档、数据模型、转换 runbook 和两个插件 README，使文档只描述现行 v2 行为，且所有 HTTP 路径仍为 `/api/v1`。

## 2. 范围与非目标

### 2.1 范围内

- Go 采集解码、规范化摘要、查重和 request details 的 usage-rate 响应。
- 请求页 API 解析及受旧响应字段影响的测试/fixture。
- DSH/PI 新事件格式、DSH 上传旧后端回退、两类离线队列转换脚本及发布文件清单。
- 一个 Go PostgreSQL 转换命令及单元/真实 PostgreSQL 集成测试。
- 本计划允许的六份文档：本计划、`docs/openapi/openapi.yaml`、`docs/data-model.md`、新 `docs/runbooks/generation-v2-conversion.md`、`plugins/dsh/README.md`、`plugins/pi/README.md`。

### 2.2 非目标

- 不增加 `/api/v2`、旧路径别名、双路由或路由转换工具；除确有必要外不改 `internal/server` 路由。
- 不修改既有 `db/migrations/00009_generation_metrics.sql`，不新增 Goose migration；migration 00008 是飞书事项，与本任务无关。
- 不修改历史 ADR、T-069 历史计划或根 `AGENTS.md`，不在 `AGENTS.md` 记录任务日志。
- 不改 PI 凭证绑定、完整服务 URL 比较、token 指纹域、SQLite `user_version=1`；这些不是旧请求协议兼容。
- 不删除独立真实 usage-speed（output/duration）统计、界面、空间权限、筛选、413 处理、暂停、死信、重试或队列可靠性。
- 不从 `duration_ms` 推导 generation duration 或 TTFT，不给未知计时造数，不回填分钟统计。
- 不执行生产转换、部署、推送或合入；不吸收 T-047、T-062 或其他未批准需求。

## 3. 已核实现状与依据

以下为 2026-11-13 在上述基线直接核实的仓库事实；未使用外部资料。

| 依据 | 已核实现状 | 本计划采用的处理 |
|---|---|---|
| `AGENTS.md:3-14` | 本地开发、沿用 `/api/v1`、旧数据只显式转换；与用户本轮决定一致 | 作为最高项目约束；OpenAPI 中冲突的历史主版本条款同步删除 |
| `docs/architecture.md:37-59` | 身份为 `(workspace_id,source_id,source_event_id)`；逐项事务；首次插入才增加分钟统计；未归属事件合法 | 保留身份、归属、事务和分钟口径 |
| `docs/decisions/0002-ingestion-idempotency-and-batch.md:7-21` | 同身份同内容 duplicate、异内容 conflict；首次行不可覆盖；accepted 只在提交后返回 | 查重简化为同 v2 比较版本/hash，不改事务语义 |
| `docs/decisions/0003-minute-aggregation-and-retention.md:7-23` | `received_at` 是首次接收时间；分钟汇总不能假设可由明细重算 | 转换不改 `received_at` 或分钟表 |
| `internal/ingestion/handler.go:413-583,692-715` | 任一计时键出现就选 v2；计时值已有 null/非负安全整数及词法校验 | 改为两个键分别检查必填，复用现有值校验 |
| `internal/ingestion/service.go:639-797` | 九元素文本由七元素加 generation/TTFT 构成，usage 六键顺序固定，null 与 0 分别编码 | 原样提取，不重新设计算法 |
| `internal/ingestion/postgres.go:54-67,312-364` | 为跨版本重建读取全部旧比较列 | 运行时只读取 `normalization_version,payload_hash` |
| `internal/requestdetails/service.go:92-105,206-235,393-417` | 旧 tokens/min 与新速度/TTFT 对象并存 | 删旧聚合，保留两个新对象和顶层原始事件计数 |
| `web/src/features/requests/api.ts:73-97,147-220,273-293` | 旧后端缺键回退和 legacy total 解析仍存在 | 必有字段缺失改 `BAD_RESPONSE`，显式 null 仍合法 |
| `plugins/dsh/lib/queue.js:40-75` | `FileQueue.load` 会建目录、修复、去重和裁剪 | 转换脚本直接读 JSON，禁止调用它 |
| `plugins/pi/lib/queue.js:5-13,55-107` | `SqliteQueue.open` 会写 PRAGMA、表和 schema；现行 journal 为 DELETE | 转换脚本使用独立 `DatabaseSync`，禁止调用该 open |
| `db/migrations/00009_generation_metrics.sql:9-17` | 两个计时列已存在且可空 | 命令只预检列，不改旧 DDL |
| `internal/config/config.go:285-289` | `LoadMigration` 只读 `TOKENPIVOT_MIGRATION_DATABASE_URL`，不回退服务凭证 | Go 转换命令复用此边界 |
| `Makefile:63-97` | 已有 `check`、`test-integration`、`test-plugin-dsh`、`test-plugin-pi` | 作为发布前检查入口，不新增平行测试框架 |

## 4. 文件与符号范围

### 4.1 现有生产文件

- `internal/ingestion/handler.go`：`decodeEvent`、`decodeTiming`；删除 `TimingFieldsPresent` 的解码用途，两个计时键分别做存在性检查。
- `internal/ingestion/service.go`：`event`、`storedEvent`、`ExistingEvent` 调用、版本选择、`compareContent/commonVersion/isKnownVersion`、project_key 解析后的重 hash、逐项事务。
- `internal/ingestion/postgres.go`：`existingEventQuery`、`ExistingEvent` 扫描和 `InsertEvent`；插入仍写完整现行行，查重读取缩到版本/hash。
- `internal/requestdetails/service.go`：`RateRow`、`RateTotal`、`RateResponse`、`Rate` 组装。
- `internal/requestdetails/postgres.go`：`rateQuery` 和扫描；独立 `Speed*`/`speedQuery` 不改。
- `web/src/features/requests/api.ts`：`RateData`、`row`、`rateSample`、`parseRate` 及仅供旧回退使用的 parser。
- `plugins/dsh/lib/mapper.js`：`mapGenerationTiming`、`mapSessionEvent`。
- `plugins/dsh/lib/uploader.js`：INVALID_ARGUMENT 后删除 `project_key` 重发的分支。
- `plugins/pi/lib/mapper.js`：新事件固定输出两个 null；现有 `duration_ms` 来源不变。
- `plugins/dsh/package.json`、`plugins/pi/package.json`：发布 `files` 只增加 `scripts/*.js`，不增加依赖。

### 4.2 新生产文件

- `internal/ingestion/normalization.go`：唯一的九元素规范化与 SHA-256 实现。
- `cmd/tokenpivot-convert-generation-v2/main.go`：PostgreSQL 默认 check、显式 apply 的一次性命令。
- `plugins/dsh/scripts/convert-generation-v2.js`：直接转换 `queue.json` 的 pending items。
- `plugins/pi/scripts/convert-generation-v2.js`：只转换 SQLite `events.status='pending'` 的 payload。

### 4.3 测试

- 更新 `internal/ingestion/handler_test.go`、`service_test.go`、`postgres_test.go`、`postgres_integration_test.go` 中旧版本选择、七元素向量、跨版本比较和全列重建断言。
- 更新 `internal/requestdetails/service_test.go`、`postgres_test.go`、`postgres_integration_test.go` 及必要的 `internal/server/request_details_test.go` fixture；独立 Speed 断言保留。
- 更新 `web/src/features/requests/api.test.ts`、`model.test.ts`、`request-panel.test.tsx` 及受影响的请求页 fixture；usage-speed 测试不改口径。
- 更新 DSH/PI 现行 mapper、collector/uploader/queue 测试；新增 `plugins/dsh/test/convert-generation-v2.test.js` 和 `plugins/pi/test/convert-generation-v2.test.js`。
- 新增 `cmd/tokenpivot-convert-generation-v2/main_test.go`、`main_integration_test.go`。cmd 包自备最小集成测试 helper，不能 import 其他包的 `_test.go`。

### 4.4 文档

仅修改第 2.1 节列出的六份文档。父代理统一处理 TODO 和交付索引；dev 只改明确分配给自己的文档。

## 5. 有序实施步骤

### 5.0 实施前核对

1. 确认实现 worktree 来自上述计划提交后的 main；核对 HEAD 和工作区是否出现新改动。
2. T-047（PI）和 T-062（前端）虽为 running，不假定其已有代码改动，也不把其需求并入本任务。若共享文件已有新变化，先停下交父代理确认基线。
3. 保留用户未提交的根 `AGENTS.md` 和其他工作区材料，不暂存、不格式化、不顺手修改。

### 5.1 提取唯一 v2 规范化实现

1. 新建 `internal/ingestion/normalization.go`，原样移动现有序列化助手、固定 usage 六键顺序、UTC 微秒时间格式、JSON 最小转义和 `appendOptionalInteger`。
2. 九元素顺序固定为：
   1. `occurred_at`，UTC、六位小数、`Z`；
   2. 解析后的 `project_id` 小写 UUID或 null；
   3. `provider`；
   4. `model`；
   5. usage 对象，依次是 input/output/total/cache_read/cache_write/reasoning，各自保留 value/quality；
   6. `status` 或 null；
   7. `duration_ms` 或 null；
   8. `generation_duration_ms` 或 null；
   9. `ttft_ms` 或 null。
   对数组文本做 SHA-256，不增加 insignificant whitespace、浮点转换或本地化。
3. 提供一个最小导出输入类型 `NormalizationInput` 和 `DigestV2(input NormalizationInput) [sha256.Size]byte`。输入只包含上述九元素所需字段；六项 usage 可直接使用现有 `aggregation.Metric`，项目 ID 使用现有 `credentials.ID`，不增加注册器、算法选择器或新依赖。
4. handler 首次 hash、project_key 解析后的重 hash 和 PostgreSQL 转换命令都调用这一实现。运行时不再保留七元素 renderer；旧七元素只作为转换输入的历史事实，不成为请求路径选项。

### 5.2 固定请求格式与运行时查重

1. `decodeEvent` 分别检查 `generation_duration_ms` 和 `ttft_ms` 的属性存在性；任何一键缺失都拒绝，显式 null 走现有 `decodeTiming` 并接受。值仍须是非负、不超过 `Number.MAX_SAFE_INTEGER` 的整数，0 不转为 null。
2. 删除 `TimingFieldsPresent`、按字段出现选版、v1 运行时常量以及 `version()` 的请求形状判断。持久化版本只写 `tokenpivot-usage-event/v2`。
3. 删除 `commonVersion/textFor` 共同版本投影和为旧列重建摘要的路径。可用一个未导出的二字段 `existingEvent` 类型收窄 `ExistingEvent` 返回，避免继续伪装成完整 `storedEvent`；不引入通用存储抽象。
4. 查重只执行：存储版本未知（包括尚未转换的 v1）→ `NORMALIZATION_INCOMPATIBLE`；存储版本为 v2 且 hash 相同 → duplicate；存储版本为 v2 且 hash 不同 → conflict。不能把旧行当新行覆盖或回填。
5. 保留 project_key 解析后以最终 project ID 重 hash、project_key 与等价 project ID 的同身份判定、逐项 Read Committed 事务、时间窗复核、首次行不覆盖、首次插入才 `ApplyMinute`、commit 失败不返回 accepted。

### 5.3 移除 usage-rate 旧响应和前端回退

1. `RateRow` 删除旧 `Sum/Events/Known/Estimated/Missing` 等仅为 tokens/min 服务的字段，保留 `AsOf`、原始 `EventCount`、`LastReceived` 和速度/TTFT 样本字段。
2. 删除 `RateTotal` 以及 `RateResponse.WindowSeconds/Unit/TotalTokens`；响应固定含 `start/end/as_of/window_last_received_at`、顶层 `event_count`、`generation_speed`、`ttft`。`event_count=count(e.id)`，不是已知 token 数或速度样本数。
3. `rateQuery` 删除旧 sum/known/estimated/missing 聚合；保留同一窗口的原始事件计数、最近接收时间和两个新指标。两个指标对象必须存在，无样本时分别为 `value:null,sample_count:0`。
4. 前端 `RateData` 删除 legacy value/known/estimated/missing 字段；`parseRate` 从顶层读取 `event_count`，两个 metric 对象及其 `unit/value/sample_count` 均为必有字段。
5. 请求事件解析中的 `generation_speed_tps`、`ttft_ms` 也由“缺键按旧后端处理”改为“缺键 `BAD_RESPONSE`”；显式 null、0、最大安全整数仍按现行规则接受。样本数继续独立校验，不用一个指标的样本数填另一个。
6. 保留请求页 event_count、最近收到时间、筛选、空间权限和独立 usage-speed 数据流。`web/src/features/requests/api.ts` 的 `base()` 继续 `/api/v1`，server 路由不变。

### 5.4 更新 DSH/PI 现行写入方

1. DSH 对合法 usage 事件始终输出两个计时键：未知为 null，已知为数值。完全无有效 usage/timing 仍跳过；只有有效 timing 的事件仍可上报。cache/input/total/reasoning 的部分 usage 恢复规则不变。
2. PI 当前没有 generation/TTFT 来源，因此新事件固定两个 null，不从 `duration_ms` 推导。
3. 删除 DSH uploader 在 INVALID_ARGUMENT 且存在 project_key 时“删除 project_key 再以同一 body 重发”的兼容。原请求失败按现行结果处理，不生成第二种事件内容。
4. 保留 413 分批/单条死信、暂停、退避、死信、原 seq 与 session 对象的微任务截止、stop 接受批次持久化。所有现行写入方测试同步为两个必有键。

### 5.5 Go PostgreSQL 一次性转换命令

入口：`cmd/tokenpivot-convert-generation-v2/main.go`。默认 check；只有显式 `--apply` 进入写事务。复用 `config.LoadMigration`、数据库 URL 校验、pgx stdlib 单连接、固定超时和 `database.SafeError` 模式；只读取 `TOKENPIVOT_MIGRATION_DATABASE_URL`，不回退服务角色，不打印 DSN、事件或凭证，不调用 Goose。

#### check 与预检

1. check 使用只读事务，输出 v1/v2/待转换等计数及安全状态，不输出事件内容。
2. 两种模式都先整体预检：00009 的 `generation_duration_ms`、`ttft_ms` 列存在；存储版本只出现 v1/v2；所有 v1 的两个计时列均为 NULL；`payload_hash` 非空且为 32 字节 SHA-256 摘要。缺列、未知版本、v1 带非 NULL timing、摘要长度/读取/重算异常均整体失败，不做部分转换。
3. 从 v1 行业务列构造 `NormalizationInput`，两个新计时值固定为 nil，通过 `DigestV2` 得到九元素末尾两个 null 的新摘要。不能从旧摘要直接拼接，不能改 usage 的 known/estimated/missing。

#### apply 事务与核对

1. 没有 v1 时，check/apply 都不执行 UPDATE，不需要备份，不改 Goose；重跑报告 converted 0。
2. 有待转换项时，apply 在一个事务内逐行按事件身份只更新 `normalization_version,payload_hash` 为 v2/新摘要。不得改其他业务列、`received_at`、workspace/source/event/project 身份、已有 v2 行或分钟表。
3. 在提交前做前后一致性核对：每个转换行除 version/hash 外的列与转换前相同；已有 v2 行完整不变；分钟表不变；转换后的 v1 计数为 0、新 v2 摘要与共享 `DigestV2` 结果一致。核对失败则回滚。核对结果只汇总计数，不打印事件。
4. 提交前任何错误都使事务回滚并返回失败。`COMMIT` 自身报错时结果可能已经成功，必须返回明确的“提交结果未知，保持停写并复核”状态；不得报告 converted 成功，不得自动重试 apply，也不得按“未提交”恢复备份。
5. runbook 要指导使用新连接执行只读 check 来核实提交结果；命令不替操作者作恢复决定。

备份和停写由 runbook 负责，不在 Go 命令里实现另一套备份框架。无 v1 时 runbook 在 check 后结束，因此不创建备份。

### 5.6 DSH JSON 队列转换脚本

入口：`plugins/dsh/scripts/convert-generation-v2.js`，默认 check，显式 `--apply` 才写入。

1. 直接读取目标 `queue.json`，禁止 `FileQueue.load`。严格校验顶层 `format === 1`、`items` 为数组、`source_event_id` 唯一；保留 item 顺序和所有未转换字段。queue.json 中 items 是 pending；dead-letter 文件不打开、不修改、不重投。
2. 对每个 pending item，只补缺失的 `generation_duration_ms`、`ttft_ms`，补值均为 null；已有合法 null 或非负安全整数原样保留。任一为负数、小数、超安全整数、错误类型或事件结构损坏时整体中止，不做部分写入。
3. 文件不存在或没有待补键时报告 0，不创建目录、备份、临时文件或新 queue 文件；二次运行无改动。
4. apply 前要求操作者停插件。只有确有转换项时才备份原 queue.json，再在同目录写临时文件、fsync、原子 rename 替换；失败保留原文件和备份。脚本不主动控制插件进程。
5. check/no-op/损坏场景比较源文件内容，确认零变化；apply 后除两个补键外，ID、顺序和其他字段完全一致。

### 5.7 PI SQLite 队列转换脚本

入口：`plugins/pi/scripts/convert-generation-v2.js`，默认 check，显式 `--apply` 才写入。

1. 禁止 `SqliteQueue.open`。预检和 check 使用独立只读 `DatabaseSync`；只处理 `events.status='pending'` 的 payload，dead/overflow 不更新、不重投。
2. 当前恢复单元是 `queue.sqlite` 与 `queue.sqlite-journal`，journal 模式固定 DELETE。发现 rollback journal 或其他需要恢复的现场时，在任何可写打开前停止并保留源文件组；不自行触发恢复。只读确认 journal 模式，非 DELETE 的异常情况交人工处理，不扩展通用 WAL 转换方案。
3. 只读预检现行表/字段和 `user_version=1`，按 pending 的现行 `created_at,rowid` 顺序读取精确 payload 文本。解析后只给事件中缺失的两个 timing 键补 null；已有合法值不动，非法值、损坏 JSON 或异常结构整体中止。
4. 无待转换 pending 项时，不建目录、不备份、不写 DB、不改版本；二次运行 converted 0。check/no-op/损坏/journal 场景必须证明 DB 与相关源文件组零变化。
5. apply 必须先停插件，再在任何可写打开或恢复前备份 DB 源文件组。随后使用独立 `DatabaseSync`，仅在一个 `BEGIN IMMEDIATE` 事务内按事件 ID 更新仍为 pending 的 payload；失败回滚。
6. 不改 dead/overflow/meta、URL 绑定、token 指纹、revision、created_at/updated_at、rowid、`user_version` 或 journal 配置。脚本不调用现行队列 open 来“顺便修复”。

### 5.8 同步六份文档

1. `docs/openapi/openapi.yaml`
   - 删除破坏性变更必须升级 URI 主版本的历史条款，改为本地开发沿用 `/api/v1` 并同步所有调用方；不得新增 v2 路径。
   - 采集事件 required 为原五项加 `generation_duration_ms,ttft_ms` 共七项；两个字段 nullable、非负安全整数，示例无观测值时显式写两个 null。
   - 摘要说明只保留九元素 v2、同 v2 hash duplicate/conflict、未知存储版本拒绝；删除七元素现行向量、跨版本投影和旧客户端可省略条款。
   - usage-rate 删除 tokens/min、`window_seconds/unit/total_tokens` 和旧后端省略回退；`event_count` 提升为顶层必填，`generation_speed/ttft` 为必有对象，保留 `start/end/as_of/window_last_received_at`。
   - 独立 `UsageSpeedResponse` 不改。
2. `docs/data-model.md`：只描述 v2 持久化和显式一次性转换；说明服务角色仍无 UPDATE，转换使用独立 migration DSN；保留身份、首次接收和分钟口径。
3. 新 `docs/runbooks/generation-v2-conversion.md`：写清同一停写窗口升级服务端和插件、check、仅在需要时备份、apply、提交未知复核、重跑和恢复。顺序为停写→只读 check；有转换项才备份并 apply；commit unknown 保持停写并用新连接 check；确认未提交前不得恢复备份。无 v1/无队列待转换项不创建备份。
4. `plugins/dsh/README.md`、`plugins/pi/README.md`：路径仍为 `/api/v1`，新事件必有两个 timing 键及未知 null 口径，记录对应队列工具的 check/apply、停写、备份、异常 journal 和幂等重跑说明。
5. 文档中的实际帮助、退出码和脚本调用命令必须在实现后运行确认再写入，不编造示例输出；不记录凭证或真实事件。

## 6. 可验证验收标准

### 6.1 Go 采集与查重

- 两个计时键分别缺失都被拒绝；两个 null 被接受；0、最大安全整数被接受；负数、小数、超安全整数和错误类型被拒绝。
- 固定向量证明九元素文本及 SHA-256 与提取前算法一致，两个 null 位于末尾，null 与 0 的 hash 不同。
- 同 v2 同 hash 为 duplicate；仅计时不同导致异 hash 为 conflict；未知版本和未转换 v1 存储行返回 `NORMALIZATION_INCOMPATIBLE`。
- project_key 与解析后的等价 project ID 判定一致；首次行不被重试覆盖，无旧行回填，分钟增量仍只发生在首次插入。

### 6.2 PostgreSQL 转换（真实 PG）

使用混合 v1/v2、known/estimated/missing usage、已归属/未归属、null/真实零值测试数据：

- check 无 UPDATE；apply 只改变 v1 的 version/hash，已有 v2、全部其他业务列、`received_at` 和分钟表逐值不变。
- apply 后重跑 converted 0；无 v1 时 check/apply 都无 UPDATE、无 Goose 变化，runbook 不产生备份步骤。
- 缺 00009 列、未知版本、v1 非 NULL timing、异常摘要均整体失败；事务中故障回滚，无部分 v2。
- 模拟 commit 返回错误时输出“结果未知/需复核”的安全结果，不宣称成功、不自动重试或恢复；随后只读 check 能分别核实已提交和未提交两种现场。
- 输出只含安全计数/状态；日志和错误不含 DSN、事件、token 或驱动敏感细节。

### 6.3 DSH/PI

- DSH 原 seq、stop 接受批次持久化、project_key 上传可靠性、413/暂停/死信/重试不回归；删除 project_key 后重发的测试改为原 body 只发送一次。
- DSH JSON 转换保持 ID/顺序/所有其他字段；缺文件、空待转换项、check、损坏输入均无副作用；apply 有备份且原子替换。
- PI 新事件两个 null 且不从 duration 推导；转换只影响 pending payload，dead/overflow/meta/URL/token 绑定/revision/time/rowid/user_version 不变。
- PI DELETE rollback-journal 现场在任何写打开前停止；check、空 pending、no-op、损坏和 journal 异常下 DB 源文件组内容不变；apply 只用一个 BEGIN IMMEDIATE。
- 两插件二次转换均为 0；`package.json` 发布包含 scripts，未新增依赖。

### 6.4 request details 与前端

- usage-rate 不含旧 tokens/min 字段；顶层 event_count 是原始事件数，最近收到时间保留；两个新 metric 对象必有，null/count 0 正常，样本数独立。
- 前端缺 `generation_speed_tps`、`ttft_ms`、整个 `generation_speed` 或 `ttft` 均 `BAD_RESPONSE`；显式 null、0 和最大安全整数显示正确。
- 筛选、空间权限、event_count、最近收到时间和独立 usage-speed（output/duration）统计/界面不回归。
- 所有 HTTP 调用和 OpenAPI 路径仍为 `/api/v1`，没有新增别名或 `/api/v2`。

### 6.5 发布前检查

在 Go 1.26.4、fnm Node 26.13.0 下执行；当前默认 Node 24 不用于 web 测试：

```sh
fnm exec --using 26.13.0 -- make check
fnm exec --using 26.13.0 -- make test-plugin-dsh
fnm exec --using 26.13.0 -- make test-plugin-pi
```

真实 PostgreSQL 集成测试只使用 loopback、名称以 `_test` 结尾的数据库和两个非特权角色。可在关闭 xtrace 的 shell 中静默加载既有环境后执行，绝不读取、打印或提交其中的值：

```sh
set +x
set -a
. /root/.tokenpivot-test/integration.env
set +a
fnm exec --using 26.13.0 -- make test-integration
```

另需运行三个新工具的 `--help`、默认 check、apply/no-op/失败场景，核实文档中的实际命令、退出码和输出；本计划不预填未经运行确认的帮助文本或退出码数字。

## 7. 执行分工与界限

- **单块 dev**：按 5.0→5.8 串行实施全部代码、测试和获分配文档；共享 `normalization.go`、采集存储、request details、前端 API、插件 package/queue 文件不并行拆写。
- **父代理**：审查并只提交本计划，随后创建隔离 worktree、分派 dev、统一 TODO/交付索引、整合未提交的实现并审查验证；本轮代码不提交、不合入原分支。本 architect 不提交、不建 worktree、不改 TODO。
- **明确边界**：dev 不改根 `AGENTS.md`、历史 ADR、T-069 计划、既有 migration 或无关模块，不执行生产转换/部署，不吸收 running 状态其他 TODO 的需求。
- 若实施中发现必须改变 API 路径、摘要算法、恢复单元、事务边界或文件授权范围，停止该部分并交父代理，不在 dev 内自行扩需求。

## 8. 授权记录与风险

### 8.1 授权记录

- 用户已明确选择继续实施，授权同步上述六份文档；父代理只提交本计划文件，代码不在该提交中，当前不合入。
- 用户明确覆写旧 OpenAPI“破坏性升级 URI 主版本”条款：本轮保持 `/api/v1`，因此文档必须同步而非保留矛盾。
- 根 `AGENTS.md` 是用户另行要求新增的未提交文件，本任务不修改、不代提交。
- 既有只读 review 提出的 rollback-journal 与 commit-unknown 两项已纳入 5.5–5.7；此前因引入 `/api/v2` 产生的 PI 绑定问题随“保留 `/api/v1`”决定消除，不新增迁移。
- 不执行生产数据转换或部署；runbook 只是后续人工操作步骤。

### 8.2 主要风险与控制

| 风险 | 控制 |
|---|---|
| 运行时与转换命令的 hash 漂移 | 共用 `DigestV2`、固定九元素向量和真实 PG 前后核对 |
| 把合法未知值误造为 0 或从 duration 推导 | 全部新写入方/转换工具只补 null，专项测试 null 与真 0 |
| 离线工具通过现行 queue open 触发修复或写 schema | DSH 直接读 JSON；PI 独立只读 `DatabaseSync`；journal 异常先停 |
| COMMIT 错误被误判为未提交而恢复备份 | 明确 commit-unknown 状态、保持停写、新连接只读 check，不自动恢复 |
| 误删仍有效的可靠性和 usage-speed 行为 | 保留逐项事务、首次行/分钟语义、队列状态机及独立 Speed 测试 |

本计划没有待用户补充的设计决策；只有第 6.5 节所列实际 CLI 帮助、退出码和脚本命令需在实施运行后确认并写入文档。
