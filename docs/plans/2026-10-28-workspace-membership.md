# T-019：个人工作空间与成员关系实施计划

> 状态：已完成规划；独立 doc-review 的隔离级别必要修正已落实，可执行。实施仍需另行授权，并须满足第 13 节开工条件。
> 日期：2026-10-28。关联 T-019。规划分支：`main`。
> 本轮只编写本计划，未修改代码、迁移、OpenAPI 或其他文档，也未运行构建、迁移或测试。

## 1. 已确认决策

1. 保留并复用迁移 00002 的 `workspaces`，继续作为项目、来源、接入凭证和用量数据的归属与隔离边界；不替换主键、不重复建表。
2. 当前所有工作空间均为个人空间。注册为每个新用户创建唯一初始个人空间，并写入其 `owner` 成员关系。
3. 本期不增加 `personal/team` 类型字段，不实现团队空间、团队创建、邀请/接受/移除、角色变更、跨空间迁移、空间删除或重命名。
4. 不实现双 key 上报、数据双写、多空间切换 API/界面，也不为未来团队需求预留框架。用户后来加入其他空间时，其个人历史仍归原个人空间，不自动复制、移动或共享。
5. 本期只交付成员关系表、个人空间创建能力和调用方所需的成员角色查询；不新增工作空间 HTTP API。

## 2. 目标与非目标

### 目标

- 新增 `workspace_memberships` 及真实查询所需索引、注释和最小数据库权限。
- 在 `internal/workspaces` 提供事务内个人空间创建能力：同一用户的重复或并发调用只产生一个初始个人空间和一条 `owner` 成员关系。
- 提供统一的 `(user_id, workspace_id) → role` 内部查询，供 T-010 的 owner 操作和 T-021 的统一授权复用。
- 支持 T-017 在同一数据库事务中编排用户、个人空间、owner 成员关系和首个会话，任一步失败整体回滚。
- 用单元测试、真实 PostgreSQL 测试和跨任务联调覆盖回滚重试、并发、越权与历史归属边界。

### 非目标

- 不实现注册、登录或任何其他 HTTP API；T-017 负责注册流程，T-016 负责用户表与会话能力。
- 不建通用权限中间件、通用存储层、通用幂等框架或通用事务框架。
- 不实现成员列表、成员写入、邀请/接受/移除、角色变更或最后 owner 保护；T-020 另行规划。没有移除和角色变更路径时，不做空转的最后 owner 检查。
- 不实现跨空间业务授权、项目权限或所有查询的隔离改造；T-021 负责。
- 不新增 `current workspace`、空间列表、团队创建或空间切换端点，不修改前端。
- 不回填或改写既有项目、来源、凭证、用量数据的 `workspace_id`，不增加字段、索引或配置项供假想未来需求使用。

## 3. 已核实的仓库现状

| 事实 | 证据 |
|---|---|
| `workspaces` 已是 UUID v7 主键、`name`、UTC `created_at` 的现有表，服务角色已有 SELECT/INSERT/UPDATE；无需另建空间表 | `db/migrations/00002_foundation_tables.sql:4-8,37,47` |
| 项目、来源、凭证已经按 `workspace_id` 归属；数据模型也要求用量事件按 workspace 隔离 | `db/migrations/00002_foundation_tables.sql:9-36`；`docs/data-model.md:32-34,47` |
| `workspace_memberships` 仍只是设计：字段为 workspace/user/role/joined_at，复合唯一并支持按 user 查空间，角色语义为 owner/member | `docs/data-model.md:3,9-10,31` |
| 项目约定 UUID v7、timestamptz UTC、应用层引用校验，不用 FK/CHECK/触发器/RLS | `docs/data-model.md:19-23` |
| 当前 HEAD 最新迁移为 00004；T-016 已规划目标 00005，因此本任务暂定 00006，开工必须重新核对 | `db/migrations/00004_usage_minute_stats.sql:1-6`；`docs/plans/2026-10-28-user-sessions-password-login.md:42,51` |
| 当前生产 Go 代码尚无用户、成员、注册或工作空间路由；`main` 仅建池并装配探针/前端 handler | `cmd/tokenpivot/main.go:27-57`；`internal/server/server.go:24-62` |
| 现有凭证内核只有固定归属和凭证操作，没有 owner 查询；管理 API 明确等待 T-019 | `internal/credentials/service.go:51-77`；`docs/plans/2026-10-27-ingestion-credentials.md:55-57` |
| OpenAPI 当前只冻结采集端点，没有 workspace/membership/current API；本期不应为其追加路径 | `docs/openapi/openapi.yaml:1-14,37-44` |
| 现有 service/store 采用窄接口和调用方所有的连接池；聚合写入由调用方传入同一事务，不自行提交 | `internal/credentials/service.go:51-77`；`internal/credentials/postgres.go:25-32`；`internal/aggregation/minute.go:1-4` |
| 现有数据库错误边界会去除 SQL、驱动细节和敏感值，只保留静态阶段及取消/超时身份 | `internal/database/database.go:17-29` |
| 已合入代码没有读取“当前个人空间”的调用方，因此不需要 current 内部查询或 HTTP endpoint | `cmd/tokenpivot/main.go:27-57`；`internal/server/server.go:24-62`；`docs/openapi/openapi.yaml:9-14` |
| T-016 旧计划仍包含基础注册 HTTP API，与本轮“T-017 负责注册流程”冲突；实施前必须由主代理统一职责，不能双实现 | `docs/plans/2026-10-28-user-sessions-password-login.md:9-10,97-100,186-191,304-307` |

## 4. 数据表、索引与权限

### 4.1 迁移编号与顺序

- 当前目标为 **`db/migrations/00006_workspace_memberships.sql`**，因为 00004 是当前 HEAD 最新迁移，T-016 已把 00005 用于 users/sessions。
- 主代理在开工和合并前重新核对 HEAD；若编号已被占用，只调整尚未合入的本迁移编号及对应测试，不修改任何已合入迁移。
- 逻辑引用虽由应用校验，注册运行仍依赖 T-016 的 `users` 表。发布顺序须保证 users 先于个人空间创建路径可用。

### 4.2 目标 DDL

```sql
-- +goose Up
CREATE TABLE workspace_memberships (
    workspace_id uuid NOT NULL,
    user_id uuid NOT NULL,
    role text NOT NULL,
    joined_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uk_workspace_memberships_workspace_id_user_id
    ON workspace_memberships (workspace_id, user_id);
CREATE INDEX idx_workspace_memberships_user_id
    ON workspace_memberships (user_id);
COMMENT ON TABLE workspace_memberships IS
    'User membership and role within a workspace. The current release only writes owner rows for personal workspaces.';
COMMENT ON COLUMN workspace_memberships.role IS
    'Application-validated role: owner or member. Only the personal-workspace creation path writes owner in this release.';
GRANT SELECT, INSERT ON workspace_memberships TO tokenpivot_app;

-- +goose Down
DROP TABLE workspace_memberships;
```

设计理由：

- 不增加代理 `id`：当前身份就是 `(workspace_id,user_id)`，没有按另一 ID 读取的调用方。
- 唯一索引同时防止同一用户重复加入同一空间，并以其 `workspace_id` 前缀支持成员角色查询。
- `idx_workspace_memberships_user_id` 有当前真实读取方：个人空间创建时查找该用户已经建立的初始空间。不是为未来空间列表预建。
- **不对 `user_id` 建唯一索引**。当前并发唯一性由第 5 节用户行锁和事务保证；若把 user 唯一写进数据库，会错误禁止数据模型已允许的未来多空间成员关系。
- 不增加空间类型、成员状态、邀请信息、`created_by`、`updated_at` 或软删字段；本期没有读写方。
- `role` 只用 text 和应用层校验，不加 CHECK/枚举类型。当前唯一生产写路径固定写 `owner`；`member` 仅保留既有查询语义，不能借此新增 member 写入入口。
- `tokenpivot_app` 只获得 SELECT/INSERT。UPDATE/DELETE 等权限待 T-020 有真实操作后另行授权；`workspaces` 的既有 INSERT/SELECT 足够本任务使用。

### 4.3 引用与既有数据

- 创建成员前在同一事务内读取并锁定对应 users 行；workspace 与 membership 同事务写入，避免应用路径产生半套数据。
- 不做自动回填。当前 HEAD 没有 users 表或已合入注册实现，也没有可推断既有 workspace owner 的数据。
- T-016 若先独立开放注册，期间用户会没有个人空间；发布编排应避免暴露这个中间状态。若共享环境已存在用户或需要管理的既有 workspace，主代理须先取得明确的 owner 映射和一次性回填决策，不得在本迁移猜测。

## 5. Go 服务接口与事务边界

### 5.1 最小内部接口

在 `internal/workspaces` 使用现有 `pgtype.UUID` ID 口径，提供值类型 `Role`（`owner`/`member`）和只含当前调用方所需字段的 `Workspace{ID, Name}`。对外语义固定为：

```go
MembershipRole(ctx context.Context, userID, workspaceID ID) (role Role, found bool, err error)
CreatePersonal(ctx context.Context, userID ID) (Workspace, error)
```

- `MembershipRole`：唯一成员读取入口。找到返回 owner 或 member；非成员返回 `found=false`；数据库故障与未知角色返回真实错误，不能伪装成非成员。
- T-010 用 `found && role == owner` 判断 owner；T-021 用同一结果实施 member 可读、owner 可写等统一规则。两者不得再各自查询 `workspace_memberships`。
- `CreatePersonal`：只供 T-017 的注册事务调用，固定使用非空展示名 **`Personal`**，不从注册请求新增空间名称、类型或角色参数。返回新建或已存在的初始空间。
- 不提供成员列表、按 workspace 列成员、任意角色写入、空间切换或 current 查询。`CreatePersonal` 的返回值已足够注册编排使用。

### 5.2 创建顺序与并发唯一性

`CreatePersonal` 必须在调用方提供的同一事务中依次完成：

1. `SELECT ... FROM users WHERE id=$1 FOR UPDATE`，确认用户存在并串行化同一用户的个人空间创建；用户不存在返回 `ErrUserNotFound`。
2. 通过 `workspace_memberships.user_id` 查找当前已经建立的 owner 空间；找到则直接返回同一 workspace，不改名、不新增成员。
3. 未找到时插入 `workspaces(name=Personal)`，用 `RETURNING` 取得数据库生成的 UUID v7。
4. 插入 `(workspace_id,user_id,role=owner)` 的 membership。

同一用户的并发事务会在 users 行锁上等待；第二个事务取得锁后重新查询并返回第一个事务已提交的空间。顺序和并发重复调用因此都不会额外建空间。不要用“无锁先查再插”或 `user_id` 唯一索引替代这一机制。

**该并发保证要求 T-017 的注册事务适配显式使用 `pgx.ReadCommitted`，不得使用 REPEATABLE READ 或 SERIALIZABLE。** 在 READ COMMITTED 下，等待 users 行锁的是一条 SQL，锁释放后再执行的下一条 membership 查询使用新快照，因而能看见首个事务已提交的成员关系；两条 SQL 必须保持上述先后顺序，不能合并成依赖旧快照的查询。现有连接池没有固定隔离级别，所以事务入口必须显式指定，不靠数据库实例默认值。无需为此增加唯一字段、重试框架或其他锁机制。

这里的“按用户查 owner 空间”只在本期所有空间皆为个人空间的语义下成立。未来 T-020 真正引入多空间前必须重审创建和读取规则，不能把它继续当作通用 current workspace 查询。

### 5.3 PostgreSQL 适配与事务所有权

- `internal/workspaces/postgres.go` 提供池绑定读取和事务绑定创建所需的最小适配。创建适配器的构造参数只接受 `pgx.Tx`，池绑定适配器只暴露 `MembershipRole`，从类型边界防止误用连接池执行创建、导致 users 行锁随单条 SQL 提前释放；包内可用窄 `DBTX` 复用 SQL 方法，但不新建全局数据库抽象。
- T-017 的 registration service 拥有注册事务边界和窄 `TxRunner`。其 PostgreSQL 适配显式以 `pgx.ReadCommitted` 开启事务，并在同一 `pgx.Tx` 上绑定 T-016 用户/会话能力和本任务 workspaces 能力；业务 service 不导入 pgx。
- T-019 不自行 Begin/Commit/Rollback，也不在注册事务内调用 SMTP、HTTP 或其他外部服务。
- 非事务的 `MembershipRole` 使用调用方所有的现有连接池；T-010/T-021 在实际装配 handler 时注入同一 `workspaces.Service`。T-019 本身不为无处使用的路由修改 `main`。

## 6. 错误、回滚与重试

- 非成员是正常授权输入，用 `found=false` 表达；用户不存在、未知数据库角色和真实数据库故障必须与非成员区分。
- PostgreSQL 错误经 `database.SafeError` 转换，使用固定阶段文本；不向响应或日志泄露 SQL、参数和驱动细节。
- 不把任意唯一冲突都当成创建成功。用户行锁已负责正常重复/并发路径，异常冲突应使事务失败并暴露数据问题。
- 任一用户、workspace、membership 或首个 session 写入失败，T-017 返回错误并回滚整个注册事务；不采用补偿删除、不保留“稍后再补空间”的状态。
- `CreatePersonal` 对已建空间的重复调用是幂等读取并返回既有结果；注册 HTTP 请求整体是否幂等仍由 T-017 定义，本任务不增加幂等键或响应缓存。
- 注册提交成功但响应丢失时，不自动重放创建。重试同邮箱若被用户唯一性拒绝，客户端走登录；不得因此再建个人空间。提交结果未知时以数据库实际提交状态为准。
- 迁移 Down 只用于确认过的本地/测试库，完整回滚顺序先 drop memberships、再允许 T-016 drop users；共享或生产环境只做前向修复，不用 Down 删除真实成员数据。

## 7. 跨任务责任边界

| 任务 | 负责 | 明确不负责/对接方式 |
|---|---|---|
| **T-019** | memberships 表、个人空间创建、统一成员角色查询 | 无 HTTP API；不查业务表、不做通用权限中间件 |
| **T-010** | 凭证管理 API 的会话、Origin、owner 准入和归属校验 | 调用 `MembershipRole` 并拒绝非 owner；不自行查成员表，不另造 owner 规则 |
| **T-016** | users、sessions、密码与会话认证，以及供注册事务复用的用户/会话写入能力 | 按最新决策不拥有注册流程或个人空间；users 行和权限须支持事务内 `FOR UPDATE` |
| **T-017** | 注册开关、注册 service/HTTP，编排 user + workspace + owner membership + 首个 session 的单一事务 | 不另写 workspace/membership SQL；通过 T-019 事务绑定能力创建个人空间 |
| **T-020** | 未来邀请、接受、移除、角色变更和最后 owner 保护 | 届时新增所需权限与迁移；本期不提前实现 member 写入或 owner 计数 |
| **T-021** | 统一业务授权、项目权限和所有业务查询的跨空间隔离 | 复用 T-019 角色查询；membership 只是授权输入，不改变数据本身的 workspace 归属 |

主代理须先修订/协调 T-016 旧计划和尚未规划的 T-017，使“T-017 拥有注册流程”只有一个实现来源。本计划不修改其他任务计划文件。

## 8. 文件范围与实施顺序

### 新增

- `db/migrations/00006_workspace_memberships.sql`：编号以开工核对为准。
- `cmd/tokenpivot-migrate/workspace_memberships_integration_test.go`：列、索引、权限、无禁用约束及 Up/Down 测试。
- `internal/workspaces/service.go`：Role、Workspace、创建与角色查询、窄 store 接口及业务错误。
- `internal/workspaces/postgres.go`：池/事务绑定的 SQL 适配与安全错误转换。
- `internal/workspaces/service_test.go`：重复返回、锁定后重查、非成员、角色与错误语义。
- `internal/workspaces/postgres_integration_test.go`：真实并发、回滚、跨用户及历史归属测试。

### 修改

- `docs/data-model.md`：把 memberships 从草案更新为实际迁移、索引、权限和本期个人空间写入边界。
- `docs/decisions/0004-auth-and-workspace.md`：记录 2026-10-28 的个人空间限定和注册原子性决策。
- `docs/architecture.md`：同步当前无团队/切换、注册事务及 owner 初始化；未来团队只保留为另行规划的演进方向。
- `docs/prd.md`：同步当前个人空间范围，移除把团队切换和最小成员管理写成本期已交付能力的表述。

### 明确不动

- 不改迁移 00002 或其他已合入迁移，不改 users/sessions 表。
- 不改 `cmd/tokenpivot/main.go`、`internal/server/*`、前端或 `docs/openapi/openapi.yaml`；本任务没有 HTTP 接口。
- 不改凭证、采集、聚合业务包，不新增 Go 依赖、配置键、生成代码或 lockfile 变化。
- 不修改其他任务计划，不手工修改 `docs/todo.md`。

### 顺序

1. 主代理核对迁移编号、T-016/T-017 注册职责和 users 事务接口；确认不存在需要回填的共享环境数据。
2. 先完成 memberships 迁移及真实 PostgreSQL schema/权限测试。
3. 实现角色查询和 PostgreSQL 读取，接好跨用户、非成员和未知角色失败语义。
4. 实现事务绑定个人空间创建，覆盖用户行锁、重复和并发调用。
5. 更新上述文档，运行本任务发布前检查；再由主代理安排 T-017 注册联调和 T-010/T-021 消费接入。

## 9. 文档同步要求

- `docs/data-model.md`：保留 owner/member 词汇和复合唯一设计；注明当前只有个人空间创建路径写 owner、应用权限只有 SELECT/INSERT，不把未来成员管理写成已存在。
- ADR-0004：明确“当前仅个人空间、无空间切换、未来团队另行规划”，同时保留 workspace 隔离和个人历史不随未来团队成员关系自动共享的原则。
- `docs/architecture.md:35,77-83` 与 `docs/prd.md:36-43,81,217`：清理当前范围中的团队切换、首版成员管理和未决首次 owner 旧表述，避免计划与产品文档互相矛盾。
- OpenAPI 不变更。只有未来出现真实 HTTP 调用方时，才由对应任务先更新接口文档再实现；不得因假想前端需要追加 current endpoint。
- T-033/T-041 等待办中的切换器、成员界面范围由主代理另行协调，不在本任务实现或预留接口。

## 10. 测试与验收

以下均为实施后必须实际执行并报告结果的验收项；本规划轮次没有运行这些测试。

### 10.1 迁移与权限

- 精确核验四列类型、NOT NULL、`joined_at` 默认值、两条索引的列顺序和唯一性；无额外字段/索引，无 FK/CHECK/触发器/RLS。
- `tokenpivot_app` 对 memberships 恰有 SELECT/INSERT，没有 UPDATE/DELETE/TRUNCATE/REFERENCES/TRIGGER；对 workspaces 的创建和 users 的事务内锁定在完整迁移链上可执行。
- 同一 `(workspace_id,user_id)` 重复插入由唯一索引拒绝；不同用户可加入同一 workspace，同一用户也可拥有不同 workspace 的种子成员关系，证明未误加 user 唯一限制。
- Down/Up 只在专用测试库验证；已有迁移矩阵和权限回归不得被破坏。

### 10.2 创建、回滚与重试

- 新用户创建成功时恰好有一个新 workspace 和一条 owner membership，名称为 Personal，ID 由数据库生成；注册响应不依赖额外 current 查询。
- 顺序重复 `CreatePersonal` 返回同一 workspace，空间名不变，workspace/membership 数量不增加。
- 两个显式 READ COMMITTED 的并发事务对同一用户调用时，构造第二个事务等待 users 行锁、第一个事务随后提交的次序；第二个事务必须返回同一空间，最终只有一个 workspace 和一条 owner membership。两个不同用户则得到不同 workspace，互不影响。
- 分别注入 workspace 插入、membership 插入失败，均不产生半套提交；用户不存在时不得创建 workspace。
- T-017 联调分别注入用户、个人空间、membership、首个 session 写入失败：四类数据全部回滚；随后以相同注册输入重试可完整成功且只有一套用户/个人空间。
- 注册成功响应丢失和并发同邮箱注册按 T-017 规则处理，不得产生第二个用户或第二个个人空间；不宣称注册 HTTP 请求已变成可自动重放的幂等操作。

### 10.3 成员查询与授权边界

- owner 查询返回 owner/found=true；测试直接种子 member 行时返回 member/found=true；非成员返回 found=false。
- 用户 A 对用户 B 的个人 workspace 查询必须是非成员，数据库故障不得被映射成非成员；不存在“只按 workspace 列成员”的旁路。
- 未知 role 返回错误并使调用方默认拒绝，不能降级为 member 或 owner。
- T-010/T-021 联调中，member 与非成员执行 owner 写操作均被拒绝，且不发生业务写库；T-019 不直接返回 HTTP 状态，具体 403/404 映射由对应调用方负责。
- 全仓库调用检查确认 T-010/T-021 没有各自重复查询 memberships 或另建 owner 判断。

### 10.4 个人历史不自动共享

- 为用户 U 的个人空间 P 种子项目、来源、凭证/用量归属，再直接种子 U 与另一空间 T 的成员关系，模拟未来加入团队；P 中既有行的 `workspace_id` 不变，T 下不出现复制行。
- `CreatePersonal` 对另一用户始终创建独立空间，不能复用 U 的 P；成员角色查询本身不得修改任何业务归属。
- T-021/查询任务联调须证明：以 T 为目标的读取不会仅因 U 同时属于 P 和 T 就返回 P 的历史数据。该业务读取验收不能由 T-019 的角色单元测试替代。

### 10.5 发布前检查

```sh
make check
# 安全注入两个 TEST 数据库变量并使用独占专用 _test 库后：
make test-integration
```

新增 PostgreSQL 测试使用现有 `integration` 标签和专用测试库；缺环境时明确报告集成验收未完成，不以 skip 代替通过。跨任务联调结果与本任务本地结果分开报告。

## 11. 执行切分

**单块，不拆。** memberships 迁移、创建事务和角色查询共享同一小组接口，并行拆分只会增加协调。

跨任务必须串行协调：

- 主代理拥有迁移编号分配；T-016 当前目标 00005，T-019 暂定 00006。开工与合并前均以最新 HEAD 为准。
- 主代理先统一 T-016/T-017 注册职责和事务接口，T-017 再接入 T-019 的事务绑定创建能力；不得同时实现两条注册路径。
- T-019 不修改 `main`、server 或 OpenAPI。T-017、T-010、T-021 分别在真实装配需求出现时修改共享装配文件，由主代理安排串行。
- T-019 块拥有上述新增文件及四份文档的本范围修改；若 T-016 同时修改 ADR/PRD/data-model，主代理按内容合并，不覆盖注册与会话的新决策。

## 12. 外部依据

本方案只依赖仓库已核实的 PostgreSQL/Go 约定和用户已确认的个人私有语义，没有需要外查才能决定的技术事实，因此未进行外部搜索，也未采用第三方参考实现、依赖或许可证内容。

## 13. 风险、开工条件与待确认事项

### 开工条件（主代理协调，不需用户重新拍板）

1. 核对 HEAD 最新迁移和 T-016 实际编号；发生竞争时只重排未合入迁移。
2. 统一 T-016/T-017 注册职责：T-016 提供用户/会话能力，T-017 拥有注册流程及单一事务编排；删除或调整旧计划中的重复注册实现范围。
3. T-016 的 users 表、事务绑定写入能力及 `tokenpivot_app` 权限已可支持同事务插入 users 和 `SELECT ... FOR UPDATE`。
4. 确认注册不会在 T-017+T-019 集成前对真实环境开放；若已有无空间用户或无 owner 的既有 workspace，先另行确定回填/映射，不静默跳过。
5. T-010/T-021 开工时使用本计划唯一角色查询，主代理核对其装配与跨空间验收。

### 主要风险

- 当前“按用户查 owner 空间”依赖所有空间皆个人的本期前提；未来引入多空间时若直接复用为 current 查询会选错空间，T-020 必须先重审。
- 无 FK 意味着引用完整性依赖同事务校验和写入；绕过 service 的手工 SQL 仍可制造孤儿数据，测试和运维不得把应用约束误称为数据库保证。
- T-016 若先交付独立注册，会形成无个人空间用户或重复注册路径；发布编排必须避免该中间状态。
- 角色查询只提供授权输入，不会自动给现有业务 SQL 增加 workspace 条件；T-021 未接入前不得宣称跨空间隔离已经完整交付。
- 现有测试 workspace 若没有对应 owner membership，后续凭证管理测试会被正确拒绝；测试夹具须显式创建成员关系，不能绕过授权。

### 待用户确认

无。个人空间语义、团队范围和注册责任均已由本轮决策确定；上述事项属于实施协调与证据核验，不重新开放产品问题。
