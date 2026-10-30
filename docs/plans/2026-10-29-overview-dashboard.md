# T-057：总览页——筛选栏、指标区、趋势图与排名

> 状态：**已批准实施（2026-10-30）**。用户选择完成整个里程碑二后部署 BF，并批准第 3.1 节两个只读接口与共享请求/缓存清理改动。T-054、T-055、T-056 已合入 main；按第 8 节 A→B→C 串行实施，T-058 接续本任务。
> 日期：2026-10-29。仅本文件属于本次规划交付；提交、TODO 更新与实施授权由主代理负责。

## 1. 目标、范围与非目标

### 1.1 目标

在 T-056 提供的已认证前端壳内交付一个真实数据驱动的总览页：

1. Provider、Model、Project 多选组合筛选；最近一小时、今天、昨天、最近七天、自定义范围；分钟/小时/天粒度；可刷新、可分享的 URL 状态。
2. 总 Token、输入 Token、输出 Token、请求数四项指标，以及明确的零值、缺失、估算和部分覆盖状态。
3. 输入与输出面积趋势及独立的服务端总量线；仅在接口证明输入、输出可加且口径一致时堆叠，并将总量标为上沿。不把缓存等子集再加进总量。
4. 项目/模型/供应商排名切换、点击筛选和“其他”展开。
5. 显示当前个人空间、时区、服务端实际统计范围、统计截点、保留截点和页面更新时间；不显示接口无法提供的来源健康或延迟。

### 1.2 非目标与任务边界

- **T-055**：聚合与排名接口、聚合及权限校验；其既定范围不含 Provider/Model 完整选项。T-057 页面块只消费定稿接口，不另定聚合口径。
- **T-056**：已交付 F 设计变量、登录注册、QueryClientProvider、React Router 和认证请求/会话竞态处理；没有当前 workspace 或业务共享请求层。T-057 先行块只补本阶段真实缺口，不重建这些基础。本期只有 `Personal` 个人空间，不做空间切换。
- **T-058**：滚动 60 秒速率、tokens/min、请求明细、日志流及明细页查询。本页不显示原型中的 60 秒卡片。
- **T-042**：完整响应式重排、统一动效和减少动态效果的最终验收。T-057 仍保证基本键盘可用、文本状态和不依赖颜色的含义，但不建设通用响应式/动效体系。
- 不做趋势框选联动明细、模型别名合并、费用/预算、延迟分布、状态码、缓存经济性、热度格、额外 sparkline、数据源在线判断、顶部横向滚动事件条。原型中的示例数字、示例空间名和额外图表一律不迁移。
- 除第 3.1 节经用户批准的最小只读接口外不改后端、数据库或认证规则；不新增多空间界面，不执行浏览器 UI 自动化验收。

## 2. 当前 HEAD 已核实事实

| 已核实现状 | 证据 | 最小补齐结论 |
|---|---|---|
| T-054/T-056 已合入；当前 HEAD 为 `4c1d602`，T-055 不在主 checkout | `git log`；本计划不读取其独立工作区 | 先等 T-055 合入并核对其 OpenAPI，再开始 T-057 |
| 会话成功体只有 `user` 与 `expires_at`；`Personal` 只是页面常量 | [auth.go:38-43](../../internal/server/auth.go#L38-L43)、[model.ts:11-14](../../web/src/features/auth/model.ts#L11-L14)、[home-page.tsx:37-62](../../web/src/features/auth/pages/home-page.tsx#L37-L62) | 需要服务端返回真实 workspace ID/名称，不能把常量变成查询 ID |
| 个人空间读取 SQL 已在注册事务适配器内部，但公开 Service 只有成员角色查询 | [postgres.go:18-23](../../internal/workspaces/postgres.go#L18-L23)、[service.go:75-103](../../internal/workspaces/service.go#L75-L103) | 在 workspaces 增加一个小的当前个人空间读取能力；不改注册事务与成员规则 |
| 项目列表已是 owner/member 可读的 GET，顶层数组含归档项目 | [projects.go:47-71](../../internal/server/projects.go#L47-L71)、[OpenAPI:859-892](../openapi/openapi.yaml#L859-L892) | 直接复用，不新增项目选项接口 |
| `send` 私有且认证专用；业务 401/403 没有共享处理 | [api.ts:11-18](../../web/src/features/auth/api.ts#L11-L18) | 新建一个薄的同源业务请求模块，不让认证 API 承担业务职责 |
| 退出成功只取消会话查询并写 `null`，未清业务缓存 | [queries.ts:64-74](../../web/src/features/auth/queries.ts#L64-L74) | 退出开始时取消业务请求，成功后移除业务缓存；失败保留页面与数据 |
| 实际业务挂载点是 `HomePage` 的 `ready` 分支；Query Provider 已在入口 | [home-page.tsx:37-62](../../web/src/features/auth/pages/home-page.tsx#L37-L62)、[main.tsx:9-18](../../web/src/main.tsx#L9-L18) | 不新增路由、平行应用壳或 Provider |
| Query 5.105.0、Router 8.5.1、Vitest/RTL/MSW/jsdom 和测试脚本已存在 | [package.json:9-47](../../web/package.json#L9-L47)、[vite.config.ts:29-38](../../web/vite.config.ts#L29-L38) | 不重复添加测试基础、Vitest 配置或 Router；T-057 只增加 Recharts |
| T-055 已定模型排名键为 `(provider,model)`，Provider/Model 筛选仍是独立维度；未归属值为 `unassigned` | [T-055 计划:82-88](2026-10-29-aggregate-query-api.md#L82-L88)、[T-055 计划:144-150](2026-10-29-aggregate-query-api.md#L144-L150) | 点击模型行只切换 Model；不自动添加 Provider；沿用 `unassigned` |
| T-055 趋势不生成空桶、不填零，仅返回有数据桶 | [T-055 计划:115-120](2026-10-29-aggregate-query-api.md#L115-L120) | 前端必须显式保留缺口，不能只设置 `connectNulls={false}` |
| T-055 响应提供 `range/stats/partial`，没有来源覆盖、来源最后接收或延迟字段 | [T-055 计划:181-223](2026-10-29-aggregate-query-api.md#L181-L223) | 页面显示实际范围/as_of/保留截点；不虚构来源健康或延迟 |

## 3. 前置与先行块交付

### 3.1 待用户批准的公开接口与共享导出

以下均为**拟新增/拟修改，不是当前已有能力**，实施前须由用户明确批准并先改 OpenAPI；不得直接写代码：

1. `GET /api/v1/workspaces/current`：Cookie 会话认证，返回 `{workspace_id,name}`。在每人仅一个初始 Personal 空间的既定阶段，复用“最早 owner 空间”的现有读取口径；本期成员仍可读取其个人空间数据。找不到属于数据完整性故障，返回安全 `500 INTERNAL`，不创建空间、不返回伪空；401/500/503 沿用现有认证错误边界，成功与失败均 `no-store`。未来多空间上线时必须重审，不能把它当空间列表或切换接口。
2. `GET /api/v1/workspaces/{workspaceID}/usage/options`：会话 + `requireMember`，无查询参数，返回 `{providers:[string],models:[string]}`。每次请求只采样一次 `as_of`，复用 T-055 的 `RetentionCutoff(as_of)`，仅扫描获准 workspace 内 `bucket_start >= retention_cutoff AND bucket_start < as_of` 的分钟统计；不得依赖尚未实现的物理清理。Provider 与 Model 分别去重、稳定排序，不静默截断，保持两个独立筛选维度；项目不进入此端点。空数组不是失败；跨空间身份不泄露。无筛选选项时显示空态，不从排名反推“完整选项”。
3. `web/src/lib/api.ts` 导出共享 `getJson/post` 薄函数与带 `status/code/details: unknown` 的错误类型：同源 Cookie、Accept、AbortSignal、unknown JSON 与安全错误解析；401 进入统一匿名清理，403 显示无权限且清除业务数据，400/网络/5xx 交给功能区域展示。总览适配层负责收窄 `max_points`、`suggested_granularity`、`retention_cutoff`，展示限制与调整建议，不增加通用错误框架。它不包含 schema 库、重试框架或自动刷新。
4. `web/src/features/auth/queries.ts` 增加功能内部业务缓存清理：logout `onMutate` 在发出 POST 前 `cancelQueries` 排除 `sessionKey`，`onSuccess` 再取消并 `removeQueries` 排除会话后写 `null`；登录/注册成功采用同一清理顺序，防止跨账号残留。业务 401 或既有会话重查确定匿名时，统一取消在途会话与业务查询、移除业务缓存，再写入匿名状态；保留现有登录/退出的会话取消逻辑。已取消或旧身份请求不得触发迟到的 401/403 清理副作用，不得恢复旧会话或清除新登录。不要 `clear()` 掉整个 QueryClient，也不要让取消拒绝阻止退出请求。

接口形态批准后，T-055 已合入的 OpenAPI、`internal/server/server.go` 与 `cmd/tokenpivot/main.go` 由 T-057 先行块在 T-055 之后**串行修改**；T-058 再串行消费，禁止并行争写。

### 3.2 已满足与待满足依赖

- T-054：现有项目列表、workspace 归属与 owner/member 读取已满足；无需新增管理接口。
- T-056：现有 Router、Query、认证、测试工具、语义 token 和 Button/Alert/Field 等已满足；总览所需多选、Tabs、Card、Badge、Empty、Skeleton 等只按实际渲染需要补充，不预建组件大全。
- T-055：合入后核对 aggregate/rankings 正式路径、字段、示例、限制和错误；前端严格按实际文档适配。它不交付第 3.1 节两个选项/身份接口。
- 任一条件未满足即停止相应实施块，不使用假 workspace、假选项、假数据或第二请求层绕过。

## 4. 设计决策

### 4.1 功能目录保持局部，不新增全局状态层

拟新增生产文件：

```text
web/src/features/overview/OverviewPage.tsx
web/src/features/overview/OverviewFilterBar.tsx
web/src/features/overview/OverviewMetrics.tsx
web/src/features/overview/OverviewTrendChart.tsx
web/src/features/overview/OverviewRanking.tsx
web/src/features/overview/overviewFilters.ts
web/src/features/overview/overviewApi.ts
web/src/features/overview/overviewQueries.ts
web/src/features/overview/overviewView.ts
```

职责：

- `overviewFilters.ts`：规范化筛选类型、URL 解析/序列化、时间范围计算与校验；不依赖 React。
- `overviewApi.ts`：唯一读取 T-055 与项目/选项接口结构的位置，基于先行块共享 `getJson` 完成 unknown 收窄和视图模型转换；组件不直接 `fetch`，不拼鉴权头。
- `overviewQueries.ts`：查询键、查询函数、取消和可见页刷新配置；所有影响结果的输入都进键。
- `overviewView.ts`：数字/时间格式化、质量和空态文案映射、趋势与排名展示数据；不重新计算后端指标口径。
- 组件只负责各自交互与展示；`OverviewPage.tsx` 组合筛选、指标、趋势、排名和状态行，导出给 T-056 的挂载点。

不引入全局 store、第二请求层、通用图表封装层或新的路由库。以后若换图表库，改动主要局限在 `OverviewTrendChart.tsx` 和视图模型，不影响筛选、API 与排名。

### 4.2 筛选与 URL：严格解析，非法输入不发查询

建议的页面筛选模型包含：去重并规范排序后的 Provider/Model/Project 标识数组、时间预设或固定自定义范围、粒度、IANA 时区。排名 tab 和“其他”展开是局部 UI 状态，不进入全局筛选。

- 多值参数使用重复键，例如 `provider=a&provider=b`，避免逗号等分隔符与合法值冲突；实际键名可由 T-056 的 URL 约定调整，但语义不变。
- 枚举只接受 `1h|today|yesterday|7d|custom` 和 `minute|hour|day`。单值参数重复、未知枚举、非法时间、非法时区、超过 T-055 数量/长度上限均使页面进入“链接筛选无效”状态，保留 URL 供检查，提供重置按钮，**不发出聚合请求，也不静默改成别的筛选**。
- 与总览无关的 URL 参数保留。总览自身参数写回时去重、规范排序；除时区外的默认值可省略。筛选连续变化使用 replace 语义，避免每次勾选都制造一条浏览器历史。
- URL 中不出现 workspace、用户、凭证或项目展示名等不必要信息。有效但当前选项列表里不存在的标识不擅自丢弃；交由服务端判定权限/有效性，界面只显示已获授权的展示名。
- URL 缺少时区时，仅首次进入取浏览器 `Intl.DateTimeFormat().resolvedOptions().timeZone` 并显示，再以 replace 语义规范写回 URL。所有预设与自定义范围的可分享 URL 均保存已解析的 IANA 时区，不因它等于本机默认值而省略，确保不同浏览器时区读取同一链接时统计范围和分桶语义一致；相对预设仍以打开时刻解析，不冻结分享时刻。本块不新增时区管理界面。

**以下为建议的默认与点击行为，不是用户已确认的产品决定：**

- 默认“今天 + 小时”，空选择表示全部 Provider/Model/Project。默认值只存在于 `DEFAULT_OVERVIEW_FILTERS`，以后改动不影响 API。
- 排名默认“项目”。点击普通排名行是在该维度的现有多选集合中切换选中/取消；已选筛选始终有可删除的文本标记，因此排名被自身筛选收窄后仍可撤销。点击“其他”只展开/收起长尾，不设置筛选。切换排名维度或改变筛选时收起旧长尾。

### 4.3 时间与时区边界

- 所有计算统一 `[start,end)`，分钟对齐，自定义 `start < end`。秒/毫秒输入不被静默截断；URL 非法时拒绝，表单输入时提示用户确认改成明确的分钟值。
- `1h`、`7d` 是滚动相对时长；`today` 是所选时区当前自然日开始，`yesterday` 是完整上一自然日。相对范围的结束包含当前未完成分钟，页面根据服务端统计截点标记它，而不是把未来时间当成已有数据。
- 相对范围在页面可见且跨到新分钟时重新解析起止；URL 仍保存预设，不写成每次轮询都变化的绝对时间。自定义范围保存固定、带 UTC 偏移的 RFC 3339 时间。
- 自定义本地时间必须带明确偏移；偏移须与所选 IANA 时区在该时刻一致。不存在的本地时间拒绝；DST 重复小时由不同偏移区分，不能仅按本地小时文本合并。具体请求编码服从 T-055，转换只放在筛选/API 适配边界。
- 粒度和时间范围都由用户显式选择。遇到点数/时长上限时展示接口给出的限制与可选调整，不自动把分钟改小时或缩短范围。
- 横轴标签按服务端桶唯一时间标识和所选时区格式化；DST 重复小时带 UTC 偏移，日标签不假定每天 24 小时。显示服务端实际范围、统计截点与时区。

### 4.4 指标、趋势与排名只呈现真实口径

**指标区**：总 Token、输入 Token、输出 Token、请求数四张卡。

- 已知 `0` 显示 `0`；缺失/不支持显示 `—` 和“缺失/不可用”；估算显示数值和“估算”；服务端声明部分覆盖时显示“部分数据”。
- 总量不由前端拿缺失的输入/输出强行相加；请求数不可用时绝不填零。缓存、推理等子集不进入这四项计算。
- 页面更新时刻、服务端统计截点与保留截点分别命名，不混成“实时”，不显示接口未提供的来源最后接收或延迟。

**趋势图**：采用 Recharts + T-056 体系内的 shadcn chart 组件。

- 总量 Line 直接读取服务端 total，不由前端相加生成。仅在接口证明输入、输出可加且口径一致时使用同一 `stackId` 堆叠两个 Area，并可标明“总量（上沿）”；输入/输出缺失、质量不一致或 total 不等于两项之和时，保留各自有效数值与独立总量线，面积改为不堆叠，明确质量/差异，缺失分量不补零。不把总量作为第三个堆叠 Area 重复累计。
- 接口没有返回的桶不是已知零，也不是单个指标缺失：相邻返回点的 `previous.bucket_end !== next.bucket_start` 时，视图模型在两点间加入 `input/output/total=null` 的缺口项，再设 `connectNulls={false}`，使所有系列断段且不填零；不重新枚举完整桶序列，也不能仅把稀疏响应直接交给图表。真实返回的零保持零；已完成且 `request_count="0"`、指标全缺失的桶显示“该桶用量不可用”，不宣称完整无事件。未完成桶用文本/图案标记，估算与质量在 tooltip 中说明。
- 横轴、tooltip 和图例使用所选时区及实际桶标识；图表使用语义色 token、等宽数字、明确高度和 `accessibilityLayer`。本块关闭主动入场动效，后续由 T-042 统一处理。
- 不加缓存面积、热度图、速率 sparkline、框选或示例脉冲。

**排名**：项目/模型/供应商 Tabs，服务端按总 Token 口径排序并返回占比。

- 普通行是带可访问名称的按钮，展示名称、字符条/比例、Token 与质量；百分比使用接口结果，不在缺失总量时自行补算。
- “其他”展开只请求当前维度、当前筛选的长尾；折叠不修改筛选。长尾行同样可点击，未归属项目只在接口明确返回该分类时显示。
- 模型行按 T-055 的 `(provider,model)` 键区分和展示。点击模型行只在独立 Model 筛选中切换该行的 `model` 值，**不自动添加 Provider**；同名模型若已被另一 Provider 的筛选排除，结果显示当前组合筛选无数据，用户可调整独立 Provider 筛选。

### 4.5 请求并发、缓存与权限隔离

- 查询键至少包含：`workspaceID`、规范化 Provider/Model/Project、解析后的 `start/end`、时区，以及该资源实际使用的粒度、排名维度、长尾游标/限制。相对预设名本身不能替代已解析时间。
- 汇总、趋势、排名按 T-055 的真实资源分别使用查询；排名 tab 只改变排名查询，不无效化汇总/趋势。展开“其他”只增加对应长尾查询。
- 所有请求函数把 TanStack Query 的 `AbortSignal` 传给请求层。快速连续筛选、收起页面或组件卸载时按 Query 机制取消/忽略旧请求；不手写加载四件套，不增加自行实现的并发队列。
- 不同查询键之间不复用 `placeholderData`，避免把上一筛选或上一空间的数据当成新结果。相同键后台刷新可保留旧数据并显示“更新中/上次成功时间”。
- 页面可见时按架构建议使用 10 秒刷新；隐藏页不轮询，重新可见时刷新。相对时间跨分钟导致新键，分钟内部只重取当前键。10 秒不是端到端时效承诺，也不是 T-058 的滚动 60 秒查询。
- 认证和真实 workspace 未就绪、URL 非法时禁用查询。401/403 使用 T-057 先行块共享请求与清理机制；400 展示可修正的筛选错误，不自动重试。网络/5xx 由功能区域显示重试，沿用 QueryClient 默认不自动重试，不叠加第二套策略。
- 权限仍由服务端执行。筛选选项、排名和项目名称只能来自已授权响应；解析失败或越权时不展示缓存中的同名元数据。

### 4.6 加载、空、错误和部分失败

- 汇总、趋势、排名各自有 Skeleton、错误 Alert + 重试和 Empty，不让单个资源失败抹掉其他已成功区域。
- 筛选选项失败只禁用对应选项编辑并提示，已确定的合法筛选和现有数据仍可展示；不能把它伪装成“没有选项”。
- 同键后台刷新失败且有旧数据时保留旧数据、显示“更新失败，展示上次成功结果”和时间；不同键首次失败不显示旧筛选数据。
- 响应 schema 校验失败按数据错误展示并记录可定位的安全错误信息，不把缺失字段当作零。
- 空态区分“完整范围内无用量”“当前筛选无结果”“数据缺失/不可用”“无权限”。无权限不使用普通空态文案。

## 5. 依赖与文件范围

- T-057 唯一新增运行依赖 `recharts@3.10.1`，由页面块串行修改 `web/package.json` 与 `web/package-lock.json`；现有 Query/Router/Vitest/RTL/MSW 不重复添加。若缺 shadcn chart 包装，只添加实际使用的 `web/src/components/ui/chart.tsx`。
- 先行块拟改：`docs/openapi/openapi.yaml`、`internal/workspaces/service.go`、`internal/workspaces/postgres.go` 及测试、`internal/usage` 中选项查询的小模块及测试、`internal/server` 当前空间/选项 handler 与测试、`internal/server/server.go`、`cmd/tokenpivot/main.go`、`web/src/lib/api.ts`、`web/src/features/workspace/api.ts` 与 `queries.ts`、认证请求/会话清理及测试。
- 页面块拟新增 `web/src/features/overview/**` 与就近测试；复用现有测试 setup/render。拟修改 `web/src/features/auth/pages/home-page.tsx`，仅在 ready 分支挂载总览、保留登出和错误状态；不改 `App.tsx` 路由。

## 6. 有序实施步骤

1. **合并核对**：T-055 合入后，主代理确认 OpenAPI、装配文件无未合并改动；复跑现有检查。先取得第 3.1 节批准。
2. **T-057 先行块**：先改 OpenAPI，再实现 current workspace 与 usage/options 的查询、权限和错误；随后实现共享请求、401/403 与登录/退出清理及测试。合入先行块并通过合并验证后才派页面块。
3. **筛选与 API**：完成 URL、默认时间/时区/DST、规范化筛选测试；接项目列表、Provider/Model 选项与 aggregate/rankings，查询键含真实 workspace 和全部输入。
4. **页面区域**：依次实现筛选栏、四项指标、趋势和排名；完成稀疏桶断段、质量、空态、错误与取消测试。
5. **局部挂载**：由 T-057 页面块在 HomePage ready 分支挂载，不另建壳。完成全部发布前检查；真实视觉/数据由用户人工验收，不做 UI 自动化。
6. **T-058 串行接续**：先行块与页面块合入后，T-058 再修改共享 OpenAPI/server/main/HomePage，并复用同一请求、身份与筛选输入。

## 7. 验收命令与测试矩阵

```sh
GOTOOLCHAIN=local go test -mod=readonly -race -count=1 ./internal/workspaces ./internal/usage ./internal/server
npm --prefix web ci
npm --prefix web run typecheck
npm --prefix web run lint
npm --prefix web run format:check
npm --prefix web test
npm --prefix web run build
make check
```

数据库验收仅在专用 `_test` 数据库及 `TOKENPIVOT_TEST_DATABASE_URL`、`TOKENPIVOT_TEST_APP_DATABASE_URL` 安全注入后执行；不输出变量值、不在开发或生产库造数：

```sh
make test-integration
```

普通 `make check` 不执行真实 PostgreSQL 集成检查，不能替代上述数据库验收。

| 范围 | 自动验证 | 通过标准 |
|---|---|---|
| 当前空间 | Go 会话/服务测试 + MSW HomePage | ID/名称来自服务端；无 workspace 不查询；失败不创建空间、不显示伪 Personal ID |
| Provider/Model 选项与项目 | 真实 PostgreSQL 集成测试 + MSW/RTL | 截点前排除、等于截点包含、as_of 及之后排除；跨空间隔离、重复值去重、稳定排序、空结果正确；项目来自 T-054；空数组、失败与普通无用量可区分 |
| 共享请求与退出/换账号清理 | deferred MSW + 页面测试 | AbortSignal 到底层 fetch；业务 401 与会话重查返回匿名均取消会话/业务查询并移除业务缓存；403 无权限；退出成功缓存不可见、失败保留页面；旧或已取消请求的 401/403 与迟到成功均不能恢复旧会话或清除新登录 |
| 查询限制与错误 details | MSW 400 + RTL 展示测试 | 保留并安全收窄 max_points、suggested_granularity、retention_cutoff；超点数、保留期错误展示可操作建议，不静默改筛选或粒度 |
| 多选、URL、时间/DST | 纯函数 + RTL + 请求参数断言 | 合法分享恢复筛选/时区；非法 URL 不发请求；预设/自定义边界与粒度不被静默改写 |
| 指标质量 | MSW known/zero/estimated/missing | 零不写成缺失，缺失不填零；请求数与 total 来自响应，无前端伪造 |
| 趋势稀疏桶与口径 | 视图模型 + 图例/tooltip/断点断言 | 缺席桶产生断段且所有系列不填零；真实零保留；total 独立取响应；仅同口径时堆叠输入/输出 |
| 排名点击与其他 | RTL + MSW | 模型行只切 Model，不联动 Provider；project 未归属用 `unassigned`；foreign project UUID 保持 T-055 的本空间空结果，不改造成 400；other 仅展开当前维度长尾 |
| 加载/错误/竞态/刷新 | 分区故障、延迟旧响应、假计时器与 visibility | 分区错误互不明显抹除；旧响应不覆盖新筛选；可见 10 秒、隐藏暂停；无 60 秒请求 |
| 权限与范围 | 401/403/foreign ID、挂载检查 | 服务端权限不被 URL 绕过；退出无业务残留；无空间切换、明细或原型假数字 |

## 8. 执行切分与共享文件责任

**两块串行，不再把共享基础留作假设。**

| 顺序/块 | 文件范围 | 验收与责任 |
|---|---|---|
| A：T-057 共享基础 | 第 5 节列出的 current/options Go/OpenAPI、共享请求、认证清理 | 同一实施者；先合入并全量验证。唯一拥有本阶段 OpenAPI、`server.go`、`main.go` 与认证请求/清理文件 |
| B：T-057 总览页面 | `web/src/features/overview/**`、必要 UI/chart、HomePage 局部挂载、Recharts 依赖与锁文件 | A 合入后开始；同一实施者；不修改 T-055 聚合实现或 T-058 端点 |
| C：T-058 | 其计划中的明细/速率与面板 | B 合入后串行开始；消费 A 的请求/身份/清理，不争写 B 的筛选状态 |

主代理负责按 A→B→C 派发、审查与合并；共享文件不并行修改。没有数据库迁移或生成代码；锁文件仅 B 修改，主代理无需合并后重生成。

## 9. 风险、待批准与回退

- **必须用户批准**：第 3.1 节两个 GET 接口、AuthSession 不动而新增 current workspace 的选择，以及共享请求/清理导出。未批准前计划不可实施。
- T-055 的实际合入响应仍是实施前核对项；本计划只依据其已确认计划，不声称其代码已完成。
- Recharts 版本沿既有研究，当前只新增它；真实兼容以 npm/构建/测试为准。数据规模与部署性能不扩大为本任务新指标。
- current workspace 的“最早 owner 空间”只适用于当前单 Personal 空间阶段；多空间是 T-020 的重新设计点，不预建切换。
- 回退由主代理按块撤销实现提交；只读接口与前端无数据迁移，不用假数据兜底。

## 10. 依据

本轮没有新增外部事实或外部调研；所有新决定均由上述当前 HEAD 代码与 T-054/T-055/T-056 既定文档支持。Recharts 3.10.1、TanStack Query 取消/键语义与 Recharts 断点/堆叠沿用本计划 2026-10-28 已核实的官方资料：[Area](https://recharts.github.io/en-US/api/Area/)、[Query Keys](https://tanstack.com/query/latest/docs/framework/react/guides/query-keys)、[Query Cancellation](https://tanstack.com/query/latest/docs/framework/react/guides/query-cancellation)。它们不证明本仓集成已通过，实际验收以第 7 节为准。
