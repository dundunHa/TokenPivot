# T-059：bf 上 TokenPivot 单体 Docker 与独立 PostgreSQL 部署

> TODO ID：T-059
> 规划日期：2026-10-29；外部资料抓取日期：2026-10-28。
> 状态：**已批准实施与 BF 部署（2026-10-30），访问范围待澄清**。用户选择完成整个里程碑二后部署，授权严格限于 `tokenpivot-bf` 新资源的网络/卷/数据库创建、迁移、防火墙变更和应用启动；全部旧资源保持不动。用户已确认保留全部旧 PostgreSQL 资源，并批准仅本项目单机专用 Docker 网络内 `sslmode=disable`。用户对访问参数的回复为“这个不用限制”，唯一浏览器 Origin、实际绑定地址和来源范围仍须确认，不得据此默认开放公网。
> 本轮只写本计划，不写 Dockerfile、Compose、应用代码、部署文档或 TODO，不连接 bf，不改变 git 状态。主代理审阅后只提交本计划；实施须另行授权。

## 1. 已定决策与目标

### 已定

1. 将 TokenPivot 的 Go 单体和内嵌 React 页面部署到 bf 的 Docker；页面与 API 同源，应用使用 **HTTP 18080**，只在批准的局域网范围提供访问。
2. 不再比较 HTTPS、隧道或 nginx 转发方案；bf 上现有 nginx 不作为依赖。
3. 最新决定取代“先清理停止实例”：**全部旧容器、卷及其数据保留原状，不启动、不备份或处置其他项目资源**；为 TokenPivot 规划隔离的新 PostgreSQL 实例。新库实际创建仍需单独批准，旧资源清理不再是其前置条件。
4. 访问地址选用 bf 或其私网 IP，不引入专用主机名；用户已知悉并接受同一主机不同端口可能收到会话 Cookie 的风险。最终只能配置一个精确 Origin，具体地址与允许网段为 TODO(待确认)。
5. 仅本项目单机专用 Docker 网络内允许数据库 `sslmode=disable`，数据库不发布宿主端口，其他项目不加入此网络；其他远程数据库继续要求 `verify-full`。
6. 新实例使用独立持久卷，数据库角色分为 `tokenpivot_owner` 与 `tokenpivot_app`；数据库不发布到局域网。
7. 所有旧 PostgreSQL 容器及其卷、网络、镜像均不停止、不重建、不修改；8080/8081 的现有占用者也不动。
8. 沿用现有 `make build` 的“前端 → `uiembed` Go 服务 → 独立迁移二进制”顺序，不在服务启动时自动迁移，不新增代理、编排平台、监控体系或应用依赖。
9. 固定实施后的完整源码提交和镜像摘要；不自动 push、部署、回滚、删容器、删卷或清理 Docker 资源。

### 目标

- 提供可审查、可复现的 linux/amd64 应用镜像和一套仅用于 bf 的最小 Compose 配置。
- 用显式、默认关闭的应用配置允许非回环 HTTP，同时保留现有 Origin、会话和注册保护。
- 在隔离的新 PostgreSQL 18.6 实例中完成角色准备、独立迁移、应用启动和真实版本验收。
- 旧实例保持原状，不为部署 TokenPivot 读取其他项目数据；本项目任何结果未知的操作先查原执行，不重复触发。

### 非目标

- 不实现 T-055，也不介入其工作树；不修改 T-057/T-058 的计划。
- 不改业务 API、表结构、迁移内容、认证流程、前端页面或限流规则。
- 不建设 CI/CD、镜像仓库、自动重启策略、定时备份、HTTPS、反向代理、PgBouncer、Redis、指标或日志平台。
- 不升级、降级、迁移或“整理”其他项目的数据，不直接移动 Docker 管理目录。
- 不通过 `docker rm`、`docker compose rm/down`、`docker prune`、 `--rm` 或等价命令绕过 A2。

## 2. 已核实现状与证据边界

### 仓库

- [README.md:1-3](../../README.md#L1-L3) 明确服务启动需要数据库且不会自动迁移；[README.md:229-242](../../README.md#L229-L242) 明确内嵌前端运行时不需要 Node。
- [Makefile:40-47](../../Makefile#L40-L47) 已依次构建 `web/dist`、带 `uiembed` 的 `bin/tokenpivot` 和 `bin/tokenpivot-migrate`；[Makefile:62-87](../../Makefile#L62-L87) 将普通检查与真实数据库集成检查分开。
- [config.go:98-136](../../internal/config/config.go#L98-L136) 只接受一个可信 Origin，并在第 127–128 行拒绝非回环 HTTP；[config.go:93-96](../../internal/config/config.go#L93-L96) 以 scheme 决定 Cookie 的 `Secure`。
- [auth_middleware.go:83-95](../../internal/server/auth_middleware.go#L83-L95) 对浏览器写路由要求恰好一个精确匹配的 Origin；[auth_middleware.go:126-140](../../internal/server/auth_middleware.go#L126-L140) 已设置主机范围、`HttpOnly`、`SameSite=Lax` Cookie，HTTP 时不带 `Secure`。
- 当前根目录无 Dockerfile、Compose、`.dockerignore`，`docs/deploy.md` 不存在。本轮未创建它们。
- 主代理报告 main `4c1d602947e4c1d8f59d2b52833f4edeb6515652` 的 `make check` 九阶段已通过；本子代理只解析了该提交号，未重跑检查。该记录不包含数据库集成，也不覆盖本计划将新增的代码，**不能作为最终发布检查**。

### bf（来自另一 deploy 子代理本轮只读调查，本子代理未重复连接）

- hostname `DESKTOP-C28DFS7`；WSL2，Linux `6.18.33.2-microsoft-standard-WSL2`，x86_64，mirrored 网络。
- Docker 服务端 `29.1.3`；Compose `2.40.3+ds1-0ubuntu1~24.04.1`。
- 已有多套运行中的 postgres 17.9、16-alpine、18 镜像容器；这些是镜像/标签信息，不是其 `server_version` 证明。全部保持不动。
- 8080/8081 已占用；18080 必须在实际创建前重新核查，占用时停止部署，不自动换端口。
- Windows/Hyper-V 防火墙、局域网客户端可达性、路由器端口转发均未核实；nginx active 但无 80/443 监听，本方案不使用它。

## 3. 已确认的安全边界与部署前待确认参数

### Q1：全部旧 PostgreSQL 资源保留（已确认）

用户已选择保留全部旧容器和卷，继续规划新库。旧资源处置不进入本次执行范围，不构成新库的前置步骤；没有任何删除例外或其他项目备份授权。以下仅保留已有调查记录，不能作为后续操作名单。

现有清单为 27 个 PostgreSQL 镜像容器：13 running、11 exited、3 created；只有后 14 个进入候选清单，当前无需也不能停止它们：

| 状态 | 候选对象（仅清单，未批准处置） |
|---|---|
| exited | `artmind-t016-budget-pg-20261029`、`geo-eyes-t020-final-pg-20261028`、`t008-v2-test-pg-20261028`、`artmind-t008-postgres-20261027`、`t017-eval-daily-pg-dev-d050`、`t007-job1185-pg-20261026`、`t011-job-scope-check-20261025`、`geo-t007-test-pg`、`howtosay-t025-pg`、`geo-agent-autobind-itest2`、`t002-brand-pg` |
| created | `t003-pg`、`server-postgres-1`、`geo-agent-autobind-itest` |

- 另有 12 个卷与运行中容器已挂卷无交集，但这**不能证明数据无用**；本次不需要其他项目的处置矩阵，也不读取卷内数据。
- `artmind-t008-postgres-20261027` 与 `t011-job-scope-check-20261025` 无挂载，可写层是否含数据未核实；保留整个容器，不为本次部署导出或检查其数据。
- `server-postgres-1` 使用命名卷 `server_pgdata`，且绑定到已不存在的其他项目 worktree 脚本路径，不能启动；不得为“试试看”启动它。
- 状态及挂载关系仅作为保护基线；旧清单不授权停止任何容器。

### Q2：唯一浏览器地址与局域网范围

**用户选择仍使用 bf 或私网 IP，接受同主机 Cookie 的跨端口风险，不配置专用主机名。** 部署前仍须核实并确认：唯一访问地址（`http://bf:18080` 或一个确定的私网 IP 地址，二者只能选一）、Docker 私网绑定 IP、允许访问的局域网 CIDR（网段）及对应的 `TOKENPIVOT_WEB_ORIGIN`。以上实际值均为 TODO(待确认)，不得猜测，也不得同时接受两个 Origin。

不推荐直接使用与其他服务共用的 IP/主机名。Cookie 不区分端口：`tokenpivot_session` 的 `Path=/` 且无 Domain，会在浏览器访问同一主机名的其他端口时发送，bf 的 8080/8081 服务因此可能收到会话 Cookie。专用主机名是当前 HTTP 约束下避免跨端口暴露的最简单办法；若用户选择共用 IP/主机名，必须显式接受该风险，本计划不声称 Cookie 能按端口隔离。

### Q3：单机 Compose 内部连接是否允许 `sslmode=disable`

[README.md:80-87](../../README.md#L80-L87) 目前只允许回环连接使用 `sslmode=disable`，远程要求 `verify-full`。新 PG 与应用同机但没有 Unix socket 共享，应用会连接主机名 `db`，因此不能暗中套用回环例外。

**用户已批准：仅对本套 bf 单机、专用 Compose 网络使用 `sslmode=disable`，前提是没有数据库端口发布、其他项目不加入该网络、凭证不跨主机传输；README 与部署文档写明该窄例外。** Docker 主机管理员和同一 Compose 项目内被入侵的容器仍属于信任边界，内部明文不防止这些主体。其他远程数据库连接继续要求 `verify-full`；这项设计决定不授权实际创建数据库或部署。

## 4. 拟修改文件与职责

以下是**拟议路径**，除本计划外均未在本轮创建或修改。

| 路径 | 拟议改动与理由 |
|---|---|
| `Dockerfile`（新增） | 固定 Go/Node 构建镜像摘要；复用 `make build`；产出静态 linux/amd64 服务和迁移二进制；最终使用无 shell 的 `scratch`、数值非 root 用户和源码提交标签 |
| `.dockerignore`（新增） | 排除 `.git`、所有真实 `.env*`、`bin/`、`web/node_modules/`、`web/dist/`、备份和日志，避免密钥、旧产物及本机依赖进入构建上下文 |
| `compose.yaml`（新增） | 唯一项目 `tokenpivot-bf`；定义 `db`、显式调用的 `migrate`、`app`；固定镜像、独立网络/卷、唯一局域网端口绑定，不发布 PostgreSQL |
| [internal/config/config.go](../../internal/config/config.go) | 新增严格布尔配置 `TOKENPIVOT_ALLOW_INSECURE_HTTP`，默认 `false`；仅在该值为精确 `true` 时允许非回环 HTTP Origin |
| [internal/config/config_test.go](../../internal/config/config_test.go) | 覆盖默认拒绝、显式开关、非法值脱敏、其余 Origin 规则不变及 HTTP 的 `Secure=false` |
| [internal/server/auth_middleware_test.go](../../internal/server/auth_middleware_test.go) | 调整 Origin 解析调用；增加非回环 HTTP 下 Cookie 仍 `HttpOnly`、`SameSite=Lax`、`Secure=false` 的回归 |
| [internal/server/auth_test.go](../../internal/server/auth_test.go) | 仅随 Origin 解析签名调整，既有认证行为不变 |
| [.env.example](../../.env.example) | 记录新开关的默认值、严格取值与风险，不提供真实部署值或凭证 |
| [README.md](../../README.md) | 同步非回环 HTTP 开关、同源 Cookie 边界及经批准的单机 Compose 数据库连接例外 |
| [docs/openapi/openapi.yaml:1595](../openapi/openapi.yaml#L1595) | 只更新“非回环 HTTP 一律拒绝”的过时描述；不改变接口 schema、状态码或认证规则 |
| `docs/deploy.md`（新增，拟议） | 写实际执行前的占位值、审批点、命令、执行记录、验收和停止/恢复步骤；不写真实密码、URL 或令牌 |

明确不改 [Makefile](../../Makefile)、[go.mod](../../go.mod)、`go.sum`、前端包定义/锁文件、迁移 SQL、[auth_middleware.go](../../internal/server/auth_middleware.go) 的生产逻辑及其他任务计划。若最新 HEAD 已改变这些边界，停止并交主代理协调，不覆盖 T-055 或其他并行改动。

## 5. 应用 HTTP 支持：最小且默认不放宽

### 配置行为

1. 新增 `TOKENPIVOT_ALLOW_INSECURE_HTTP`：未设置和精确 `false` 都保持现有行为；只接受精确的 `true`/`false`，空值、大小写变体、空白、`1`、`yes` 等启动失败且不回显输入。
2. `config.Load` 先解析该开关，再调用调整为 `ParseOrigin(value string, allowInsecureHTTP bool)` 的现有解析逻辑。只有 `true` 跳过第 127–128 行的非回环 HTTP 拒绝；URL 的 scheme、host、port、路径、查询、用户信息和尾斜杠等其余校验不变。
3. 不给 `Config` 增加运行期无用字段，不新增第二套 Cookie 配置、环境探测、CORS 或 Host/Forwarded 信任。[Origin.Secure](../../internal/config/config.go#L93-L96) 继续只由 scheme 决定：HTTPS 为 `true`，获批准的 HTTP 为 `false`。
4. bf 的 Compose 显式写 `TOKENPIVOT_ALLOW_INSECURE_HTTP: "true"`；其他环境不写即关闭。不能用启动命令跳过 `config.Load`，也不能通过监听 `:18080` 隐式获得 HTTP 例外。

这个开关解决的是当前明确的局域网 HTTP 需求，而不是通用“关闭安全”选项。它不证明一个 Origin 位于局域网；实际范围由固定私网绑定、Windows/Hyper-V 防火墙和无路由器转发共同保证。

### Cookie、Origin 与跨站请求伪造（CSRF）边界

- 保留写请求恰好一个 Origin、与配置的 scheme/host/port 精确相等；错误、`null`、多值、缺失仍 403，不读取 `Host`、`Forwarded` 或 `X-Forwarded-Host` 建立信任。
- `SameSite=Lax` 不是完整 CSRF 防线，所有现有及后续浏览器写路由仍必须复用 Origin 中间件。采集 Bearer 接口不因 Cookie 或该开关改变认证。
- HTTP 下凭证、会话 Cookie 和页面内容在链路上可被窃听或篡改；Origin 校验、`HttpOnly` 和 Argon2id 都不能弥补这一点。获得会话者可持续滑动续期，网络攻击者还可修改 JavaScript 后冒充同源操作。
- `Secure=false` 是浏览器通过 HTTP 返回会话 Cookie 的必要结果，不把它描述成安全等价于 HTTPS。只在用户批准的局域网使用，不开放到公网。
- `TOKENPIVOT_REGISTRATION_OPEN` 保持显式 `false`：空库首个用户之后关闭注册，不因局域网部署开放注册。

## 6. 镜像与 Compose 设计

### 6.1 固定构建输入

Dockerfile 使用两个官方构建镜像的 **linux/amd64 镜像清单摘要**，不是可漂移的 `latest`，也不是仅写标签：

| 用途 | 固定引用 |
|---|---|
| Node 工具来源 | `node:26.11.1-bookworm@sha256:253729d3f2b77dbab6a5694aa20da4b484bc759bfb7004aa2e0b683c390e825e` |
| Go 构建器 | `golang:1.26.4-bookworm@sha256:5a94593d87a066df5abb02969be911524963f53908292aa5a1a6096fc019012a` |
| PostgreSQL | `postgres:18.6-bookworm@sha256:41a369edc91d9bdd7a58be6bc71ea0f8f7a3f49e82818a9f0829021ea7840b7c` |

构建器从 Node 官方镜像复制其 `/usr/local/` Node/npm 安装到 Go 构建器，再在同一干净构建阶段：

1. 先复制 `go.mod`/`go.sum`，执行 `GOTOOLCHAIN=local go mod download` 与 `go mod verify`。
2. 复制前端包定义/锁文件并执行 `npm --prefix web ci`。
3. 复制其余受 `.dockerignore` 过滤的源码，记录实际 `node`、`npm`、Go 版本。
4. 以 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64` 执行现有 `make build`，不复制其 recipe，不在 Dockerfile 另写一套前端/Go 构建顺序。

最终 `FROM scratch`，复制两个静态二进制，并从固定摘要的构建阶段复制 `/usr/share/zoneinfo` 与 `/etc/ssl/certs/ca-certificates.crt`，保留标准路径。构建时检查这些文件实际存在；缺失即停止，不以 UTC 或跳过证书验证兜底。需要私有 CA 时仅按批准路径只读挂载并配置可信证书，不将私钥或数据库凭证写入镜像。使用 `USER 65532:65532`，入口为 `/tokenpivot`；同一镜像通过覆盖入口执行 `/tokenpivot-migrate`。不为容器健康检查额外加入 shell、curl 或新的应用子命令，健康验收从宿主机和局域网客户端执行。

构建参数 `SOURCE_COMMIT` 必须是实施合入后的完整 40 位提交，写入开放容器倡议（OCI）的 `org.opencontainers.image.revision` 标签；本地镜像名为 `tokenpivot:<完整提交>`。构建前要求工作树干净、HEAD 与该值一致；构建后记录镜像 ID。不能拿上述基线提交冒充含本计划改动的最终提交。

### 6.2 Compose 资源与数据流

- 顶层项目名 `tokenpivot-bf`，独立网络 `tokenpivot-bf-backend`，独立命名卷 `tokenpivot-bf-pgdata-18`。不得复用旧项目网络或卷。
- `db`：固定 PostgreSQL 引用和 `platform: linux/amd64`；卷挂载到 **PG 18 官方要求的 `/var/lib/postgresql`**，不覆盖其默认 `PGDATA=/var/lib/postgresql/18/docker`；没有 `ports`。
- `db` 使用 `POSTGRES_DB=tokenpivot`、`POSTGRES_HOST_AUTH_METHOD=scram-sha-256`、`POSTGRES_INITDB_ARGS=--auth-host=scram-sha-256`，不设置任何 `trust`。超级用户密码只经官方 `POSTGRES_PASSWORD_FILE` 从宿主机受限文件挂载；Compose 的本地 secret 是文件挂载，不宣称它是加密密钥管理服务。
- `migrate`：使用同一应用镜像，只接收 `TOKENPIVOT_MIGRATION_DATABASE_URL`，入口 `/tokenpivot-migrate`、命令 `up`；置于显式 migration profile，普通 `up` 不得连带执行迁移。保留一次性容器供审计，禁止使用 `--rm`。
- `app`：只接收 `TOKENPIVOT_DATABASE_URL`、可信 Origin、`TOKENPIVOT_ALLOW_INSECURE_HTTP=true`、`TOKENPIVOT_REGISTRATION_OPEN=false` 与 `TOKENPIVOT_HTTP_ADDR=:18080`；不接收迁移账号。
- 两条数据库 URL 均明确 `host=db`、数据库 `tokenpivot`、`search_path=tokenpivot` 和经 Q3 批准的 sslmode；真实密码由本地凭证工具注入进程环境，不写仓库、命令参数、shell 历史或日志。不要运行会打印插值后密钥的 `docker compose config`，也不要输出完整容器环境。
- 应用唯一端口映射为 `<批准的bf私网IP>:18080:18080`，不使用 `0.0.0.0` 兜底；如果 mirrored WSL 下该绑定不能工作，停止并重新审查，不自动改绑全部接口。
- 不设置自动 restart，不运行不带服务名的 `docker compose up`，避免意外执行 migration profile 或重建资源。初始创建/启动分开执行；未来更新/重建不在本次授权内。

数据流：凭证工具 → Compose 进程环境/密码文件 → 独立迁移（仅 owner）→ PostgreSQL；浏览器 → 批准的私网 IP:18080 → 内嵌 Go 页面/API → app 角色 → PostgreSQL。迁移凭证不会进入 app 容器，数据库流量不跨主机。

## 7. 新 PostgreSQL 初始化、迁移与备份

### 7.1 显式创建后的角色准备

新容器健康只说明 PostgreSQL 可接受连接，不代表 TokenPivot 角色或迁移完成。经单独数据库初始化审批后，管理员在该实例内交互执行，不在命令参数或日志中写密码：

```sql
CREATE ROLE tokenpivot_owner LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE ROLE tokenpivot_app LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;
\password tokenpivot_owner
\password tokenpivot_app
REVOKE ALL ON DATABASE tokenpivot FROM PUBLIC;
GRANT CONNECT ON DATABASE tokenpivot TO tokenpivot_owner, tokenpivot_app;
\connect tokenpivot
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
CREATE SCHEMA tokenpivot AUTHORIZATION tokenpivot_owner;
GRANT USAGE ON SCHEMA tokenpivot TO tokenpivot_app;
```

不预授未来表权限；现有迁移逐表向固定角色 `tokenpivot_app` 授予 `SELECT/INSERT/UPDATE`。迁移账号不是超级用户，服务账号不是 schema/表 owner，不授予 DELETE、TRUNCATE 或迁移历史写权限。

### 7.2 单独迁移审批与执行记录

1. 确认生产库不是 `_test` 库，确认实际 `SELECT version()` 为 18.6、`tokenpivot_app` 存在、目标 schema 正确；不得用镜像标签代替这些检查。
2. 先执行普通发布检查和**独立专用测试库**上的 `make test-integration`，后者目标服务端也应为 18.6。生产库绝不承担集成测试；测试环境未准备好则发布停止。
3. 为迁移分配并记录唯一执行 ID 和一次性容器名；先安全注入 owner URL，再显式启动 `migrate`。不自动重试 SQL，不用服务启动替代迁移。
4. 成功后才核对迁移状态并创建/启动 app。若命令超时、执行 ID 丢失或提交结果未知，先查询原执行、该容器状态和数据库迁移历史/对象；结果未明前不得再次 `up`，不得 `status` 之后武断假定未提交。
5. 失败保持 app 未启动，按现有迁移规则检查实际状态和前向修复；不自动 `down-one`、reset、force、删库或删卷。

### 7.3 新实例备份与恢复

- 初次验收后用 owner 做自定义格式 `pg_dump`，保存到批准的仓库外受限备份目录；业务备份包含密码哈希和会话摘要，按敏感数据处理，不打印内容。记录 PG 版本、源码提交、迁移版本、时间和 SHA-256 校验值，并用 `pg_restore --list` 检查可读性。
- 角色创建步骤以无密码文档单独保留。恢复验证仅使用另行批准的隔离新实例/新卷和空库：先创建无密码记录对应的角色，由有权限的管理员恢复归档中的 schema、对象、所有权与授权；不重复预建应用 schema，不对已有对象使用覆盖或清理选项。恢复遇错停止；实际核对迁移版本、备份时记录的数据数量与代表性结果，以及 app 角色权限。`pg_restore --list` 仅证明归档可读，不能替代恢复验证。绝不覆盖唯一生产副本。
- 后续每次迁移前另行备份和审批，本计划不新增定时任务。回滚应用不等于回滚数据库；优先前向修复。

## 8. 旧资源保护边界

用户已经确认全部保留：14 个停止/未启动容器、12 个候选卷及 `server_pgdata` 均保持原状，13 个原运行容器也不停止、不修改。新项目不复用它们的卷、网络或容器。

仅为避免名称、挂载、端口冲突和确认未影响原服务检查必要的非敏感元数据，不读取、导出或备份其他项目数据，不启动旧实例。不得直接 `mv` Docker 的 containers/volumes 目录，不得用 Docker 删除命令或全局 prune 绕过 A2。未来若要清理，必须另行明确范围、保护和恢复方案与授权，不属于本计划。

## 9. 有序实施与停止条件

### A. 代码与镜像准备（仍需实施授权）

1. 主代理先协调 T-055 和共享文件，再在最新 HEAD 实施第 4 节；若 OpenAPI/README 正被其他任务修改，串行等待而不是覆盖。
2. 完成 HTTP 配置与测试、Dockerfile/Compose/部署文档；主代理只读 review。
3. 在最终提交实际运行 `make check`，并在独立专用 18.6 测试库运行 `make test-integration`。缺任一项即不可发布；旧提交的九阶段记录仅作历史参考。
4. 合入后记录完整源码提交，构建 linux/amd64 镜像并记录镜像 ID；不 push。

### B. bf 预检（只读，部署前重新执行）

1. 核对 Docker/Compose、项目名/网络/卷无冲突、18080 在 WSL 与 Windows 均未占用，8080/8081 原占用者保持。
2. 重新核对旧 PostgreSQL 的运行状态与挂载关系，仅记录必要非敏感元数据；发现与本项目冲突或原服务状态异常时停止协调，不处置旧资源。Q1、Q3 已确认，Q2 的实际地址/网段未确认时不进入变更步骤。
3. 核实选定的 bf 解析或私网 IP、允许 CIDR、Windows 与 Hyper-V 实际过滤层、路由器无 18080 转发。只检查拟用密码文件及备份目录的路径、权限与可用容量，不创建或修改任何文件、不读取/输出秘密内容。
4. 从最终 Compose 做不泄露秘密的静态审查，确认只有 app 有端口、db 无端口、 migrate 不在默认启动路径、所有摘要和源码提交固定。

### C. 变更步骤（每一步单独显式批准）

1. **变更准备**：在获明确批准后创建本项目受限密码文件和仓库外备份目录，凭证由工具安全注入；不输出内容。旧资源一律保留，不执行清理。
2. **创建新实例**：获单独批准后只创建本项目的网络、卷和 db 容器，再启动 db 并等待健康；不附带 app 或 migrate。
3. **初始化**：创建数据库角色、权限和 schema；验证 SCRAM 与实际角色权限。
4. **防火墙**：备份现有规则后，只新增/修改允许批准 CIDR 到指定私网 IP 的 TCP 18080 规则；不得禁用 Windows/Hyper-V 防火墙或开放任意来源。
5. **迁移**：按第 7.2 节执行；结果未知先查，不重触发。
6. **部署**：创建并启动 app；不自动重启、重建、回滚或清理旧资源。
7. **验收**：保存不含秘密的命令退出码、版本、镜像 ID、端口和客户端结果。任何一步失败都停在当前阶段。

### 失败与回滚

- 构建失败：不接触 bf 数据库或防火墙。
- 端口占用、地址绑定失败、主机名解析不符：不启动 app，不换端口、不改绑全部接口。
- 迁移失败：不启动 app；按实际历史处理，不自动 Down。
- app 启动或 readyz 失败：只检查本项目容器、配置和网络；不重建数据库。healthz 成功而 readyz 失败仍算未就绪。
- 需要撤回时，经批准只停止本项目 app，保留应用容器、数据库容器、卷、日志和备份。数据库已迁移时不承诺旧应用兼容，不自动回滚或删除数据。

## 10. 测试与验收标准

### 10.1 代码、镜像与配置

1. 新开关未设置/false 时，既有非回环 HTTP 拒绝全部通过；true 只放开该限制，非法 Origin 仍拒绝，非法开关值报错且脱敏。
2. HTTP 会话 Cookie 仍无 Domain、`Path=/`、`HttpOnly`、`SameSite=Lax`、`Secure=false`；HTTPS 行为与 Origin 写请求校验无回归。
3. 最终提交的 `make check` 九阶段和独立专用 18.6 测试库的 `make test-integration` 均实际通过并记录；未运行项明确标未完成。
4. Docker 构建内确实执行 `make build`，产物包含真实 React 页面、两个静态二进制、标准路径的时区数据与 CA；运行时不含 Node/npm、源码、`.env` 或凭证，服务与迁移都以非 root 用户运行。必须从最终镜像启动的服务验证非 UTC 时区与 `Australia/Lord_Howe` 的聚合边界，不能用宿主机测试代替。支持远程 `verify-full` 的能力用隔离 TLS 测试库验证可信证书成功、不可信证书失败，不因此给 bf 新库增加 TLS 或接触生产数据；缺少测试环境时记为未验证，不声称已通过。
5. Dockerfile/Compose 中的标签、摘要和完整源码提交与本计划/执行记录一致；无 `latest`，无自动 push。

### 10.2 实际 PostgreSQL 与迁移

1. 在生产实例执行 `SELECT version()` 和 `SHOW server_version`，实际为 PostgreSQL **18.6**；同时记录 `pg_isready` 与容器所用镜像摘要。标签不能替代该结果。
2. `current_user`、角色属性和权限查询证明 owner/app 分离，服务角色不能建表、改迁移历史或 DELETE；host 规则不使用 `trust`。
3. 迁移在 app 启动前成功，实际版本与最终源码迁移一致；app 容器没有 owner URL。不得为验收重复触发结果未知的迁移。
4. `docker inspect` 的选定非敏感字段和 `docker port` 证明 db 没有任何宿主机端口发布，卷为本项目新卷且挂载 `/var/lib/postgresql`；旧卷未被挂载。

### 10.3 HTTP、健康与只局域网

1. 从批准的局域网客户端访问唯一 Origin：页面及静态资源成功，`GET /healthz`、`GET /readyz` 均 200，HEAD/405/404 行为符合 [README.md:136-155](../../README.md#L136-L155)；页面不依赖运行时 Node。
2. 用无真实凭证的请求验证错误 Origin 仍为 403 且无业务写入。成功 Cookie 的部署观察只在用户进行首个真实注册时记录属性，不记录令牌；不为了测试擅造首个生产用户。
3. 端口/防火墙证据共同证明：Docker 仅绑定批准私网 IP:18080，Windows/Hyper-V 仅允许批准 CIDR，其他来源连接失败，路由器无 18080 转发。缺少非批准来源或路由器证据时，不得宣称“只局域网”验收完成。
4. 批准网段内另一台设备可访问；bf 上 8080/8081 与全部 13 个原 running PostgreSQL 容器保持运行，挂载/卷无变化。数据库无新 LAN 监听，不能用“本机 5432 总体不可达”掩盖其他既有服务状态。
5. 记录停止/失败恢复结果：停止本项目 app 后页面不可达而数据库和卷仍保留；恢复启动须经批准，不删除或重建任何资源。

## 11. 执行切分与共享文件责任

### 共享基础（保持在最小）

本计划已经固定以下接口供两个实现块使用：新环境变量名及语义、最终二进制路径 `/tokenpivot` 与 `/tokenpivot-migrate`、Compose 服务名 `db/migrate/app`、项目/网络/卷名和端口。**无新依赖、生成代码、迁移编号、package.json 或锁文件变更。**

### 块 A：HTTP 配置与现有文档

- 文件：`internal/config/config.go`、`internal/config/config_test.go`、`internal/server/auth_middleware_test.go`、`internal/server/auth_test.go`、`.env.example`、`README.md`、`docs/openapi/openapi.yaml`。
- 验收：第 10.1 的开关/Cookie/Origin 用例及最终 `make check`；数据库集成由主代理统一执行。
- 主代理须先等 T-055 对共享文件的改动合入或明确无冲突；块 A 独占上述文件，不与 T-055 并行写。

### 块 B：容器与部署文档

- 文件：`Dockerfile`、`.dockerignore`、`compose.yaml`、`docs/deploy.md`。
- 职责：dev 只写前三个容器文件；deploy 以“文档维护”模式先报告 `docs/deploy.md` 要点，取得明确写入批准后在独立工作区只写该文档，由主代理审阅并合入。
- 依赖：只依赖本计划已经固定的变量名和二进制名，可与块 A 并行；不得修改 Makefile、README、OpenAPI 或依赖文件。
- 验收：静态审查无秘密/默认迁移/DB 端口，linux/amd64 构建成功，镜像内实际运行 `make build`，最终检查由主代理在合并后重跑。

### 块 C：bf 执行

- 文件范围：无仓库文件；主代理在全部开放问题解决、A/B 合入、发布检查通过后，按第 9 节逐步取得执行批准，再派 deploy 以“部署执行”模式执行，不由文档任务自动切换。
- 主代理向 deploy 明确提供 bf、固定完整源码提交/镜像 ID、环境、目标、允许动作和批准边界、现有入口及版本/健康验收要求；源码上传只用干净固定版本，不从文档工作区上传。deploy 回报精确执行 ID、实际运行版本和健康证据，未知结果先查原执行。脚本缺陷交回 dev，迁移、回滚和资源变更不得越权。
- 主代理核对 Compose 实际动作、执行 ID 和验收记录。任何共享文件冲突由主代理串行处理，不在合并后重新生成锁文件。

## 12. 依据

外部查证只覆盖版本、官方镜像摘要和官方镜像初始化规则；这是标准多阶段构建与官方 PostgreSQL 镜像的直接组合，没有需要社区模板才能解决的方案分歧，因此未复制 GitHub 参考实现。以下“已核实”指官方文本/API 已取得，不代表本仓构建或 bf 拉取已执行。

| 编号 | 事实、采用/拒绝及原因 | 来源与日期 | 状态 |
|---|---|---|---|
| E1 | 官方版本表将 18 的 current minor 列为 **18.6**，支持至 2030-11-14；采用当前稳定 18.6，不根据环境日期猜测 19 或使用 latest | [PostgreSQL Versioning Policy](https://www.postgresql.org/support/versioning/)，抓取 2026-10-28 | 已核实；生产仍以 `SELECT version()` 复验 |
| E2 | Docker Hub 官方标签 API 返回 `18.6-bookworm`，last_updated `2026-10-28T07:10:59.869689Z`；多架构清单摘要为 `sha256:5c4783a8cc3f0074912164f94dfa1be4e043438f0f707cf968d7af9a0d31a2c8`，linux/amd64 清单摘要为 `sha256:41a369edc91d9bdd7a58be6bc71ea0f8f7a3f49e82818a9f0829021ea7840b7c`。固定后者匹配 bf | [官方标签 API](https://hub.docker.com/v2/repositories/library/postgres/tags/18.6-bookworm)，抓取 2026-10-28 | 已核实 API；实际拉取未验证 |
| E3 | PG 18 官方镜像默认 `PGDATA=/var/lib/postgresql/18/docker`，`VOLUME` 已移至 `/var/lib/postgresql`；采用新挂载点，拒绝沿用 17 及以下的 `/var/lib/postgresql/data` | [官方镜像文档](https://github.com/docker-library/docs/blob/master/postgres/README.md)、[18/bookworm Dockerfile](https://github.com/docker-library/postgres/blob/master/18/bookworm/Dockerfile)，抓取 2026-10-28 | 已核实文档与源码 |
| E4 | 官方镜像支持 `POSTGRES_PASSWORD_FILE`；host 连接默认 SCRAM，文档不推荐 trust；采用文件挂载超级用户密码和显式 SCRAM，不采用 trust/明文密码写入 Compose | [官方镜像 Docker Secrets 与认证说明](https://github.com/docker-library/docs/blob/master/postgres/README.md)，抓取 2026-10-28 | 已核实；角色密码仍交互设置 |
| E5 | `golang:1.26.4-bookworm` 的 linux/amd64 摘要为 `sha256:5a94593d87a066df5abb02969be911524963f53908292aa5a1a6096fc019012a`；与项目最低 Go 基线一致，不声称它是最新 Go | [Go 官方标签 API](https://hub.docker.com/v2/repositories/library/golang/tags/1.26.4-bookworm)，抓取 2026-10-28 | 已核实 API；构建未运行 |
| E6 | `node:26.11.1-bookworm` 的 linux/amd64 摘要为 `sha256:253729d3f2b77dbab6a5694aa20da4b484bc759bfb7004aa2e0b683c390e825e`；采用 Node 26 基线的已观测补丁，不声称它是最新 Node 或单独锁 npm | [Node 官方标签 API](https://hub.docker.com/v2/repositories/library/node/tags/26.11.1-bookworm)，抓取 2026-10-28 | 已核实 API；镜像内 npm 补丁待构建记录 |

## 13. 主要风险与未核实事项

- **当前不能实施**：Q1 保留旧资源、Q3 专用网络内部明文已确认；Q2 已选 bf/私网 IP，但唯一实际 Origin、绑定 IP 和允许网段仍为 TODO(待确认)。代码/镜像实现、新实例创建、初始化、迁移、防火墙和部署尚未获执行批准；旧资源处置已排除。
- bf 的只读事实来自交接报告，未由本子代理复核；执行前必须重新核对易变状态，但不因此重复做无目的调查。
- Windows/Hyper-V 防火墙、 mirrored 网络下指定私网 IP 的 Docker 端口绑定、局域网客户端、非批准来源隔离及路由器配置均未验证。
- 固定摘要的可拉取性、Node 与 Go 工具合并构建、scratch 中两个静态二进制、Compose migration profile 行为均未在本轮执行；必须通过实际构建和分阶段启动验收，失败不得换成 latest。
- 局域网 HTTP 会暴露明文凭据/会话及被篡改页面；共用主机名还存在 Cookie 跨端口发送风险。专用主机名不能解决链路明文，只解决 Cookie 主机隔离。
- 内部 PostgreSQL `sslmode=disable` 仅在用户已批准的本项目单机专用网络适用；其他远程连接不得因“同机”说法或部署便利而降级。
- 旧卷与运行卷无交集不代表数据无用；备份也不授予删除权。直接移动 Docker 管理目录可能损坏全部项目，明确禁止。
- 初次部署没有可直接恢复的旧 TokenPivot 应用/数据库状态；撤回只保证停止本项目 app 并保留数据，不承诺自动回到某个业务版本或自动清理新资源。
