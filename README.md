# TokenPivot

当前提供 Go HTTP 服务骨架与可选前端工程占位页，不代表业务已可用。纯后端仅依赖 Go 标准库，无需 PostgreSQL、Node、SMTP 或私密配置；内嵌页面仅在构建时需要 Node，运行二进制不需要 Node 或磁盘前端文件。

## 工具链与启动

使用 **Go 1.27.3**，不自动切换工具链；先确认 `go version`。若本机版本不同，将独立安装的 Go 1.27.3 `bin` 目录放入当前进程的 PATH，不修改全局配置。

```sh
GOTOOLCHAIN=local go version
GOTOOLCHAIN=local go build -o /tmp/tokenpivot-t049 ./cmd/tokenpivot
TOKENPIVOT_HTTP_ADDR=127.0.0.1:18080 /tmp/tokenpivot-t049
```

也可用 `GOTOOLCHAIN=local go run ./cmd/tokenpivot` 开发，但退出信号验收须针对构建后的二进制，不针对 `go run` 父进程。

## 配置

Go 服务配置 `TOKENPIVOT_HTTP_ADDR` 未设置时为 `127.0.0.1:8080`；设置后覆盖默认，显式空值报错。必须为 host:port，端口仅允许数字 1–65535。支持 IPv4、主机名、方括号 IPv6（如 `[::1]:8080`）、空 host（如 `:8080`）。端口 0 不允许用于服务配置。

`:8080` 表示明确监听所有接口；默认回环监听不是生产网络安全方案。主机解析或地址占用由启动监听报告错误，不重试或换端口。配置只加载一次，不读取 `.env`/YAML，不支持命令行配置或热加载；忽略未知环境变量。配置错误不回显输入。

HTTP 超时集中在 server 包：请求头 5s、读取 10s、写入 10s、空闲 60s；关闭等待最多 10s。它们不是业务请求时限承诺，当前不提供环境开关。

## 健康探针

| 请求 | 响应 |
|---|---|
| `GET /healthz` | 200，`text/plain; charset=utf-8`，正文 `ok\n` |
| `HEAD /healthz` | 200，相同内容类型，无正文 |
| `POST /healthz` 等不支持方法 | 标准库 405 |
| `/api`、`/api/...`、`/readyz`、`/healthz/` | 404，不返回页面 |
| `/` | 纯后端 404；内嵌构建返回工程占位页 |

探针无需认证，仅证明进程存活，不检查外部依赖或业务就绪，不返回配置、版本或环境信息。

```sh
curl --fail --show-error --silent http://127.0.0.1:18080/healthz
curl -I http://127.0.0.1:18080/healthz
curl -i -X POST http://127.0.0.1:18080/healthz
curl -i http://127.0.0.1:18080/readyz
```

## 停止与日志

对二进制发送 SIGINT（前台 Ctrl-C）或 SIGTERM：先停止接收新连接，再等待进行中的请求排空，成功退出 0 并记录 `stopped`。请求 context 不继承停机信号。关闭超时会强制 Close 连接、报告失败并退出 1，不记录正常停止。配置、绑定、非预期 Serve 或关闭错误也退出 1；没有重试、重启或降级。

stderr 输出 Info 及以上的 slog JSON；监听成功后才记录 `started` 和实际地址。错误只在入口记录一次；net/http ErrorLog 使用同一日志输出。不记录逐请求成功日志、完整配置、环境、请求头、Cookie、Authorization、查询、请求体或响应体。系统监听/关闭错误可以保留本地原因；这不代表未来外部依赖的连接串可安全直接记录。

## 验证

```sh
GOTOOLCHAIN=local go test ./...
GOTOOLCHAIN=local go test -race ./...
GOTOOLCHAIN=local go vet ./...
GOTOOLCHAIN=local go build -o /tmp/tokenpivot-t049 ./cmd/tokenpivot
gofmt -l cmd/tokenpivot/*.go internal/config/*.go internal/server/*.go
```

`gofmt -l` 应无输出；race 需要本机 C 编译工具链。测试含真实 HTTP HEAD、活动请求排空、超时强制关闭、端口占用和子进程信号退出；信号测试针对 Unix 开发环境，不覆盖 Windows 服务管理。

## 前端开发与内嵌构建

前端工程位于 [web](web/README.md)，使用 Node **24.22.1** 与 npm **11.19.1**，先确认版本。开发时分别启动 Go 与 Vite：

```sh
GOTOOLCHAIN=local go run ./cmd/tokenpivot
# 另一终端
npm --prefix web ci
npm --prefix web run dev
```

页面为 `http://127.0.0.1:5173`，仅 `/api` 及其子路径经 Vite 转发到 `http://127.0.0.1:8080`；开发覆盖变量 `TOKENPIVOT_DEV_API_TARGET` 只在 Vite 服务端读取。`/healthz` 直接访问 Go。代理保留 Host/Origin/Cookie，不代表认证或 Origin 校验已经实现。

无 `uiembed` 标签时保持纯后端行为，缺少前端产物不影响 Go 测试或构建。发布内嵌页面：

```sh
npm --prefix web ci
npm --prefix web run build
GOTOOLCHAIN=local go test -tags uiembed ./...
GOTOOLCHAIN=local go test -race -tags uiembed ./...
GOTOOLCHAIN=local go vet -tags uiembed ./...
GOTOOLCHAIN=local go build -tags uiembed -o /tmp/tokenpivot-t031-ui ./cmd/tokenpivot
TOKENPIVOT_HTTP_ADDR=127.0.0.1:18080 /tmp/tokenpivot-t031-ui
```

带标签缺少 dist 时编译失败；缺少可读普通 index.html 时初始化失败，不会启动成假成功。构建后可将二进制放到不含仓库的目录运行。页面与 API 必须同源，生产 TLS 由外层入口提供。所有静态成功响应 `Cache-Control: no-cache`；不存在的资源不返回 HTML，只有显式接受 HTML 的无扩展名导航可回退入口。带哈希资源不承诺升级后旧页面永远可用。

当前不含数据库/readiness、业务 API、认证授权、TLS、请求 ID、指标追踪、后台任务、Redis/队列；占位页不是产品视觉或业务页面。也不建设 Makefile、CI、Docker 或统一 lint 平台。公网部署前须独立完成入口与业务安全设计。
