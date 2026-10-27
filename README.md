# TokenPivot

Go HTTP 服务和独立 PostgreSQL 迁移入口。当前没有业务表或业务 API；数据库连接可用不代表业务已可用。服务启动必须连接数据库，**不会自动迁移**。

## 工具链与本地数据库

固定 Go **1.27.3**、pgx **v5.11.0**、goose **v3.29.0**；本地验收 PostgreSQL **18.6**。将独立安装的工具 `bin` 放入当前 shell 的 PATH，不修改全局配置，使用 `GOTOOLCHAIN=local`。

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

密码用 `\password` 交互设置，再由本地凭证管理工具安全注入；不要把真实密码/URL 写进仓库、日志、命令参数或 shell 历史。服务只有 CONNECT/USAGE，不是 schema owner，不能写迁移历史；业务表授权由后续迁移确定，不预授未来所有表权限。

## 配置与启动

| 环境变量 | 规则 |
|---|---|
| `TOKENPIVOT_HTTP_ADDR` | 默认 `127.0.0.1:8080`，显式空值非法；host:port，端口数字 1–65535 |
| `TOKENPIVOT_DATABASE_URL` | 服务必填，PostgreSQL URL，明确 host、database、user、sslmode |
| `TOKENPIVOT_MIGRATION_DATABASE_URL` | 独立迁移账号，必填；不会回退到服务 URL |

非敏感 URL 模板（不含密码，需安全注入凭证）：

```text
postgres://tokenpivot_app@127.0.0.1:5432/tokenpivot_local?sslmode=disable&search_path=tokenpivot
postgres://tokenpivot_owner@127.0.0.1:5432/tokenpivot_local?sslmode=disable&search_path=tokenpivot
```

`sslmode=disable` 仅用于明确的回环本地实例；远程须用 `verify-full` 和可信 CA，不静默降级 TLS。pgx 仍支持 PG 环境变量、pgpass/service 等默认机制；应用不读取或打印其内容。操作前隔离这些默认值，使用完整 URL 和明确 `search_path`，避免误连默认库；不要依赖 shell 中遗留的 PG 配置。

配置只加载一次，不读取 `.env`/YAML，不支持热加载。支持 IPv4、主机名、方括号 IPv6、空 host；`:8080` 表示明确监听所有接口，默认回环监听不是生产安全方案。地址占用失败，不自动换端口或重试。

```sh
GOTOOLCHAIN=local go version
GOTOOLCHAIN=local go build -o /tmp/tokenpivot-t050 ./cmd/tokenpivot
GOTOOLCHAIN=local go build -o /tmp/tokenpivot-migrate-t050 ./cmd/tokenpivot-migrate
# 先安全注入 TOKENPIVOT_MIGRATION_DATABASE_URL
/tmp/tokenpivot-migrate-t050 status
/tmp/tokenpivot-migrate-t050 up
/tmp/tokenpivot-migrate-t050 up
# 仅在确认的本地/测试库演练撤销一条
/tmp/tokenpivot-migrate-t050 down-one
/tmp/tokenpivot-migrate-t050 up
# 再安全注入最小权限的 TOKENPIVOT_DATABASE_URL
TOKENPIVOT_HTTP_ADDR=127.0.0.1:18080 /tmp/tokenpivot-t050
```

发布顺序：**独立迁移成功 → 启动/发布服务**。迁移命令只支持 `status`、`up`、`down-one`，SQL 内嵌，不依赖工作目录；首次 `status` 可能创建历史表，并非绝对只读。重复 `up` 成功并记录 `no change`。第一条 bootstrap 的 Up/Down 仅 `SELECT 1`，不是业务 schema；唯一持久结构为 goose 管理的 `goose_db_version`，down 不删除历史表。后续文件从 00002 编号，已在共享环境执行的文件不能改写。

迁移直连 PG，不可经过事务池化 PgBouncer。固定 session advisory lock 在同一数据库不同 schema 也可能争用；争用短暂等待后失败，不重试 SQL。逐文件事务保证失败文件的对象及历史一起回滚，之前成功文件保留。SQL 失败先核对状态和对象，再修复尚未发布文件或新增前向修复；不手工标记成功、不 reset/force。进程被杀或提交失联可能已提交，必须核对后再重跑。生产优先前向修复，不自动 down。

## 健康检查与生命周期

| 请求 | 响应 |
|---|---|
| GET/HEAD `/healthz` | 200，GET 为 `ok\n`，不查库 |
| GET/HEAD `/readyz` | 1s context 内真实 Ping；成功 200 `ok\n`，失败 503 `not ready\n` |
| 不支持方法 | 标准库 405，不检查数据库 |
| 其他路径，包括 `/readyz/`、`/healthz/` | 404 |

内容类型为 `text/plain; charset=utf-8`，HEAD 无正文。探针不认证，不回显配置、账号或数据库错误；不逐请求记日志。**readyz 仅证明连接可用，不检查迁移版本或业务表。** 运行中断库不影响 healthz、不退出服务；恢复后 readyz 可重新成功。

```sh
curl --fail --show-error --silent http://127.0.0.1:18080/healthz
curl --fail --show-error --silent http://127.0.0.1:18080/readyz
curl -I http://127.0.0.1:18080/readyz
curl -i -X POST http://127.0.0.1:18080/readyz
```

启动顺序为配置、创建 pgxpool 并 Ping、HTTP 监听。数据库不可用时不监听；监听失败也关闭池。SIGINT/SIGTERM：先 HTTP 排空，再同步关闭池，成功才记录 `stopped` 并退出 0。失败退出 1；关闭超时强制关闭 HTTP，不伪造正常停止。信号验收针对构建的二进制，不针对 `go run` 父进程。

服务池：最大 4、最小 0、最小空闲 0、连接寿命 1h、空闲寿命 5min。启动和底层连接超时 5s；readyz context 1s 不等于后台建连已停止。迁移池最大/空闲均 1，总操作 context 30s，服务端 lock_timeout 5s、statement_timeout 15s。会话解锁独立于取消 context；断网时清理及 pgxpool Close 不能承诺严格总时限。HTTP 请求头/读取/写入/空闲/关闭时限分别 5s/10s/10s/60s/10s。

stderr 输出 slog JSON；数据库错误只包含安全阶段/类别，不输出原始 Error/Detail/Hint、SQL、完整 URL 或配置。goose 内部日志丢弃，入口自行记录版本和结果；解锁/关闭失败仍是失败。

## 验证

不连接 PG 的检查：

```sh
GOTOOLCHAIN=local go test ./...
GOTOOLCHAIN=local go test -race ./...
GOTOOLCHAIN=local go vet ./...
GOTOOLCHAIN=local go build -o /tmp/tokenpivot-t050 ./cmd/tokenpivot
GOTOOLCHAIN=local go build -o /tmp/tokenpivot-migrate-t050 ./cmd/tokenpivot-migrate
gofmt -l cmd/tokenpivot/*.go cmd/tokenpivot-migrate/*.go internal/config/*.go internal/database/*.go internal/server/*.go db/migrations/*.go
```

`gofmt -l` 应无输出，race 需要本机 C 编译工具链。真实 PG 集成测试必须单独准备专用库（数据库名以 `_test` 结尾），可在专用实例复用上述两种角色；管理员执行：

```sql
CREATE DATABASE tokenpivot_test;
REVOKE ALL ON DATABASE tokenpivot_test FROM PUBLIC;
GRANT CONNECT, CREATE ON DATABASE tokenpivot_test TO tokenpivot_owner;
GRANT CONNECT ON DATABASE tokenpivot_test TO tokenpivot_app;
```

测试服务角色必须使用密码认证（推荐 `scram-sha-256`），不能命中 `trust`；在专用实例的 `pg_hba.conf` 中，将精确测试库/服务角色/回环地址规则放在通配规则之前，reload 后验证正确密码可连接。测试会用同一真实角色的错误密码断言 SQLSTATE `28P01`，并验证服务退出和日志脱敏。

安全注入 `TOKENPIVOT_TEST_DATABASE_URL`（迁移 owner）和 `TOKENPIVOT_TEST_APP_DATABASE_URL`（服务角色）。URL 必须明确回环 **IP**（例如 127.0.0.1）、同一 host/端口和 `_test` 数据库、不同用户及 sslmode；查询参数仅接受 sslmode/search_path。测试验证角色身份及权限，缺变量直接失败，不 skip。测试清除 PG 默认环境并隔离默认凭证文件；只使用明确注入的两个测试 URL。每个用例新建随机 schema、设置 search_path，记录非敏感归属，关闭本次连接/进程后仅清理本次 schema；失败保留并报告 schema 名，不删除数据库、不扫描清空旧对象。

先结束手工验收进程，独占运行；**禁止同库并行多套测试或手工迁移**：

```sh
GOTOOLCHAIN=local go test -tags=integration -count=1 -p 1 -v ./...
```

覆盖单连接初始化/重复迁移/撤销重上、同连接锁与 SQL、事务回滚、争锁/取消/杀进程恢复、最小权限、服务信号退出和真实 TCP 断连恢复。测试专用 TCP 转发器阻断服务连接，不停止数据库、不终止未知会话。Unix 子进程信号测试不覆盖 Windows 服务管理。

不含业务 API、业务表、前端、认证授权、TLS 入口、指标追踪、Redis/队列、Makefile、CI 或容器平台；公网部署前仍需独立安全设计。
