# T-066 采集单条事件事务数据库往返无损合并优化实施计划

> 关联待办：`T-066`
> 依据文档：[OpenAPI 接口文档](../openapi/openapi.yaml) §5.3 / §8、[ADR-0002](../decisions/0002-ingestion-idempotency-and-batch.md)、[ADR-0003](../decisions/0003-minute-aggregation-and-retention.md)、[数据模型](../data-model.md) §4–§5

---

## 1. 目标与边界

### 1.1 目标
在完全保持现有对外 API 行为、[OpenAPI §5.3 采样点要求](../openapi/openapi.yaml) 与 [ADR-0002 逐项独立事务语义](../decisions/0002-ingestion-idempotency-and-batch.md) 不变的前提下，利用 `pgx/v5` 的底层扩展查询单次同步批处理（`pgconn.Batch` + `ExecBatch` 配合 `pgx.ExtendedQueryBuilder`），将单条事件 `READ COMMITTED` 事务内的相邻独立 SQL 打包在同一次网络往返（RTT）中按序发送，确保无论在冷连接还是热连接上，单条事件（含带 `project_id` 的新事件与重复/冲突事件）的数据库网络往返均从 **7～8 次 RTT 严格降至 5 次 RTT**，且第 1 条语句始终在第 2 条语句解析与执行之前完成。

### 1.2 非目标与严禁触碰的边界
- **不改数据库表结构与迁移**：不新增或修改 `db/migrations/` 下的任何文件。
- **不改 OpenAPI 接口文档与错误码映射**：`docs/openapi/openapi.yaml` 保持不变。
- **不改 `internal/aggregation` 公共接口**：`aggregation.ApplyRequest(ctx, tx, in)` 签名、校验逻辑与 `minuteUpsert` SQL 文本保持不变。
- **不跨事件合并事务、不用可写 CTE 合并冲突查询**：
  - 保持每条事件独立 `BEGIN READ COMMITTED` 与 `COMMIT / ROLLBACK`，避免多桶死锁（`40P01`）并保证 7 秒预算截止时前序已提交项不回滚；
  - 保持冲突后的 `ExistingEvent` 作为独立于 `InsertEvent` 的下一条 SQL 语句执行，确保在 `READ COMMITTED` 下获取最新语句快照，规避同一 SQL 内 CTE 快照看不到并发刚提交行的缺陷。

---

## 2. 方案设计

### 2.1 单条事件事务的 5 次网络往返（RTT）排布

| RTT 序号 | 发送内容（`postgresTx` 内部操作） | 服务端执行顺序与快照语义 |
|---|---|---|
| **RTT 1** | `BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})` | 建立单条事件的独立 `READ COMMITTED` 事务 |
| **RTT 2** | 首次 `Now(ctx)`：<br>• 无 `project_id`：单发 `SELECT clock_timestamp()`<br>• 有 `project_id`：通过 `pgconn.Batch`（`ExecBatch`）单次 `Sync` 同批发送 `SELECT clock_timestamp()` + `SELECT 1 FROM projects WHERE workspace_id = $1 AND id = $2` | 服务端按消息流顺序先解析并执行第 1 条 `SELECT clock_timestamp()`，再解析并执行第 2 条 `projectExistsQuery`（即便 `projects` 表解析或权限失败，第 1 条时间采样结果也已先返回）；若窗口为 `expired` 或 `FUTURE_EVENT` 直接返回并丢弃预取项目结果或错误；若窗口有效，紧随其后的 `ProjectExists` 直接读取已缓存结果或错误（**0 RTT**） |
| **RTT 3** | `InsertEvent(ctx, event)`：<br>通过 `pgconn.Batch`（`ExecBatch`）单次 `Sync` 同批发送 `insertEventQuery` + `SELECT clock_timestamp()` | 服务端先解析并执行 `INSERT INTO usage_events ... ON CONFLICT DO NOTHING RETURNING 1`（含唯一约束锁等待），待其完成后立即执行同批次的 `SELECT clock_timestamp()`；锁后时间缓存在 `postgresTx` 中，供紧随其后的 `windowExpired` (`Now`) **0 RTT** 消费 |
| **RTT 4** | • **新增路径（`inserted == true`）**：`ApplyMinute(ctx, input)` 通过 `minuteBatchTx` 包装 `t.tx`，在 `aggregation.ApplyRequest` 调用 `tx.Exec` 时通过 `pgconn.Batch`（`ExecBatch`）单次 `Sync` 同批发送 `minuteUpsert` + `SELECT clock_timestamp()`<br>• **冲突路径（`inserted == false`）**：`ExistingEvent(ctx, ...)` 通过 `pgconn.Batch`（`ExecBatch`）单次 `Sync` 同批发送 `existingEventQuery` + `SELECT clock_timestamp()` | 两条路径均在新一轮网络往返中以新的 `READ COMMITTED` 语句快照执行；写桶或查旧行（含行锁等待）完成后立即采样 `clock_timestamp()` 并缓存，供提交前或比对前的 `windowExpired` (`Now`) **0 RTT** 消费 |
| **RTT 5** | `Commit(ctx)` 或 `Rollback(ctx)` | 窗口复核通过后提交事务，否则回滚 |

### 2.2 关键实现细节

1. **可选项目预取提示（`internal/ingestion/service.go`）**
   - 定义包内可选接口：
     ```go
     type projectPrefetcher interface {
         PrefetchProject(workspaceID, projectID credentials.ID)
     }
     ```
   - 在 `processItem` 首次调用 `sampleWindow(ctx, tx, e)` 之前，若 `e.ProjectID.Valid` 为真且 `tx` 实现了 `projectPrefetcher`，调用 `p.PrefetchProject(credential.WorkspaceID, e.ProjectID)`。
   - `fakeTx` 与 `contextRecordingTx` 无需实现该接口，原有单元测试调用序列记录（`now` → `project` → `insert` → `now` → `minute` → `now` → `commit`）保持 100% 不变。

2. **单次同步批处理（`pgconn.Batch` + `ExecBatch`）与阶段错误隔离（`internal/ingestion/postgres.go`）**
   - 为什么不用 `tx.SendBatch` 默认的 `QueryExecModeCacheStatement`：
     - `tx.SendBatch` 在语句未命中缓存（冷连接或失效后）时会先发起一轮全批次 `Prepare + Sync` 往返，再发起第二轮 `Bind/Execute + Sync` 往返：这既使冷连接多出额外网络往返，也会在第 2 条语句（如 `projectExistsQuery`）预处理失败时提前中断整个批次，导致第 1 条 `nowQuery` 未能执行、错误被归为 `"ingestion server time"`，使过期或未来事件无法按窗口优先原则返回 `expired` / `FUTURE_EVENT`。
     - 改用 `pgx.ExtendedQueryBuilder`（`eqb.Build(typeMap, nil, args)`）配合 `pgconn.Batch.ExecParams` 与 `conn.PgConn().ExecBatch(ctx, &batch)`：在单个扩展协议管道帧内按序写入第 1 条语句的 `Parse + Bind + Describe + Execute` 与第 2 条语句的 `Parse + Bind + Describe + Execute`，最后附带**唯一一个 `Sync`**。无论冷连接还是热连接都严格只发生 **1 次网络往返**，且 PostgreSQL 严格在第 1 条语句解析并执行完毕（写出结果行）之后才开始解析第 2 条语句。
   - 每次 `mrr := conn.PgConn().ExecBatch(ctx, &batch)` 必须配合 `defer func() { _ = mrr.Close() }()`，确保任何一步出错退出时连接上的管道消息都被排空关闭，不会导致外层 `defer tx.Rollback(rollbackCtx)` 因 `conn busy` 失败。
   - **阶段错误归属保持不变**：
     - 初检 `Now(ctx)` 附带 `projectExistsQuery` 时：若第 2 条参数编码失败，将其缓存在 `bufferedProject.err` 并退化为单发 `nowQuery`；批处理执行后，先读取第 1 条 `nowQuery`（若失败返回 `database.SafeError("ingestion server time", err)`），再读取第 2 条 `projectExistsQuery`（即便 `projects` 表不存在导致 Parse 失败或无权限导致 Execute 失败，第 1 条 `now` 已成功读出，第 2 条错误缓存在 `bufferedProject.err` 中，仅在窗口有效且 `ProjectExists` 被调用时返回 `database.SafeError("ingestion project", err)`；过期或未来事件直接返回 `expired` / `FUTURE_EVENT`）。
     - `InsertEvent`、`ExistingEvent`、`ApplyMinute` 附带 `nowQuery` 时：若第 1 条主语句失败，立即返回对应阶段错误（`"ingestion insert event"` / `"ingestion existing event"` / `"aggregation minute upsert"`）并清空 `sampledNow`；若第 1 条主语句成功，将第 2 条 `nowQuery` 的时间或错误存入 `sampledNow`，由紧随其后的 `Now(ctx)` 返回。

3. **集成测试 `hookStore` 兼容与新增验证（`internal/ingestion/postgres_integration_test.go`）**
   - 在 `hookTx` 上实现 `PrefetchProject`：仅当 `t.hooks.now == nil`（未注入脚本化时钟）时转发给 `t.inner`；当测试注入了 `t.hooks.now` 时跳过预取，`ProjectExists` 自动回退为单独执行 `projectExistsQuery`，保证既有锁等待与脚本时钟测试完全兼容。
   - 新增集成测试 `TestBatchPipelineSamplesClockAfterLockWait`：
     - 验证带 `project_id` 的新事件、重复事件、冲突事件通过 `postgresTx` 批处理管道完整成功；
     - 通过包装底层 `net.Conn`（`ConnConfig.DialFunc`）统计客户端与 PostgreSQL 的请求-响应往返（RTT）次数，验证在**冷连接（首条事件）与热连接（后续事件）**上，新增、重复、冲突单条事件事务均严格为 **5 次网络往返**；
     - 验证当 `projects` 表查询在解析阶段失败（例如临时重命名 `projects` 表）或执行阶段失败时，带 `project_id` 的过期事件仍返回 `expired`、未来事件仍返回 `FUTURE_EVENT`，而有效窗口事件返回 `retryable` 且日志阶段为 `"ingestion project"`（而非 `"ingestion server time"`）；
     - 验证在真实数据库行锁阻塞下，`InsertEvent` 与 `ApplyMinute` 批处理返回的 `clock_timestamp()` 严格不早于持锁事务释放前采样的数据库服务端时间（证明管道内第 2 条 `SELECT clock_timestamp()` 确实在锁等待结束后求值）。

---

## 3. 执行切分

- **单块，不拆**：
  - 修改文件范围：
    1. `internal/ingestion/service.go`
    2. `internal/ingestion/postgres.go`
    3. `internal/ingestion/postgres_integration_test.go`
    4. `internal/ingestion/handler_test.go`（如需补充单元测试）
  - 不得改动的文件：`internal/aggregation/minute.go`、`docs/openapi/openapi.yaml`、`db/migrations/*`。

---

## 4. 验收标准

1. **单条事件 RTT 缩减与语义等价**：
   - `internal/ingestion/postgres.go` 在 `Now`（带项目预取）、`InsertEvent`、`ExistingEvent`、`ApplyMinute` 四处使用 `pgconn.Batch`（`ExecBatch`）+ `pgx.ExtendedQueryBuilder` 合并相邻查询，冷连接与热连接单条事件事务均为 5 次网络往返；
   - 第 2 条语句（如 `projectExistsQuery`）即使发生 SQL 解析错误或权限错误，也不会阻断第 1 条 `nowQuery` 的采样，过期或未来事件仍返回 `expired` 或 `FUTURE_EVENT`；
   - 阶段错误脱敏标识（`ingestion server time`、`ingestion project`、`ingestion insert event`、`ingestion existing event`、`aggregation minute upsert`）及连接排空回滚行为保持不变。
2. **自动化检查全绿**：
   - `GOTOOLCHAIN=local go test -race -count=1 ./internal/ingestion/...` 通过；
   - `make check`（全部 9 个检查阶段）通过；
   - 若环境配置了 `TOKENPIVOT_TEST_DATABASE_URL` 与 `TOKENPIVOT_TEST_APP_DATABASE_URL`，执行 `GOTOOLCHAIN=local go test -mod=readonly -tags=uiembed,integration -count=1 -p 1 -v ./internal/ingestion` 通过。
