# T-056：前端基础、登录注册与最小已认证页

> 日期：2026-10-29。状态：规划完成；已完成一次只读评审，三项必修意见经修订与主代理核对关闭；不授权实施。
> 关联 T-056。原有 refs 继续有效：视觉基准 [F 原型](../prototypes/dashboard-f.html)，产品要求 [PRD](../prd.md)。

## 已确认范围与旧待办口径的差异

用户已明确确认以下三项推荐，后续按本计划范围执行：

1. **空间切换完整移至 T-020**。T-056 不做空间列表、当前空间或切换 API，不做切换入口，不预建工作空间缓存键、空间状态容器或切换慢响应测试；本计划不代表已经完成空间切换。旧 T-056 名称或 note 中与此不同的表述，由主代理在 TODO 中另行校正，本任务不修改其他文档。
2. 基础组件只建设登录、注册和最小已认证页实际使用的部分，不建设仪表盘组件。
3. 登录、注册成功进入 `/` 的最小已认证页，展示当前用户、固定 `Personal` 文案与登出；浏览器刷新通过现有会话接口恢复身份，不交付仪表盘。

## 目标、范围与非目标

### 目标

在现有 Vite + React + TypeScript 工程中落入 F 原型的设计变量和认证所需基础组件，接通已经存在的四个浏览器认证端点，形成可独立验收的登录、注册、会话恢复和登出流程。

### 范围

- F 原型的背景、面板、文字、边线、强调色、成功/异常色、等宽字体和等宽数字成为 Tailwind v4 语义变量。
- `/login`、`/register`、`/` 三个页面及未知路径回退；使用真实 URL，浏览器深链接刷新仍由现有 SPA 静态处理支持。
- 注册、登录、当前会话、登出四个端点的请求、响应解析、可见错误和提交状态。
- 会话只在内存中保存；未认证、会话过期、退出以及迟到的旧会话响应之间要有确定的竞态处理。
- 非受控表单、账号原样提交、密码边界校验、字段错误关联、键盘操作、焦点样式和减少动态效果。
- Vitest + jsdom 的代码级组件/页面测试，MSW 在 HTTP 边界模拟真实状态和延迟；保留现有 Node 原生代理测试。

### 非目标

- 不做邮箱验证、忘记密码、重置密码、SMTP、邀请或 T-018 的其他能力，不放无目标页面的链接。
- 不做任何空间切换相关内容，不伪造第二个空间，不把固定 `Personal` 文案做成下拉框。
- 不做仪表盘、统计 API、图表、日志、筛选、顶栏业务信息或完整 F 原型迁移。
- 不改 Go、OpenAPI、数据库、Cookie、Origin、注册开关或认证输入规则；不增加注册是否开放的探针。
- 不做跨源认证、token localStorage、持久化查询缓存、全局客户端状态库、主题切换或 UI 自动化。

## 仓库事实与接入边界

- 实际前端基线是 Node **`>=26.0.0`**，React 19.3.1、Vite 8.3.5、Tailwind 4.3.3；不能沿用旧脚手架计划的 Node 24 建议。[package.json](../../web/package.json#L6-L36)
- 当前入口只有占位页，没有路由、请求状态库或认证页面。[main.tsx](../../web/src/main.tsx#L1-L12) [App.tsx](../../web/src/App.tsx#L1-L14)
- 当前 CSS 只有两个中性变量，shadcn 已配置但尚未生成组件。[index.css](../../web/src/index.css#L1-L17) [components.json](../../web/components.json#L1-L17)
- 四个认证路由已在后端实际注册。[auth_middleware.go](../../internal/server/auth_middleware.go#L30-L35) [auth_middleware.go](../../internal/server/auth_middleware.go#L62-L72)
- 成功响应为 `{user:{id,email},expires_at}`；没有 workspace 身份，本任务不得猜加字段。[auth.go](../../internal/server/auth.go#L31-L43)
- 注册和登录成功直接写 Cookie 会话，无需再次请求登录。[auth.go](../../internal/server/auth.go#L56-L85)
- 注册关闭、邮箱占用、凭证错误、限流等错误已经由后端返回稳定 `error.code`。[auth.go](../../internal/server/auth.go#L264-L289)
- 认证请求只接受 `email`、`password`，邮箱和密码限制以服务端为准。[openapi.yaml](../openapi/openapi.yaml#L1907-L1929)
- 服务端唯一账号规范化是 `strings.ToLower(strings.TrimSpace(raw))`，随后才检查字节长度、Unicode 空白/控制字符和 `@`；前端不能用自己的 trim/lowercase 顺序或 Unicode 规则提前改变、拒绝账号。[service.go](../../internal/auth/service.go#L298-L315)

## 关键设计决定

### 1. 最小依赖，不建设额外框架

所有直接依赖继续使用固定版本；Node 26 是前提，不用 `--force` 或 `--legacy-peer-deps` 掩盖冲突。以下版本来自 2026-10-28 官方发布元数据，**尚未在本仓共同安装验证**。

| 依赖 | 固定版本 | 当前需求理由；不选什么 |
|---|---:|---|
| `react-router` | `8.5.1` | 提供三个真实 URL、导航、深链接和测试用 `MemoryRouter`；不手写 history/popstate 路由，也不引入框架模式或 data router。 |
| `@tanstack/react-query` | `5.105.0` | 管理会话服务端状态、请求去重、AbortSignal、取消和 mutation pending；不另建 AuthProvider/store 和手写加载四件套。 |
| `class-variance-authority` | `0.7.1` | Button、Alert 的有限语义变体集中声明并推导类型；调用处不散落颜色分支。 |
| `@radix-ui/react-slot` | `1.5.0` | 保留 shadcn Button `asChild`，让登录/注册互链复用 Button 且仍是真实链接。 |
| `@radix-ui/react-label` | `2.1.17` | shadcn FieldLabel 的实际底层依赖，保证 label 与输入关联。 |
| `vitest` | `5.0.5` | 在现有 Vite/TS 配置运行纯函数与组件测试；peer 支持 Vite 8 和现有 Node 类型。 |
| `jsdom` | `30.1.3` | 提供代码级 DOM、焦点、表单和键盘环境；不是浏览器/UI 自动化。 |
| `@testing-library/dom` | `10.4.2` | React Testing Library 和 user-event 的必要 peer，显式固定而不是依赖隐式安装。 |
| `@testing-library/react` | `16.3.3` | 按角色、标签和可见结果渲染及断言 React 19 页面。 |
| `@testing-library/user-event` | `14.6.8` | 覆盖真实点击、输入和键盘提交路径，避免只调用内部 handler。 |
| `msw` | `3.0.3` | 在 HTTP 边界执行真实请求、响应解析和延迟竞态，不 mock 业务 API 模块。使用 v3 的 `msw/http`、`msw/node` 入口。 |

不新增 `zod`、react-hook-form、TanStack Form、全局 store、图标库、`cn` 包或统一 `radix-ui` 包：两个字段用原生 `FormData` 与一个纯校验函数足够；响应结构很小，用 `unknown` 收窄即可；现有 [`cn()`](../../web/src/lib/utils.ts#L1-L6) 已满足类名合并。

shadcn CLI `4.23.0` 只作为生成参考，不进入产品依赖。其 dry-run 会生成 Button、Input、Alert、Label、Separator、Field 六文件并使用 `cn` + 统一 `radix-ui`；本任务只取实际使用的 Button、Input、Alert、Label、Field，导入改回现有 `@/lib/utils` 和上述独立 Radix 包。Field 只保留 `FieldGroup`、`Field`、`FieldLabel`、`FieldDescription`、`FieldError`，不保留未使用的 FieldSeparator，因此不新增 Separator 或其 Radix 依赖。Button 只保留 default、outline、link 与实际尺寸；不为未来组件预建完整变体。

### 2. F 变量与认证布局

在现有 `src/index.css` 中定义一套单一深色语义主题，不复制 shadcn 默认浅色主题，不添加 `.dark` 覆盖：

| 语义 | 原型值 |
|---|---|
| `background` | `#0a0a0b` |
| `card`/面板 | `#0e0e10` |
| `foreground` | `#f4f1ea` |
| `muted-foreground` | `#a9a49a` |
| 弱化文字 | `#6e6a62` |
| `border`、`input` | `rgba(244,241,234,.11)` |
| 次级分隔线 | `rgba(244,241,234,.055)` |
| `primary`、`ring` | `#e0a64b` |
| `primary-foreground` | `#0a0a0b` |
| `success` | `#8fbf7f` |
| `destructive` | `#d97b6b` |
| `accent` | 采用原型低对比暖白底色，hover 后文字仍为 `#f4f1ea` |
| 圆角 | 采用原型 2–3px 的终端面板尺度，不使用大圆角卡片 |

`@theme inline` 只映射实际使用的颜色、字体和圆角。body 使用原型字体栈 `ui-monospace, "SF Mono", SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace`，设置 `font-variant-numeric: tabular-nums`、`color-scheme: dark` 和清晰的 focus ring。状态同时显示文字，不只改变颜色。认证页无必要循环动画；已有组件过渡在 `prefers-reduced-motion: reduce` 下关闭。

`AuthLayout` 只负责品牌、简短终端式标题、细分隔线、居中的窄表单区域和 `main` 地标。首页只显示 TokenPivot、当前用户邮箱、固定 `Personal` 和登出，不引入统计卡片或空间选择器。

### 3. 页面与状态流

`main.tsx` 只增加 `BrowserRouter` 和 `QueryClientProvider`；`App.tsx` 直接声明：

- `/login` → LoginPage；`/register` → RegisterPage，两者共用 AuthLayout/AuthForm。**首次进入这两个公开页不查询 session，也不因已有会话自动跳转**；只有提交认证表单才写会话。
- `/` → HomePage，自行使用会话查询区分加载、匿名、错误和已认证，不为一个受保护页面预建通用 SessionGate。
- `*` → `Navigate` 到 `/`，再由 `/` 的会话结果决定去首页还是登录页。

会话查询使用唯一 key `['auth','session']`，**不含 workspace，也不形成空间缓存框架**。配置 `retry: false`、`staleTime: Infinity`、关闭 focus/reconnect 自动重取，避免无意义的隐式请求；浏览器刷新会创建新 QueryClient，仍必须请求 `/api/v1/auth/session`。`staleTime` 不负责会话失效，必须由下面的 `expires_at` 计时补充。

`useAuthSession` 在成功响应的 `expires_at` 变化时安排本地到期计时；effect 清理必须取消旧 timeout 并移除 visibility listener，旧会话的计时器不能触发新会话重查：

- HomePage 只有在“有 session、未到期、且未重查”时展示身份。到期时先更新本地到期状态，再开始重查；挂载已有缓存时也直接比较当前时间与 `expires_at`，不能等 effect 运行后短暂展示过期身份。重查期间用 `isFetching` 隐藏旧身份。
- 计时到点后，每个不同的 `expires_at` 最多自动重查一次。`setTimeout` 延迟须限制在 `2_147_483_647ms` 内，提前醒来但尚未到期时只续排计时，不发请求；避免 30 天会话发生超时溢出。
- 页面重新变为 `document.visibilityState === 'visible'` 时检查当前时间；若后台节流导致到期计时未执行，则补做这一次到期重查。已经对该 `expires_at` 做过自动重查的，不因重复 visibility 事件再发请求。
- 登录、注册、登出统一使用 mutation key `['auth','write']`，通过 `useIsMutating` 订阅其 pending。计时或 visibility 检查发现认证写操作 pending 时，仍将会话标为不可展示，但不启动重查；写操作结束后按最新 session/expires_at 重新安排。公开页本就不挂会话计时。
- 到期只隐藏旧身份，**不把缓存写成匿名**；旧数据仅用于内部状态与错误处理。重查得到 `401 UNAUTHENTICATED` 才写 `null` 并跳登录；网络、5xx 或坏响应显示错误和手动重试，visibility 不得形成自动重试循环。

其余状态流：

- 首次进入 `/`：pending 显示明确加载状态；只有 `401 UNAUTHENTICATED` 转为匿名并跳 `/login`。
- 网络错误、5xx、超时、非 JSON、响应结构错误都不是匿名：留在错误页，显示服务不可用并提供手动重试，不能清 Cookie 或跳登录。
- 登录/注册 mutation 成功后，把返回的 AuthSession 直接 `setQueryData` 到会话 key，再 `navigate('/', {replace:true})`；不再额外 GET session。
- HomePage 从同一会话缓存读取当前用户；不复制到另一份全局身份 state。
- 登出失败且会话尚未到期时保持当前已认证页面并显示错误；只有真实 `204` 后才写匿名状态并进入登录页。

### 4. 会话竞态：显式取消后取代，不依赖同 key 推断

`fetchSession` 接收 TanStack Query 的 `AbortSignal` 并传给 `fetch`。相同 key 只表示共享缓存，首次无缓存时还可能去重，**不能据此声称新请求已经取代旧请求**。

在 `queries.ts` 提供本功能内部的 `replaceSessionQuery`：先 `await queryClient.cancelQueries({queryKey:['auth','session']})`，再 `await queryClient.refetchQueries({queryKey:['auth','session']})`。到期重查、visibility 补查和错误后的手动重试必须走这个顺序；首次 `/` 的无缓存加载仍由 useQuery 自己发起，不人为制造第二个请求。

登录、注册、登出 mutation 都遵守以下顺序：

1. `onMutate` 先 await 取消 session 查询，再发送认证写请求；取消期间不宣称匿名或登录成功。
2. 登录/注册成功后，在 `setQueryData(session)` **之前再次 await 取消** session 查询，然后写入新会话并导航；登出成功后同样在写 `null` 前再次取消。写请求失败不覆盖会话。
3. 被取消但忽略 AbortSignal、迟到返回的旧 200 不得覆盖较新的登录会话或匿名状态；测试用 deferred HTTP 响应证明，而不是只检查 cancel 函数被调用。

上述机制只使用 QueryClient、AbortSignal 和页面本地的到期计时状态，不新增 epoch、全局布尔位或第二个身份状态容器。

### 5. 请求层、响应校验与错误

`features/auth/api.ts` 只包含四个薄函数，不建设通用 HTTP 框架：

- 所有 URL 为同源相对 `/api/v1/auth/*`，显式 `credentials: 'same-origin'`；POST 使用 `Content-Type: application/json`。浏览器自动产生 Origin，前端不手工设置、改写或伪造 Origin。
- 请求体只有 `email`、`password`；email 必须是从表单读出的原始字符串，password 也原样提交。注册和登录共用 AuthCredentials。登出 204 不解析响应体。
- 响应先按 `unknown` 处理，再收窄 AuthSession/AuthError；缺字段、类型错误、无效日期或 JSON 解析失败是协议/服务错误，不能当成匿名。
- `AuthApiError` 保留 HTTP status、字符串 code 和可选 Retry-After。code 不做封闭枚举，未知 code 使用通用错误文案而不崩溃。
- 不把限流、注册关闭、邮箱占用或凭证错误自动重试；mutation 使用默认不重试。网络失败由用户明确重试，避免重复创建会话。

输入前端校验只改善体验，不替代服务端：

- **email 不 trim、不 lowercase、不做 Unicode 规范化，也不在前端检查长度、空白、`@` 数量或控制字符。** 前端只对完全空字符串给“请输入邮箱”的必要提示；其余原始值全部交给服务端，由 `normalizeEmail` 先 `strings.TrimSpace` 再 `strings.ToLower` 并验证。这样不会先拒绝服务端规范化后可接受的账号，也不复刻 Go 的 Unicode 行为。
- password 不 trim、不做 Unicode 规范化；8–64 个 Unicode 码点且 UTF-8 不超过 256 字节。使用 `Array.from` 与 `TextEncoder`，不把 UTF-16 `length` 当码点数。
- 表单 `noValidate`，以同一纯 `validateCredentials` 函数给出必要字段错误，避免浏览器另一套 email 规则抢先拦截。校验函数返回的 email 必须与输入字符串逐码点相同，校验通过才发请求。

可见错误按 code 区分：`INVALID_CREDENTIALS` 为邮箱或密码错误；`EMAIL_UNAVAILABLE` 提示改用登录；`REGISTRATION_CLOSED` 提示当前部署未开放注册；`RATE_LIMITED` 提示稍后重试，可展示 Retry-After 但不自动倒计时重发；`PERMISSION_DENIED` 不伪装成密码错误；`INVALID_ARGUMENT` 提示检查输入；`INTERNAL`、`UNAVAILABLE`、网络错误和未知 code 显示安全通用文案。任何错误都不显示密码、令牌、SQL 或响应原文。

### 6. 表单与可访问性

- 原生非受控 form + `FormData`，只有一个提交入口；字段错误单独存放，不复制整个表单值到 state。
- 每个输入有可见 FieldLabel；错误用 FieldError，通过稳定 id、`aria-describedby` 关联；Field 使用 `data-invalid`，Input 使用 `aria-invalid`。
- email 使用 `type="text"`、`inputMode="email"`、`autoComplete="email"`，以纯文本输入保留原始账号；登录密码为 `current-password`，注册密码为 `new-password`。密码要求有 FieldDescription，不用 placeholder 代替 label。
- 无效提交聚焦第一个错误字段；服务端整表失败用 `role="alert"` 的 Alert。失败保留输入，成功后跳转。
- mutation `isPending` 驱动 `aria-busy`、提交按钮禁用和可见进度文字；提交入口再检查 pending，防止快速双击产生两个请求。
- 所有操作可键盘完成，焦点清晰可见，页面各有一个明确 h1 和 main 地标。

## 精确文件范围

### 现有文件，允许修改

| 文件 | 修改内容 |
|---|---|
| `web/package.json` | 固定上述依赖；`test` 改为依次运行现有 Node 代理测试和 `vitest run`，其余脚本与 Node engines 不变。 |
| `web/package-lock.json` | 由 npm 根据最终直接依赖重新生成；不得手工编造，不得残留未声明的 `cn` 或统一 `radix-ui`。 |
| `web/vite.config.ts` | 改用 `vitest/config` 的 defineConfig，追加 jsdom、src 测试 include 和 setupFiles；保留现有代理、build、alias。 |
| `web/eslint.config.js` | 只补充实际使用的浏览器/测试全局，不新增 globals 依赖，不放宽 strict 或关闭规则。 |
| `web/src/main.tsx` | 装 BrowserRouter 与 QueryClientProvider，保留 StrictMode。 |
| `web/src/App.tsx` | 声明三条页面路由与未知路径回退。 |
| `web/src/index.css` | F 语义变量、字体、基础布局与减少动态效果。 |
| `web/README.md` | 更新已实现的页面、测试命令、Node 26 前提和认证边界；保留同源部署说明。 |

不修改 `web/components.json`、`web/tsconfig.json`、`web/src/lib/utils.ts`、Go 文件、OpenAPI、PRD、ADR、TODO 或其他计划。

### 拟新增：认证实际使用的 UI 组件

```text
web/src/components/ui/button.tsx
web/src/components/ui/input.tsx
web/src/components/ui/alert.tsx
web/src/components/ui/label.tsx
web/src/components/ui/field.tsx
```

### 拟新增：认证功能

```text
web/src/features/auth/model.ts          # 类型、unknown 收窄、纯输入校验
web/src/features/auth/api.ts            # 四个同源端点与 AuthApiError
web/src/features/auth/queries.ts        # session key、会话查询、认证/登出 mutation
web/src/features/auth/auth-form.tsx     # 共用两字段表单
web/src/features/auth/auth-layout.tsx   # 认证页共用壳
web/src/features/auth/pages/login-page.tsx
web/src/features/auth/pages/register-page.tsx
web/src/features/auth/pages/home-page.tsx
```

### 拟新增：测试与测试工具

```text
web/src/features/auth/model.test.ts
web/src/features/auth/auth-flow.test.tsx
web/src/features/auth/session-race.test.tsx
web/src/test/setup.ts
web/src/test/server.ts
web/src/test/render.tsx
```

测试工具只存在于 `src/test`，不进入生产组件。`render.tsx` 用独立 QueryClient + MemoryRouter 包装每个用例；`server.ts` 建立 MSW setupServer；`setup.ts` 负责 cleanup、resetHandlers 和关闭 server，不用固定 sleep。

## 有序实施步骤

1. 核对 Node `>=26.0.0` 和现有工作区；安装并固定依赖，确认 npm 依赖树无 peer 冲突。用 shadcn dry-run 复核组件范围，只落五个精简组件，保留现有 `cn`。
2. 落入 F 语义变量和认证布局，先保证 Tailwind 构建及所有生成组件引用的语义 token 存在；删除生成源里的默认浅色/`dark:` 双主题和未使用变体，不增加第二套样式。
3. 实现 model 与四个 API 薄函数，先完成 unknown 收窄、email 原样通过、password 边界和错误分类测试，再接 UI。
4. 实现 QueryClient、路由、会话查询和三个 mutation 流程；接入 expires_at 到期计时、visibility 补查及 replaceSessionQuery，登录/注册/登出均在写缓存前显式取消旧查询。
5. 实现共用表单、三个页面、字段/整表错误与可访问性；注册入口始终可到达，只在真实 `REGISTRATION_CLOSED` 后提示关闭，不增加注册状态探针。
6. 用 MSW 完成主流程、错误、深链接和 deferred 竞态测试，用 Vitest 假时间覆盖到期与 visibility，不用真实等待；保留并继续运行原代理测试。更新 web README 后执行全部发布前检查。

## 验收命令

以下是**实施后的未来验收，本规划未执行**：

```sh
node --version  # 必须 >=26.0.0
npm --prefix web ci
npm --prefix web ls --depth=0
npm --prefix web run typecheck
npm --prefix web run lint
npm --prefix web run format:check
npm --prefix web test
npm --prefix web run build
GOTOOLCHAIN=local go test ./...
GOTOOLCHAIN=local go test -tags uiembed ./...
```

`npm test` 必须同时证明原 Node 代理测试和新 Vitest 测试通过；`npm ls` 不得出现 invalid/extraneous。Go 测试只作回归，不在本任务修改其实现。

## 逐项可验证验收矩阵

| 范围 | 验证方法 | 必须证明的可观察结果 |
|---|---|---|
| F 变量 | 代码检查 + build | 表中颜色、边线、字体栈和 tabular-nums 均成为语义 token；认证组件无原始色值/`dark:` 双主题，无缺失 token。 |
| 页面路由 | MemoryRouter + 请求计数 | 新 QueryClient 直接进 `/login`、`/register` 均为 **0 次 GET session**，互链为真实链接且不自动跳转；直接进 `/` 才有首次查询；未知路径到 `/`，登录/注册成功 replace 到 `/`。 |
| 登录成功 | MSW + user-event | 请求只含**逐字未改的原始 email** 和原样 password；成功后显示服务端返回的规范化用户邮箱、固定 `Personal` 和登出，不再额外 GET session。 |
| 注册成功 | MSW + user-event | 201 与登录走同一原样提交、取消旧查询、写会话缓存及首页流程；没有邮箱验证或重置密码入口。 |
| 输入边界 | model 纯函数 + 请求体测试 | 只有完全空 email 在客户端报错；带首尾空白、`İ`、组合点、U+0085、U+FEFF、希腊 sigma 上下文的 email 都逐码点原样进入请求，不在前端 trim/lower 或按格式/长度拒绝。password 覆盖 7/8/64/65 码点及 256 字节边界，不依赖 UTF-16 length，不 trim/规范化。 |
| 响应边界 | model + MSW 测试 | 成功/错误体按 unknown 校验；坏 JSON、缺 user、错误 expires_at、未知 error.code 都不崩溃，也不会被当成匿名。 |
| 提交状态 | user-event + 请求计数 | pending 有可见文字、aria-busy 和禁用；快速双击/重复 Enter 只有一个 POST，失败保留输入。 |
| 业务错误 | 参数化 MSW 测试 | 401 INVALID_CREDENTIALS、409 EMAIL_UNAVAILABLE、403 REGISTRATION_CLOSED、429 RATE_LIMITED、403 PERMISSION_DENIED 显示各自文案；未知 code 有通用兜底。 |
| 无缓存首次恢复 | 空 QueryClient + deferred GET | 首次 `/` 恰好 1 次 GET，fetch 收到 AbortSignal，pending 不展示身份；200 进入首页，401 才跳登录，500/503/网络/坏响应显示错误与手动重试。不得以“同 key 会取代”为由发起或断言第二次请求。 |
| 已有缓存重查取代 | 预置有效 session + deferred A/B | 已有缓存时，replaceSessionQuery 先 await cancel A 再启动 B：总共 2 次 GET、A.signal.aborted 为 true；B 返回 401 后再让 A 返回旧 200，仍保持匿名。无缓存 A 进行中若显式调用同一 helper，也同样只产生 A/B 两次且 A 被 abort，不发生首次去重造成的需求误判。 |
| 到期计时 | Vitest 假时间 + MSW | 200 响应按 expires_at 排计时；到期前身份可见且无额外 GET，到期后立即隐藏旧身份并恰好启动一次重查。401 后匿名；服务错显示错误与手动重试，继续推进时间/重复 visibility 不自动循环。30 天延迟不会溢出成立即请求。 |
| 到期补查与写操作 | 假时间 + visibility + pending mutation | hidden 导致计时未执行时，visible 补查一次；认证写操作 pending 到期时不启动 GET 且旧身份不可见，写操作结束后按最新 expires_at 安排，旧响应不能复活身份。 |
| 登出 | MSW + 页面测试 | 204 后进入登录页；未到期时登出失败仍显示当前用户并出现错误，不伪造成功；无 localStorage token 或持久查询缓存。 |
| 登出竞态 | deferred session + logout | 旧 session 请求尚未完成时登出，onMutate 先 abort 旧请求，204 后写匿名前再次 cancel；再返回旧 200 仍保持匿名。 |
| 登录/注册竞态 | 预置旧 session + deferred GET + mutation | mutation 发起前取消旧 GET；成功响应后、setQueryData 前再次取消。旧 GET 最后返回也不能覆盖新账号；每个用例明确记录 GET/POST 次数和旧 GET signal。 |
| 可访问性 | Testing Library 角色/标签断言 | label、字段错误关联、aria-invalid、alert、h1/main、focus ring 样式、键盘提交和首个无效字段聚焦均存在；状态不只靠颜色。 |
| 工程回归 | 上述命令 | 原代理测试、类型、lint、格式、Vitest、前端构建和 Go/uiembed 回归全绿；无 UI 自动化。 |

不设置覆盖率数字，不为每个 UI 组件写快照；测试聚焦上述真实行为与失败路径。

## 执行切分

**单块，不拆。** 设计变量、五个 UI 组件、会话 key、API 类型和页面测试构成一个紧耦合的小块；拆成视觉与认证并行会共同修改 `App.tsx`、`index.css`、依赖和锁文件，反而增加合并成本。

同一实施者拥有本计划全部文件，按“依赖/组件 → 请求模型 → 会话与页面 → 测试/文档”顺序执行。`package.json`、lockfile、`vite.config.ts` 与 `App.tsx` 不与其他块共享；无需预建共享类型、路由注册表、迁移、生成接口或空间基础层。

## 风险、未核实事项与回退

- TODO(待确认)：外部库目前只核实了官方文档、npm 版本/engines/peer/license 元数据及 shadcn dry-run，**没有在本仓共同安装、编译或运行**。Node 26 下 npm 依赖树和全部测试是最终验证。
- TODO(待确认)：React Router 8.5.1、TanStack Query 5.105.0、Vitest 5.0.5、MSW 3.0.3 的实际集成未运行；尤其 MSW 在 jsdom 中对 deferred/abort 的行为，以及假时间/visibility 与 Query 状态协作，必须由测试证明。失败时先修测试环境，不得用 sleep 或跳过竞态、到期验收。
- TODO(待确认)：shadcn 生成源只做了 dry-run 和源码审阅，精简后的五组件尚未编译；实施时须删除未使用依赖与变体，而不是保留一套未使用主题。
- TODO(待确认)：本计划未连接真实 Go 服务做人工 UI 验收，也不声称视觉、构建或认证实施已通过。现有接口行为来自源码和 OpenAPI 核对。
- 无数据库或 API 迁移。回退由主代理撤销本任务前端提交或回滚同版本前端产物/内嵌二进制；服务端认证和 Cookie 不变，不通过混用不同版本页面与 API 回退。

## 依据

以下外部资料抓取/核验日期均为 **2026-10-28**；官方文档和 npm 发布元数据为一手证据。没有采用 GitHub 参考实现，因为官方 API 已回答本任务的开放问题，无需借代码。

| 事实与采纳/拒绝 | 来源 | 状态 |
|---|---|---|
| React Router 8.5.1 支持 Node `>=22.22.0`、现有 React/类型；Declarative Mode 从 `react-router` 导入 BrowserRouter/Routes/Route。采用最小库模式。 | [安装](https://reactrouter.com/start/library/installation)、[路由](https://reactrouter.com/start/library/routing)、[发布元数据](https://registry.npmjs.org/react-router/latest) | 已核实文档与元数据；未本仓运行 |
| TanStack Query 5.105.0 支持 React 19；queryFn 提供 AbortSignal，cancelQueries 可取消请求并恢复之前状态，mutation 提供 pending/onSuccess 且默认不自动重试。采用其处理会话竞态。 | [取消](https://tanstack.com/query/latest/docs/framework/react/guides/query-cancellation)、[Mutations](https://tanstack.com/query/latest/docs/framework/react/guides/mutations)、[发布元数据](https://registry.npmjs.org/@tanstack%2freact-query/latest) | 已核实；逆序行为待本仓测试 |
| shadcn 4.23.0 dry-run 确认组件文件和生成源；最新结果使用 `cn` 与统一 `radix-ui`。拒绝重复 cn 和未使用统一 primitive 依赖，采用现有工具及独立 Slot/Label。 | [组件文档](https://ui.shadcn.com/docs/components/radix/field)、[CLI 元数据](https://registry.npmjs.org/shadcn/latest) | 已 dry-run；未生成到仓库 |
| CVA 0.7.1、Radix Slot 1.5.0、Label 2.1.17 的 peer 与现有 React 19 相容；只引入实际导入项。 | [CVA](https://registry.npmjs.org/class-variance-authority/latest)、[Slot](https://registry.npmjs.org/@radix-ui%2freact-slot/latest)、[Label](https://registry.npmjs.org/@radix-ui%2freact-label/latest) | 已核实元数据；未共同安装 |
| Vitest 5.0.5 支持现有 Vite 8、Node 26；jsdom 30.1.3 支持 Node 26，Vitest 可复用 vite.config 并设置 jsdom。采用代码级 DOM 测试，不采用 Browser Mode。 | [指南](https://vitest.dev/guide/)、[environment](https://vitest.dev/config/environment)、[Vitest 元数据](https://registry.npmjs.org/vitest/latest)、[jsdom 元数据](https://registry.npmjs.org/jsdom/latest) | 已核实；未运行测试 |
| React Testing Library 16.3.3 支持 React 19，官方要求同时安装 @testing-library/dom；user-event 用于真实交互。 | [官方安装](https://testing-library.com/docs/react-testing-library/intro)、[RTL 元数据](https://registry.npmjs.org/@testing-library%2freact/latest)、[DOM 元数据](https://registry.npmjs.org/@testing-library%2fdom/latest)、[user-event 元数据](https://registry.npmjs.org/@testing-library%2fuser-event/latest) | 已核实；未共同安装 |
| MSW 3.0.3 支持 Node `>=22.12.0`，Vitest 的 Node 进程使用 setupServer；v3 从 `msw/http` 导入 http/HttpResponse。采用 HTTP 边界 mock。 | [Quick Start](https://mswjs.io/docs/quick-start)、[发布元数据](https://registry.npmjs.org/msw/latest) | 已核实；jsdom 集成待测试 |

仓库内视觉和认证依据：[F 变量](../prototypes/dashboard-f.html#L8-L25)、[PRD 视觉与响应式](../prd.md#L182-L197)、[ADR-0004](../decisions/0004-auth-and-workspace.md#L9-L21)、[OpenAPI 认证](../openapi/openapi.yaml#L634-L655)。空间切换后置依据：[架构](../architecture.md#L35)、[PRD 后续边界](../prd.md#L217-L238)。
