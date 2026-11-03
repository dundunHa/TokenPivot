# T-066 采集单条事件事务数据库往返无损合并优化实施计划

> 关联待办：`T-066`
> 依据文档：[OpenAPI 接口文档](../openapi/openapi.yaml) §5.3 / §8、[ADR-0002](../decisions/0002-ingestion-idempotency-and-batch.md)、[ADR-0003](../decisions/0003-minute-aggregation-and-retention.md)、[数据模型](../data-model.md) §4–§5

---

## 1. 目标与边界

### 1.1 目标
在 100% 保持现有对外 API 行为、[OpenAPI §5.3 采样点要求](../openapi/openapi.yaml)（含窗口判断严格先于项目校验与身份查重）以及 [ADR-0002 逐项独立事务语义](../decisions/0002-ingestion-idempotency-and-batch.md) 不变的前提下，利用 `pgx/v5` 的底层扩展查询单次同步批处理（`pgconn.Batch` + `ExecBatch` 配合 `pgx.ExtendedQueryBuilder`）将单条事件 `READ COMMITTED` 事务内的写操作/冲突读取与其紧随其后的锁后时间采样（`SELECT clock_timestamp()`）合并至同一次网络往返（RTT）中，同时对单发查询显式启用 `pgx.QueryExecModeExec` 消除冷连接额外预处理往返，使无论冷连接还是热连接上：
- **不带 `project_id` 的有效窗口事件**（新增 / 重复 / 冲突）：从 **7 次 RTT 严格降至 5 次 RTT**；
- **带 `project_id` 的有效窗口事件**（新增 / 重复 / 冲突）：从 **8 次 RTT 严格降至 6 次 RTT**；
- **过期（`expired`）或未来（`FUTURE_EVENT`）事件**：在首次时间采样后立即回滚返回（**3 次 RTT**），绝不触碰 `projects` 或 `usage_events` 表。

### 1.2 非目标与严禁触碰的边界
- **不改数据库表结构与迁移**：不新增或修改 `db/migrations/` 下的任何文件。
- **不改 OpenAPI 接口文档与错误码映射**：`docs/openapi/openapi.yaml` 保持不变。
- **不改 `internal/ingestion/service.go` 与 `internal/aggregation` 公共接口**：业务编排层 `service.go` 及 `aggregation.ApplyRequest(ctx, tx, in)` 签名、校验逻辑与 `minuteUpsert` SQL 文本保持 100% 不变。
- **不将首次 `Now` 与 `ProjectExists` 合并至同一批次**：
  - 经 `review` 与 `oracle` 裁决，单连接 `ExecBatch` 仅在批次末尾发送单个 `Sync`，若将 `Now` 与 `ProjectExists` 同批发送，当 `projects` 表被 `ACCESS EXCLUSIVE` 排他锁阻塞时，服务端不会提前刷出第 1 条时间采样结果，会导致本应立即返回 `expired` / `FUTURE_EVENT` 的事件也被迫等待锁并消耗 8 秒请求预算；因此保持首次 `Now` 与 `ProjectExists` 分离，严格维护窗口判断优先于项目查询的契约。
- **不跨事件合并事务、不用可写 CTE 合并冲突查询**：
  - 保持每条事件独立 `BEGIN READ COMMITTED` 与 `COMMIT / ROLLBACK`，避免多桶死锁（`40P01`）并保证 7 秒预算截止时前序已提交项不回滚；
  - 保持冲突后的 `ExistingEvent` 作为独立于 `InsertEvent` 的下一条 SQL 语句执行，确保在 `READ COMMITTED` 下获取最新语句快照，规避同一 SQL 内 CTE 快照看不到并发刚提交行的缺陷。

---

## 2. 方案设计

### 2.1 单条事件事务的网络往返（RTT）排布

| 步骤 | 发送内容（`postgresTx` 内部操作） | RTT 消耗（冷/热连接一致） | 服务端执行顺序与快照语义 |
|---|---|---|---|
| **1. 开启事务** | `BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})` | **1 RTT** | 建立单条事件的独立 `READ COMMITTED` 事务 |
| **2. 首次窗口采样** | 首次 `Now(ctx)`：单发 `SELECT clock_timestamp()`（带 `pgx.QueryExecModeExec`） | **1 RTT** | 立即校验时间窗口；若为 `expired` 或 `FUTURE_EVENT` 直接进入回滚（总 3 RTT），绝不触碰 `projects` 或 `usage_events` |
| **3. 项目归属校验（仅 `project_id` 非空）** | `ProjectExists(ctx, workspaceID, projectID)`：单发 `projectExistsQuery`（带 `pgx.QueryExecModeExec`） | 无项目 **0 RTT**<br>有项目 **1 RTT** | 仅在时间窗口有效且事件携带 `project_id` 时执行 |
| **4. 幂等插入 + 锁后时间采样** | `InsertEvent(ctx, event)`：通过 `pipelinedBatch`（`pgconn.Batch` + `ExecBatch`）单次 `Sync` 同批发送 `insertEventQuery` + `SELECT clock_timestamp()` | **1 RTT** | 服务端先解析并执行 `INSERT INTO usage_events ... ON CONFLICT DO NOTHING RETURNING 1`（含唯一约束锁等待），待其完成后立即执行同批次的 `SELECT clock_timestamp()`；锁后时间缓存在 `postgresTx.sampledNow` 中，供紧随其后的 `windowExpired` (`Now`) **0 RTT** 消费 |
| **5. 分钟聚合或冲突比对 + 锁后时间采样** | • **新增路径（`inserted == true`）**：`ApplyMinute(ctx, input)` 通过 `minuteBatchTx` 包装 `t.tx`，在 `aggregation.ApplyRequest` 调用 `tx.Exec` 时通过 `pipelinedBatch` 单次 `Sync` 同批发送 `minuteUpsert` + `SELECT clock_timestamp()`<br>• **冲突路径（`inserted == false`）**：`ExistingEvent(ctx, ...)` 通过 `pipelinedBatch` 单次 `Sync` 同批发送 `existingEventQuery` + `SELECT clock_timestamp()` | **1 RTT** | 两条路径均在新一轮网络往返中以新的 `READ COMMITTED` 语句快照执行；写桶或查旧行（含行锁等待）完成后立即采样 `clock_timestamp()` 并缓存至 `sampledNow`，供提交前或比对前的 `windowExpired` (`Now`) **0 RTT** 消费 |
| **6. 结束事务** | `Commit(ctx)` 或 `Rollback(ctx)` | **1 RTT** | 窗口复核通过后提交事务，否则回滚 |

- **合计**：无项目有效窗口事件 **5 RTT**（原 7 RTT）；有项目有效窗口事件 **6 RTT**（原 8 RTT）。

### 2.2 关键实现细节（`internal/ingestion/postgres.go`）

1. **单次同步批处理 `pipelinedBatch`（`pgconn.Batch` + `ExecBatch` + `pgx.ExtendedQueryBuilder`）**
   - 对每个两语句组（`InsertEvent + nowQuery`、`ExistingEvent + nowQuery`、`minuteUpsert + nowQuery`），使用 `eqb.Build(typeMap, nil, args)` 编码参数并通过 `batch.ExecParams` 写入 `pgconn.Batch`，最后调用 `conn.PgConn().ExecBatch(ctx, &batch)` 发送单次 `Sync`。
   - 每次 `mrr := batch.exec(ctx)` 配合 `defer func() { _ = mrr.Close() }()`，确保任何一步出错退出时连接上的管道消息都被排空直到 `ReadyForQuery`，不会导致外层 `defer tx.Rollback(rollbackCtx)` 因 `conn busy` 失败。
   - 单发查询 `Now(ctx)` 与 `ProjectExists(ctx, ...)` 均显式传入 `pgx.QueryExecModeExec`，避免冷连接在默认 `cache_statement` 模式下产生额外的 `Prepare` 往返。
2. **阶段错误归属保持不变**
   - `InsertEvent`、`ExistingEvent`、`ApplyMinute` 附带 `nowQuery` 时：若第 1 条主语句失败，立即返回对应阶段错误（`"ingestion insert event"` / `"ingestion existing event"` / `"aggregation minute upsert"`）并保持 `sampledNow = nil`；若第 1 条主语句成功，调用 `captureSampledNow(mrr, batch.typeMap)` 将第 2 条 `nowQuery` 的时间或错误（包装为 `"ingestion server time"`）存入 `sampledNow`，由紧随其后的 `Now(ctx)` 单次消费返回。
3. **集成测试新增验证（`internal/ingestion/postgres_integration_test.go`）**
   - 新增集成测试 `TestBatchPipelineSamplesClockAfterLockWait`：
     - 验证新事件、冲突事件、重复事件通过 `postgresTx` 批处理管道完整成功；
     - 验证在真实数据库行锁阻塞下，`InsertEvent` 与 `ApplyMinute` 批处理返回的 `clock_timestamp()` 严格不早于持锁事务释放前采样的数据库服务端时间（证明管道内第 2 条 `SELECT clock_timestamp()` 确实在锁等待结束后求值，且紧随的 `Now(ctx)` 直接返回该缓存值）；
     - 通过包装底层 `net.Conn`（`ConnConfig.DialFunc`）统计客户端与 PostgreSQL 的请求-响应往返（RTT）次数，完整覆盖 `{无项目 (5 RTT), 有项目 (6 RTT)} × {冷连接, 热连接} × {新增 (accepted), 重复 (duplicate), 冲突 (conflict)}` 共 **12 种组合**（冷连接重复与冲突均先通过非计数连接预置数据，再新建冷计数连接实测首条事务）。

---

## 3. 执行切分

- **单块，不拆**：
  - 修改文件范围（仅 2 个文件）：
    1. `internal/ingestion/postgres.go`
    2. `internal/ingestion/postgres_integration_test.go`
  - 不得改动的文件：`internal/ingestion/service.go`、`internal/ingestion/handler_test.go`、`internal/aggregation/minute.go`、`docs/openapi/openapi.yaml`、`db/migrations/*`。

---

## 4. 验收标准

1. **单条事件 RTT 缩减与严格语义等价**：
   - `internal/ingestion/service.go` 零改动；`internal/ingestion/postgres.go` 在 `InsertEvent`、`ExistingEvent`、`ApplyMinute` 三处使用 `pipelinedBatch`（`pgconn.Batch` + `ExecBatch`）合并锁后时间采样，并在 `Now`、`ProjectExists` 单发查询上使用 `pgx.QueryExecModeExec`；
   - 冷连接与热连接下，无 `project_id` 有效窗口事件（新增/重复/冲突）均为 **5 RTT**，有 `project_id` 有效窗口事件（新增/重复/冲突）均为 **6 RTT**（12 项矩阵全覆盖）；
   - 阶段错误脱敏标识（`ingestion server time`、`ingestion project`、`ingestion insert event`、`ingestion existing event`、`aggregation minute upsert`）及连接排空回滚行为保持不变。
2. **自动化检查全绿**：
   - `GOTOOLCHAIN=local go test -race -count=1 ./internal/ingestion/...` 通过；
   - `make check`（全部 9 个检查阶段）通过；
   - 在真实 PostgreSQL 18 实例上执行 `GOTOOLCHAIN=local go test -mod=readonly -tags=uiembed,integration -race -count=1 -p 1 -v ./internal/ingestion` 全部通过。
