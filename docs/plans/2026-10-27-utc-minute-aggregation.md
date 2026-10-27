# T-012：UTC 分钟聚合写入

> 日期：2026-10-27。状态：**已完成只读评审并落实修正；条件可实施**。独立的分钟表、事务内写入函数及其测试可按本计划实施；真实 `usage_events` 联调、服务角色授权和最终迁移编号受第 3 节前置条件约束。本文不代表已获实施授权，也不替 T-007/T-008/T-009/T-010/T-011 定稿。

## 1. 目标与范围

### 目标

1. 新建 `usage_minute_stats`，以 `occurred_at` 对应的 UTC 分钟起点和指定业务维度唯一确定一行。
2. 提供一个由调用者传入 `pgx.Tx` 的分钟写入函数；明细首次插入与分钟增量由调用者放在同一事务中。
3. 对六类 token 分别累加可展示值及 `known/estimated/missing` 数量，保留真实零、部分缺失和估算信息。
4. 用数据库唯一约束和单条原子 UPSERT 防止并发丢增量；任何失败都交还调用者回滚，不产生“明细成功、分钟失败”的部分成功。
5. 只允许请求级最终事件进入本写入路径；汇总级事件不得增加 `event_count` 或伪造请求数。

### 非目标

- 不实现来源校验、事件归一化、项目归属校验、认证、HTTP 批处理或 duplicate/conflict 判定；这些属于 T-007/T-011 等任务。
- 不决定来源 `total` 与分项不一致时如何归一化；该问题仍由 T-007/T-043 及 OpenAPI 证据决定。
- 不实现明细或分钟查询、小时/日汇总、缓存率、费用、保留清理、重建、异步聚合或性能承诺。
- 不引入 Redis、消息队列、ORM、通用 repository/TxRunner、新 UUID 库或其他依赖；不改连接池和配置。
- 不修改 `cmd/tokenpivot` 装配或 HTTP 路由。T-011 后续只调用本文接口，避免 `ingestion ↔ aggregation` 循环依赖。

## 2. 已核实的现状

### 文档决定

- `docs/decisions/0003-minute-aggregation-and-retention.md:7-23`：`occurred_at` 决定分钟桶，仅首次接受事件同步双写；明细按首次 `received_at` 保留 180 天，分钟统计保留 13 个日历月。滚动 60 秒另查明细，小时/日由分钟汇总。
- `docs/data-model.md:40-58`：明细表为 `usage_events`，业务唯一键是 `(workspace_id,source_id,source_event_id)`；六类 token 为 input/output/total/cache_read/cache_write/reasoning，缓存与推理是子集，不能额外加到 total。
- `docs/data-model.md:62-73`：分钟表为 `usage_minute_stats`；逻辑键为 `(workspace_id,bucket_start,source_id,project_group,provider,model)`，包含 source、排除 credential。每指标保存值之和及三类质量数量，真实零不是缺失。
- `docs/data-model.md:75-88`：已知 `0+0` 是完整真实零；`input=100, output=20, cache_read=60` 时 total 为 120 而非 180；`output=20, reasoning=8` 时 output 仍为 20；只有小时汇总不得虚构请求数。
- `docs/plans/2026-10-27-batch-ingestion.md:32-46`：T-012 可独立交付分钟写入；T-011 仅在首次插入后调用，重复或冲突不得再次增量。调用者拥有事务与 Commit，T-012 不借池、不自行提交。
- `docs/architecture.md:37-71`：历史重建只在明细完整覆盖范围内进行，并需暂停摄入和事务替换；重建不在本任务实施。

### 代码与命令

- 规划时 `db/migrations/00001_bootstrap.sql:1-6` 是唯一迁移，Up/Down 均为 `SELECT 1`；后续从 00002 编号，但实际编号必须按实施时已合入文件重新确定。
- `cmd/tokenpivot-migrate/main.go:36-55` 使用嵌入迁移和独立入口；`:58-77` 为迁移连接设置 `lock_timeout=5s`、`statement_timeout=15s`；`:91-121` 支持 `status/up/down-one`。服务不会自动迁移。
- `cmd/tokenpivot-migrate/main_integration_test.go:152-214` 当前硬编码迁移版本 1 和“无业务表”。任何首个业务迁移合入时都需同步修正这些既有断言，不能为保留旧断言而跳过新迁移测试。
- `internal/database/database.go:46-80` 已提供进程共享 `pgxpool.Pool`，最大连接数 4；本任务不修改它。`:17-30` 的安全错误会丢弃驱动详情，不能从普通错误文本反解 SQLSTATE。
- `cmd/tokenpivot-migrate/main_integration_test.go:29-150` 的测试辅助函数是包内私有函数，不能被 `internal/aggregation` 测试直接调用；不得声称可以复用。真实测试仍须遵守双角色、回环 IP、`_test` 数据库、随机自有 schema、归属核验和独占运行约束。
- `Makefile:61-86` 已有 `make check` 与 `make test-integration`，无需新增命令。
- 规划时不存在 `internal/aggregation/*.go` 或 `internal/ingestion/*.go`；本文出现的这些路径均为**拟新增**，不是现有接口。
- `go.mod:3-8` 已固定 Go 1.26.4、pgx v5.11.0、goose v3.29.0；本计划不升级或新增依赖。
- `docs/plans/2026-10-27-toolchain-baseline.md:9,46-61` 声明 PostgreSQL 18.4 已由用户确认，但实际服务端尚未核实；集成测试必须记录真实服务端版本。

## 3. 前置条件与协调边界

| 条件 | 本任务如何处理 | 阻塞范围 |
|---|---|---|
| 主代理在实施后分配下一个迁移编号 | 文件名使用 `NNNNN_usage_minute_stats.sql` 占位；不得预设 00002，不改已发布迁移 | 只阻塞迁移落盘 |
| 实际测试/运行服务端支持 `NULLS NOT DISTINCT` | PostgreSQL 18.4 声明满足语法要求；集成启动时核实服务端版本，若低于 15 则停止并报告，不静默改唯一性口径 | 阻塞真实数据库验收 |
| T-007/T-011 提供已归一化的六指标值与逐指标质量 | 本任务定义分钟包自己的最小输入，不导入 ingestion；未来由 T-011 做一次字段映射 | 不阻塞独立 writer；阻塞真实来源联调 |
| T-009 的 `usage_events` 已合入 | 独立测试用测试专用明细探针表验证同事务模式；真实明细重复/冲突联调由 T-011 在 T-009 后完成 | 只阻塞端到端验收 |
| T-008 确定业务表服务角色授权方式 | 本迁移不猜固定角色、不授 PUBLIC、不把 owner 当服务角色；合入前按统一方式给分钟表最小 `SELECT/INSERT/UPDATE` 权限，并在真实 app 角色下验收 | 阻塞生产可用与服务角色验收 |
| T-011 保持“逐项独立事务、首次插入后才调用、Commit 成功才 accepted” | 本任务只提供函数，不改其批处理状态机，不在 writer 内重试 | 阻塞最终采集联调 |

没有需要用户重新选择的产品口径。以上均为实施顺序或环境核实事项；若授权方式与既有任务无法协调，主代理应停下报告，而不是自行增加角色配置。

## 4. 数据结构与数据流

### 4.1 拟新增迁移

文件：`db/migrations/NNNNN_usage_minute_stats.sql`（编号由主代理分配）。

表不使用代理主键，不建外键、CHECK、触发器或 RLS。逻辑 `project_group` 直接映射为可空 `project_id`：真实项目保存其 ID，未归属保存 NULL；非法或跨空间项目 ID 已由上游拒绝，不能降成 NULL。

DDL 形状如下，六组指标列必须完整展开，不能省略 cache_write 或 reasoning：

```sql
CREATE TABLE usage_minute_stats (
    workspace_id uuid NOT NULL,
    bucket_start timestamptz NOT NULL,
    source_id uuid NOT NULL,
    project_id uuid NULL,
    provider text NOT NULL,
    model text NOT NULL,
    event_count bigint NOT NULL,

    input_tokens_sum bigint NOT NULL,
    input_tokens_known_count bigint NOT NULL,
    input_tokens_estimated_count bigint NOT NULL,
    input_tokens_missing_count bigint NOT NULL,
    output_tokens_sum bigint NOT NULL,
    output_tokens_known_count bigint NOT NULL,
    output_tokens_estimated_count bigint NOT NULL,
    output_tokens_missing_count bigint NOT NULL,
    total_tokens_sum bigint NOT NULL,
    total_tokens_known_count bigint NOT NULL,
    total_tokens_estimated_count bigint NOT NULL,
    total_tokens_missing_count bigint NOT NULL,
    cache_read_tokens_sum bigint NOT NULL,
    cache_read_tokens_known_count bigint NOT NULL,
    cache_read_tokens_estimated_count bigint NOT NULL,
    cache_read_tokens_missing_count bigint NOT NULL,
    cache_write_tokens_sum bigint NOT NULL,
    cache_write_tokens_known_count bigint NOT NULL,
    cache_write_tokens_estimated_count bigint NOT NULL,
    cache_write_tokens_missing_count bigint NOT NULL,
    reasoning_tokens_sum bigint NOT NULL,
    reasoning_tokens_known_count bigint NOT NULL,
    reasoning_tokens_estimated_count bigint NOT NULL,
    reasoning_tokens_missing_count bigint NOT NULL,

    CONSTRAINT usage_minute_stats_key UNIQUE NULLS NOT DISTINCT (
        workspace_id, bucket_start, source_id, project_id, provider, model
    )
);
```

- 所有 sum/count 显式写入，不用默认值掩盖漏列；未知值在 sum 列中以增量 0 参与运算，是否正确代表“不可用”由三类质量数量判断。
- 唯一约束已以 `(workspace_id,bucket_start,...)` 开头，可支持 workspace 的时间范围访问；本任务不再建重复的 workspace+bucket 索引。source/project 查询索引留给后续查询任务 T-024/T-027 按实际查询与 EXPLAIN 决定。实施时同步数据模型 §5，注明这些候选索引暂缓，并非已建成。
- `ON CONFLICT` 使用与约束完全相同的六列推断，不依赖硬编码约束名。
- Down 仅 `DROP TABLE usage_minute_stats`，其唯一索引随表删除。生产共享环境不自动 down；已执行迁移需要修复时新增前向迁移。

### 4.2 拟新增 Go 接口

文件：`internal/aggregation/minute.go`。

```go
func ApplyRequest(ctx context.Context, tx pgx.Tx, in MinuteInput) error
```

`MinuteInput` 是 aggregation 包拥有的最小输入，包含：

- workspace/source/project/provider/model、与明细写入完全相同且已按 T-007 冻结规则完成精度规范化的 `occurred_at`；不传入规范化前的原始时间，T-012 不另选舍入规则；
- 显式事件级别，至少能区分 request 与 summary；
- 六个**命名**指标字段，每项为可空的非负 `int64` 值和 `known/estimated/missing` 质量。非负及数值范围由 T-011 在调用前校验一次；writer 不重复该校验，联调必须证明负数不会进入明细和分钟双写。

标识符的 Go 类型沿用实施时已确定的明细类型，不为此新增 UUID 依赖。六指标不用 map，避免拼写键和漏指标。这个输入只用于隔离包依赖，不是另一套来源归一化模型；T-011 负责从最终事件直接映射，不在映射中重算 total。

函数责任严格限定为：

1. 拒绝非 request 级别；拒绝 `known/estimated` 却无值、`missing` 却有值或未知质量值。该校验防止分钟质量计数被写坏，其他来源有效性仍由上游负责。
2. 用 `in.OccurredAt.UTC().Truncate(time.Minute)` 计算 `bucket_start`，只绑定该时间，不读取会话时区，不用 `received_at`。
3. 将输入转换成一条 UPSERT 并传入调用者的 `ctx` 与 `tx`。
4. 成功即返回；校验或 SQL 失败返回不含 SQL、参数或凭据的错误。函数不 Begin、Commit、Rollback、不重试、不新建连接，也不把错误吞成成功。

### 4.3 单条 UPSERT 的增量规则

每个指标独立计算 EXCLUDED 值：

| 输入质量 | 值增量 | known_count 增量 | estimated_count 增量 | missing_count 增量 |
|---|---:|---:|---:|---:|
| known，值允许为 0 | 输入值 | 1 | 0 | 0 |
| estimated，值允许为 0 | 输入值 | 0 | 1 | 0 |
| missing，输入值必须为 NULL | 0 | 0 | 0 | 1 |

`event_count` 的 EXCLUDED 增量固定为 1。冲突更新一律采用 `usage_minute_stats.col + EXCLUDED.col`，没有 `WHERE`，不先 SELECT 再写回。total 只累加输入的 total 指标，**不由 input/output 重算，也不加 cache_read/cache_write/reasoning**。

由此得到每个桶、每个指标的不变量：

```text
known_count + estimated_count + missing_count = event_count
sum = 所有 known/estimated 输入值之和（missing 不贡献值）
```

查询层以后用三类数量区分完整真实零、完整已知非零、部分覆盖和全部缺失；本任务不提前返回“完整/部分”的 API 结论。

### 4.4 未来真实调用顺序（拟接入，不是现有代码）

```text
T-007/T-011 归一化并校验最终事件
  → 调用者 Begin
  → INSERT usage_events
      ├─ 唯一键冲突/其他失败：不调用分钟写入，Rollback
      └─ 首次插入成功：aggregation.ApplyRequest(同一 tx)
            ├─ 失败：Rollback，明细与分钟均不保留
            └─ 成功：调用者 Commit
                  ├─ Commit 明确成功：才可报告 accepted
                  └─ Commit 结果不确定/失败：不报告 accepted；重试由明细唯一键防重
```

分钟函数不含事件 ID，**自身不是幂等防重接口**；恰好一次增量依赖上述明细唯一键和调用顺序。不得通过在分钟键中增加事件 ID 来“修复”重复调用，那会破坏分钟桶唯一性。

## 5. 并发、溢出、失败与隔离

- **相同事件并发**：由 `usage_events` 唯一约束仲裁；只有实际插入的一方调用 writer。独立测试用带唯一键的测试明细表验证该模式，真实表验收留给 T-011。
- **不同事件同分钟并发**：六列唯一约束配合单条 `ON CONFLICT DO UPDATE` 自加，保证每次已提交调用只增加一次，不能丢失另一事务的增量。`project_id=NULL` 也参与同一个唯一仲裁。
- **溢出**：sum/count 使用 PostgreSQL `bigint` 加法；超出范围由数据库报错。不得改用静默环绕、截断、 clamp 到最大值或跳过分钟更新。调用者回滚本次明细和分钟增量，已存在桶保持原值；不根据错误字符串或 SQLSTATE 做自动重试。
- **锁等待、取消、断连、权限或 SQL 错误**：使用 `internal/database.SafeError` 配合静态阶段信息脱敏返回，仅保留取消/超时身份，丢弃驱动详情及 SQLSTATE；调用者收到任何失败均回滚，不依赖驱动错误分类。writer 内无无限重试、无另开连接“补写”。请求生命周期超时由调用者现有 context 决定，本任务不新增超时配置。
- **事务隔离**：沿用 T-011 的 Read Committed，不提升隔离级别。唯一键与原子 UPSERT 已覆盖本任务的两类写冲突；不在本任务引入 advisory lock。
- **包依赖**：`internal/aggregation` 不导入未来 `internal/ingestion`、HTTP server 或 pool；未来只允许 ingestion 依赖 aggregation。数据库错误统一使用现有 `internal/database.SafeError`，不暴露原始 PG 细节。
- **服务权限**：迁移 owner 与服务角色分离。授权方式未统一前，不用 owner 通过来冒充 app 角色可写，也不授予迁移历史或建表权限。

## 6. 拟改文件与实施步骤

### 文件归属

| 路径 | 状态与归属 |
|---|---|
| `db/migrations/NNNNN_usage_minute_stats.sql` | 本任务新增；主代理分配编号并协调 T-008/T-009 |
| `internal/aggregation/minute.go` | 本任务新增，拥有 UTC 分桶、输入检查、单条 UPSERT |
| `internal/aggregation/minute_test.go` | 本任务新增，纯 Go 边界与输入检查 |
| `internal/aggregation/minute_integration_test.go` | 本任务新增，真实 PG 事务/并发/溢出测试；包内安全辅助代码，不新建共享测试框架 |
| `cmd/tokenpivot-migrate/main_integration_test.go` | 共享文件；仅在新迁移使旧版本/表数量断言失效时最小修改，由主代理协调，不能同时重构其他迁移测试 |
| `docs/data-model.md` §5 | 实施时由本任务同步实际分钟列、唯一性及暂缓的查询索引；不改 T-009 所属 §4，本次规划不修改该文件 |
| `go.mod`、锁文件、`cmd/tokenpivot/main.go`、HTTP/OpenAPI | 本任务不改 |

### 有序步骤

1. 主代理核对当前迁移目录、T-008/T-009 合入顺序和服务角色授权方式，分配编号；实施者核实实际 PG 服务端版本。
2. 新增分钟表迁移与必要最小授权。通过独立迁移入口在随机测试 schema 执行 `status → up → up → down-one → up`，验证重复 up 无变化、Down 仅删除本表、最终对象及唯一约束存在。
3. 新增最小输入和 `ApplyRequest`。先实现 UTC 分桶与质量表示检查，再实现一条完整展开的 UPSERT；不加额外抽象。
4. 添加纯 Go 测试：UTC/时区/秒纳秒边界、request/summary、六指标 known/estimated/missing 表示检查。SQL 结果以真实 PG 测试为准，不用 mock 证明数据库正确性。
5. 添加真实 PG 测试。测试自建仅用于事务验证的明细探针表，先插明细再调用 writer；覆盖提交、回滚、唯一身份冲突不增量、不同事件并发增量、NULL 项目、溢出和 app 最小权限。测试不跨包复制连接池，不 `t.Parallel`，不跳过缺失环境变量。
6. 若旧迁移测试硬编码失败，仅更新其对当前迁移集合的断言；保留单连接、事务、锁和最小权限检查。
7. 运行第 7 节检查并记录真实 PG 版本。T-009/T-011 就绪后，由 T-011 增加真实 `usage_events` 联调；本任务不抢占其文件或批处理实现。

## 7. 验收矩阵

| 场景 | 可观察预期 | 独立/联调 |
|---|---|---|
| 同一 instant 使用不同 Go Location/固定偏移表示 | 得到相同 UTC `bucket_start` | 独立 |
| `12:34:00`、`12:34:59.999999999`、`12:35:00` | 前两者同桶，第三者进入下一桶；不受本地时区/DST影响 | 独立 |
| 分钟末尾超精度时间按 T-007 规范化后写入明细和分钟 | 两者使用同一规范化值并归于同一分钟；纳秒单测不代表生产输入精度 | T-009/T-011 联调 |
| known/estimated 输入负数或超出 int64 范围 | T-011 调用前拒绝，明细与分钟均无增量 | T-011 联调 |
| 同一 NULL 项目键顺序或并发写两次不同事件 | 只有一行，`event_count=2`，各质量数量按两次输入累加 | 独立 |
| 改 workspace/source/project/provider/model 中任一维度 | 进入不同桶；credential 不在输入和键中 | 独立 |
| known=0 | sum 为 0、known_count 增 1，不是 missing | 独立 |
| 单指标 estimated=100，其他指标已知 | 值计入 sum，estimated_count 增 1；不改成 known | 独立 |
| 单指标或六指标全部 missing | sum 不变、missing_count 对应增加；请求仍使 event_count 增 1，全部缺失不能被解释为真实零 | 独立 |
| 两请求 output=20 与 output missing | output sum=20、known=1、missing=1、event_count=2 | 独立 |
| input=100/output=20/total=120/cache_read=60；output=20/reasoning=8 | total 始终 120，不因缓存变 180；output 始终 20，不因推理变 28 | 独立 |
| summary 级输入 | 整个调用报错且无任何分钟增量；不会只跳过 event_count 而仍加 token | 独立 |
| 明细探针插入+writer 成功后未 Commit | 另一连接不可见未提交分钟行；调用者 Commit 后两者同时可见 | 独立 |
| writer 前/中失败后 Rollback | 本次明细和分钟均不可见；既有桶不变 | 独立 |
| 相同测试明细身份并发，仅插入成功者调用 writer | 一个事务成功，event_count 只增 1 | 独立；真实表由 T-011 复验 |
| N 个不同明细身份并发写同一桶 | 全部 Commit 后 event_count=N，sum 和三类计数等于 N 次输入之和，无丢更新 | 独立 |
| 既有 sum 或 event_count 接近 bigint 上限，再正增量 | 数据库报错，调用者回滚；本次明细不存在，旧桶数值不变 | 独立 |
| 非 request、非法质量表示、取消、权限不足 | 返回错误，不吞错、不重试、不自行 Commit/Rollback | 独立 |
| app 角色执行 writer | 仅需表现有 SELECT/INSERT/UPDATE 权限即可成功；不能建表或写 goose 历史 | 待授权方式协调 |
| 真实 `usage_events` duplicate/conflict、Commit 结果不确定与重试 | duplicate/conflict 不增量；只有 Commit 明确成功才 accepted | T-009/T-011 联调，非本任务单独完成 |

发布前检查使用既有命令，不新增脚本：

```bash
GOTOOLCHAIN=local go test -mod=readonly -race -count=1 ./...
GOTOOLCHAIN=local go vet -mod=readonly ./...
make check
```

真实数据库验收只在已授权的专用 `_test` 数据库、两个安全测试 URL 均注入且无人并行使用时执行：

```bash
make test-integration
```

不得用普通单测、mock 或 owner 连接成功替代并发、权限和事务验收；规划阶段不执行上述集成命令。

## 8. 回滚与恢复

- 单次事件失败由调用者回滚整个事务，无需分钟补偿任务。
- 迁移由 goose 逐文件事务执行；失败文件的对象和版本记录一起回滚。已成功文件保留，先核对状态再处理。
- 新表对旧服务是附加对象，代码回滚到未使用分钟表版本不需要删表。共享/生产环境优先前向修复，不自动执行 Down。
- 显式 Down 会删除分钟表及全部分钟统计，只能按现有 `down-one` 管理流程使用；不能把它当无数据损失的代码回滚。
- 从 180 天明细重建分钟、清理后重建和超过明细覆盖期的历史恢复属于后续任务，不在本文伪造恢复能力。

## 9. 依据

| 编号 | 结论与取舍 | 来源 | 日期/状态 |
|---|---|---|---|
| E1 | PostgreSQL 唯一约束默认把 NULL 视为不相等，`NULLS NOT DISTINCT` 改变该行为并自动建立唯一 btree 索引。采用可空 `project_id + NULLS NOT DISTINCT`，拒绝哨兵 UUID、额外生成列和多个“未归属”桶。 | [PostgreSQL 18 CREATE TABLE](https://www.postgresql.org/docs/18/sql-createtable.html) | 2026-10-27 抓取官方正文，已核实语法；实际服务端待集成核实 |
| E2 | 冲突目标可推断列集合完全匹配的非部分唯一索引；`ON CONFLICT DO UPDATE` 在无独立错误时保证并发下原子 INSERT 或 UPDATE。采用六列冲突目标和单条自加 UPSERT，拒绝 SELECT 后写回。 | [PostgreSQL 18 INSERT](https://www.postgresql.org/docs/18/sql-insert.html) | 2026-10-27 抓取官方正文，已核实；不以文档替代本项目并发测试 |
| E3 | 既有 Lago/Langfuse 源码研究只用于确认业务唯一身份、按发生时间归属、租户隔离和失败测试形状；不复制实现，不引入其存储栈。 | `docs/research/2026-10-26-tokenpivot-ingestion-storage-reference.md:9-21,29-40,83-114` | 既有仓库研究，本轮未重新抓取；无新的 GitHub 调查或性能断言 |

## 10. 执行切分

**单块，不拆。** 分钟表、唯一 UPSERT、质量计数和事务测试必须作为一个不可分改动验收，拆成并行块只会增加临时接口和共享迁移冲突。

- 本块拥有第 6 节四个新增文件及数据模型 §5 的同步；主代理协调迁移编号、业务表授权和共享迁移测试。
- T-008 拥有基础表和授权方式约定；T-009 拥有明细表；后续查询任务拥有查询筛选索引；T-011 拥有真实调用与采集状态机。合入顺序变化时只调整编号和共享断言，不改变本计划分钟口径。
- 无共享新依赖、注册表、生成代码、配置键或锁文件；主代理无需为本块重新生成依赖文件。

## 11. 未决项（均非用户产品决策）

- TODO(待确认)：实施时的实际迁移编号，以及是否已有共享环境执行过业务迁移。
- TODO(待确认)：实际 PostgreSQL 服务端版本与专用集成环境；声明的 18.4 不是运行验证。
- TODO(待确认)：T-008 的业务表服务角色授权写法；未协调前不得用固定测试角色或 PUBLIC 代替。
- TODO(待确认)：T-007/T-011 最终事件字段名到 `MinuteInput` 的机械映射；不得借此改变六指标质量或 total 归一化决定。
- 真实 `usage_events` 的重复、冲突、Commit 不确定和 accepted 联调仍由 T-011 完成；这些未决项不妨碍独立 writer 实施，但未完成前不得宣称端到端采集已验收。
