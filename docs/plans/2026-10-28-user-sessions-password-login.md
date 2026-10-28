# T-016：用户、直接注册、密码登录与滑动会话实施计划

> 状态：已完成规划；独立 doc-review 的三项必要修正已落实，可执行。实施仍需用户另行授权。
> 日期：2026-10-28。关联 T-016。规划分支：`main`。
> 本轮只编写本计划，不修改业务代码、迁移、ADR、PRD、OpenAPI 或 TODO 文件；没有运行构建、迁移或测试。

## 1. 用户已确认的决策

1. 初期不提供发邮件能力，允许直接注册账号密码，**不以邮箱验证作为登录前置**。
2. 基础注册后端并入 T-016；T-017 后续只处理注册开关与首次初始化策略。T-018 的 SMTP、邮箱验证和密码重置不在本任务。
3. 会话采用 **30 天滑动过期**：连续 30 天没有有效会话请求才过期；活跃请求可以续期，没有额外绝对有效期上限。
4. 上述第 1、2 项取代 [ADR-0004:11,17](../decisions/0004-auth-and-workspace.md#L11-L17) 和 [PRD:43](../prd.md#L43) 的“SMTP 验证且不免验证”旧口径。实施时必须同步这两份文档，不能同时保留互相矛盾的规则。

没有其他待用户拍板事项。

## 2. 目标与非目标

### 目标

- 新增 `users`、`sessions` 表及最小权限，完成邮箱账号直接注册、密码登录、登出和当前会话查询。
- 使用 Argon2id 哈希密码；浏览器会话使用高熵不透明令牌，数据库只保存令牌 SHA-256 摘要。
- 提供同源、主机范围会话 Cookie，以及所有浏览器写请求都可复用的严格 Origin 校验。
- 在请求 `context` 中提供可信用户身份，供 T-010 等浏览器管理 API 使用；插件认证继续独立使用 Bearer 凭证。
- 提供登录/注册限流、OpenAPI、单元测试、HTTP 测试和真实 PostgreSQL 集成测试。

### 非目标

- 不实现 SMTP、验证/重置邮件、`auth_actions`、密码修改或“撤销全部会话”。T-018 需要时另加字段和行为。
- 不实现注册开关、首次 owner 初始化、个人/团队空间、membership 或 owner 判断；分别归 T-017/T-019。
- 不实现登录注册页面或工作空间切换器，归 T-033；不做 UI 自动化。
- 不实现接入凭证管理 API 或采集 API，归 T-010/T-011；只交付它们需要的会话身份与 Origin 能力。
- 不引入 Redis、缓存、独立 worker、会话清理后台任务、JWT、跨源 Cookie、可信代理配置、通用存储层或通用幂等框架。
- 不预建 `email_verified_at`。初期没有读取方或写入方；未来 T-018 若恢复邮箱验证，再以新迁移增加并定义存量用户规则。

## 3. 已核实的仓库现状

| 事实 | 证据 |
|---|---|
| 当前只有健康探针和静态前端分发，`NewHandler(checkReady, frontend)` 尚无业务路由 | [internal/server/server.go:24-62](../../internal/server/server.go#L24-L62) |
| `main` 目前只加载配置、建池并装配上述 handler | [cmd/tokenpivot/main.go:27-56](../../cmd/tokenpivot/main.go#L27-L56) |
| 服务配置只有监听地址和数据库 URL，启动时一次读取 | [internal/config/config.go:12-40](../../internal/config/config.go#L12-L40) |
| 当前最新迁移为 00004，因此本任务以 **00005** 为目标编号；开工仍须重新核对并行分支 | `db/migrations/00004_usage_minute_stats.sql:1-6` |
| 数据模型已经约定 UUID v7、UTC `timestamptz`、无 FK/CHECK/触发器/RLS、应用校验引用及索引命名 | [docs/data-model.md:19-23](../data-model.md#L19-L23) |
| 既有凭证内核已经使用 `crypto/rand.Text` 和 SHA-256，只保存 32 字节摘要；会话可复用相同基础做法但不共用令牌类型 | [internal/credentials/service.go:79-109](../../internal/credentials/service.go#L79-L109) |
| 现有插件 OpenAPI 明确禁止 Cookie 回退，且文档目前自称只有采集端点，增加认证 API 时必须同步范围描述 | [docs/openapi/openapi.yaml:37-44](../openapi/openapi.yaml#L37-L44)、[:445-446](../openapi/openapi.yaml#L445-L446) |
| OpenAPI 已有 `bearerAuth`，尚无 Cookie 认证方案 | [docs/openapi/openapi.yaml:618-629](../openapi/openapi.yaml#L618-L629) |
| 当前 Go 为 1.26.4，尚无 `golang.org/x/crypto` 依赖 | [go.mod:3-8](../../go.mod#L3-L8) |
| 普通发布前检查为 `make check`，真实 PG 测试为显式 `make test-integration`，后者使用联合标签且缺变量会失败而非跳过 | [Makefile:61-86](../../Makefile#L61-L86)、[README:185-195](../../README.md#L185-L195) |
| 子进程测试使用独立环境，新增必填 Web origin 后必须同步 fixture，不能靠继承本机环境通过 | [cmd/tokenpivot/main_test.go:48-58](../../cmd/tokenpivot/main_test.go#L48-L58) |

## 4. 数据库迁移 `00005_auth_users_sessions.sql`

### 4.1 表与索引

沿用项目已有约定：对象名不加 schema 前缀；`id uuid PRIMARY KEY DEFAULT uuidv7()`；`created_at timestamptz NOT NULL DEFAULT now()`；不加 FK、CHECK、触发器或 RLS；不用 `IF [NOT] EXISTS`。

| 表 | 列 | 索引与含义 |
|---|---|---|
| `users` | `id`；`email text COLLATE "C" NOT NULL`；`password_hash text NOT NULL`；`created_at` | `uk_users_email UNIQUE (email)`。只保存规范化后的邮箱；密码列为 Argon2id 编码字符串，不是明文或快速摘要 |
| `sessions` | `id`；`user_id uuid NOT NULL`；`token_hash bytea NOT NULL`；`expires_at timestamptz NOT NULL`；`revoked_at timestamptz NULL`；`created_at` | `uk_sessions_token_hash UNIQUE (token_hash)` 支撑每次认证；`idx_sessions_user_id (user_id)` 保留用户维度撤销/查询所需的引用索引。`revoked_at IS NULL` 且 `expires_at > now()` 才有效 |

- `token_hash` 保存完整会话令牌的 SHA-256 原始 32 字节，不保存明文、hex 文本或额外 pepper。
- `expires_at` 无默认值，由登录/注册/续期 SQL 明确写入；所有过期判断与更新使用数据库 `now()`，避免应用与数据库时钟差影响判断。
- 不为 `expires_at` 预建清理索引：本任务没有清理任务，不为假想查询加索引。过期行即使暂存也不能通过认证。
- 表级 COMMENT 和必要列 COMMENT 使用英文；明确密码/会话令牌只存哈希，以及 `revoked_at` 的 NULL 含义。
- `GRANT SELECT, INSERT, UPDATE ON users, sessions TO tokenpivot_app;` 不授 DELETE、TRUNCATE、REFERENCES、TRIGGER。
- Down 依次 `DROP TABLE sessions; DROP TABLE users;`，只允许在确认过的本地/测试库使用；生产只做前向修复。

目标 DDL：

```sql
-- +goose Up
CREATE TABLE users (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    email text COLLATE "C" NOT NULL,
    password_hash text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uk_users_email ON users (email);
CREATE TABLE sessions (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    user_id uuid NOT NULL,
    token_hash bytea NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uk_sessions_token_hash ON sessions (token_hash);
CREATE INDEX idx_sessions_user_id ON sessions (user_id);
GRANT SELECT, INSERT, UPDATE ON users, sessions TO tokenpivot_app;
-- COMMENT ON ...：按上述说明补齐。
-- +goose Down
DROP TABLE sessions;
DROP TABLE users;
```

### 4.2 事务与唯一性

- 注册在**同一事务**内插入 user 和首个 session；任一步失败整体回滚，不出现“已注册但响应成功所需会话缺失”。
- 事务边界由 `internal/auth` service 通过窄 `TxRunner` 定义，PostgreSQL 实现负责开启/提交/回滚；service 不导入 pgx。
- 并发重复邮箱由 `uk_users_email` 仲裁，不用“先查再插”代替唯一约束。只有该唯一索引的 SQLSTATE `23505` 映射为 `ErrEmailUnavailable`；其他数据库错误原样进入安全错误映射。
- 登录是读取用户后新建独立会话，不需要事务。丢响应后重试可能创建多个有效会话，允许这一行为，不保存明文令牌做重放缓存。
- 登出按 `token_hash` 单条条件更新 `revoked_at = COALESCE(revoked_at, now())`；重复登出保留首次时间并返回相同 204。

## 5. 账号、密码与注册

### 5.1 输入规则

- 请求体上限 **16 KiB**；只接受 `application/json`、单个 JSON 对象、无尾随内容。拒绝未知字段、重复键、非法 UTF-8 和重复 JSON 键，避免认证字段出现多义解析。
- 邮箱：先 `TrimSpace`，再按 Unicode 简单小写映射 `strings.ToLower`，全项目统一把结果作为登录账号并只存此规范化值。总长 3–254 字节，恰好一个 `@`，两侧非空，不含控制字符或空白。这里定义的是产品账号格式，不声称实现完整 RFC 邮箱语法。
- 密码：合法 UTF-8，8–64 个 Unicode 码点且不超过 256 字节；不 trim、不做 Unicode 规范化，哈希的是用户原始密码字节。响应、日志和错误均不包含密码。
- 注册只接收 `email`、`password`，不接收用户 ID、角色、workspace、`email_verified_at` 或会话参数。

### 5.2 Argon2id

新增 `golang.org/x/crypto v0.58.0`（E1），使用 `argon2.IDKey` 和包内 `argon2.Version`（E2）。初始参数：

| 参数 | 值 |
|---|---|
| memory | **19456 KiB（19 MiB）** |
| iterations | **2** |
| parallelism | **1** |
| salt | 每个密码 16 字节，来自 `crypto/rand` |
| key length | 32 字节 |

该组合采用 OWASP 的最低推荐之一（E3），不是已经压测得出的本机最优值。实施时在 CI/目标开发机记录注册与登录哈希耗时；若需调整，只能通过明确的参数变更和验收完成，不在请求路径动态调参。

`password_hash` 使用标准 Argon2id 编码，包含算法、版本、参数、salt 和派生值。校验严格解析该编码并用恒定时间比较派生值；不自行实现 Argon2，不用 SHA-256 直接哈希密码，不加 pepper。

登录时，邮箱不存在与密码错误都执行一次同参数 Argon2id 校验：不存在时使用公开、固定、无对应真实密码的 dummy hash，统一返回 `401 INVALID_CREDENTIALS`。数据库故障不能伪装成凭证错误。

### 5.3 注册与重试

- 注册成功直接创建 30 天会话并返回 Cookie，避免要求刚注册的用户再次输入密码。
- 已占用邮箱返回 `409 EMAIL_UNAVAILABLE`；并发注册只有一个事务提交，另一个不创建会话。
- 直接注册无法同时承诺“注册成功自动登录”和“完全隐藏邮箱是否已注册”。本计划只保证登录不直接暴露用户是否存在；注册接口的占用结果仍可能被用于探测，因此注册和登录都必须限流，文档不宣称注册可完全防枚举。
- 注册不是幂等操作。成功响应丢失后用同邮箱重试会得到 409，客户端改用登录；不自动重放创建会话响应。

## 6. 会话、Cookie 与滑动续期

### 6.1 令牌与认证

- 明文格式：`tps_` + `crypto/rand.Text()`。Go 1.26.4 官方源码保证至少 128 位密码学随机性（E4）；解析不硬编码随机部分长度。
- 对完整明文计算 `sha256.Sum256`，只把 32 字节原始摘要写入 `sessions.token_hash`。插件 `tpk_` 与浏览器 `tps_` 不互相接受。
- 每次注册/登录生成全新令牌，不复用请求里已有 Cookie，避免会话固定。
- 认证按摘要定位 `revoked_at IS NULL AND expires_at > now()` 的会话，并通过 §6.3 的单条条件更新同时取得对应 user 与完成续期。格式错误、不存在、过期、已撤销统一为未认证；数据库错误单独返回，不回退匿名、Bearer 或其他身份。
- `internal/auth` 只把 `User{ID, Email}` 放入请求 `context`，使用私有 context key；handler 不读取请求体或路径里的用户 ID。

### 6.2 Cookie

Cookie 名固定 `tokenpivot_session`：

- `Path=/`；**不设置 Domain**，因此为主机范围；`HttpOnly`；`SameSite=Lax`。
- 可信 Web origin 为 `https` 时 `Secure=true`；仅允许明确回环开发 origin 使用 `http`，此时 `Secure=false`。生产非回环 HTTP origin 配置直接拒绝，不另加环境判断或第二套 Cookie 配置。
- 签发和实际续期时写 `Expires=<数据库返回的 expires_at>`、`Max-Age=<距 expires_at 的秒数>`。正常新会话为 2592000 秒。
- 登出无论会话是否存在，在 Origin 合法后都清 Cookie并返回 204；清除使用相同名称、Path 和 Secure 属性，`Max-Age=-1`、过期时间为 Unix epoch。数据库故障时不宣称已撤销，也不返回成功。

### 6.3 30 天滑动过期

- 新会话：`expires_at = now() + 30 天`，没有独立的绝对截止列。
- **每次有效会话认证都在同一条件更新中续期并返回用户身份**，确保从最后一次有效请求起完整保留 30 天；不设置刷新阈值。认证与续期使用一条 SQL，避免先读后更新之间与登出竞争：

```sql
UPDATE sessions
SET expires_at = now() + interval '30 days'
FROM users
WHERE sessions.token_hash = $1
  AND users.id = sessions.user_id
  AND sessions.revoked_at IS NULL
  AND sessions.expires_at > now()
RETURNING users.id, users.email, sessions.expires_at;
```

- 更新返回一行才算认证有效，并在该响应写入与数据库相同到期时间的 Cookie；返回 0 行统一为未认证。格式错误的令牌在执行 SQL 前拒绝。
- 并发续期由 PostgreSQL 行更新串行化，不需要应用锁，每个有效请求都取得自己的新到期时间。与登出竞争时，续期先提交则该在途请求可以完成，登出提交后的新请求必须失败；登出先提交则条件更新返回 0 行。
- 这会按用户确认的精确滑动语义产生每次受保护请求一次数据库写，不用限频续期换取更短的真实有效期，也不添加缓存。
- 不设置独立 `last_seen_at`：`expires_at` 已能表达会话有效性，当前没有展示最后活跃时间的需求。
- 不清理过期行、不启动 goroutine；未来出现真实存储清理需求再单独规划。

## 7. HTTP API、Origin 与限流

### 7.1 最小 API

路径均为 `/api/v1`，成功 JSON 使用 `user` 与 `expires_at`；时间格式 RFC 3339。错误统一为 `{ "error": { "code": "...", "message": "..." } }`，程序使用大写 `code`，message 不含敏感值。采集专用 `BatchError` 不强行复用为认证 schema，也不在本任务新增请求 ID 系统。

| 操作 | 方法与路径 | 准入 | 成功 | 主要失败 |
|---|---|---|---|---|
| 注册 | `POST /api/v1/auth/register` | 合法 Origin、合法 JSON、限流通过 | `201`，`{user, expires_at}` + 会话 Cookie | 400/409/413/415/429 |
| 登录 | `POST /api/v1/auth/login` | 合法 Origin、合法 JSON、限流通过 | `200`，同上 + 新会话 Cookie | 400/401/413/415/429 |
| 当前会话 | `GET /api/v1/auth/session` | 有效会话 Cookie；按 §6.3 续期 | `200`，同上 | 401 `UNAUTHENTICATED` |
| 登出 | `POST /api/v1/auth/logout` | 合法 Origin；有效会话则撤销 | `204`，空响应 + 清 Cookie | 403/5xx |

- 其他参数校验失败：`400 INVALID_ARGUMENT`；请求过大：`413 BODY_TOO_LARGE`；媒体类型错误：`415 UNSUPPORTED_MEDIA_TYPE`。
- **四个认证端点的所有成功与失败响应都设置 `Cache-Control: no-store`**，禁止浏览器或中间缓存复用会话响应绕过认证、撤销检查或续期；在中间件集中设置并逐端点验收。
- 限流：`429 RATE_LIMITED` 并提供 `Retry-After`；Origin：`403 PERMISSION_DENIED`；普通服务故障：`500 INTERNAL`，数据库不可用：`503 UNAVAILABLE`。
- `GET /api/v1/auth/session` 与所有受保护 API 共用会话认证及续期逻辑。无 Cookie、无效或过期均返回同一 401；可在明确无效时清 Cookie，数据库故障不得误清。
- OpenAPI 增加 `cookieAuth`，并在认证路径写清 Origin、Cookie 属性和错误；同时修改 `info` 中“只有一个采集端点”的过时描述。保留 `bearerAuth` 和采集路径的 Cookie 不回退规则。

### 7.2 可信 Origin

新增必填 `TOKENPIVOT_WEB_ORIGIN`：

- 只允许一个 HTTP(S) origin，不含用户信息、路径、查询、片段或尾斜杠；解析后规范化为小写 scheme/host，并省略协议默认端口。开发示例为 `http://127.0.0.1:5173`。
- `https` 可用于生产；`http` 仅允许 `localhost`、`127.0.0.1` 或 `::1` 回环地址。配置错误在启动时失败，错误不回显完整配置值。
- 写请求要求恰好一个 Origin 值，拒绝缺失、空白、`null`、多值列表和与规范化可信 origin 不相等的值。比较精确到 scheme、host、port；不从请求 `Host`、`Forwarded` 或 `X-Forwarded-Host` 建立信任。
- 提供 `internal/server` 可复用的 Origin 中间件，先校验 Origin 再执行会话和写操作。它只施加于浏览器写路由，不把插件 Bearer 端点改成 Cookie/Origin 认证。
- 提供可复用的会话中间件与 context 身份读取，供 T-010 管理路由使用；不在本任务实现 owner 判断。

### 7.3 单进程限流

使用 `internal/auth` 内带 `sync.Mutex` 的内存固定窗口计数器，时间函数可注入：

- 登录与注册共享两组尝试计数：每个规范化邮箱 **10 分钟 5 次**，每个远端 IP **10 分钟 20 次**；任一达到上限即拒绝后续尝试。
- 对通过格式校验的每次注册/登录，在进入密码哈希或数据库写入前，于**同一个互斥临界区**检查两个窗口并分别占用一次额度；检查与自增不可拆开，避免并发请求同时通过后才计数。
- 所有合法尝试都消耗额度，成功不返还、不清零，注册成功也不例外。达到上限的请求直接 429，不执行哈希或写库。
- 返回当前窗口剩余秒数作为 `Retry-After`。只在计数请求时惰性清理过期项，不创建后台 goroutine。
- IP 只取 `RemoteAddr`，不信任任何转发头。若入口是反向代理，IP 维度会看到代理地址；部署需要真实客户端 IP 时必须另做可信代理设计，不能默认信任 `X-Forwarded-For`。
- 这是单进程能力，多实例不会共享计数。当前按单体单进程验收；未来水平扩容若仍依赖这些阈值，须另行实现共享限流，不在本任务引入 Redis。

## 8. 文件范围与实施顺序

### 新增

- `db/migrations/00005_auth_users_sessions.sql`
- `cmd/tokenpivot-migrate/auth_tables_integration_test.go`
- `internal/auth/service.go`：User/Session、注册/登录/认证/登出、窄 Store 与 TxRunner、业务错误。
- `internal/auth/password.go`、`internal/auth/limiter.go`
- `internal/auth/postgres.go`：SQL、事务、唯一冲突与安全错误映射。
- `internal/auth/service_test.go`、`password_test.go`、`limiter_test.go`
- `internal/auth/postgres_integration_test.go`
- `internal/server/auth.go`：四个认证 handler、Cookie 与响应适配。
- `internal/server/auth_middleware.go`：Origin、会话认证/续期、私有 context 身份访问。
- `internal/server/auth_test.go`、`auth_middleware_test.go`

### 修改

- `go.mod`、`go.sum`：仅新增 `golang.org/x/crypto v0.58.0` 及其必要间接依赖。
- `internal/config/config.go`、`config_test.go`：Web origin 解析、校验及 Cookie Secure 派生。
- `internal/server/server.go`、`server_test.go`：注册认证路由，保留探针、前端和 `/api` 不回退行为。
- `cmd/tokenpivot/main.go`：装配 auth Postgres/service/限流/handler；main 不写业务规则。
- `cmd/tokenpivot/main_test.go`、`main_integration_test.go`：为子进程补充合法合成 origin，并增加配置/日志脱敏回归。
- `docs/openapi/openapi.yaml`、`docs/data-model.md`、`docs/decisions/0004-auth-and-workspace.md`、`docs/prd.md`、`README.md`、`.env.example`：按 §10 同步。

明确不改前端、采集/凭证业务包、其他任务计划；不手工编辑 `docs/todo.md`。

### 顺序

1. 开工核对 HEAD 的迁移编号、T-011 对 server/main/OpenAPI 的实际改动以及 T-019 是否已有 users 计划或代码；冲突由主代理先协调，不覆盖他人改动。
2. 先更新 OpenAPI 和 ADR/PRD 的认证口径，再实现迁移、密码、service/store。
3. 完成会话与滑动续期，再接 HTTP 中间件、handler、配置和 main 装配。
4. 完成单元/HTTP/真实 PG 测试和文档，运行 `make check` 与独占测试库的 `make test-integration`。
5. 交付后由主代理协调 T-019 的个人空间事务扩展和 T-010 管理 API 接入；未实际联调的能力不得宣称已经通过。

## 9. 测试与验收

### 9.1 迁移与 PostgreSQL

使用现有迁移测试辅助和真实 PG，新增测试固定到本迁移版本，不改写已发布迁移：

- 精确检查两表列、类型、NULL、默认值、索引顺序与唯一性；无 FK/CHECK/触发器/RLS；`tokenpivot_app` 恰有 SELECT/INSERT/UPDATE。
- 同规范化邮箱重复及并发插入报 23505；不同邮箱可插入；摘要列可保存 32 字节且重复摘要被拒绝。
- 有效/过期/已撤销会话认证；登出重复与并发均保留首次 `revoked_at`。
- 注册 user+session 同事务提交；模拟 session 插入失败时 user 也回滚。
- 每次有效认证都把到期时间更新为约 30 天后并返回用户；无效、过期、已撤销令牌不更新；并发续期不产生错误，过期或与登出竞争后不能再次续期。
- Down/Up 仅在专用测试 schema 验证；已有迁移矩阵和真实角色权限回归全部通过。

### 9.2 密码、service 与限流

- Argon2id 编码包含预期算法/版本/参数；相同密码不同 salt 的 hash 不同；正确密码通过，错误密码、畸形 hash 和非法 UTF-8 输入被拒绝。
- 登录不存在用户与错误密码返回同一业务错误，并各执行一次 dummy/真实校验；store 错误不映射成密码错误。
- 邮箱大小写/首尾空白规范化一致；密码不做 trim；长度边界、未知字段、重复键、尾随 JSON、请求体上限均覆盖。
- 注册并发只产生一个用户和首个会话；丢响应后的重试不恢复明文会话令牌。
- 限流验证邮箱/IP 两个维度、哈希前原子占用额度、并发不超限、成功与注册成功也计数且不返还、窗口边界、Retry-After 和 `go test -race`；不使用 sleep 等待时间。

### 9.3 HTTP 与安全

- 注册 201 并设置新会话；登录 200 换发全新令牌；当前会话返回用户和到期时间；登出 204 撤销并清 Cookie，重复登出仍 204。
- Cookie 不含 Domain，Path/HttpOnly/SameSite/Secure/Expires/Max-Age 正确；HTTP 仅回环开发可用，生产 HTTP 配置拒绝。
- Origin 缺失、错误、`null`、多值、scheme/host/port 变化均拒绝且数据库无写入；GET 当前会话不要求写操作 Origin。
- 每次有效请求都续期，Cookie 与数据库到期同步；连续 30 天无有效请求后 401；登出后旧 Cookie 401。
- 四个认证端点的成功与失败响应均有 `Cache-Control: no-store`，缓存响应不能替代新的会话认证。
- 插件请求即使同时携带有效 Cookie，没有有效 Bearer 仍按现有采集规则 401；Cookie 不能访问 owner 管理路由，owner 仍由 T-019 判断。
- 任何响应、日志和错误不含密码、明文会话令牌、token_hash、password_hash 或数据库 URL。
- 使用 `httptest` 和真实 PG；不要求 UI 自动化。

### 9.4 命令

```sh
make check
# 安全注入 TOKENPIVOT_TEST_DATABASE_URL 与 TOKENPIVOT_TEST_APP_DATABASE_URL，
# 独占专用 _test 库后执行：
make test-integration
```

新增依赖后另执行 `git diff --exit-code -- go.mod go.sum`，确认依赖变化只来自本任务。测试缺专用库或变量时报告集成验收未完成，不以 skip 代替通过。

## 10. 文档同步与后续任务边界

- `docs/data-model.md`：写入 00005 的实际 users/sessions 字段、索引和权限；移除初期无用途的 `email_verified_at` 草案字段；auth_actions 仍归 T-018。
- `docs/decisions/0004-auth-and-workspace.md`：记录 2026-10-28 用户新决策——初期直接注册且不要求邮箱验证、30 天滑动会话；保留未来 SMTP/注册开关需另行决定的表述。
- `docs/prd.md` §2：同步初期直接注册、无邮件依赖，避免仍宣称 SMTP 是首版前置。
- `docs/openapi/openapi.yaml`：认证 API、cookieAuth、错误、Origin、Cookie；更新文档范围，保留采集认证规则。
- `README.md`、`.env.example`：新增 `TOKENPIVOT_WEB_ORIGIN`、回环 HTTP/生产 HTTPS 规则、users/sessions 表和测试说明；不写真实凭证。
- T-017：只保留开放注册开关和首次初始化策略；不重复实现基础注册。TODO 文案由主代理在对应任务协调时更新。
- T-018：未来若实现验证/重置，新增 auth_actions 和必要用户字段，并明确存量未验证用户规则；本任务不预留令牌体系。
- T-019：复用 `users.id` 和已有 `workspaces.id`，只建 memberships 等自有表。个人空间创建需要接入注册事务时，修改本任务注册路径并保证 user/session/workspace/membership 的整体原子性，不另建用户体系。
- T-010：消费 request-scoped User、会话中间件、可信 Origin 和 Cookie 配置；不解析 Cookie、不信任 Host，owner 检查仍由 T-019 提供。
- T-033：调用四个 API；由前端处理 401、429 和注册占用错误，不在本任务实现页面。

## 11. 执行切分

**单块，不拆。** users/sessions、密码、事务、Cookie、会话中间件和 main 装配共享同一组窄接口，拆成并行块只会增加接口协调。

本块独占上述文件；主代理在派发前核对 T-011/T-019 与迁移编号。若 T-011 已修改 `internal/server/server.go`、`cmd/tokenpivot/main.go` 或 OpenAPI，先基于最新 HEAD 调整本块并保留其路由；不与另一个写子代理同时修改这些共享文件。

## 12. 风险与边界

- 直接注册必然存在通过注册结果探测邮箱占用的可能；限流只能降低滥用，文档和验收不得宣称完全防枚举。
- 精确的 30 天滑动会话要求每次受保护请求都执行认证并续期写库，这是用户选择带来的明确成本；不通过缓存或限频续期改变语义。数据库不可用时受保护 API 失败。
- 单进程限流和代理解析都有已写明边界；不能将多实例计数或伪造转发头场景宣称为已防护。
- 30 天无绝对上限意味着长期活跃的被盗会话可持续续期；登出可撤销当前会话。密码重置/全量撤销由 T-018 后续处理，本任务不暗自缩短用户选择的期限。
- 过期会话行暂不清理由是有意的范围控制，不是已完成的保留策略。
- T-016 独立交付时，注册用户尚未自动获得个人空间；完整产品注册→进入工作空间流程须与 T-019 联调，不能提前宣称端到端可用。

## 13. 外部依据（抓取日期 2026-10-28）

| 编号 | 事实 | 来源 | 状态 |
|---|---|---|---|
| E1 | `golang.org/x/crypto@latest` 为 **v0.58.0**，模块要求 Go 1.26.0，符合本项目 Go 1.26.4 | [Go module proxy](https://proxy.golang.org/golang.org/x/crypto/@v/v0.58.0.mod)；本机 `GOTOOLCHAIN=local go list -m -json golang.org/x/crypto@latest` 返回版本时间 2026-10-09T16:24:17Z | 已核实 |
| E2 | v0.58.0 提供 `argon2.IDKey`、Argon2 版本常量 `0x13`，memory 单位为 KiB | [golang/crypto v0.58.0 argon2.go:47-101](https://github.com/golang/crypto/blob/v0.58.0/argon2/argon2.go#L47-L101) | 已核实 |
| E3 | OWASP 推荐 Argon2id，最低组合之一为 m=19456（19 MiB）、t=2、p=1 | [OWASP Password Storage Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html#argon2id) | 已核实 |
| E4 | Go 1.26.4 `crypto/rand.Text` 适用于秘密/令牌，结果至少包含 128 位随机性，未来长度可变 | [Go 1.26.4 crypto/rand/text.go:9-21](https://github.com/golang/go/blob/go1.26.4/src/crypto/rand/text.go#L9-L21) | 已核实 |

未采用第三方项目架构或性能断言；Argon2 参数仍需在本项目实施验收时记录实际耗时。
