# TokenPivot 前端工程

T-031 只提供工程占位页与同源接入，不提供业务 API、认证、登录、统计数据或产品视觉。

> 仓库根的统一入口在 [根 README](../README.md) 与 [Makefile](../Makefile)：根说明有完整的工具前提、首次安装、命令表与最短双终端启动，Makefile 提供 `make build-web`、`make dev-web` 等组合目标。本页只保留前端专项细节，不重复第二套安装步骤。
>
> 开发前的前置条件在根一侧：先按根 README 准备专用本地 PostgreSQL、独立执行迁移并安全注入 `TOKENPIVOT_DATABASE_URL`，Go 服务才能监听；Go 未启动时 Vite 的 `/api` 请求会失败，不会回退页面。

## 工具与命令

使用 Go **1.26.4**（`go.mod` 的最低要求）、Node **26**（engines: `>=26.0.0`）、随 Node 26 分发的 npm（本机 11.20.0，不单独锁 npm 版本）。不自动切换工具链；如使用隔离安装，把对应 bin 放入当前进程 PATH，不修改全局配置。

`@types/node` 仍为 **24.19.2**：它是编译时 API 类型包，与运行时 Node 26 主版本不一致，本任务不动依赖图，留待后续单独评估对齐。版本口径来源见 [T-052 工具链版本基线计划](../docs/plans/2026-10-27-toolchain-baseline.md)。

以下从仓库根目录执行：

```sh
node --version
npm --version
npm --prefix web ci
npm --prefix web ls --depth=0
npm --prefix web run typecheck
npm --prefix web run lint
npm --prefix web run format:check
npm --prefix web test
npm --prefix web run build
```

这些 npm 脚本保持独立可用；根 Makefile 只是把它们组合成 `make build-web`（等同 `npm --prefix web run build`）等目标，不改变脚本语义。

首次更新锁文件使用正常 `npm --prefix web install`；直接依赖固定版本，不使用 force 或 legacy-peer-deps 隐藏冲突。TypeScript strict 覆盖 src 与 Vite 配置，ESLint 负责 JS/TS 规则，Prettier 负责前端格式，Go 文件仅用 gofmt。测试使用 Node 原生 node:test 与真实 Vite/HTTP 上游，不使用 UI 自动化。

## 开发同源代理

```sh
# 根目录，终端一
GOTOOLCHAIN=local go run ./cmd/tokenpivot
# 根目录，终端二
npm --prefix web run dev
```

页面固定 `http://127.0.0.1:5173`，端口被占用就失败，不漂移端口。默认 Go 地址 `http://127.0.0.1:8080`。仅代理 `/api`、`/api?x=1`、`/api/...`，不代理 `/apix` 或 `/healthz`。健康检查直接访问 Go。

需要覆盖上游时在启动 Vite 的进程设置 `TOKENPIVOT_DEV_API_TARGET`，值必须为没有凭证、路径、查询或片段的 HTTP(S) origin（允许末尾 `/`）；显式空值与无效值拒绝且错误不回显输入。不加载该变量的 dotenv 配置，不使用 VITE_ 前缀，不注入客户端产物。Go 地址仍通过已有 `TOKENPIVOT_HTTP_ADDR` 配置。

未来浏览器只请求相对 `/api/...`。代理不去前缀、不重写 Host/Origin/Cookie/Set-Cookie、不伪造认证头，也不打开 WebSocket。上游错误保留，连接失败不返回页面。开发无需 CORS；保留 Origin 不代表允许来源或会话认证已经完成。

## 构建与静态服务

Vite base 为 `/`，产物写到忽略提交的 `web/dist/`，JS/CSS 使用带内容哈希的 assets 文件名，不生成 source map。运行页面不依赖开发 target 配置。

普通 Go 测试/构建不要求 Node 或 dist，也不提供前端。需要把真实产物内嵌到二进制时：

```sh
npm --prefix web run build
GOTOOLCHAIN=local go test -tags uiembed ./...
GOTOOLCHAIN=local go test -race -tags uiembed ./...
GOTOOLCHAIN=local go vet -tags uiembed ./...
GOTOOLCHAIN=local go build -tags uiembed -o /tmp/tokenpivot-t031-ui ./cmd/tokenpivot
```

缺 dist 会导致带标签编译失败，缺可读普通入口文件会导致启动失败，不回退假页面。构建后的二进制可在不含仓库/dist 的目录运行，无需 Node。

`/api`、`/healthz`、`/readyz` 及其子路径始终交后端。其他路径只接受 GET/HEAD，真实文件按 MIME 返回；目录不列出内容。不存在的无扩展名路径仅在 Accept 明确接受 text/html 且 q 大于 0 时回退入口；缺失资产、扩展名文件、非 HTML 请求均 404，读取失败 500。拒绝点文件与穿越路径。回退只证明深链接可刷新，不代表业务路由已实现。

所有静态成功响应均为 `Cache-Control: no-cache`，不设置 immutable；不承诺服务升级后旧页面仍能取回旧资源。生产页面与 API 必须同源，TLS 交由外层入口处理。本任务不支持子目录部署、CDN 配置或多版本资源保留。

## shadcn 与后续范围

在 web 目录执行固定 CLI：

```sh
npx shadcn@4.22.1 info --json
```

[components.json](components.json) 与 [cn 工具](src/lib/utils.ts) 只准备接入，没有生成任何组件、安装图标包或选择 primitive 库。new-york、neutral 是可替换工程默认，不是产品视觉决定。T-032 添加首个组件前可以一次替换；T-033 才处理登录等认证页面。TanStack Query 等到首个服务端状态需求再引入，当前不预建请求框架。

人工验收由用户自行打开开发页与内嵌页查看占位内容及 CSS 加载；不属于业务交互或视觉设计验收。
