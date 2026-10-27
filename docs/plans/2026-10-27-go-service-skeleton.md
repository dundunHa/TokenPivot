# T-049：Go 服务骨架实施计划

> 状态：规划完成，模块路径已由用户确认；未实施、未获得实施授权。
> 日期：2026-10-27（Asia/Shanghai）。
> 上位依据：[架构](../architecture.md)、[ADR-0001](../decisions/0001-go-monolith-postgresql.md)。TODO 状态及提交由主代理负责。

## 目标与边界

在空代码仓库中建立可运行、可测试、可停止的 Go HTTP 服务；不依赖数据库或前端即可启动。验收只证明进程及 HTTP 骨架工作，不代表 TokenPivot 业务已可用。

已核实根目录仅有 `.git/` 和 `docs/`，无 Go 模块、源码、测试或构建配置；本次查找没有已有计划。根与 docs 的 AGENTS.md 不存在由委派方确认。已有文档及其他未提交内容不修改。

不做：T-050 数据库驱动、连接、迁移和 readiness；T-031 的 web 工程、静态产物内嵌；T-051 的 Makefile、CI、Docker、lint 汇总。也不实现业务接口、认证授权、Cookie/Origin 防护、限流、请求 ID、指标、追踪、后台任务、Redis、队列或通用抽象框架。这些能力不得在交付说明中宣称已具备。

## 最小目录与模块

以下均为**拟新增**，不是现存文件：

```text
go.mod
cmd/tokenpivot/main.go
cmd/tokenpivot/main_test.go
internal/config/config.go
internal/config/config_test.go
internal/server/server.go
internal/server/server_test.go
README.md
```

- 根目录一个 Go module，入口 `cmd/tokenpivot`，不另建 backend 子工程。
- `main` 负责配置、日志、信号、监听器及退出码装配；配置包隔离环境读取和校验，server 包隔离路由与 HTTP 生命周期以便真实连接测试。暂不需要 app/domain/service/store/platform、接口层、依赖注入库或 middleware 框架。
- 零第三方依赖：`net/http`、`log/slog`、`os/signal`、`context`、`net`、`time` 及标准测试库即可满足需求；无需主动创建空 `go.sum`。数据库依赖留给 T-050。
- 模块路径已由用户提供的仓库地址 `https://github.com/dundunHa/TokenPivot.git` 确定为 `github.com/dundunHa/TokenPivot`（去除协议与 `.git` 后缀，保留大小写）。当前已是 Git 仓库；实施只初始化 Go module，不重新初始化 Git 仓库。
- Go 策略：本次官方发布 JSON 返回稳定版 `go1.27.3`，推荐实施基线及 `go` 指令均为 `1.27.3`，不写 `latest`、不添加多余 `toolchain` 指令。实施当天再核验该版本仍受支持且可取得；若需更改基线，由主代理更新本计划后实施，不静默换版本。
- 本机已核实 `go version go1.26.4 darwin/arm64`，与建议基线不同。实施者须先准备匹配工具链；本规划不安装、不自动下载或更改全局设置。验收采用 `GOTOOLCHAIN=local`，缺少基线时直接说明环境阻塞。
- README 写启动、配置、探针、退出语义、工具链要求及本任务非目标；配置说明直接放 README，避免另加仅一个配置项的示例文件。

## 配置及对外行为

### 配置

只加载一个环境变量 `TOKENPIVOT_HTTP_ADDR`，默认 `127.0.0.1:8080`；环境变量优先于默认值，显式空字符串属于错误而非回退。用 `os.LookupEnv`，不加载 `.env`、YAML 或命令行配置，不支持热加载。

配置结构只含 `HTTPAddr string`。启动时校验 host:port、数字端口 1–65535；支持主机名、IPv4、方括号 IPv6 和空 host（例如 `:8080`，表示用户明确选择监听所有接口）。错误只报告变量名和格式要求，不回显输入。地址解析及占用等实际网络错误归 `net.Listen` 处理，不重复预探测端口、不重试、不换端口。未知环境变量忽略，不枚举环境以“发现拼写错误”。

默认绑定回环地址降低意外对外暴露风险；容器部署可显式设置 `:8080`。这不是生产网络安全方案。

HTTP 初始超时为计划建议：ReadHeaderTimeout=5s、ReadTimeout=10s、WriteTimeout=10s、IdleTimeout=60s；关闭期限=10s。集中为 server 包常量，暂不增加五个无当前运维需求的环境开关。它们防止慢连接长期占用资源及关闭无限等待；不是业务超时承诺。后续真实部署或长响应需求出现时再局部配置化。

### 最小健康探针

使用专用 `http.NewServeMux()`，不使用全局 DefaultServeMux。

| 请求 | 结果 |
|---|---|
| `GET /healthz` | 200，`Content-Type: text/plain; charset=utf-8`，正文 `ok\n` |
| `HEAD /healthz` | 200、相同内容类型，无正文；沿用 ServeMux 的 GET/HEAD 语义 |
| `POST /healthz` 等不支持方法 | 405，采用标准库默认响应，不建立业务错误封装 |
| `/`、`/api/...`、`/readyz`、`/healthz/` | 404；没有假页面或业务成功响应 |

仅进程存活，不检查外部依赖，无需认证，不返回配置、版本、环境、堆栈或凭证信息。健康检查不写逐请求成功日志。此表即本阶段接口说明，不为单个运维探针引入 OpenAPI 生成工具。

## 启动、关闭与日志

建议保持直接函数调用：`config.Load() (Config, error)`、`server.NewHandler() http.Handler`、`server.Run(ctx context.Context, srv *http.Server, listener net.Listener, shutdownTimeout time.Duration) error`。后者负责 Serve/Shutdown 的协作，接受具体类型和关闭期限供测试使用；不预建生命周期接口。

1. 初始化标准库 slog JSON logger（stderr、Info），加载并校验配置，创建 handler/server；日志对象显式传递，不保存可变全局配置。
2. 主流程同步 `net.Listen`。成功后才记录启动成功及实际监听地址；失败一次记录并非零退出，不因启动 goroutine 隐藏绑定失败。
3. `signal.NotifyContext` 监听 SIGINT/SIGTERM，启动 Serve 并等待服务错误或取消。结果 channel 有缓冲，goroutine 归 Run 管理并在返回前回收。
4. 收到信号后，使用**新的**限时 context 调用 Shutdown，不能把已取消的信号 context 传给 Shutdown。主流程必须等待 Shutdown 完成，不能看到 ErrServerClosed 就先退出。
5. 正常信号关闭及预期 `http.ErrServerClosed` 视为成功，退出码 0。监听失败、非预期 Serve 错误、关闭超时或关闭错误返回错误，入口只记录一次并退出 1。
6. Shutdown 超时后调用 Close 强制释放连接并回收 Serve；报告关闭失败，不能打印正常停止。处理 Close 错误并保留原失败原因，不吞错。没有重试、重启或降级路径。正常完成才记录 stopped。

入口可用 `run() error`，让其 defer 先执行，再由 main 决定 `os.Exit(1)`。HTTP 请求 context 不直接继承用于停机的信号 context，避免收到信号立即取消本应排空的请求。

日志仅包含时间、级别、固定事件名、阶段、允许公开的监听地址与本地服务错误。配置错误不带输入；不序列化完整配置，不记录环境、请求头、Cookie、Authorization、URL 查询、请求体或响应体。net/http ErrorLog 接入同一日志输出；不增加自制 recover 中间件。绑定与关闭错误可保留操作系统原因，因为本阶段无密钥字段；未来加入数据库等外部依赖时，须在该依赖边界屏蔽连接串，不能把本任务的日志策略当成自动脱敏能力。

## 实施顺序

1. 获得用户单独的实施授权后，准备 Go 1.27.3；主代理按规则建立实施 worktree。模块路径已确认，使用 `github.com/dundunHa/TokenPivot`。确认实现工作区不覆盖已有未提交文档。
2. 创建根 go.mod 与上述最小源码文件；只用标准库，不建空目录或占位业务文件。
3. 实现单次配置加载、校验、路由和显式 HTTP 超时。
4. 实现同步监听、信号处理、Shutdown 等待和失败退出；补充配置、handler、生命周期及入口测试。
5. 写 README，执行下列验证，记录真实结果。T-051 可以复用这些命令，但本任务不建设汇总脚本。

## 验收与测试

所有命令为**未来实施验收**，本次未执行构建或测试。

```sh
GOTOOLCHAIN=local go version
GOTOOLCHAIN=local go test ./...
GOTOOLCHAIN=local go test -race ./...
GOTOOLCHAIN=local go vet ./...
GOTOOLCHAIN=local go build -o /tmp/tokenpivot-t049 ./cmd/tokenpivot
```

对新增 Go 文件运行 gofmt，并检查格式无差异。race 验收需要本机支持的编译工具链；缺少时报告未验收，不能声称通过。

手动冒烟（终端一）：

```sh
TOKENPIVOT_HTTP_ADDR=127.0.0.1:18080 /tmp/tokenpivot-t049
```

终端二执行 `curl --fail --show-error --silent http://127.0.0.1:18080/healthz`，应输出 `ok`；用 `curl -I` 验证 HEAD，用 `curl -i -X POST` 验证 405，用 `curl -i http://127.0.0.1:18080/readyz` 验证 404。终端一 Ctrl-C 应正常退出 0。不要使用 `go run` 的父进程信号行为作为二进制退出测试。

自动化验收必须覆盖：

- 配置：未设置取默认；合法 IPv4/IPv6/主机名/空 host；显式空值、缺端口、非数字及越界端口拒绝；错误中不出现故意注入的测试字符串。仅使用合成输入，不读真实环境秘密；使用 t.Setenv 的测试不并行。
- Handler：验证上表状态、正文和类型；HEAD 用真实 httptest HTTP 客户端确认无正文；未知路径不能得到健康成功响应。
- 生命周期：测试通过 `127.0.0.1:0` 获取临时端口，不放开用户配置的端口 0；可接收请求；取消后停止监听，返回 nil；用测试专用阻塞 handler 和 channel 确认进行中请求先结束再返回，而非依赖 sleep 猜时序。
- 超时：注入短关闭期限，保持一个活动请求，确认返回截止错误且连接被 Close 释放；释放测试 handler，避免测试泄漏 goroutine。
- 真实失败：先占用端口后启动子进程，必须非零退出且没有启动成功日志；错误配置也必须非零退出；关闭后不遗留监听器。
- 入口：main_test 采用标准库子进程测试方式运行同一入口流程，验证 SIGINT 和 SIGTERM 均退出 0，绑定失败退出 1，日志无合成敏感标记。仅针对当前 Unix 开发环境的信号语义；Windows 服务管理不属于本任务。
- 无 PostgreSQL、Node、SMTP 或私密配置时也能完成上述启动验收。

## 执行切分

**单块，不拆。** 同一实施者拥有 go.mod、入口、config/server 及测试、README；无需共享类型预建、依赖注入、生成代码或锁文件协作。顺序为模块与配置 → 服务生命周期 → 测试及说明。后续任务通过普通 Go 装配扩展，不在本任务预建插件入口。

## 风险、待确认与回退

- **已确认**：正式 Go 模块路径为 `github.com/dundunHa/TokenPivot`。目前没有待用户决定的规划阻塞项；实施仍需单独授权。
- Go 1.27.3 是本次查询结果；本机仅 1.26.4。匹配工具链的取得与实际编译尚未验证，实施前必须解决，不能用技能里的版本文字代替发布来源。
- HTTP 超时、监听默认值是本计划建议，不是用户既定部署参数。暴露公网前需独立完成 TLS/入口、认证与业务安全规划；`/healthz` 200 不代表依赖就绪。
- 没有数据库、数据写入或迁移，无数据回滚。本任务回退为撤销其独立实现提交，保留已有文档；主代理依项目流程操作，不以删除整个 docs 目录回退。

## 依据

有界查证三个问题：模块目录形状、当前 Go 版本、HTTP 关闭正确性。只深读一个 GitHub 参考：Go 自身示例测试与 Server 源码；不复制模板工程。web_fetch 因域名解析至非公网地址被拒，改用 curl 获取相同官方内容，没有扩大研究范围。日期均为抓取日期 2026-10-27 UTC。

| 事实与采用/拒绝 | 来源 | 验证状态 |
|---|---|---|
| 服务实现可放 internal、命令放 cmd；采用这两个边界，不据此创建整套分层 | [官方模块布局](https://go.dev/doc/modules/layout) | 已核实一手正文 |
| 发布接口返回 go1.27.3、stable=true；采用明确版本，不采用搜索摘要中的 1.27.1 或无版本 latest | [官方发布 JSON](https://go.dev/dl/?mode=json) | 已核实接口响应；未本地安装验证 |
| Shutdown 关闭监听并等待活动连接；Serve 先返回 ErrServerClosed，主流程仍须等待 Shutdown；采用等待，另加本项目有限期限 | [官方 Server 源码](https://github.com/golang/go/blob/master/src/net/http/server.go)、[官方示例测试](https://github.com/golang/go/blob/master/src/net/http/example_test.go) | 已核实 master 的注释、实现及 ExampleServer_Shutdown；未执行参考测试，未核对 1.27.3 标签源码 |
| golang/go 非 archived，pushed_at 为 2026-10-27T00:53:35Z，有官方示例测试，BSD-3-Clause；标准库同语言同服务边界，高相关，非模板依赖 | [仓库元数据](https://api.github.com/repos/golang/go)、[许可证](https://github.com/golang/go/blob/master/LICENSE) | 已核实；仅借鉴行为，不复制代码 |

版本兼容验证以本项目在固定 Go 基线下的编译与生命周期测试为最终验收，不把 master 示例视为目标版本测试已通过。没有性能、生产可用性或业务安全方面的外部证明。
