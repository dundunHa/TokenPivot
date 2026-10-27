# T-050：数据库接入与迁移工具初始化

> **规划完成，可供实施；本次不授权实施。** 用户已批准服务 pgxpool + goose 独立迁移、数据库启动必需、新增 `/readyz`、禁止随服务启动自动迁移。精确依赖及 PostgreSQL 版本仍是计划建议，需实施核实，不是用户单独确认的版本决定。
> 日期：2026-10-27。关联 T-050；主代理负责仅提交本文件及更新 TODO，本次不改代码、看板或其他设计材料。
> 本次只读仓库和官方固定版本资料，未安装依赖、未连接数据库、未执行迁移或测试；下文验收均为实施要求，不是已通过结论。

## 目标与边界

让 Go 服务具备明确的 PostgreSQL 连接生命周期，提供独立、固定版本、可重复执行的 SQL 迁移入口和本地配置说明。验收证明连接、探针、迁移记录及失败恢复流程可用，不证明业务已可用。

既定路线来自[架构](<../architecture.md#L23>)与[数据模型](<../data-model.md#L19>)：Go 单体、PostgreSQL 唯一核心存储；不引入 Redis、ORM、通用存储接口。业务表不使用外键、CHECK、触发器、RLS，本任务不建任何业务表。

任务分界：
- T-049 已有代码是本次基础，不重建骨架。其[旧计划](<2026-10-27-go-service-skeleton.md#L9>)中的“空仓库”“未实施”已过时，以实际代码为准。
- T-008、T-009 负责业务表、查询、索引、授权及事务；本任务不提前生成 sqlc、repository、TxRunner、种子账号或分区。
- T-051 负责统一构建/CI/容器运行入口；本任务只交付可直接执行的 Go 命令和本地数据库准备说明，不新增 Makefile、Compose、发布平台。
- 不修改已有未跟踪设计材料；后续实施如需同步这些材料，须由主代理另行协调其归属。

## 当前代码与拟修改范围

| 现有文件 | 已核实事实与实施改动 |
|---|---|
| [go.mod](<../../go.mod>) | 模块为 `github.com/dundunHa/TokenPivot`，Go 1.27.3，无第三方依赖；保留 Go 基线，加入实施核实后的精确依赖版本 |
| [config.go](<../../internal/config/config.go>) 与[测试](<../../internal/config/config_test.go>) | `Config` 只有 HTTPAddr；`Load()` 校验 HTTP 地址且不回显输入；增加服务数据库配置及独立迁移配置加载。完整配置打印实际在测试第 17、28 行，须改为只输出非敏感字段，并注入合成配置 |
| [main.go](<../../cmd/tokenpivot/main.go>) 与[测试](<../../cmd/tokenpivot/main_test.go>) | `run(logger)` 装配信号、HTTP、监听；增加 pgxpool 创建/Ping/关闭顺序；继续保留子进程允许列表环境，需 PG 的测试移入 integration |
| [server.go](<../../internal/server/server.go>) 与[测试](<../../internal/server/server_test.go>) | `NewHandler()` 只注册 healthz，`Run` 已有排空、强制关闭；只扩充 readiness 注入及用例，保留 Run 的现有责任 |
| [README](<../../README.md>) | 当前明确可无数据库启动、readyz 为 404；按已批准行为更新说明、配置示例、独立迁移命令和验收步骤 |

**拟新增路径**（不是现有文件）：
- `internal/database/database.go`、`database_test.go`、`database_integration_test.go`：服务 pgxpool 装配、连接配置解析、有限时 Ping、安全错误输出；不是存储抽象层。
- `cmd/tokenpivot/main_integration_test.go`：需真实数据库的服务子进程验收及仅用于测试的本地 TCP 转发器。
- `cmd/tokenpivot-migrate/main.go`、`main_test.go`、`main_integration_test.go`：独立迁移命令及测试，迁移进程在此装配 pgx stdlib/sql.DB，不增加 migration service 包。
- `db/migrations/embed.go`、`db/migrations/00001_bootstrap.sql`：内嵌 SQL 与第一条无业务结构的迁移。
- `go.sum`：由 Go 工具生成。集成测试文件采用 `integration` build tag；失败 SQL 使用测试内 `fstest.MapFS`，不进入正式迁移目录。

若实现时发现上述拟新增路径已有用户内容，停止覆盖并交主代理处理。

## 已批准方案与版本建议

### 驱动及连接 API

服务使用 `github.com/jackc/pgx/v5/pgxpool` 原生池；迁移命令因 goose Provider 接收 `*sql.DB`，使用 `database/sql` + pgx stdlib。**两个独立进程各自一个池，不构成服务双池；不为统一 API 改变服务驱动。** 服务二进制不装配 sql.DB，也不导入迁移执行入口。无需共同连接池接口或适配框架。[E1、E2、E3]

建议 pgx **v5.11.0**、goose **v3.29.0**、本地 PostgreSQL **18.6**；前轮研究记录了固定版本来源，Go 最低要求分别为 1.25.0、1.26.0，低于仓库 1.27.3。本轮补核 pgxpool 固定版本 API。依赖下载、本机编译和 PG 安装包可取得性仍须实施核实；不把 PostgreSQL 版本写成用户已确认事项，不静默替换不可取得的版本。[E1—E6]

### 迁移工具与简化理由

采用 goose Provider：SQL 文件默认事务，结构操作与迁移历史在同一事务提交；支持内嵌文件及显式 PostgreSQL 会话锁。不导入 goose CLI、多数据库驱动或 dotenv，不为本项目实现一套迁移调度器。[E3、E4]

前轮比较过 golang-migrate v4.20.1：支持独立 CLI 和 dirty 状态，但多语句事务需作者明确安排，失败后需核对实际数据库再处理 dirty/version。采用 goose 的逐文件事务不是声称另一工具不安全；选型已定，不重复提问。共享环境执行后更换工具需专门历史对照，不能直接让第二个工具重跑旧迁移。[E5]

不使用表锁租约/心跳方案；本项目直连 PostgreSQL，session advisory lock 已能解决迁移并发。不会因技能示例引入 sqlc、业务 SQL 自动重试或账号轮换框架。

## 配置与安全边界

沿用环境变量一次性读取，不自动加载 `.env`/YAML、不引入配置库。

| 配置 | 用途与规则 |
|---|---|
| `TOKENPIVOT_HTTP_ADDR` | 保留已有默认与校验 |
| `TOKENPIVOT_DATABASE_URL` | 服务必填、空值报错；要求 PostgreSQL URL，明确 host、database、user 和 sslmode；值只传给驱动，不日志输出 |
| `TOKENPIVOT_MIGRATION_DATABASE_URL` | 迁移命令必填；**不回退**到服务 URL，以免混用高权限账号 |

配置包提供 `Load() (Config, error)` 和拟新增 `LoadMigration() (MigrationConfig, error)`；后者不依赖 HTTP 配置。配置包只检查必填，数据库边界负责连接 URL 的公共规则及驱动解析，不在入口重复校验。服务拟用 `Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error)`，成功返回即已 Ping，失败负责关闭已创建池。迁移用 `pgx.ParseConfig`、`stdlib.OpenDB` 和 `PingContext`；公共 URL 规则和安全错误转换可在现有拟新增 database 包用小函数复用，但不抽象两种池的生命周期，不预建工厂接口。

连接策略是建议初始值，不是性能结论：
- 服务：`pgxpool.ParseConfig` 后设置 `MaxConns=4`、`MinConns=0`、`MinIdleConns=0`、`MaxConnLifetime=1h`、`MaxConnIdleTime=5min`、`ConnConfig.ConnectTimeout=5s`，其余池行为保留驱动默认。`MinConns` 不是 database/sql 的 MaxIdleConns，不机械照搬旧配置。
- 迁移：sql.DB `MaxOpenConns=1`、`MaxIdleConns=1`，连接配置 `ConnectTimeout=5s`；只用 SQL 迁移，不启用可能在锁连接之外借第二连接的 Go 迁移。**池大小 1 与锁/初始化共用连接的可行性尚未实测，必须按下文完整矩阵验证，不能根据源码印象宣布通过。**
- 启动创建/Ping 共用 5s context；readyz 每次 1s；迁移操作总 context 30s，迁移连接设置 `lock_timeout=5s`、`statement_timeout=15s`，保护元数据初始化及后续 SQL。
- pgxpool 获取连接取消后，后台建连仍可能继续；显式 ConnectTimeout 用于限制真实建连，不把 1s readiness 时限误当作底层连接已全部结束。`Pool.Close()` 无 context，整个清理耗时不承诺等于 Ping 时限。[E2]

超时常量集中在负责它的包，不新增一组没有实际调优需求的环境开关；T-008/T-009 的大表迁移按实际操作重新评估时限。

安全要求：
- 本地说明允许明确 `sslmode=disable`，仅用于回环本地库；远程部署说明要求 `verify-full` 和可信 CA，不静默降级 TLS。
- 文档说明 PG 环境/pgpass 等驱动机制的影响；示例显式填写非敏感连接字段，避免误连默认库。应用不扫描或输出环境，不读取密码文件内容。
- 本地服务账号与迁移 owner 分离：服务仅 CONNECT/USAGE（当前无业务表），迁移账号管理选定 schema；服务不是超级用户或 schema owner。业务表授权由 T-008/T-009 随表迁移确定，不预授所有未来表和迁移历史表写权限。
- README 给专用本地库、独立 schema 和两个角色的准备说明；密码交互设置或安全注入。创建角色/数据库不进入正式迁移，不读取开发者现有库。
- 日志只保留事件、阶段、迁移版本、成功/失败类别；可提取结构化 SQLSTATE，但不输出驱动原始 Error、Detail、Hint、SQL 或连接串。原始连接错误不得一路 `%w` 到 main 日志；数据库/迁移输出边界转换为安全错误，取消/超时可保留 `errors.Is` 判断。
- goose 自带日志显式适配到丢弃目标，命令自行记录允许字段；所有执行、解锁、sql.DB 关闭错误仍导致非零退出并记录安全阶段，不吞错。服务 `Pool.Close()` 无返回值，不能虚构关闭错误分支。
- 修复[配置测试:17](<../../internal/config/config_test.go#L17>)、[配置测试:28](<../../internal/config/config_test.go#L28>) 的整个 Config 打印，只输出 HTTPAddr 等非敏感字段及安全错误。每个相关配置测试显式设置合成数据库 URL；测试缺失值时显式清除对应变量后恢复，不读取/打印真实父环境值。驱动解析单测隔离 PG 环境和默认凭证文件影响，子进程继续只传允许列表环境；合成敏感标记必须覆盖错误及日志泄露断言。

## 服务生命周期与 HTTP 行为

数据流：环境 → config → database.Open（创建 pgxpool 并 Ping）→ handler 装配 → 同步 Listen → Run → HTTP 排空 → pool.Close。

1. 建立信号 context 后，在 HTTP 监听之前限时创建池并执行 `pool.Ping(ctx)`；`NewWithConfig` 返回不等于已连接，配置必须由 `pgxpool.ParseConfig` 创建。失败关闭已创建池，只记录一次安全错误并退出 1；不重试、不启动降级服务、不运行迁移。[E2]
2. 成功后把 `pool.Ping` 作为 `func(context.Context) error` 注入拟调整的 `server.NewHandler(checkReady func(context.Context) error) http.Handler`。这是 HTTP 层唯一新增依赖，不引入数据库接口体系。
3. 监听失败也关闭池。只有首次 Ping 和监听均成功才能记录 started。
4. SIGINT/SIGTERM 沿用 Run 排空/超时关闭流程。Run 返回后同步调用 `pool.Close()`，再决定 stopped 和退出码；Run 错误不因清理成功被覆盖。Close 等待借出连接归还且无返回值，本次仅 Ping，驱动会归还连接；不新建“超时后遗弃 Close goroutine”机制。请求 context 不直接继承停机信号，保留排空语义。关闭异常耗时需由集成测试记录，不承诺总停机固定上限。
5. 运行中数据库故障不影响 healthz、不主动退出进程；readyz 的真实 Ping 反映故障，恢复后重新 200。无自建后台探测、缓存或重连重试层，驱动正常连接管理不算业务 SQL 重试。

| 请求 | 行为 |
|---|---|
| GET/HEAD `/healthz` | 保留 200、文本类型、`ok\n` / HEAD 无正文；不查库 |
| GET `/readyz` | 在 1s 检查 context 内 Ping 成功：200 `ok\n`；失败/超时：503 `not ready\n` |
| HEAD `/readyz` | 同样检查与状态码，文本类型相同，无正文 |
| 不支持方法 | 标准库 405；不执行数据库检查 |
| 其他未知路径及 `/readyz/` | 404，不建立业务成功响应 |

readyz 不返回地址、账号、错误正文或版本，不逐次记录探针日志；不认证。**Ping 只证明连接可用，不证明迁移已最新或业务表齐全。** 发布顺序为独立迁移成功 → 启动/发布服务；T-051 不得用 readyz 替代迁移结果。本任务不加运行时 schema 版本兼容检查。

## 独立迁移入口

拟提供二进制 `tokenpivot-migrate`：只支持 `up`、`status`、`down-one`。缺少/未知参数先报安全用法并退出，不连接数据库；不支持 reset、force、自动修复、任意目录参数或自动建库。服务二进制不导入迁移执行代码。

- 独立环境变量连接迁移账号，Provider 使用 PostgreSQL dialect 和内嵌 SQL；内嵌解决运行目录变化和漏发 SQL，不是插件框架。
- `up` 使用 Provider.Up；无待执行项算成功并输出 no change。`status` 输出版本/待执行状态；goose 状态读取可能初始化历史表，不承诺绝对只读，首次使用同样需要迁移账号。
- `down-one` 使用 Provider.Down，只撤销一条；仅用于操作者确认的本地/测试库，生产优先前向修复。不给撤销全部入口，不将 down 当成自动失败处理。
- 显式 `WithSessionLocker(lock.NewPostgresSessionLocker(...))`，沿用固定锁 ID；采用 `WithLockTimeout(1, 1)`、`WithUnlockTimeout(1, 1)` 缩短工具锁轮询。只在锁争用时短暂等待，不重试 SQL。精确次数/耗时以测试为准，参数乘积不等于严格墙钟上限。
- 直连 PG，不经事务池化 PgBouncer，否则会话锁不成立。两个命令可先后完成或一个锁失败，但不能交错执行同一版本。测试使用不同 schema 也不能假定固定 advisory lock 相互独立。
- goose 解锁使用脱离取消的 context；网络故障下解锁/关闭总耗时不承诺严格 30s。保留服务端时限与有限锁轮询，测试取消/断连并记录真实耗时；若部署要求严格退出上限，另行明确方案，不以丢弃清理 goroutine 伪造成功。

### 无业务表 bootstrap

正式 `00001_bootstrap.sql` 使用 goose Up/Down 注解，两个方向仅 `SELECT 1;`，头部声明这是工具初始化标记，不是业务 schema。唯一新增持久结构是工具管理的 `goose_db_version`，不留下演示表。

执行 `status → up → up → down-one → up`，核对版本 0/1、第二次 up 无变化、重上成功，并查询选定 schema 的 catalog 确认无业务表或禁止约束。版本表由工具管理，不手工仿制。后续业务迁移从 00002 顺序编号；主代理协调编号，已在共享环境执行过的文件不可改写。

默认逐文件事务，不将整批包装成一个事务。事务内 SQL 失败应回滚该文件及版本记录，之前成功文件保留。本次不用 `NO TRANSACTION`、并发索引、数据回填；未来引入必须另说明部分成功恢复路径。

### 失败与恢复

- 配置/认证/网络/锁超时：非零退出、安全阶段日志；人工排除原因后再运行，不自动换账号或无限重试。
- SQL 失败：先核对 status 和实际结构，修正尚未发布文件或新增前向修复；不手工把版本改成成功。
- 提交时失联/进程被杀：结果可能不确定，恢复后检查历史和对象再决定重跑；没收到成功响应不等于事务未提交。
- bootstrap 的 down 不删除工具历史表；应用回退无需销毁数据库，不能据此承诺未来业务 down 无损。

## 实施顺序与执行切分

**单块，不拆。** 同一实施者拥有全部配置、DB/入口、HTTP、迁移编号、[go.mod](<../../go.mod>)/拟新增 go.sum 和 [README](<../../README.md>)；不并行修改共享文件，不建立共享基础层。

1. 主代理审阅并仅提交本计划，实施另行授权。实施前核对 Go 1.27.3、建议依赖和 PG 补丁版本可用；不可取得时记录实际阻塞，不静默换版本，也不重复询问已批准工具组合。
2. 加入核实的精确依赖，只导入服务 pgxpool、迁移 pgx stdlib、goose Provider/lock；生成 go.sum 并审查依赖。goose 模块列有多数据库依赖不等于需要导入 CLI 全家桶。
3. 完成配置、两种进程各自的连接装配、安全错误、有限时 Ping；接入服务生命周期及 readiness，修复配置测试敏感输出。
4. 增加内嵌 bootstrap、独立命令和参数检查；优先跑迁移单连接完整矩阵，若发现第二连接等待/初始化死锁，停止交付并定位具体调用，不盲目增大池或换服务驱动。
5. 补真实 PG 集成测试、测试隔离和 TCP 故障注入、本地准备说明；跑下列验收。主代理安排代码评审与 T-051 交接，不扩建 CI。

## 测试与可执行验收

以下是**拟实现后的命令**，本次未运行。README 给出从仓库根执行的完整步骤；先安全注入本地专用连接变量，不将真实 URL 写入命令参数或 shell 历史。

```sh
GOTOOLCHAIN=local go version
GOTOOLCHAIN=local go test ./...
GOTOOLCHAIN=local go test -race ./...
GOTOOLCHAIN=local go vet ./...
GOTOOLCHAIN=local go build -o /tmp/tokenpivot-t050 ./cmd/tokenpivot
GOTOOLCHAIN=local go build -o /tmp/tokenpivot-migrate-t050 ./cmd/tokenpivot-migrate

# 预先安全配置迁移账号 URL，指向本地手工验收 schema。
/tmp/tokenpivot-migrate-t050 status
/tmp/tokenpivot-migrate-t050 up
/tmp/tokenpivot-migrate-t050 up
/tmp/tokenpivot-migrate-t050 down-one
/tmp/tokenpivot-migrate-t050 up

# 预先配置服务账号 URL；另一终端只做 HTTP 检查。
TOKENPIVOT_HTTP_ADDR=127.0.0.1:18080 /tmp/tokenpivot-t050
curl --fail --silent --show-error http://127.0.0.1:18080/healthz
curl --fail --silent --show-error http://127.0.0.1:18080/readyz

# 结束上面的手工验收进程后，再独占运行集成测试。
GOTOOLCHAIN=local go test -tags=integration -count=1 -p 1 ./...
```

### 离线测试与环境隔离

- 普通测试不依赖 PG：配置缺失/空值/非法 URL、安全错误、命令参数、探针成功/失败/超时/HEAD/405/404、现有 HTTP 排空及失败路径。readiness 使用函数替身，无 mock 框架。
- 配置测试覆盖整个 Config 不再被打印，并全部使用合成数据库配置；不得从父环境取真实服务/迁移 URL。驱动解析测试同样使用隔离环境，避免 PG 环境及默认密码文件影响结果。
- [入口子进程测试](<../../cmd/tokenpivot/main_test.go#L51>)继续构造允许列表环境。原有成功启动/信号/监听占用中需 PG 的部分移入 integration，不能用缺数据库错误冒充监听占用通过；配置失败仍离线验证。

### 集成测试库与包间状态

- `TOKENPIVOT_TEST_DATABASE_URL` 指向操作者预建的本地专用测试库及迁移账号，`TOKENPIVOT_TEST_APP_DATABASE_URL` 指向同库服务账号。带 integration tag 缺变量必须失败，不静默 skip。只允许回环地址和明确测试库标识；记录实际 database/角色校验结果但不打印连接串，不自动删除数据库或用户对象。
- **跨 Go 包用 `-p 1` 串行；数据库用例不调用 `t.Parallel()`；禁止同时在该库跑另一套手工、集成或 race 验收。** 锁争用用例内部有意启动两个 Provider/进程是唯一需要的并发，不是允许另一套验收并行。
- 每包每次运行由迁移账号创建唯一随机 schema；本地准备仅给迁移测试账号在此专用库 CREATE 权限，不给服务账号 CREATE/owner/终止会话权限。新 schema 名冲突即换名，不复用现有 schema。为服务角色仅授 USAGE，两个测试 URL 的 `search_path` 都固定为本次 schema；迁移历史、失败 fixture 及 catalog 检查全部限定此 schema，不回退 public。
- “空库初始化”在测试中明确指**本包新建 schema 内无表/历史**，不是每包检查整库为空。上包留下历史、手工验收 schema 或失败运行残留都不能使后包空库检查失败；不扫描后清空已有对象。需要独立迁移状态的用例再分配本次新 schema，不能依赖包/用例执行顺序。
- 每次只操作本次创建对象，cleanup 先关闭 Provider/sql.DB/pgxpool/子进程，再清理本次 schema。清理前校验本次创建记录与精确 schema 标识；不删 public、不清空整库、不触碰用户对象；失败保留并报告非敏感 schema 名。SQL 标识符正确引用，不能拼接未验证外部名称。

### 必须通过的行为

- 服务：连接有效；错误凭证/不可达/超时非零且无合成敏感标记；缺库不启动 HTTP；监听占用正确失败并释放池；SIGINT/SIGTERM 排空后关闭连接，记录实际关闭耗时。使用实际 pgxpool，不以 sql.DB 替代服务测试。
- **迁移单连接矩阵**：使用真实 PG、实际 `MaxOpenConns=1`/`MaxIdleConns=1` 和 session locker，分别验证首次 `status` 初始化历史、直接 `up` 初始化历史、重复 up、down-one/up、操作取消、锁争用。检查各阶段没有等待第二连接而超时/死锁，锁和 SQL 在同一借出连接上完成；结合固定版本调用链及测试可见连接记录验证，不单靠成功退出或推测。记录结果、退出码及耗时；尚未执行前不得标通过。
- 失败事务：fixture 创建测试对象后故意报错，对象和该文件历史均回滚，先前成功版本保留。只在本次测试 schema 中使用测试 FS，不进入发布产物。
- 锁争用：两个独立 Provider/进程、各一个连接，在同一测试 schema 争同一 session 锁；通过受控持锁使另一个确定发生争用，验证不能交错执行同一版本、等待有限结束。取消/杀掉本次迁移子进程后，核对事务与锁已释放，后续迁移可成功。不得终止未知会话。
- **运行中断连与恢复**：由服务集成测试启动仅该测试服务使用的回环 TCP 转发器，服务 URL 指向其端口；迁移/断言连接直连测试 PG。先 readyz 200，再阻断转发：原子切换阻断状态、关闭全部已有双向连接、拒绝所有新连接，等待关闭完成后连续检查 healthz 200、readyz 503。仅杀一个 PG 会话不算验收，因为池能立刻重连。解除阻断后在测试截止时间内轮询至 readyz 200，进程始终存活；无服务或数据库账号权限扩大、无全局数据库停止。测试负责关闭转发器并等待转发 goroutine 退出；无需外部代理依赖或生产故障开关。
- 权限：服务角色不能建表、不能修改迁移历史；迁移角色能执行 bootstrap；选定 schema 中除工具历史外无业务对象。不能用管理员连接成功代替服务权限验收。
- 记录真实 Go/PG/依赖版本、命令退出码、未执行项；只跑离线测试不等于数据库验收通过。新增 Go 文件 gofmt 无差异。

## 依据

前轮研究覆盖驱动/API、迁移事务与并发、独立入口、PG 基线，比较两个迁移候选，深读 pgx 与 goose；未复制第三方代码。本轮只补查已选 pgx **v5.11.0** 的官方 `pgxpool/pool.go` 与 `pool_test.go`，问题限于创建/Ping、取消/建连、Close，来源已足够即停止，不重新选型或扩展社区调查。日期为抓取日期 **2026-10-27**。E1、E3—E6 延续前轮已记录研究，不声称本轮重新抓取；所有源码核实都不等于本仓运行验证。

| 编号 | 事实、采用/拒绝及原因 | 来源 | 核实状态 |
|---|---|---|---|
| E1 | 前轮发布接口返回 pgx v5.11.0（2026-09-07），Go 1.25.0，MIT、未归档，最近推送 2026-10-27；建议固定版本，不用无版本 latest | [发布接口](<https://api.github.com/repos/jackc/pgx/releases/latest>)、[固定 go.mod](<https://github.com/jackc/pgx/blob/v5.11.0/go.mod>)、[仓库元数据](<https://api.github.com/repos/jackc/pgx>) | 前轮一手核实；本机依赖下载/编译未验证 |
| E2 | `NewWithConfig(ctx context.Context, config *Config) (*Pool, error)` 要求 ParseConfig 创建配置；返回不保证连接。`Ping(ctx context.Context) error` 借连接并 defer Release；`Close()` 无返回值且等待借出连接归还。Acquire 取消后后台建连可能继续，未指定 ConnectTimeout 则源码设为 2min；采用显式 5s ConnectTimeout。析构连接源码使用独立 15s context，不承诺总体 Close 固定上限。服务原生 pgxpool，stdlib 只给迁移 | [固定池源码](<https://github.com/jackc/pgx/blob/v5.11.0/pgxpool/pool.go#L220-L340>)、[Close](<https://github.com/jackc/pgx/blob/v5.11.0/pgxpool/pool.go#L468-L475>)、[Ping](<https://github.com/jackc/pgx/blob/v5.11.0/pgxpool/pool.go#L848-L857>)、[官方测试](<https://github.com/jackc/pgx/blob/v5.11.0/pgxpool/pool_test.go>)、[stdlib](<https://github.com/jackc/pgx/blob/v5.11.0/stdlib/sql.go>) | 本轮核实池源码及 TestConnectConfig、TestIdempotentPoolClose、TestPoolAcquirePingTimeout；后者覆盖超时连接替换，不等同断网验收。stdlib OpenDB 为前轮核实，均未运行测试 |
| E3 | goose v3.29.0（2026-10-11），Go 1.26.0，依赖 pgx v5.11.0；Provider 接收 sql.DB 与 fs.FS，源码 MIT。未归档，最近推送 2026-10-25，同语言同 SQL 迁移形态，高相关 | [发布接口](<https://api.github.com/repos/pressly/goose/releases/latest>)、[go.mod](<https://github.com/pressly/goose/blob/v3.29.0/go.mod>)、[Provider/许可证目录](<https://github.com/pressly/goose/tree/v3.29.0>) | 前轮核实源码及元数据；license API 为 Other，LICENSE 正文为 MIT，以正文为准；本仓构建未验证 |
| E4 | SQL parser 默认 useTx=true，逐文件 SQL 与历史同事务；会话锁须显式启用，采用短轮询不叠加自制重试。上游 TestConcurrentProvider 不等同跨进程 PG 验收；单连接初始化/执行/取消/争锁必须实测 | [执行源码](<https://github.com/pressly/goose/blob/v3.29.0/provider_run.go>)、[并发测试](<https://github.com/pressly/goose/blob/v3.29.0/provider_run_test.go>)、[锁实现目录](<https://github.com/pressly/goose/tree/v3.29.0/lock>) | 前轮核实相关源码和 parser 默认值；池大小 1 完整矩阵、同连接与跨进程验证未完成 |
| E5 | 前轮 golang-migrate 发布接口返回 v4.20.1（2026-09-09）；官方说明多语句需事务，dirty 需核对后 force；已不选用，不采用自动 force | [发布接口](<https://api.github.com/repos/golang-migrate/migrate/releases/latest>)、[固定入门文档](<https://github.com/golang-migrate/migrate/blob/v4.20.1/GETTING_STARTED.md>) | 前轮核实一手说明，未深读实现/许可证；不声称完整审计 |
| E6 | 前轮官方版本表返回 PG 18.6，18 系列支持至 2030-11-14，官方建议当前 minor；仅建议作为本地基线，不代表用户已批准此精确版本 | [版本政策](<https://www.postgresql.org/support/versioning/>) | 前轮核实页面；本机安装包/镜像可取得性、实例运行均未验证 |

## 风险与未核实事项

- 无需用户重复作出选型决定，无实质规划阻塞；实施授权仍由主代理向用户取得，本计划不代表 T-050 已开始执行。
- 精确依赖可下载、Go 1.27.3 构建、PG 18.6 安装及全部集成测试均未验证，作为实施前置核实和交付验收，不冒充通过。若版本不可取得或固定 API 行为与实际不符，先报告具体阻塞再修订。
- 迁移池大小 1 的完整矩阵、解锁/关闭实际耗时尚未证明；不得靠扩大池掩盖错误或承诺任何网络故障都在 30s 内退出。pgxpool Close 也无可取消接口，必须正确归还连接、先排空 HTTP。
- readiness 只测连接；schema 兼容、业务 DDL/授权、大表迁移与数据恢复由 T-008/T-009 验收。
- 已去除无当前需求的设计：不建通用存储/连接池接口，不加配置/mock 库，不在服务双池装配，不启动时迁移，不加自动修复或可选数据库开关。保留独立命令、内嵌 SQL、会话锁分别解决误触迁移、漏发文件及并发执行；测试独立 schema 和 TCP 转发器分别解决包间状态污染及无法稳定复现断连的问题。
