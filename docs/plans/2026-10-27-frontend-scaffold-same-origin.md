# T-031：前端工程脚手架与同源接入

> 状态：规划完成，只读评审通过；不授权实施。关联 T-031，前置 T-049。
> 日期：2026-10-27。仅本文件属于本次规划交付；提交、TODO 更新与实施授权由主代理负责。

## 目标、范围与非目标

建立独立 `web/` 工程：React + TypeScript strict + Vite + Tailwind，准备 shadcn/ui 的配置与工具函数；开发页面经 Vite 同源访问 `/api`，发布时可将前端构建产物内嵌进 Go 二进制，运行不依赖 Node。

交付只含工程占位页、配置、开发代理、静态服务及其测试，不代表业务可用。遵循[架构第 3、6 节](../architecture.md#L25-L35)、[ADR-0001](../decisions/0001-go-monolith-postgresql.md)、[ADR-0004](../decisions/0004-auth-and-workspace.md)。

不做：T-032 视觉组件、F 风格与设计变量；T-033 登录等认证页面；业务 API、会话、Origin 授权规则；TanStack Query 数据查询与缓存；路由库、图表、表单、全局状态；T-051 顶层 Makefile、CI、Docker 与构建汇总；CDN 实际部署、跨源 Cookie、子目录部署、Service Worker、预压缩文件。TanStack Query 的既定路线不改变，等首个服务端状态需求出现再安装，不预建请求框架。

## 当前状态：代码与计划分开看

- [Go 骨架计划](2026-10-27-go-service-skeleton.md)仍写“未实施”，但本次已直接读取实际代码：不能据此认为仓库为空。
- [go.mod](../../go.mod#L1-L3)实际为 `github.com/dundunHa/TokenPivot`、Go `1.27.3`；本任务不修改模块与工具链版本，不增加 Go 第三方依赖。
- [server.NewHandler()](../../internal/server/server.go#L22-L33)实际创建私有 ServeMux，仅注册 `GET /healthz`；[现有测试](../../internal/server/server_test.go#L15-L58)覆盖 HEAD、405、根路径/API/readiness 未知 404。
- [入口 run](../../cmd/tokenpivot/main.go#L25-L49)实际直接调用 `server.NewHandler()`，并负责监听与停机；[README](../../README.md)已有基础开发说明。本任务保持生命周期、超时与配置语义不变。
- 本次路径发现未发现 `web/`、前端依赖文件或根 `.gitignore`；下文全部前端路径均为拟新增。已有未跟踪文档不覆盖、不纳入实施提交。
- T-049 是集成前置：源码已出现不等于已验收或已提交。实施前主代理确认其已经稳定合入，并复跑原有测试；若源码接口变化，只按实际接口修正本计划的接入点，不重做骨架。

## 决策一：最小独立工程

### 工具链与兼容版本

建议基线 Node `24.22.1`（官方 LTS 发布记录），使用随其分发的 npm `11.19.1`，不额外引入 pnpm/Corepack；`web/package.json` 记录 `packageManager: npm@11.19.1`，Node engines 限定 `>=24.22.1 <25`。独立 `web/package-lock.json`，不创建根 Node workspace。所有直接依赖写明确版本，提交锁文件。

以下为本次发布元数据核验后的**建议固定版本**，不是已经在本仓安装成功的版本：

| 依赖 | 版本 | 当前必要性 |
|---|---|---|
| react、react-dom | 各 19.3.1 | 页面渲染；两者匹配 |
| clsx | 2.1.1 | shadcn 的 `cn()` 条件类名输入 |
| tailwind-merge | 3.7.0 | `cn()` 合并 Tailwind 冲突类；不另加 classnames |
| vite | 8.3.5 | 开发服务器、代理、生产构建 |
| @vitejs/plugin-react | 6.1.2 | React 编译与开发刷新，peer 支持 Vite 8 |
| typescript | 5.9.3 | strict 类型检查；主动不采用当前 latest 7.0.2 |
| @types/react、@types/react-dom | 各 19.3.0 | React 19 类型声明 |
| @types/node | 24.19.2 | Vite 配置中的 Node API 类型，与运行时主版本一致 |
| tailwindcss、@tailwindcss/vite | 各 4.3.3 | 官方 Vite CSS 插件；peer 支持 Vite 8，无需另装 PostCSS/autoprefixer |
| eslint | 10.13.0 | 静态规则检查 |
| typescript-eslint | 8.72.1 | TypeScript lint；声明支持 ESLint 10、TypeScript `>=4.8.4 <6.1.0` |
| eslint-plugin-react-hooks | 7.1.1 | Hooks 规则；支持 ESLint 10 |
| eslint-plugin-react-refresh | 0.5.7 | Fast Refresh 导出规则；支持 ESLint 10 |
| prettier | 3.9.10 | 前端唯一格式化工具；不与 ESLint 混合格式规则；Go 文件使用 gofmt |

除前三项运行时依赖外均为开发依赖。ESLint 使用上述包的推荐 flat config，必要的浏览器全局直接显式声明，不为复制模板增加无使用点的包。

shadcn CLI `4.22.1` 仅按需通过固定版本 `npx shadcn@4.22.1` 运行，不安装进产品运行依赖，不添加完整组件集。React Compiler、可选 Babel/OXC 插件不是此脚手架要求，不主动安装。Node 原生 `node:test` 足以验证实际 HTTP 代理，不新增 Vitest、DOM 模拟或浏览器自动化依赖。

**兼容发布检查**：安装时不得使用 `--force`、`--legacy-peer-deps` 掩盖冲突；依赖树、类型检查、lint、构建全部通过后才认为组合成立。如果出现元数据之外的真实冲突，停止并报告具体包，主代理修改固定版本后重验，不能静默全部升级。

### 配置与页面

- Vite `base: '/'`，默认输出 `web/dist/`，所有编译后的 JS/CSS/导入资产采用带内容哈希的 `assets/` 文件名；不开 source map。不创建 public 资产或 manifest 消费层。
- 一个 `tsconfig.json` 即可包含 `src/` 与 `vite.config.ts`，`strict`、`noUncheckedIndexedAccess`、`noImplicitOverride`、`noFallthroughCasesInSwitch`、`verbatimModuleSyntax`；`module: ESNext`、`moduleResolution: bundler`、`jsx: react-jsx`、`noEmit: true`。不使用项目引用，避免根 typecheck 空跑。
- 只用 `@/* → ./src/*` 一层别名，Vite 与 TypeScript 同步配置。
- `src/main.tsx`、`src/App.tsx` 仅展示 TokenPivot 工程占位内容，明确业务尚未提供；无 API 请求、假统计、登录操作或视觉原型迁移。
- `src/index.css` 接入 Tailwind v4；只保留占位页需要的中性背景/前景语义变量，不建设完整设计变量体系。
- 手工准备 `components.json`：`rsc: false`、`tsx: true`、`tailwind.config: ''`、CSS 路径 `src/index.css`、`cssVariables: true`、组件与工具别名指向 `@/components`、`@/components/ui`、`@/lib/utils`。以 schema 已支持的 `style: new-york`、`baseColor: neutral` 作为可替换工程默认，不生成组件、不据此确定产品视觉或 primitive 库；T-032 添加首个组件前可一次替换此配置，当前没有组件迁移成本。可选 iconLibrary 不设置，图标包不安装。
- `src/lib/utils.ts` 提供 shadcn 常规 `cn()`，不再封装第二层；占位页实际使用它以验证别名与依赖链。shadcn 配置能被固定 CLI `info --json` 识别即满足本阶段接入验收。
- npm scripts：`dev` 为 Vite，`typecheck` 为 `tsc --noEmit`，`build` 为先 typecheck 后 `vite build`，`lint` 为 `eslint .`，`format:check` 为 `prettier --check .`，`test` 为 `node --test test/*.test.mjs`。格式与 lint 均忽略 dist/node_modules；Prettier 的 ignore 明确排除 `*.go`，ESLint 只检查 JS/TS 文件；Go 使用 gofmt。不加 Git hooks 或根汇总脚本。

## 决策二：开发同源代理

页面默认地址 `http://127.0.0.1:5173`，Go 默认 `http://127.0.0.1:8080`。Vite 绑定回环地址、固定端口并 `strictPort: true`，避免端口漂移改变开发 Origin。Go 仍按已有 `TOKENPIVOT_HTTP_ADDR` 启动。

仅新增一个**开发服务器侧**环境变量 `TOKENPIVOT_DEV_API_TARGET`：未设置时取上述 Go 默认地址；显式设置必须为不含凭证、路径、查询或片段的 HTTP(S) origin，空值与格式错误在加载 Vite 配置时拒绝，错误不回显输入。从进程环境读取，不增加 dotenv 加载、不使用 `VITE_` 前缀，不用 define 注入前端。

- 代理匹配 `^/api(?:/|[?]|$)` 对应的正则规则：覆盖 `/api`、`/api?x=1`、`/api/...`，不误代理 `/apix`。不去掉 `/api`，保留路径和查询。
- `changeOrigin: false`；不重写 Origin、Cookie、Set-Cookie、不伪造认证头，不启用 WebSocket 或跨源放开选项。测试证明浏览器原始 Host/Origin 到达上游；后续认证实现仍须独立正确配置允许来源，不能把代理视为已完成 Origin 防护。
- 浏览器未来只请求相对 `/api/...`，不把 Go 地址写进 bundle、不增加跨源 credentials 设置。开发不需要 CORS。
- 上游的状态码、响应体、内容类型、Set-Cookie 原样透传；上游拒绝连接必须返回非 2xx 的代理错误，不能回退到 index.html。沿用 Vite 错误处理，不加重试或假成功。
- `/healthz` 不做代理；它是 Go 探针，开发直接访问 Go。测试代理使用测试进程临时上游的合成 `/api` 路径，不新增生产“测试 API”。

## 决策三：构建标签内嵌，默认 Go 测试不需要前端

### 包与装配

在 `web/` 同时放 Go 的资源适配文件，属于**根 Go module**，不另建 go.mod。它们仅服务编译产物，不混写前端业务。

拟新增：

- `web/embed.go`：`//go:build uiembed`，使用 `//go:embed dist`；`Handler() (http.Handler, error)` 取得 `fs.Sub(..., "dist")` 后调用内部 `newHandler(fsys fs.FS) (http.Handler, error)`。
- `web/noembed.go`：`//go:build !uiembed`，同签名 `Handler()` 返回 `(nil, nil)`，明确表示此构建不提供前端。
- `web/handler.go`：上述内部 `newHandler` 与静态处理逻辑；`web/handler_test.go` 使用 `fstest.MapFS`，不依赖 dist；`web/embed_test.go` 只在 uiembed 下验证真实 index 与引用资源存在。

`web.Handler()` 在入口启动、监听之前调用；初始化失败由现有入口记录一次并退出，不能启动成“有前端”的假成功。缺少 dist 时 uiembed 编译失败；dist 存在但没有可读普通 index.html 时初始化失败。无需运行时磁盘模式或缺文件时切回占位 HTML。

将现有签名改为 `server.NewHandler(frontend http.Handler) http.Handler`：nil 保留现有纯后端行为；实际前端 handler 从 main 显式传入。不加可变全局变量、注册框架或函数选项层。现有 server 测试调用传 nil；其生命周期测试不变。main 不改配置/监听/停机逻辑。

这个构建标签是必要复杂度：Go embed 模式必须匹配真实文件；普通 Go 开发不应先安装 Node，而发布不得用占位文件掩盖缺少产物。相比提交假 dist 或自动生成隐藏文件，双构建路径职责更明确。以后静态资源独立托管可以继续使用不带标签的纯后端二进制，不需要改 API。

### 路由归属：先分派，不按响应状态猜测

`server.NewHandler` 内保留现有后端 mux，外层根据路径归属直接选择后端 mux 或前端 handler：

1. `/api` 及 `/api/` 子路径始终由后端处理，所有方法均不进入静态回退。
2. `/healthz`、`/healthz/` 子路径，以及 `/readyz`、`/readyz/` 子路径留在后端。保留当前 healthz GET/HEAD 200、其他方法 405，以及未知探针 404；不能因添加根 GET handler 变成页面或改变方法错误。
3. 其他路径：frontend 非 nil 时交给前端；否则交后端 mux，仍为原有 404。

不捕获后端 404 再改写 HTML，不复制/缓冲后端响应。未来真实 API 注册到同一后端 mux 即可；本任务不实现端点、响应 schema 或 OpenAPI。

### 静态服务规则

- 只允许 GET/HEAD；其余方法 405、`Allow: GET, HEAD`，不返回页面正文。
- `/` 和 `/index.html` 返回入口 HTML；HEAD 无正文。
- 真实普通文件按正确 MIME 返回；目录不列出内容，不返回任意子目录 index。使用标准库 ServeContent 等能力处理文件响应，避免修改请求路径后交 FileServer 产生 index.html 重定向循环。
- 只有**没有匹配文件、最后一段没有扩展名、不在 `/assets` 及其子路径中、且 Accept 明确允许 `text/html`（q 不为 0）**的 GET/HEAD 请求，才返回入口 HTML。它只证明 SPA 深链接可刷新，不代表对应业务页面已实现。
- 不存在的 `.js`、`.css`、图片、favicon、source map、`/assets/*` 返回 404，哪怕 Accept 带 HTML；不是 HTML 导航的未知路径也为 404。错误响应不可返回入口 HTML。
- 路径使用 URL 已解码值校验，不二次解码；拒绝目录穿越与点文件段，不把任意磁盘路径传给文件服务。仅访问限定的内嵌 FS。Go mux 的规范化重定向可保留，但最终不能越过资源目录或显示目录列表。
- 初始化时读取并确认 index；文件不存在归 404，真实读取错误归 500，响应不暴露内部路径/文件错误；不把所有 IO 错误都当成 SPA 回退。handler 内不另建日志框架。

### 缓存与发布边界

本阶段所有静态成功响应统一 `Cache-Control: no-cache`，HTML 与深链接同样处理；不使用 immutable，也不额外实现 ETag 算法。这样先保证升级入口与资源正确，不引入“如何识别所有带哈希文件”的规则。HTTP 条件响应可沿用标准库；允许重复验证而非禁止存储。

带哈希文件名用于版本隔离，但不承诺旧页面在服务升级后加载旧资源永不失败。未来若增加懒加载或 CDN，需在相应任务处理旧资产保留与入口重新加载，届时可仅对确定不可变的资产添加长期缓存。本阶段不建设多版本资源仓库。生产源站公开地址由外层 TLS/入口提供；所有页面/API 浏览器访问仍必须同源。

## 拟文件范围及执行顺序

下列未链接路径均为**拟新增**：

```text
web/package.json                 web/package-lock.json
web/tsconfig.json                web/vite.config.ts
web/eslint.config.js             web/.prettierignore
web/components.json              web/index.html
web/src/main.tsx                 web/src/App.tsx
web/src/index.css                web/src/lib/utils.ts
web/src/vite-env.d.ts            web/test/proxy.test.mjs
web/embed.go                     web/noembed.go
web/handler.go                   web/handler_test.go
web/embed_test.go                web/README.md
.gitignore
```

现存文件仅修改：[server.go](../../internal/server/server.go)、[server_test.go](../../internal/server/server_test.go)、[main.go](../../cmd/tokenpivot/main.go)、[README](../../README.md)。变更前确认函数引用，只更新真实调用者；不改已有架构、ADR、TODO 或其他计划。

1. 主代理取得单独实施授权，确认 T-049 已合入、原有测试通过；读取实施工作区实际配置，若上述拟新增路径已出现则先核对，禁止覆盖并行工作。
2. 创建 web 工程、固定依赖和锁文件，完成 strict、lint、格式化、Tailwind、shadcn 配置与占位页。`.gitignore` 精确忽略 `/web/node_modules/`、`/web/dist/`、本地开发环境文件，不忽略所有 docs 或全仓产物。
3. 实现 Vite 精确代理及真实 HTTP 测试；先证明代理，不依赖业务接口。
4. 实现两个构建标签分支、静态 handler 与 Go 路由装配。先以 MapFS 跑测试，再构建真实前端验证 uiembed；不为测试提交假 dist。
5. 更新两份 README：根文档解释纯后端与内嵌两种构建；web 文档记录工具版本、同源代理、命令、shadcn 的可替换工程默认和 T-032/T-033 边界。不建设根构建汇总。
6. 执行下列验收、记录真实结果，再交主代理评审；本规划未运行依赖安装、初始化、测试或构建。

## 验收命令与测试矩阵

以下全部是**未来实施验收命令，本次未执行**。工具链必须已就绪；不自动安装全局工具，不以版本不符时悄悄下载替代。

第一阶段，在一个尚无 `web/dist/` 的干净实施检出中，先证明没有 Node/产物也能测试 Go：

```sh
gofmt -l web/*.go internal/server/*.go cmd/tokenpivot/*.go
GOTOOLCHAIN=local go test ./...
GOTOOLCHAIN=local go test -race ./...
GOTOOLCHAIN=local go vet ./...
GOTOOLCHAIN=local go build -o /tmp/tokenpivot-t031-api ./cmd/tokenpivot
```

`gofmt -l` 必须无输出（仅退出码为 0 不代表格式通过）。此时 `go build -tags uiembed ./cmd/tokenpivot` 应失败且指向缺少 dist；用独立检出验证，不为验收删除本地文件。随后执行：

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
GOTOOLCHAIN=local go test -tags uiembed ./...
GOTOOLCHAIN=local go test -race -tags uiembed ./...
GOTOOLCHAIN=local go vet -tags uiembed ./...
GOTOOLCHAIN=local go build -tags uiembed -o /tmp/tokenpivot-t031-ui ./cmd/tokenpivot
```

首次生成锁文件使用正常 npm install；之后从锁文件 `ci` 重现。`web/` 目录内运行 `npx shadcn@4.22.1 info --json`，确认配置识别正确，不运行 init/add、不生成视觉组件。

| 层级 | 必须证明的行为 |
|---|---|
| 前端静态检查 | strict 覆盖 src 与 Vite 配置；禁止 any/类型绕过；别名一致；CSS 与 JS 构建成功；产物不包含开发 target 地址或私密配置 |
| 代理集成测试 | Node 测试建立临时 HTTP 上游与真实 Vite server，监听回环临时端口；加载真实 Vite 配置；GET/POST 的方法、路径、query、body、Host/Origin/Cookie 保留，Set-Cookie 和 401/404/500 不被改为 HTML；关闭上游后返回非 2xx；`/api`、`/api?x=1`、`/api/x` 命中、`/apix` 不命中；finally 关闭两边 server，不用 sleep 猜启动完成 |
| Go 基础回归 | 无 dist、不带标签，原有健康检查、405、404、监听和优雅停止测试不变；不依赖 Node/数据库 |
| Go 路由测试 | 注入前端 handler 后，所有 API/保留探针请求仍交后端；特别覆盖 `POST /healthz` 仍 405、未知 API 带 Accept HTML 仍 404；纯后端 nil 模式根路径仍 404 |
| 静态单元测试 | MapFS 验证根/index、真实 JS/CSS MIME、HEAD 无正文、深链接 HTML、非 HTML 请求 404、缺文件/缺资产不吞错、目录列表禁止、方法 405、路径穿越/点文件不可读；读取失败 500、缺入口初始化失败；no-cache 覆盖直接入口与回退 |
| 内嵌集成测试 | 带标签使用真实 dist，入口及其 JS/CSS 引用存在且可获取；缺入口不能静默启动；构建成功后从不含仓库/dist 的工作目录启动二进制，GET `/` 与 `/healthz` 正常、`/api/not-implemented` 404，证明运行不读磁盘前端也不启动 Node |
| 人工检查 | 用户自行打开开发页与内嵌页，观察占位内容及样式加载；不是 UI 自动化，也不是 F 风格/业务交互验收 |

人工启动示例：根目录 `GOTOOLCHAIN=local go run ./cmd/tokenpivot`，另终端 `npm --prefix web run dev`；生产冒烟用 `TOKENPIVOT_HTTP_ADDR=127.0.0.1:18080 /tmp/tokenpivot-t031-ui`。可用 curl 查看 `/`、`/healthz`、`/api/not-implemented` 状态及类型，用带 `Accept: text/html` 的 `/test-deep-link` 检查入口回退。全部端口仅为计划默认/示例，不宣称当前服务正在运行。

## 执行切分

**单块，不拆。** 文件虽跨 TS 与 Go，但只有一个“开发代理 → 产物 → 内嵌入口”接入目标，拆分会增加签名与路由归属协调。

同一实施者拥有 web 全部文件、锁文件、根 `.gitignore`、server/入口改动及 README；先依赖与配置，再代理，再 Go 内嵌，最后联合验收。无需预建共享类型、迁移编号、生成接口或根注册框架。若 T-051 同时开工，主代理串行处理根 README 与 `.gitignore`；T-031 不创建/修改根 package.json、Makefile 或 CI。T-032/T-033 等本块工程入口稳定后再开始。

## 风险、未核实事项与回退

- 无待用户决定的实质阻塞；npm、构建标签、缓存保守策略和 shadcn 临时工程默认是本计划推荐方案，不冒充既有用户决定。产品风格/组件基础库仍归 T-032，不在本次确认。
- 主代理收尾时已核实 main HEAD 为 `0fdf385 merge(T-049): deliver reviewed Go service skeleton`，T-049 已合入；本轮未重跑其验收，实施前仍须复跑原有测试。Go 1.27.3 沿用实际模块，不以旧计划的外部发布查询作为本次新结论。
- 所有依赖版本和 peer 范围仅核实发布元数据；本机 Node/npm 可用性、完整安装、原生依赖平台支持、TS 5.9.3 对本次整套库的实际编译仍未验证。Node 官方随包 npm 版本已核实，未单独安装验证。
- master 文档/源码说明当前机制，不等于 Go 1.27.3 或固定 Vite 版本的测试已通过；项目测试是最终验收。不声称此脚手架具备认证、Origin 校验、生产 TLS 或安全部署能力。
- 无数据库迁移、持久数据变更。回退由主代理撤销本任务实现提交或部署上一份完整二进制；页面与 API 产物一起回退，不混用版本，不通过删除已有文档或业务代码处理。

## 依据

有界研究三个问题：前端工具兼容、同源开发代理、Go embed 与静态边界。以下抓取日期均为 **2026-10-27**；官方来源为一手证据。web_fetch 被本机域名非公网解析防护拒绝后，使用 curl 获取相同官方资料；未安装工具或 clone 模板。

| 事实、采纳/拒绝及理由 | 来源 | 核实状态 |
|---|---|---|
| Node LTS 列表返回 24.22.1、随包 npm 11.19.1；选 Node 24 线，不追逐 npm latest 12 | [Node 官方发布列表](https://nodejs.org/dist/index.json) | 已核实发布记录；未本地运行 |
| Vite 8.3.5、React 插件 6.1.2；Node 下限兼容本计划，插件 peer 支持 Vite 8，可选编译器插件不必主动引入 | [Vite 元数据](https://registry.npmjs.org/vite/latest)、[插件固定版本元数据](https://registry.npmjs.org/@vitejs/plugin-react/6.1.2) | 已核实版本/engines/peer/optional 字段 |
| React/DOM 同为 19.3.1；Tailwind 与 Vite 插件同为 4.3.3，插件支持 Vite 8 | [React](https://registry.npmjs.org/react/latest)、[React DOM](https://registry.npmjs.org/react-dom/latest)、[Tailwind](https://registry.npmjs.org/tailwindcss/latest)、[Tailwind 插件](https://registry.npmjs.org/@tailwindcss/vite/latest) | 已核实发布元数据，未安装 |
| TS latest 7.0.2 超出 typescript-eslint 8.72.1 支持范围；选已发布的 TS 5.9.3，不强装冲突版本 | [TS 固定版本](https://registry.npmjs.org/typescript/5.9.3)、[typescript-eslint](https://registry.npmjs.org/typescript-eslint/latest)、[ESLint](https://registry.npmjs.org/eslint/latest) | 已核实版本、peer；实际 lint 待验收 |
| 类型及辅助依赖版本来自发布记录；避免 Node 类型跨到最新 26 主版本 | [React 类型](https://registry.npmjs.org/@types/react/latest)、[DOM 类型](https://registry.npmjs.org/@types/react-dom/latest)、[Node 类型版本列表](https://registry.npmjs.org/@types/node)、[clsx](https://registry.npmjs.org/clsx/latest)、[tailwind-merge](https://registry.npmjs.org/tailwind-merge/latest)、[Prettier](https://registry.npmjs.org/prettier/latest)、[Hooks lint](https://registry.npmjs.org/eslint-plugin-react-hooks/latest)、[Refresh lint](https://registry.npmjs.org/eslint-plugin-react-refresh/latest) | 已核实元数据；`@types/node/24` 不是有效版本 URL，404 后改从版本列表确认 24.19.2 |
| 官方 shadcn Vite 指南采用 Tailwind Vite 插件、CSS import、TS/Vite 两侧 alias；schema 允许 new-york；采用最小配置，不复制整套 preset 或组件 | [Vite 安装指南](https://ui.shadcn.com/docs/installation/vite)、[配置 schema](https://ui.shadcn.com/schema.json)、[CLI 元数据](https://registry.npmjs.org/shadcn/latest) | 已核实正文、schema、CLI 4.22.1；未运行 CLI |
| Vite proxy 支持正则 key，匹配代理的请求不再被 Vite transform；采用精确 API 边界，不照抄示例的去前缀与 changeOrigin:true | [官方代理文档源码](https://github.com/vitejs/vite/blob/main/docs/config/server-options.md) | 已核实正文；保留头部与错误行为须本项目集成测试证明 |
| embed pattern 必须匹配非空内容、不能使用 ..；采用 web 包内 dist 与构建标签，拒绝从 internal 包跨目录 embed 或提交假入口 | [Go embed 源码文档](https://github.com/golang/go/blob/master/src/embed/embed.go) | 已核实一手注释，未核对目标版本标签 |
| FileServer 的 index.html 重定向和目录行为不能无条件等同于 SPA 回退；测试使用 MapFS/真实 HTTP；采用小型边界处理而非第三方 SPA 框架 | [Go 静态服务实现](https://github.com/golang/go/blob/master/src/net/http/fs.go)、[对应测试](https://github.com/golang/go/blob/master/src/net/http/fs_test.go) | 已读 serveFile、FileServerFS、TestFileServerFS、TestFileServerCleans、TestServeFile 相关片段；未运行上游测试 |
| GitHub 参考选 Go 标准库本身：同语言/相同 HTTP 边界、有测试、非 archived、pushed_at 为 2026-10-27T00:53:35Z，BSD-3-Clause；仅借鉴行为，不复制代码 | [仓库元数据](https://api.github.com/repos/golang/go) | 已核实维护及许可证元数据；搜索到的旧 embed 模板与非必要 SPA 仓库不采纳、不深入，避免引入第二套路由框架 |

本次没有性能或生产安全背书；未执行 UI 自动化。无需修改 AGENTS.md：只有计划，没有经过实现验证的可复用工程事实。
