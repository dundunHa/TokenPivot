# TokenPivot

Go HTTP 服务和独立 PostgreSQL 迁移入口。迁移 00002 已建立四张基础业务表（`workspaces`、`projects`、`sources`、`ingest_credentials`），但还没有业务 API；数据库连接可用不代表业务已可用。服务启动必须连接数据库，**不会自动迁移**。 可选内嵌前端工程占位页仅在构建时需要 Node，运行不依赖 Node 或磁盘前端文件。

## 工具前提、首次安装与根命令入口

工具前提（不自动安装、不自动切换版本）：Go **1.26.4**（`gofmt` 必须来自同一安装；这是 `go.mod` 里的最低要求，新机器用 1.26.10 也满足，但不把 1.26.10 当作目标）、Node **26**、npm **随 Node 26 分发（本机 11.20.0）**、`make`、POSIX shell；`make check` 还要求 race 检测可用的本地 C 编译工具链。把这些工具的 `bin` 放进当前 shell 的 PATH；Go 命令统一由 Makefile 以 `GOTOOLCHAIN=local` 执行，版本不匹配就直接失败，不会下载另一套工具链，因此本机不再依赖模块缓存里的 1.27.3。

首次安装（在仓库根执行）。`npm ci` 要求已有锁文件、会替换 `web/node_modules`，且不改写 `web/package.json` / `web/package-lock.json`；锁文件与包定义不一致时它会失败，不要用 `npm install`、force 或 legacy-peer-deps 掩盖：

```sh
GOTOOLCHAIN=local go mod download
GOTOOLCHAIN=local go mod verify
npm --prefix web ci
```

根 [Makefile](Makefile) 只组合既有的 go/npm 命令，不安装依赖、不迁移、不加载 `.env`、不管理后台进程。默认目标 `make` 等同 `make help`，只打印清单：

| 命令 | 行为 | 输出 |
|---|---|---|
| `make help` | 打印命令清单 | 仅标准输出 |
| `make dev-go` | `GOTOOLCHAIN=local go run -mod=readonly ./cmd/tokenpivot`，继承当前进程环境，不迁移 | 前台进程 |
| `make dev-web` | `npm --prefix web run dev`，固定 `127.0.0.1:5173` | 前台进程 |
| `make build-go` | 分别构建两个命令，**不加** `uiembed`，不需要 Node 或 dist | `bin/tokenpivot-api`、`bin/tokenpivot-migrate` |
| `make build-web` | 先 `tsc --noEmit`，再 `vite build` | `web/dist/` |
| `make build` | 顺序执行 build-web，再构建 `-tags=uiembed` 的服务，再构建迁移二进制 | `bin/tokenpivot`（内嵌前端）、`bin/tokenpivot-migrate` |
| `make format-check` | `gofmt -l cmd internal db web/*.go` 列出文件即退出 1，再跑前端 `prettier --check` | 失败时列出文件，从不改写 |
| `make check` | 见下节「验证」 | 失败即非零 |
| `make test-integration` | 先 build-web，再跑 `uiembed,integration` 联合标签测试 | 需要专用 PG |

`bin/` 与 `web/dist/` 都是被忽略提交的构建产物，不随代码发布。组合目标用顺序 recipe/递归 `$(MAKE)` 实现，所以 `make -j build`、`make -j check` 仍然是先 web 后内嵌 Go。任一阶段非零即整体失败；此时可能留有旧的或部分的产物，不承诺原子发布，也不能把旧产物当作本次构建成功。带 `uiembed` 的编译在缺少真实 `web/dist` 时会直接失败，不会生成占位页。

`.env.example` 只是变量清单与安全占位示例，不是可直接启动的有效配置，也不会被自动加载；Go 程序与 Makefile 都只读进程环境。例外是 Vite：`make dev-web` 与 `make build-web` 会读取 `web/.env*`，并把 `VITE_` 前缀变量内联进浏览器产物，所以凭证不得写进 `web/.env*`。

## 工具链与本地数据库

Go 最低要求 **1.26.4**（新机器用 1.26.10 也满足，但不把它当作目标）；固定 pgx **v5.11.0**、goose **v3.29.0**；本地验收 PostgreSQL **18.4**。将独立安装的工具 `bin` 放入当前 shell 的 PATH，不修改全局配置，使用 `GOTOOLCHAIN=local`。

PG 18.4 这一项只有客户端/开发库证据（本机 `psql --version` 与 `pg_config --version` 均为 18.4），**未核实实际运行服务端**；本次只是改声明，不连接、不升级、不降级任何实例。迁移 00002 用 `uuidv7()` 作 id 默认值，因此**服务端最低版本是 PostgreSQL 18**（低于 18 时迁移失败）。版本口径来源见 [T-052 工具链版本基线计划](docs/plans/2026-10-27-toolchain-baseline.md)。

只在专用本地实例执行以下准备，不连接或改动已有开发数据库。管理员通过交互式 `psql` 执行，角色/数据库创建不属于迁移：

```sql
CREATE ROLE tokenpivot_owner LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE ROLE tokenpivot_app LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE DATABASE tokenpivot_local;
REVOKE ALL ON DATABASE tokenpivot_local FROM PUBLIC;
GRANT CONNECT ON DATABASE tokenpivot_local TO tokenpivot_owner, tokenpivot_app;
\connect tokenpivot_local
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
CREATE SCHEMA tokenpivot AUTHORIZATION tokenpivot_owner;
GRANT USAGE ON SCHEMA tokenpivot TO tokenpivot_app;
\password tokenpivot_owner
\password tokenpivot_app
```

密码用 `\password` 交互设置，再由本地凭证管理工具安全注入；不要把真实密码/URL 写进仓库、日志、命令参数或 shell 历史。服务只有 CONNECT/USAGE，不是 schema owner，不能写迁移历史。业务表权限不预授予未来所有表，而是随建表迁移逐条 `GRANT SELECT, INSERT, UPDATE` 给固定角色 `tokenpivot_app`（不授予 DELETE、TRUNCATE 等），所以**执行 `up` 之前这个角色必须已存在**；不存在时整个迁移文件回滚、命令以非零码退出。部署用的登录名如果不是 `tokenpivot_app`，就另建一个 NOLOGIN 的 `tokenpivot_app` 角色并对该表授权，再把 `tokenpivot_app` 授予实际登录角色，由登录角色继承权限；不要改迁移里的角色名。逐表授权清单以迁移文件和 [数据模型](docs/data-model.md) 为准，这里不重复列出。

## 配置与启动

| 环境变量 | 规则 |
|---|---|
| `TOKENPIVOT_HTTP_ADDR` | 默认 `127.0.0.1:8080`，显式空值非法；host:port，端口数字 1–65535 |
| `TOKENPIVOT_DATABASE_URL` | 服务必填，PostgreSQL URL，明确 host、database、user、sslmode |
| `TOKENPIVOT_MIGRATION_DATABASE_URL` | 独立迁移账号，必填；不会回退到服务 URL |

哪个进程需要哪些变量（各变量之间没有回退关系）：

| 进程 | 需要的变量 |
|---|---|
| Go 服务（`make dev-go`、`bin/tokenpivot-api`、`bin/tokenpivot`） | 服务连接 `TOKENPIVOT_DATABASE_URL`，可选监听地址 `TOKENPIVOT_HTTP_ADDR` |
| 独立迁移（`bin/tokenpivot-migrate`） | 只需 `TOKENPIVOT_MIGRATION_DATABASE_URL`；不要同时向服务进程注入迁移账号 |
| Vite 开发服务（`make dev-web`） | 只需可选的公开上游地址 `TOKENPIVOT_DEV_API_TARGET`，不接收任何连接信息 |
| 集成测试（`make test-integration`） | `TOKENPIVOT_TEST_DATABASE_URL` 与 `TOKENPIVOT_TEST_APP_DATABASE_URL` |

完整的变量清单与安全占位见 [.env.example](.env.example)。它是参考，不是可直接启动的配置，也不会被自动加载；变量必须由启动该进程的 shell 或本地凭证工具注入。Vite 侧的例外同上：它会读 `web/.env*` 并把 `VITE_` 前缀变量内联进产物，那里不得放任何凭证。

非敏感 URL 模板（不含密码，需安全注入凭证）：

```text
postgres://tokenpivot_app@127.0.0.1:5432/tokenpivot_local?sslmode=disable&search_path=tokenpivot
postgres://tokenpivot_owner@127.0.0.1:5432/tokenpivot_local?sslmode=disable&search_path=tokenpivot
```

`sslmode=disable` 仅用于明确的回环本地实例；远程须用 `verify-full` 和可信 CA，不静默降级 TLS。pgx 仍支持 PG 环境变量、pgpass/service 等默认机制；应用不读取或打印其内容。操作前隔离这些默认值，使用完整 URL 和明确 `search_path`，避免误连默认库；不要依赖 shell 中遗留的 PG 配置。

配置只加载一次，不读取 `.env`/YAML，不支持热加载。支持 IPv4、主机名、方括号 IPv6、空 host；`:8080` 表示明确监听所有接口，默认回环监听不是生产安全方案。地址占用失败，不自动换端口或重试。

最短双终端启动（**先独立迁移成功，再启动服务**）。迁移是一次性的前置步骤，之后两个终端分别跑 Go 与 Vite；每个会话只注入自己需要的变量，连接值不出现在命令参数里：

```sh
# 前置（一次性）：先 make build-go 生成 bin/tokenpivot-migrate，本会话只注入 TOKENPIVOT_MIGRATION_DATABASE_URL
# 仅对已准备的本地开发库执行；需要看当前版本时先跑 status（注意它可能创建历史表）
./bin/tokenpivot-migrate up
```

```sh
# 终端一：只注入 TOKENPIVOT_DATABASE_URL（最小权限的服务角色）
TOKENPIVOT_HTTP_ADDR=127.0.0.1:8080 make dev-go
```

```sh
# 终端二：只给 Vite 公开上游地址；首次运行前先 npm --prefix web ci
TOKENPIVOT_DEV_API_TARGET=http://127.0.0.1:8080 make dev-web
```

页面在 `http://127.0.0.1:5173`，两个探针直接访问 Go 的 `http://127.0.0.1:8080`。启动顺序是配置、可选前端初始化、建池并 Ping、监听——缺连接或连不上库时服务不监听；Go 未监听时 Vite 的 `/api` 请求失败，不会回退页面。

撤销演练是独立的人工步骤，不属于最短启动，也不在任何默认目标里。只有在确认为本地/测试库、并核对过当前版本后才执行：

```sh
# 先安全注入 TOKENPIVOT_MIGRATION_DATABASE_URL
./bin/tokenpivot-migrate status
./bin/tokenpivot-migrate down-one
./bin/tokenpivot-migrate up
```

发布顺序：**独立迁移成功 → 启动/发布服务**。迁移命令只支持 `status`、`up`、`down-one`，SQL 内嵌，不依赖工作目录；首次 `status` 可能创建历史表，并非绝对只读。重复 `up` 成功并记录 `no change`。第一条 bootstrap 的 Up/Down 仅 `SELECT 1`，不建业务对象；00002 起才有业务表（四张基础表），`goose_db_version` 始终由 goose 管理、down 不删除。后续文件从 00003 编号，已在共享环境执行的文件不能改写。

第一次在有业务表的库上执行 `up` 之前，按顺序自查（管理员在 `psql` 里执行）：

```sql
SHOW server_version;                                   -- 必须 >= 18：id 默认值用 uuidv7()
SELECT 1 FROM pg_roles WHERE rolname = 'tokenpivot_app'; -- 必须有结果：迁移向这个固定角色授权
SELECT to_regclass('workspaces'), to_regclass('projects'),
       to_regclass('sources'), to_regclass('ingest_credentials'); -- 00002 尚未执行时全为 NULL
```

**迁移命令失败时输出里没有原因**：错误细节（含 SQLSTATE）在入口被脱敏丢弃，只会看到 `up or unlock: database operation failed` 这类固定文本。所以排查只能靠上面的自查加数据库服务端日志——服务端低于 18（`uuidv7()` 不存在）、`tokenpivot_app` 不存在、对象已存在，这三种情况的提示完全相同。这个脱敏是有意设计，不是缺陷。

迁移直连 PG，不可经过事务池化 PgBouncer。固定 session advisory lock 在同一数据库不同 schema 也可能争用；争用短暂等待后失败，不重试 SQL。逐文件事务保证失败文件的对象及历史一起回滚，之前成功文件保留。SQL 失败先核对状态和对象，再修复尚未发布文件或新增前向修复；不手工标记成功、不 reset/force。进程被杀或提交失联可能已提交，必须核对后再重跑。生产优先前向修复，不自动 down。

## 健康检查与生命周期

| 请求 | 响应 |
|---|---|
| GET/HEAD `/healthz` | 200，GET 为 `ok\n`，不查库 |
| GET/HEAD `/readyz` | 1s context 内真实 Ping；成功 200 `ok\n`，失败 503 `not ready\n` |
| 不支持方法 | 标准库 405，不检查数据库 |
| `/api` 及子路径、`/readyz/`、`/healthz/` 及子路径 | 404，不回退 HTML |
| 其他路径 | 纯后端 404；启用前端时按静态服务规则处理，显式 HTML 无扩展名导航可回退入口 |

内容类型为 `text/plain; charset=utf-8`，HEAD 无正文。探针不认证，不回显配置、账号或数据库错误；不逐请求记日志。**readyz 仅证明连接可用，不检查迁移版本或业务表。** 运行中断库不影响 healthz、不退出服务；恢复后 readyz 可重新成功。

```sh
curl --fail --show-error --silent http://127.0.0.1:18080/healthz
curl --fail --show-error --silent http://127.0.0.1:18080/readyz
curl -I http://127.0.0.1:18080/readyz
curl -i -X POST http://127.0.0.1:18080/readyz
```

启动顺序为配置、可选前端初始化、创建 pgxpool 并 Ping、HTTP 监听。数据库不可用时不监听；监听失败也关闭池。SIGINT/SIGTERM：先 HTTP 排空，再同步关闭池，成功才记录 `stopped` 并退出 0。失败退出 1；关闭超时强制关闭 HTTP，不伪造正常停止。信号验收针对构建的二进制，不针对 `go run` 父进程。

服务池：最大 4、最小 0、最小空闲 0、连接寿命 1h、空闲寿命 5min。启动和底层连接超时 5s；readyz context 1s 不等于后台建连已停止。迁移池最大/空闲均 1，总操作 context 30s，服务端 lock_timeout 5s、statement_timeout 15s。会话解锁独立于取消 context；断网时清理及 pgxpool Close 不能承诺严格总时限。HTTP 请求头/读取/写入/空闲/关闭时限分别 5s/10s/10s/60s/10s。

stderr 输出 slog JSON；数据库错误只包含安全阶段/类别，不输出原始 Error/Detail/Hint、SQL、完整 URL 或配置。goose 内部日志丢弃，入口自行记录版本和结果；解锁/关闭失败仍是失败。

## 验证

普通检查不需要 PostgreSQL、连接变量、业务表或登录，用根入口一条命令：

```sh
make check
```

它按顺序执行：`make format-check`（Go `gofmt -l cmd internal db web/*.go` 有文件即失败、不改写，再跑前端 `prettier --check`）→ 普通 `go vet` → 前端 `eslint` → 普通 `go test -race -count=1` → 前端 `node:test` → `make build-go` → `make build`（含前端 `tsc --noEmit`）→ `uiembed` 的 `go test -race -count=1` → `uiembed` 的 `go vet`。任一阶段非零即整体失败，日志里的 `==> [n/9]` 标明执行阶段。构建已经包含前端类型检查，所以没有单独的 typecheck 步骤；需要单独跑时仍可用 `npm --prefix web run typecheck`。

Go 命令在 Makefile 中一律带 `GOTOOLCHAIN=local` 和 `-mod=readonly`。`-mod=readonly` 会在需要改 `go.mod` 时失败，但它不是“`go.sum` 绝不会变化”的承诺；干净环境请对照依赖文件：

```sh
git diff --exit-code -- go.mod go.sum web/package.json web/package-lock.json
```

`web/test/proxy.test.mjs` 会临时启动回环 Vite 与真实上游验证代理语义，不是 UI 自动化，也不是完整产品测试。race 需要本机 C 编译工具链。真实 PG 集成测试必须单独准备专用库（数据库名以 `_test` 结尾），可在专用实例复用上述两种角色；管理员执行：

```sql
CREATE DATABASE tokenpivot_test;
REVOKE ALL ON DATABASE tokenpivot_test FROM PUBLIC;
GRANT CONNECT, CREATE ON DATABASE tokenpivot_test TO tokenpivot_owner;
GRANT CONNECT ON DATABASE tokenpivot_test TO tokenpivot_app;
```

测试服务角色必须使用密码认证（推荐 `scram-sha-256`），不能命中 `trust`；在专用实例的 `pg_hba.conf` 中，将精确测试库/服务角色/回环地址规则放在通配规则之前，reload 后验证正确密码可连接。测试会用同一真实角色的错误密码断言 SQLSTATE `28P01`，并验证服务退出和日志脱敏。服务测试账号还必须就是 `tokenpivot_app`，或者无需 `SET ROLE` 就能直接使用它的权限（例如把 `tokenpivot_app` 授予该账号）；每轮集成测试开头都会用 `pg_has_role` 检查这一点，不满足就以 `test service role must have tokenpivot_app privileges` 失败，因为迁移只向这个固定角色授权。

安全注入 `TOKENPIVOT_TEST_DATABASE_URL`（迁移 owner）和 `TOKENPIVOT_TEST_APP_DATABASE_URL`（服务角色）。URL 必须明确回环 **IP**（例如 127.0.0.1）、同一 host/端口和 `_test` 数据库、不同用户及 sslmode；查询参数仅接受 sslmode/search_path。测试验证角色身份及权限，缺变量直接失败，不 skip。测试清除 PG 默认环境并隔离默认凭证文件；只使用明确注入的两个测试 URL。每个用例新建随机 `t050_` 前缀 schema、设置 search_path，记录非敏感归属，关闭本次连接/进程后仅清理本次 schema。清理**通常仍会执行**；只有在归属检查失败、清理本身失败等情形才会保留该 schema、报出 schema 名并使测试失败，供人工核对——这不是“失败一律保留”，也不代表测试从不清理。不删除数据库、不扫描清空旧对象。

先结束手工验收进程，独占运行；**禁止同库并行多套测试或手工迁移**：

```sh
make test-integration
```

该目标先构建前端（联合标签需要真实 `web/dist`），再执行 `GOTOOLCHAIN=local go test -mod=readonly -tags=uiembed,integration -count=1 -p 1 -v ./...`。`-count=1` 避免测试缓存掩盖本次库状态，`-p 1` 保持包级串行；它是有数据库写入的显式操作，**不包含在 `make check` 里**。缺变量时会看到 `explicit integration database variables required` 并非零，此刻不会接触数据库。

覆盖单连接初始化/重复迁移/撤销重上、同连接锁与 SQL、事务回滚、争锁/取消/杀进程恢复、最小权限、服务信号退出和真实 TCP 断连恢复，以及 00002 四张基础表的列与默认值、索引、无外键/CHECK/触发器/RLS、`tokenpivot_app` 只有 SELECT/INSERT/UPDATE 权限、唯一性范围（含归档后 key 仍被占用、大小写按字节判等）、同 source 多凭证轮换与撤销、`UpTo(2)`/`DownTo(1)` 回退重上。测试专用 TCP 转发器阻断服务连接，不停止数据库、不终止未知会话。Unix 子进程信号测试不覆盖 Windows 服务管理。


## 前端开发与内嵌构建

前端工程位于 [web](web/README.md)，使用 Node **26** 与随 Node 26 分发的 npm（本机 11.20.0），先确认版本。开发时先完成上述数据库准备、独立迁移并安全注入 `TOKENPIVOT_DATABASE_URL`，再按前面的最短双终端启动分别运行 `make dev-go` 与 `make dev-web`（Vite 那端用 `TOKENPIVOT_DEV_API_TARGET` 指定上游地址）。

页面为 `http://127.0.0.1:5173`，仅 `/api` 及其子路径经 Vite 转发到 `http://127.0.0.1:8080`；开发覆盖变量 `TOKENPIVOT_DEV_API_TARGET` 只在 Vite 服务端读取。`/healthz` 与 `/readyz` 直接访问 Go。代理保留 Host/Origin/Cookie，不代表认证或 Origin 校验已经实现。

无 `uiembed` 标签时保持纯后端行为，缺少前端产物不影响 Go 测试或构建。发布内嵌页面用 `make build`（先 web 后内嵌 Go），等价的手工序列：

```sh
npm --prefix web ci
npm --prefix web run build
GOTOOLCHAIN=local go test -tags uiembed ./...
GOTOOLCHAIN=local go test -race -count=1 -tags uiembed ./...
GOTOOLCHAIN=local go vet -tags uiembed ./...
GOTOOLCHAIN=local go build -mod=readonly -tags uiembed -o /tmp/tokenpivot-t031-ui ./cmd/tokenpivot
# 先独立迁移并安全注入 TOKENPIVOT_DATABASE_URL
TOKENPIVOT_HTTP_ADDR=127.0.0.1:18080 /tmp/tokenpivot-t031-ui
```

带标签缺少 dist 时编译失败；缺少可读普通 index.html 时初始化失败，不会启动成假成功。构建后可将二进制放到不含仓库的目录运行。页面与 API 必须同源，生产 TLS 由外层入口提供。所有静态成功响应 `Cache-Control: no-cache`；不存在的资源不返回 HTML，只有显式接受 HTML 的无扩展名导航可回退入口。带哈希资源不承诺升级后旧页面永远可用。

当前除迁移 00002 的四张基础表外没有其他业务表，也没有业务 API、认证授权、TLS、请求 ID、指标追踪、后台任务、Redis/队列；占位页不是产品视觉或业务页面。根 [Makefile](Makefile) 只是薄命令入口，不建设 CI、Docker、统一 lint 平台或热重载/后台进程管理。公网部署前须独立完成入口与业务安全设计。

前端和数据库联合验收：完成前端构建并安全注入上述两个测试 URL 后，独占测试库执行 `make test-integration`（即 `GOTOOLCHAIN=local go test -mod=readonly -tags=uiembed,integration -count=1 -p 1 -v ./...`）；不能用分别编译两个标签代替联合验收。
