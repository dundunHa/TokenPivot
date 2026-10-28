# T-017：注册开关、空库首注册与个人空间事务编排实施计划

> TODO ID：T-017
> 状态：已完成规划，可实施；实施仍需主代理按流程授权。
> 日期：2026-10-28。本轮只编写本计划，不修改代码、配置、其他文档或 git 状态，也未运行测试。

## 1. 已确认决策

1. 新增环境变量 **`TOKENPIVOT_REGISTRATION_OPEN`**，默认 **`false`**。云端需要公开注册时显式设为 `true`；配置仍在启动时读取一次，不做热加载。
2. 开关关闭时，`users` 表为空仍允许一个用户注册；首个用户同时成为其 `Personal` 空间的 owner。此后所有注册均被拒绝，直到部署方显式打开开关。
3. 两个请求同时在空库注册时只能一个成功。空库判定和首个用户写入必须处于同一数据库事务，不能先在事务外查空再插入。
4. 注册被拒返回 **HTTP 403 + `REGISTRATION_CLOSED`**。不增加“注册是否开放”的公开查询端点。
5. 当前注册尚未接入 T-019；本任务须把 `CreatePersonal` 编排进现有注册事务，使用户、个人空间、owner 成员关系和首个会话同事务提交，任一步失败整体回滚。
6. 沿用现有限流：合法输入先占用邮箱/IP 额度，再进行密码哈希和注册准入判断。关闭状态下被拒绝的合法尝试也消耗额度，超限仍优先返回 429。
7. 不做 SMTP/邮箱验证、邀请、CLI 建用户、前端页面；分别维持 T-018/T-020/T-033 等既有边界。不新增依赖、迁移、通用事务框架或配置中心。

## 2. 目标与非目标

### 目标

- 交付默认关闭的注册开关及空库首注册例外，并保证多请求、多连接下的并发正确性。
- 让每次成功注册原子写入 user、`Personal` workspace、owner membership 和首个 session。
- 增加明确的 403 业务错误，同步 OpenAPI 接口文档、配置说明和产品决策文档。
- 用单元测试、HTTP 测试和真实 PostgreSQL 测试覆盖开关、并发及所有写入阶段的回滚。

### 非目标

- 不改变登录、登出、会话续期、Argon2id 参数、Cookie 或现有限流阈值。
- 不改变 T-019 的表结构、`CreatePersonal` 创建顺序或成员角色查询；不新增工作空间 HTTP API。
- 不处理既有“无个人空间用户”的回填。当前代码尚未接入个人空间，若共享环境已经用旧版本注册过用户，主代理须先取得回填决策，不能由本任务猜测 owner 映射。
- 不提供注册状态探针，不新增前端行为，不做 UI 自动化验收。

## 3. 已核实的仓库现状

| 事实 | 证据 |
|---|---|
| 当前 `Register` 的顺序是输入校验、限流、Argon2id、生成令牌、事务内 `CreateUserWithSession`；没有任何个人空间调用 | [internal/auth/service.go:138-169](../../internal/auth/service.go#L138-L169) |
| 当前 `Store.CreateUserWithSession` 把 user 与 session 写在同一方法中；为满足明确的四阶段编排和逐阶段故障验收，需要拆开 | [internal/auth/service.go:97-115](../../internal/auth/service.go#L97-L115)、[internal/auth/postgres.go:95-123](../../internal/auth/postgres.go#L95-L123) |
| 当前 `TxRunner` 只向回调提供 auth `Store`，没有事务绑定的个人空间能力 | [internal/auth/service.go:117-123](../../internal/auth/service.go#L117-L123) |
| 当前 PostgreSQL 事务用 `pool.Begin`，未显式指定隔离级别；T-019 的并发语义要求改为显式 READ COMMITTED | [internal/auth/postgres.go:56-76](../../internal/auth/postgres.go#L56-L76) |
| T-019 已实现 `workspaces.NewTx(tx).CreatePersonal`，且只在事务绑定适配器上暴露；它依次锁 users 行、查既有 owner 空间、插入 workspace 和 owner membership | [internal/workspaces/postgres.go:61-82](../../internal/workspaces/postgres.go#L61-L82)、[internal/workspaces/service.go:106-134](../../internal/workspaces/service.go#L106-L134) |
| 配置在 `config.Load` 启动时一次读取，`Config` 当前只有监听地址、数据库 URL 和 Web origin | [internal/config/config.go:13-49](../../internal/config/config.go#L13-L49) |
| `main` 已同时装配 auth 与 workspaces，但注册 service 只收到 auth store/limiter | [cmd/tokenpivot/main.go:41-54](../../cmd/tokenpivot/main.go#L41-L54) |
| HTTP 错误映射目前没有注册关闭错误；403 只用于 Origin 的 `PERMISSION_DENIED` | [internal/server/auth.go:264-285](../../internal/server/auth.go#L264-L285)、[internal/server/auth_middleware.go:83-95](../../internal/server/auth_middleware.go#L83-L95) |
| 注册 OpenAPI 仍只描述 users + sessions，事务描述和丢响应重试规则均需更新 | [docs/openapi/openapi.yaml:633-694](../openapi/openapi.yaml#L633-L694) |
| README 仍写“注册用户还不会自动获得个人空间”，实施后必须删除这条过时说明 | [README.md:159-178](../../README.md#L159-L178) |
| ADR/PRD 已确定直接注册、个人空间同事务和自托管可关闭公开注册，但尚未写入本轮默认关闭与空库例外细则 | [ADR-0004:11-15](../decisions/0004-auth-and-workspace.md#L11-L15)、[PRD:43](../prd.md#L43) |

结论：**T-019 尚未接入注册事务**，所以本任务不是只补核实测试，必须修改注册编排。

## 4. 配置与 HTTP 行为

### 4.1 配置项

在 `config.Config` 增加 `RegistrationOpen bool`，由 `TOKENPIVOT_REGISTRATION_OPEN` 加载：

| 环境变量状态 | 结果 |
|---|---|
| 未设置 | `false` |
| 精确值 `true` / `false` | 对应布尔值 |
| 显式空值、大小写变体、空白或其他值 | 启动失败；错误只指出变量名和允许值，不回显输入 |

这里沿用现有严格配置风格，不接受 `1`、`yes` 等额外拼写，避免不同部署对同一值产生不同理解。`main` 把 `cfg.RegistrationOpen` 传给 `auth.New`，由 service 保存为不可变的启动配置；handler 不自行读取环境变量。

### 4.2 拒绝响应

- 在 `internal/auth` 增加哨兵错误 `ErrRegistrationClosed`。
- `writeServiceError` 将其映射为：

```json
HTTP 403
{
  "error": {
    "code": "REGISTRATION_CLOSED",
    "message": "registration is closed"
  }
}
```

- 响应保持 `Cache-Control: no-store`，不下发会话 Cookie，不产生任何业务写入。
- Origin 不合法仍是既有 `403 PERMISSION_DENIED`，先于 JSON、限流和注册开关判断；二者在 OpenAPI 的同一个 403 响应中分别列示例。
- 关闭且已有用户时，准入判断先于用户插入和邮箱唯一冲突，因此已占用邮箱与新邮箱都返回同一个 `REGISTRATION_CLOSED`，不会额外泄露某个邮箱是否存在。开放时仍保持 `409 EMAIL_UNAVAILABLE`。
- 不新增 GET 或其他注册状态路径。

## 5. 注册顺序、接口与事务

### 5.1 完整请求顺序

保持现有安全边界，顺序固定为：

1. Origin 校验、请求体/JSON 校验、邮箱规范化与密码格式校验。
2. 现有邮箱/IP 共享限流器原子占用一次额度；超限直接 429，不哈希、不开事务。
3. 在事务外完成 Argon2id 和随机会话令牌生成。
4. 开启显式 **READ COMMITTED** 事务。
5. 仅当 `RegistrationOpen=false` 时，获取注册排他锁并检查 users 是否为空；非空则返回 `ErrRegistrationClosed`。
6. 插入 user。
7. 通过同一 `pgx.Tx` 调用 `workspaces.NewTx(tx).CreatePersonal`，创建 `Personal` 与 owner membership。
8. 插入首个 session。
9. 提交后才向 handler 返回明文令牌和到期时间，由 handler 设置 Cookie；任一步错误都回滚，不返回成功会话。

Argon2id 保持在事务和注册锁之外，避免慢哈希占住数据库连接并串行阻塞其他请求。代价是关闭状态下的合法请求仍支付一次哈希成本；现有限流照常先于它生效，本任务不为省这次哈希增加事务外预查或重复检查。

### 5.2 最小 Go 接口调整

不再让 store 用一个 `CreateUserWithSession` 方法隐藏写入顺序。将 auth 的非事务读取/会话接口与注册事务接口调整为以下形状（名称可按 Go 评审微调，职责不得改变）：

```go
// Store 保留登录、会话认证/撤销及 CreateSession；删除 CreateUserWithSession。
type RegistrationStore interface {
    Store
    CreateUser(ctx context.Context, email, passwordHash string) (User, error)
    CreatePersonal(ctx context.Context, userID ID) error
    LockRegistration(ctx context.Context) error
    HasUsers(ctx context.Context) (bool, error)
}

type TxRunner interface {
    InTx(ctx context.Context, fn func(context.Context, RegistrationStore) error) error
}
```

- `Service.Register` 只编排上述步骤；`registrationOpen` 决定第 5 步是否执行。开放模式不做无意义的 users 存在性查询，也不串行化正常注册。
- `internal/auth/postgres.go` 用一个事务绑定适配器组合现有 `sqlStore` 与 `workspaces.NewTx(tx)`。`CreatePersonal` 适配器负责 `auth.ID` 到 `workspaces.ID` 的显式转换并丢弃注册响应不需要的 Workspace 返回值；**不重写 workspace/membership SQL**。
- `auth` 的 PostgreSQL 适配可以依赖 `internal/workspaces`；service 不导入 pgx 或 workspaces，workspaces 也不反向依赖 auth，避免循环依赖。
- `CreateUser` 复用现有 user SQL 和 `uk_users_email` 的 23505 映射；`CreateSession` 复用现有 session SQL。注册与登录仍各自生成全新令牌。
- `Postgres.InTx` 改用 `BeginTx(..., pgx.TxOptions{IsoLevel: pgx.ReadCommitted})`。提交、回滚及提交结果未知时的人工核对语义不变，不增加自动重试。

这是对现有 `TxRunner` 的局部扩展，不新建 registration 包或通用事务容器。之后若还有新的注册写入步骤，只需扩展同一事务接口；当前不为假想步骤预留更多抽象。

### 5.3 空库并发方案

关闭开关时，在事务内按以下顺序执行：

```sql
SELECT pg_advisory_xact_lock(17017, 1);
SELECT EXISTS (SELECT 1 FROM users);
```

- `pg_advisory_xact_lock` 是 PostgreSQL 的事务级 advisory lock：这里指由数据库在事务提交或回滚时自动释放的排他锁。两个整数组成固定键；键是包内常量，不是环境配置，也不使用 `hashtext`，避免字符串哈希碰撞。
- 使用阻塞版本而不是 try 版本：后到请求等待前者结束。前者回滚后，后者仍看到空库并可成为首个用户；前者提交后，后者必须看到已有用户并返回 `ErrRegistrationClosed`。
- 锁与空库查询必须保持两条独立 SQL，且都在同一 READ COMMITTED 事务内。等待结束后才开始的 `SELECT EXISTS` 使用新语句快照，能够看到前一个事务已提交的 user。
- 锁一直持有到 user、个人空间、owner membership、session 全部提交或回滚。不得在检查空库后提前释放。
- 开放模式跳过此锁；同邮箱并发注册继续由 `uk_users_email` 仲裁。不同部署实例必须保持相同开关配置；配置为开放的实例本来就允许注册，不参与“关闭状态下仅首个用户”的限制。
- 获取锁或查询 users 失败都返回安全数据库错误，不能映射成“关闭”或“空库”，也不能继续写入。上下文取消/超时直接结束等待，不添加锁重试循环。

未采用表锁或新增单例表：它们需要更重的锁或新迁移，而 PostgreSQL 已有的事务级排他锁正好覆盖“空表时没有可锁行”的场景。固定键已经占用，后续版本不得随意更换；若必须更换，需避免不同键版本同时处理空库首注册。

## 6. 改动文件清单

### 生产代码与配置

| 文件 | 改动 |
|---|---|
| [internal/config/config.go](../../internal/config/config.go) | 新增 `RegistrationOpen`，严格加载 `TOKENPIVOT_REGISTRATION_OPEN`，默认 false |
| [cmd/tokenpivot/main.go](../../cmd/tokenpivot/main.go) | 将配置传给 `auth.New`，不在 main 写注册规则 |
| [internal/auth/service.go](../../internal/auth/service.go) | 新增关闭错误与开关字段；拆分注册写入编排；扩展事务回调接口 |
| [internal/auth/postgres.go](../../internal/auth/postgres.go) | 显式 READ COMMITTED；增加 CreateUser、注册锁/空库 SQL 和事务绑定 CreatePersonal 适配；删除合并写入方法 |
| [internal/server/auth.go](../../internal/server/auth.go) | 映射 403 `REGISTRATION_CLOSED`，保持其他错误不变 |
| [.env.example](../../.env.example) | 列出变量、默认值、严格取值和空库例外 |
| [README.md](../../README.md) | 配置表、启动说明、注册行为及错误；移除“注册无个人空间”的过时说明 |

### 测试

- [internal/config/config_test.go](../../internal/config/config_test.go)：默认值、显式 true/false、非法值与错误脱敏。
- [internal/auth/service_test.go](../../internal/auth/service_test.go)：调整 fake 事务接口，覆盖开关、调用顺序、限流先后及错误传播。
- [internal/auth/postgres_integration_test.go](../../internal/auth/postgres_integration_test.go)：真实四表提交、空库首注册、并发、拒绝和逐阶段回滚。
- [internal/server/auth_test.go](../../internal/server/auth_test.go)：调整 fake，增加 403 响应、无 Cookie/无写入及账号枚举边界。
- [internal/server/auth_middleware_test.go](../../internal/server/auth_middleware_test.go)：只更新 `auth.New`/fake 接口调用，保留 Origin、no-store 等回归。

### 接口与产品文档

- [docs/openapi/openapi.yaml](../openapi/openapi.yaml)：先更新再实现。注册成功事务改为四部分；说明开关、空库例外、限流消耗和条件化重试结果；403 同时列 `PERMISSION_DENIED` 与 `REGISTRATION_CLOSED`；将新码加入 `AuthError` 枚举和错误码表。开放时重复邮箱仍为 409；关闭且已有用户时统一为 403。
- [docs/decisions/0004-auth-and-workspace.md](../decisions/0004-auth-and-workspace.md)：追加本轮默认关闭、空库首注册、并发与专用错误码决策，不推翻既有直接注册/个人空间决策。
- [docs/prd.md](../prd.md)：同步默认关闭与空库例外，避免“开放注册”被理解为所有部署默认无限制开放。

明确不改：`internal/workspaces/*`、所有迁移、`go.mod`/`go.sum`、登录/会话逻辑、前端、其他任务计划及 `docs/todo.md`。

## 7. 实施顺序

1. 主代理核对 HEAD 是否出现新的注册调用方或共享环境既有用户；有既有无空间用户时先停下协调，不静默实施。
2. 先更新 OpenAPI、ADR 和 PRD 的本任务口径。
3. 实现配置及 service/store 接口调整，接入显式 READ COMMITTED、注册锁、空库检查和 CreatePersonal。
4. 接 HTTP 错误与 main 装配，再更新测试 fake 和中间件测试调用。
5. 完成单元、HTTP、真实 PostgreSQL 测试，同步 README 与 `.env.example`。
6. 运行普通与真实数据库发布前检查；主代理再做只读 review 和既有交付流程。

## 8. 测试清单

### 8.1 配置与 service 单元测试

- 变量未设置为 false；精确 `true`/`false` 生效；空值、`TRUE`、` yes `、`1` 等被拒绝，错误不含原始值。
- 开放且空库：成功；开放且已有用户：不同邮箱仍可成功，每个用户各有独立 Personal/owner；开放路径不调用 LockRegistration/HasUsers。
- 关闭且空库：恰好成功一个，调用顺序为锁 → 空库检查 → user → CreatePersonal → session。
- 关闭且已有用户：已占用和新邮箱都返回 `ErrRegistrationClosed`，CreateUser/CreatePersonal/CreateSession 均未调用。
- 锁失败、空库检查失败、user/personal/session 失败均原样传播；不得把数据库故障误报为关闭或邮箱占用。
- 合法但被关闭拒绝的尝试仍消耗限额；额度耗尽时返回限流错误且不再进入注册锁/数据库。既有非法输入先于限流拒绝的行为不变。

### 8.2 HTTP 测试

- 拒绝时精确返回 403、`REGISTRATION_CLOSED`、固定 message、`Cache-Control: no-store`，没有 `Set-Cookie`，store 无写入。
- 关闭且已有用户时，已注册邮箱与未注册邮箱的响应状态、code、body 一致，不出现 `EMAIL_UNAVAILABLE`。
- Origin 错误仍为 403 `PERMISSION_DENIED`；限流仍为 429 + `Retry-After`，两者不被新映射覆盖。
- 成功响应仍为 201 + `{user, expires_at}` + 新会话 Cookie，不增加 workspace 字段或注册状态端点。

### 8.3 真实 PostgreSQL 集成测试

- **开放注册**：连续注册两个不同邮箱均成功；每人都恰有一个 users 行、一个名为 Personal 的 workspace、一条 owner membership 和一个首个 session。重复/并发同邮箱仍只有一个完整账号，其余为 `ErrEmailUnavailable`。
- **关闭 + 空库**：第一次注册成功并提交上述四类数据；随后不同邮箱注册返回关闭错误，四表数量不增加。
- **关闭 + 并发空库首注册**：从不同邮箱同时发起至少两个 `Service.Register`，最终恰好一个成功，其余都是 `ErrRegistrationClosed`；users/workspaces/memberships/sessions 各只有一套。用独立连接或锁等待观测固定竞争过程，不靠 `sleep` 猜测。
- **等待者看到提交结果**：证明后到请求是在注册锁上等待，而不是靠邮箱唯一冲突失败；若前者回滚，则后者可在仍为空库时成功。
- **逐阶段回滚**：在彼此隔离的测试 schema 中，分别撤销 users、workspaces、workspace_memberships、sessions 的 INSERT 权限，令真实 `Service.Register` 在对应阶段失败；撤权须通过 `integrationSchema` 返回的 owner 连接执行，并先断言应用角色对该表的**有效** INSERT 权限已撤销（`has_table_privilege`，含继承权限），再断言错误确实来自目标阶段（不能只看四表为空）；每次提交后检查四表均无本次残留。恢复一个代表性故障（session INSERT）后以相同输入重试，应只生成一套完整数据。
- 所有故障路径都返回安全错误，响应/日志不含 SQL、密码、令牌、摘要或数据库 URL；既有 T-019 CreatePersonal 并发/幂等测试继续通过，不以新测试替代其验收。

## 9. 验收标准

1. 不设置新变量时，非空库注册被拒绝；空库只允许首个用户完整注册。显式 `true` 后可持续开放注册，显式 `false` 与未设置一致。
2. 两个并发空库首注册不会都成功，也不会留下 user 有而 Personal/owner/session 缺失的半套数据。
3. 每次成功注册的四类数据同事务可见；对 user、workspace、membership、session 任一阶段注入失败均整体回滚。
4. 拒绝响应为 403 `REGISTRATION_CLOSED`，不设置 Cookie；关闭时不通过邮箱唯一冲突额外暴露账号是否存在，也没有公开注册状态接口。
5. 限流、Origin、no-store、重复邮箱、会话及 T-019 成员关系既有行为不回归。
6. OpenAPI、README、`.env.example`、ADR、PRD 与实现一致。
7. 以下命令实际通过；缺专用测试库或变量时必须报告集成验收未完成，不以 skip 代替通过：

```sh
make check
# 安全注入 TOKENPIVOT_TEST_DATABASE_URL 与 TOKENPIVOT_TEST_APP_DATABASE_URL，
# 使用独占专用 _test 库：
make test-integration
```

本任务不改变依赖，完成后 `go.mod`/`go.sum` 应无 diff。

## 10. 执行切分

**单块，不拆。** 配置、注册 service、PostgreSQL 事务适配、HTTP 错误和测试共享同一组接口，并行拆分只会增加接口协调。

本块独占第 6 节文件；若其他任务同时修改 `internal/server/auth*`、`cmd/tokenpivot/main.go`、OpenAPI、ADR 或 PRD，由主代理串行协调。主代理负责 TODO 状态、worktree、提交与合并；本计划不要求新增共享生成文件或迁移编号。

## 11. 依据

外部资料抓取日期均为 **2026-10-28**；只采用与项目 PostgreSQL 18 对齐的一手官方文档，未搜索或照搬第三方参考实现。

| 编号 | 事实 | 来源 | 状态与采用情况 |
|---|---|---|---|
| E1 | `pg_advisory_xact_lock` 获取排他事务级锁，冲突时等待；事务结束自动释放且无需显式 unlock | [PostgreSQL 18：Advisory Lock Functions](https://www.postgresql.org/docs/18/functions-admin.html#FUNCTIONS-ADVISORY-LOCKS)、[PostgreSQL 18：Advisory Locks](https://www.postgresql.org/docs/18/explicit-locking.html#ADVISORY-LOCKS) | 已核实；采用阻塞的事务级版本，拒绝 session 锁和 try 版本 |
| E2 | advisory lock 可用两个 32 位整数作为键，且两个整数与单个 64 位整数的键空间不重叠 | [PostgreSQL 18：Advisory Lock Functions](https://www.postgresql.org/docs/18/functions-admin.html#FUNCTIONS-ADVISORY-LOCKS) | 已核实；采用固定双整数键，不用 hashtext |
| E3 | READ COMMITTED 的每条 SELECT 使用语句开始时快照，同一事务的后一条 SELECT 可看到此前并发事务的新提交 | [PostgreSQL 18：Read Committed Isolation Level](https://www.postgresql.org/docs/18/transaction-iso.html#XACT-READ-COMMITTED) | 已核实；锁等待后另起 `SELECT EXISTS`，并显式固定隔离级别 |

## 12. 风险、回滚与开放问题

### 主要风险

- 关闭状态下所有合法注册尝试会先做 Argon2id 再进入短事务，存在计算成本；这是避免持锁哈希的明确取舍，现有限流先于哈希生效，不增加未验证的预查优化。
- advisory lock 只约束走本注册路径的请求；直接 SQL 或未来新增注册路径必须复用同一机制。当前没有其他生产注册入口。
- 本任务无迁移、无数据库 Down。若需要回滚应用，不能直接让旧版本继续接收注册：旧代码不会创建个人空间，可能制造无空间用户；回滚期间须先停止注册流量，已提交的完整账号不做删除回滚。
- 若共享环境在本任务前已有用户，本任务会把关闭状态下的注册拒绝，但不会自动补其个人空间；这是开工前必须核对的既有数据边界，不在本计划擅自回填。

### 开放问题

**无。** 配置默认值、空库例外、并发语义、错误响应及任务范围均已由用户本轮决策确定。
