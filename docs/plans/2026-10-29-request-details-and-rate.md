# T-058：请求明细、精确 60 秒速率与明细面板

> 状态：**已批准实施（2026-10-30）**。用户选择完成整个里程碑二后部署 BF，并批准第 5 节两个只读接口及复用 T-057 共享导出。T-054、T-055、T-056 已合入 main；实施必须在 T-057 两个串行块合入后开始。
> 日期：2026-10-29（按指定文件名）；仓库与外部资料核查于 2026-10-28。
> 本文件只属于规划交付，不授权编码；保留既有 `ref prd: docs/prd.md`，TODO 与提交由主代理处理。

## 1. 目标、范围与非目标

### 目标

1. 从 `usage_events` 原始请求事件按 `occurred_at` 的精确滚动 `[as_of-60s,as_of)` 计算总 Token 的 tokens/min，不用两个自然分钟拼接，不把生成速度或接收速度混进来。
2. 提供请求级、游标分页的明细查询，Provider/Model/Project 组合筛选同时作用于明细和速率；历史补报按发生时刻归属。
3. 在同源 React 面板显示最新明细、60 秒速率和有明确边界的接收新鲜度；可见页每 5 秒轮询，隐藏页暂停。
4. 全链路区分真实零、缺失、估算、部分覆盖和无事件；状态、耗时或来源能力不支持的字段显示“不可用”，不填零、不虚构请求。
5. 所有查询先经浏览器会话与 workspace 成员校验，再以认证后的用户和路径 workspace 限定 SQL；owner 与 member 都可读。

### 非目标

- 不读取分钟汇总冒充请求，不接收或拆分区间汇总，不做请求总数之外的额外推断；本任务的速率只取原始明细。
- 不做 SSE、WebSocket、服务端推送、跨请求数据库快照或“绝不漏任何实时事件”的承诺。
- 不做单请求生成速度、Prompt/回复、费用、延迟分位数、异常检测、CSV 导出。
- 不实现数据源在线/离线探针，不把 `last_received_at` 或窗口内无事件解释为在线状态；数据源健康属其他任务。
- 不重复实现 T-054 的项目/来源管理、T-055 的分钟聚合查询、T-056 的登录注册/设计基础、T-057 的当前空间/共享请求/页面总览；不做空间切换。T-058 串行消费 T-057 已合入能力，不建立平行基础。
- 不新增数据库、消息队列、状态管理或测试框架依赖；本任务没有默认数据库迁移。

## 2. 当前 HEAD 已核实事实

以下现有路径已经在主 checkout 核实，不是拟新增设计：

- 明细字段、60 秒发生时间口径、逐字段质量与保留边界继续沿用 [PRD:117-121](../prd.md#L117-L121)、[PRD:151-179](../prd.md#L151-L179) 和 [架构:52-71](../architecture.md#L52-L71)，不重开已确认产品决定。
- `usage_events` 的 `occurred_at/received_at`、可空 `status/duration_ms`、六类 Token quality 及 `(workspace_id,occurred_at,id)` 索引真实存在：[00003_usage_events.sql:6-35](../../db/migrations/00003_usage_events.sql#L6-L35)。仍先复用索引，不预设迁移。
- `sources.last_received_at` 是 source 级维护值，不是筛选后窗口新鲜度：[ingestion/service.go:251-263](../../internal/ingestion/service.go#L251-L263)。已确认的窗口内 `max(received_at)` 决策不变。
- 会话、`requireMember` 与 owner/member 读取已存在：[auth_middleware.go:97-123](../../internal/server/auth_middleware.go#L97-L123)、[workspace_access.go:28-69](../../internal/server/workspace_access.go#L28-L69)、[workspaces/service.go:86-103](../../internal/workspaces/service.go#L86-L103)。
- 项目列表和归属语义已由 T-054 合入；T-058 不需要项目管理实现：[projects.go:47-71](../../internal/server/projects.go#L47-L71)。
- 会话响应没有 workspace ID，`Personal` 仍是 HomePage 常量：[auth.go:38-43](../../internal/server/auth.go#L38-L43)、[home-page.tsx:37-62](../../web/src/features/auth/pages/home-page.tsx#L37-L62)。T-058 必须等待 T-057 的真实身份来源。
- 认证 `send` 私有且专用，退出只清会话：[auth/api.ts:11-18](../../web/src/features/auth/api.ts#L11-L18)、[auth/queries.ts:64-74](../../web/src/features/auth/queries.ts#L64-L74)。共享业务请求、401/403 和退出清理由 T-057 先行块交付。
- 实际业务挂载点是 HomePage ready 分支；Query Provider 已装配：[home-page.tsx:37-62](../../web/src/features/auth/pages/home-page.tsx#L37-L62)、[main.tsx:9-18](../../web/src/main.tsx#L9-L18)。
- Query 5.105.0、Router、Vitest/RTL/MSW/jsdom 已安装，`npm test` 已同时运行 Node 与 Vitest：[package.json:9-47](../../web/package.json#L9-L47)、[vite.config.ts:29-38](../../web/vite.config.ts#L29-L38)。本任务不新增测试框架或依赖。
- T-055 的 foreign project UUID 是空结果，T-058 已确认 foreign UUID 为 400；两者保持各自既定行为，不在本计划统一。

## 3. 前置依赖与就绪检查

### 已满足：T-054/T-056

- 成员校验、项目归属、React Router、QueryClientProvider、认证会话与测试工具均已合入；本任务不重建。
- T-056 明确不含 workspace 身份或空间切换，不能把固定 `Personal` 当成已满足。

### 必须串行等待：T-055 → T-057 先行块 → T-057 页面块 → T-058

1. T-055 合入后，T-057 先行块交付真实 current workspace、共享同源请求/错误、401/403、登录与退出业务缓存取消清理，以及总览选项来源。
2. T-057 页面块交付 HomePage ready 分支的总览挂载与规范化 Provider/Model/Project 筛选；T-058 只消费这些输出，不读取或新建全局 store。
3. `docs/openapi/openapi.yaml`、`internal/server/server.go`、`cmd/tokenpivot/main.go` 和 HomePage 在上述任务间串行移交；T-058 开工时主代理确认无其他未合入修改。
4. 先在干净实施检出运行 `make check`。专用 PostgreSQL 与 `TOKENPIVOT_TEST_DATABASE_URL`、`TOKENPIVOT_TEST_APP_DATABASE_URL` 安全注入后再跑集成测试，只引用变量名。
5. 任一前置缺失即停止，不用假 workspace、临时请求框架、Node 专用测试替代或假数据绕过。

## 4. 决策清单

### 已定决策

1. **两个只读端点**：明细列表与固定当前速率分开，避免把历史游标、任意时间范围和当前 60 秒窗口塞进一个响应。
2. **轮询，不做 SSE**：页面可见时，最新明细页和速率每 5 秒刷新；查看更早页时不自动刷新明细，速率仍刷新；页面隐藏时速率与明细轮询都暂停。现有 5–10 秒路线和“接收后 10 秒内出现”的建议目标已能满足，无需引入连接生命周期、重放和断线补发机制。此为实现取舍，**不需要用户另选轮询或 SSE**；只有提出亚秒级或无缝推送需求时才应重开。
3. **发生时间归属**：60 秒窗口和明细范围都过滤 `occurred_at`；`received_at` 只用于接收新鲜度与保留说明。早于窗口的历史补报即使现在到达也不计入当前速率；补报发生在窗口内则下次轮询正常计入。
4. **keyset 分页**：固定 `ORDER BY occurred_at DESC,id DESC`，游标含 `(occurred_at,id)` 与首页时间范围，不用 OFFSET，不返回大表 total。
5. **UTC 精确时间**：API 使用带偏移、最多 6 位小数秒的 RFC 3339；数据库用 `timestamptz`。60 秒是 60×1 秒的经过时间，不受界面时区、自然分钟或夏令时影响。
6. **逐字段质量**：明细返回 input/output/total 各自的 `{value,quality}`，不返回一个全局“完整”布尔值掩盖差异。
7. **无默认迁移**：先使用现有 `(workspace_id,occurred_at,id)`；以真实 SQL 和接近规划量的 EXPLAIN 验证，不能凭假设预建所有筛选索引。
8. **不承诺跨请求固定快照**：游标保证相同 `occurred_at` 有稳定次序，并防止头部新事件使后续页重复；翻页期间落入已越过位置的历史补报可在返回最新页刷新后出现。UI 不把分页结果称为某一时刻的完整快照。

9. **新鲜度范围（用户已确认）**：只取当前 60 秒窗口内、符合筛选且已计入速率的事件之 `max(received_at)`；窗口无事件即为未知，不查询相同筛选下全部保留明细的最后接收时间。
10. **默认明细交互（用户已确认）**：首屏默认最近 1 小时；可见最新页每 5 秒轮询，查看更早页时暂停明细自动刷新，返回最新页丢弃旧游标并重新查询；页面隐藏时暂停所有轮询。

## 5. 接口设计（拟新增，待用户批准，先改 OpenAPI）

以下两个 GET 路径、响应 schema 及 T-057 共享导出均未在当前 HEAD 存在；用户批准后才可修改接口文档与代码。

### 5.1 共用准入、筛选和错误

拟新增：

- `GET /api/v1/workspaces/{workspaceID}/usage-events`
- `GET /api/v1/workspaces/{workspaceID}/usage-rate`

两者只接受会话 Cookie，不接受插件 Bearer；GET 不要求 Origin，所有响应 `Cache-Control: no-store`。顺序为：会话 → 解析路径 UUID → workspace 成员 → 解析/校验查询参数 → 业务查询，沿用现有会话中间件；未登录且路径 UUID 非法时仍返回 401。非成员统一 `403 PERMISSION_DENIED`，未认证 `401 UNAUTHENTICATED`，非法参数 `400 INVALID_ARGUMENT`，数据库不可用 `503 UNAVAILABLE`，其他故障 `500 INTERNAL`；复用现有安全错误信封，不暴露 SQL、堆栈或跨空间资源信息。

两个端点共用重复查询参数：

- `provider=<精确值>`、`model=<精确值>`：可出现多次，同一维度 OR，不同维度 AND；保留大小写，不做别名合并。
- `project=<UUID|unattributed>`：可出现多次；`unattributed` 只表示 `project_id IS NULL`。UUID 必须属于路径 workspace，否则在成员校验之后返回统一 `400 INVALID_ARGUMENT`，不透露它属于哪个空间。
- 空筛选表示不过滤；Source 不作为本任务新增筛选维度，来源仍随每行返回。

明细额外参数：

- `start`、`end` 必须同时出现或同时省略；`start<end`，按 `[start,end)` 过滤 `occurred_at`，不要求分钟对齐，也不静默截断。
- 范围解析顺序：无 cursor 且省略 start/end 时，每次由服务端重新取 `end=as_of`、`start=end-1h`；有 cursor 且省略 start/end 时，直接采用 cursor 内首页范围，不重新计算默认值；有 cursor 且显式提供 start/end 时，按解析后的时间点与 cursor 范围比较，匹配才接受，否则返回 400。无 cursor 且显式提供范围时使用该范围。响应始终返回实际 `start/end/as_of`。
- `limit` 默认 50、最大 100；非法、超过上限或重复单值参数返回 400。该上限是接口安全限制，不是性能承诺。
- `cursor` 为不透明 base64url JSON，含版本、`occurred_at`、`id`、`start/end`。格式或时间范围不匹配返回 400。游标不是授权凭证，不需要签名；每次 SQL 仍强制当前 workspace 与当前筛选。改变筛选必须丢弃 cursor，前端负责重置。

OpenAPI 同步更新文档范围，不再笼统写“查询接口都不在本文”；只加入这两个端点及 `RequestDetailsResponse`、`RequestDetail`、`CurrentUsageRateResponse` 等 schema，不提前描述 T-055 的聚合路径。

### 5.2 明细响应

每个 item 至少包含：

```text
id, occurred_at, received_at,
provider, model,
project: {id,key,name} | null,
source: {id,platform,instance_key},
input_tokens/output_tokens/total_tokens: {value,quality},
status: string | null,
duration_ms: integer | null
```

- 项目为 null 显示“未归属”；来源 join 只取已有 `sources` 字段，不显示不存在的插件版本。
- status 为 null 显示“状态不可用”，duration_ms 为 null 显示“耗时不详/不可用”；真实 `duration_ms=0` 显示 0。
- Token `quality=missing` 时 value 必为 null，显示“不可用”；known/estimated 的 0 都显示数值 0，estimated 另带“估算”标记。前端可根据三个质量字段显示逐字段完整性，不合并成伪造总量。
- 单事件整数沿用采集端 `0..2^53-1`，JSON number 对前端安全；不在读取端重新归一化或推导 total。

列表外壳为 `{items,next_cursor,has_more,start,end,as_of,retention_cutoff}`。`retention_cutoff=as_of-180×24h` 仅说明当前按 received_at 的保留截点；当查询范围触及更老发生时段，UI 显示“旧时段可能不完整”，即使范围较新也不宣称来源覆盖完整。空 items 只表示当前保留明细中没有匹配事件。

### 5.3 当前速率响应与估算口径

响应固定包含：

```text
start, end, as_of (= end), window_seconds: 60, unit: "tokens/min",
total_tokens: {
  value: decimal-string | null,
  event_count,
  known_count,
  estimated_count,
  missing_count
},
window_last_received_at: timestamp | null
```

- `value` 是窗口内所有 known+estimated `total_tokens` 的精确和；窗口恰为 60 秒，所以该和就是 tokens/min，不再乘以 1 或按已过去秒数外推。
- 聚合值可能超过 `2^53-1`，因此 SQL 用精确 numeric 求和，响应用十进制字符串；前端用 BigInt/字符串格式化，禁止先转 Number。明细单事件值仍为安全的 JSON number。
- `event_count=0`：value=null，UI 显示“窗口内无事件”，不是 0。
- 有事件但 known_count+estimated_count=0：value=null，UI 显示“用量不可用”，并显示 missing_count。
- 有 known/estimated 值：value 为已知与估算之和；estimated_count>0 必须标“含估算”，missing_count>0 必须标“部分覆盖”。真实 known 零可以得到 value="0"。
- `window_last_received_at=max(received_at)` 只统计进入本次速率窗口且符合筛选的事件；为 null 表示“本窗口没有可用于判断接收新鲜度的事件”，不是离线。UI 同时显示窗口 end 和最后接收时间/相对间隔，不设未经确认的“新鲜/过期”阈值，不写“实时正常”。

不采用“相同筛选下全部保留明细的最后接收时间”，避免额外查询和 received_at 索引评估；也不能用 source 级 `last_received_at` 冒充筛选后的结果。

## 6. 数据库查询、分页与时间边界

### 6.1 明细查询

在 `usage_events e` 上固定携带 `e.workspace_id=$workspace`，并按需参数化 Provider/Model 数组、Project UUID 数组与 include_unattributed 布尔值。项目展示左连接 `projects`，来源连接 `sources`，两个 join 都同时匹配 workspace 和 id，不用全局 ID 关联。

核心形状：

```sql
WHERE e.workspace_id = $1
  AND e.occurred_at >= $start AND e.occurred_at < $end
  AND ($providers IS NULL OR e.provider = ANY($providers))
  AND ($models IS NULL OR e.model = ANY($models))
  AND ($no_project_filter
       OR e.project_id = ANY($project_ids)
       OR ($include_unattributed AND e.project_id IS NULL))
  AND ($cursor_is_absent OR (e.occurred_at,e.id) < ($cursor_time,$cursor_id))
ORDER BY e.occurred_at DESC, e.id DESC
LIMIT $limit_plus_one
```

实际 SQL 可调整参数编号，但必须保持参数化，不能把筛选文本拼进 SQL。取 `limit+1` 只用于 has_more，额外一行不返回；next_cursor 来自本页最后一条实际返回行。

### 6.2 速率查询

用**一条 SQL**取得数据库语句开始时间和聚合结果，避免应用时钟、窗口端点与数据快照分别采样：

```sql
WITH p AS (SELECT statement_timestamp() AS as_of)
SELECT p.as_of,
       sum(e.total_tokens::numeric)
         FILTER (WHERE e.total_tokens_quality IN ('known','estimated')),
       count(e.id),
       count(*) FILTER (WHERE e.total_tokens_quality='known'),
       count(*) FILTER (WHERE e.total_tokens_quality='estimated'),
       count(*) FILTER (WHERE e.total_tokens_quality='missing'),
       max(e.received_at)
FROM p
LEFT JOIN usage_events e
  ON e.workspace_id=$1
 AND e.occurred_at >= p.as_of-interval '60 seconds'
 AND e.occurred_at < p.as_of
 AND /* 与明细相同的参数化筛选 */
GROUP BY p.as_of;
```

不得对 sum 使用 `COALESCE(...,0)`；无 known/estimated 输入时保留 NULL。包内查询辅助函数可为集成测试传入固定 as_of，生产路径传空并使用 `statement_timestamp()`；HTTP 不接受 as_of，避免测试 seam 变成客户端能力。

精确边界用数据库微秒验证：`start` 包含，`start-1µs` 排除，`end-1µs` 包含，`end` 排除。60 秒之外、刚接收的历史事件不影响 sum 或窗口 fresh 时间。

### 6.3 索引与并发新事件

- 现有 ASC 索引可反向扫描以满足两列同时 DESC；无需仅为 DESC 重建索引。
- 相同 occurred_at 由 id 决胜；游标是严格小于，因此下一页不会重复上一页最后一行。
- 首页取得 cursor 后，即使头部插入相同或更新的 occurred_at，后续页仍从原 cursor 之后继续，不被新头部推移。
- 历史补报可能落在已经翻过的位置，本方案不提供跨请求快照；从最新页重新查询后，仍符合范围的补报可在其排序所在页查到，不保证首页展示。已离开默认滚动范围的补报需选择覆盖它的时间范围。不得用“无重复”偷换成“任何并发补报都不会漏”。
- 对最新页无筛选/组合筛选、60 秒速率、相同时间高并发数据执行 `EXPLAIN (ANALYZE,BUFFERS)`。当前没有已确认的 P95 或机器基线，计划不虚构毫秒阈值；若出现大表 Seq Scan、外部排序或不可接受耗时，先用真实参数记录，再单独提出面向访问路径的迁移，不在本任务盲目加索引。

## 7. 面板数据流、刷新与错误

1. `RequestLogPanel` 从 T-057 页面接收真实 `workspaceID` 与规范化 Provider/Model/Project 筛选；不使用 URL workspace、全局 store 或第二份筛选状态。明细默认最近 1 小时和游标仍由面板本地管理，速率始终使用当前 60 秒；不继承总览趋势粒度。
2. 共享 UI 的“未归属”选择在 T-058 API 适配层映射为 `unattributed`，不直接复用 T-055 的字面量 `unassigned`。Provider/Model 仍是独立维度，模型排名点击不会为明细伪造 Provider+Model 配对约束。
3. 请求使用 T-057 共享同源请求函数与 AbortSignal；query key 包含 workspace、端点、全部筛选、start/end、cursor。筛选或时间范围改变时清空 cursor 栈，不用旧 key 数据占位。
4. 可见最新页的明细与速率各 5 秒轮询；更早页停止明细所有自动重取而速率继续；隐藏页全部暂停；返回最新页丢弃旧游标并重取。沿用 QueryClient 已关闭的 focus/reconnect 默认行为，不新增第二套轮询器。
5. 首次失败显示错误与重试，不显示全零；后续刷新失败保留并标记旧 `as_of/end`。401/403 使用 T-057 共享处理，退出后旧面板数据与在途请求均不得恢复；400 可回最新页，网络/5xx 不自动重试。
6. “日志流式”只表示最新页新行的视觉增量，不是 SSE，不承诺跨页完整快照。旧响应是否可展示以服务端 `as_of/end` 判断，不用浏览器时钟推断新鲜度。

## 8. 精确文件范围

以下是**拟新增/拟修改**，实施前须与 T-054/T-056 实际结果核对；当前均未改动：

### 接口与 Go

- 修改 `docs/openapi/openapi.yaml`：两个端点、参数、响应与错误。
- 拟新增 `internal/requestdetails/service.go`：查询类型、筛选/游标/质量语义与小 store 接口。
- 拟新增 `internal/requestdetails/postgres.go`：参数化 SQL、DB as_of、分页与聚合计数。
- 拟新增 `internal/requestdetails/service_test.go`：参数、游标和响应语义单元测试。
- 拟新增 `internal/requestdetails/postgres_integration_test.go`：真实 PostgreSQL 边界、权限归属和并发事件测试。
- 拟新增 `internal/server/request_details.go`：会话/成员准入、HTTP 参数与响应映射、只读路由。
- 拟新增 `internal/server/request_details_test.go`：完整 mux 的认证、授权、错误与响应测试。
- 修改 `internal/server/server.go`、`internal/server/server_test.go`：Options 与路由装配、nil 安全。
- 修改 `cmd/tokenpivot/main.go`：显式装配 requestdetails store/service/adapter。

不修改 ingestion、aggregation、T-054 的项目/来源实现；无迁移时不改 `db/migrations/*`、`docs/data-model.md`。

### 前端

- 拟新增 `web/src/features/requests/api.ts`：基于 T-057 共享请求函数做 unknown 收窄、`unattributed` 适配与十进制聚合值处理。
- 拟新增 `web/src/features/requests/model.ts`：query key、cursor 状态、纯展示与轮询决策；不复制总览筛选解析。
- 拟新增 `web/src/features/requests/useRequestLogs.ts`：TanStack Query 查询与刷新。
- 拟新增 `web/src/features/requests/RequestLogPanel.tsx`：速率、新鲜度、明细、分页及错误/空态。
- 拟新增就近 `*.test.ts(x)`：复用现有 Vitest + RTL + MSW，在 HTTP 边界验证请求、渲染、竞态、取消和轮询，不再设计 Node-only 面板测试。
- 修改 `web/src/features/overview/OverviewPage.tsx`：把面板接入已合入总览并传递 workspace/筛选；若实际组合需要改 HomePage，只由本串行块局部修改，不新建路由或应用壳。

`web/package.json`、`web/package-lock.json`、Vitest 配置与共享请求/认证清理由 T-057 对应块拥有；T-058 不修改、不新增依赖。

## 9. 有序实施步骤

1. 完成第 3 节就绪检查，并取得实施审批。
2. 先修改 OpenAPI，冻结路径、筛选、游标、精度、错误和质量口径；主代理审阅后再写代码。
3. 实现 requestdetails 纯类型/校验与 PostgreSQL 查询；先写固定 as_of 的 60 秒边界、相同时间游标和缺失/估算集成测试。
4. 实现 HTTP 适配与装配，复用会话、membership、noStore 和安全错误；先完成跨空间拒绝与 owner/member 读取测试。
5. 在 T-057 共享请求与身份上实现 api/model/hooks，再实现面板；不建立并行筛选状态，不修改依赖清单。
6. 用现有 Vitest/RTL/MSW 完成慢响应、筛选重置、退出清理、轮询停止、加载失败和字段不可用测试；不做 UI 自动化。
7. 在接近每日 10 万事件、180 天约 1800 万明细的规划量级或主代理批准的代表性数据量上 EXPLAIN；记录数据量与执行计划，不把结果写成未验证的生产 SLA。
8. 跑第 10 节全部检查，只读 review 后由主代理按项目流程收尾。

## 10. 可执行验收

### 行为与测试矩阵

1. **60 秒精确边界**：固定 as_of，分别插入 start-1µs、start、end-1µs、end 的事件；只有中间两条计入，sum 与 event_count 精确。另证自然分钟不对齐时仍严格是 60 秒。
2. **历史补报**：occurred_at 早于 start、received_at 在窗口内的新到事件不计速率、不更新 window_last_received_at；发生时间落窗口内的迟到事件下一次查询计入，历史明细按 occurred_at 排序。
3. **新鲜度非在线断言**：窗口有事件时返回这些事件 max(received_at)；无事件返回 null。前端分别显示“最后接收时间”和“窗口无事件/新鲜度未知”，任何用例不得出现“在线”“实时正常”结论。
4. **零/缺失/估算/溢出**：全 known 零得到 value="0"；无事件与全 missing 都是 null 但 UI 文案不同；known+estimated+missing 同时存在时和、三类计数与“含估算/部分覆盖”正确。两个接近 `2^53-1` 的事件求和仍由十进制字符串精确呈现。
5. **相同时间稳定分页**：多条 occurred_at 完全相同时按 id DESC 且跨页无重复；limit+1 不泄露额外行，空页/末页 next_cursor 语义正确，非法或时间范围不匹配的 cursor 返回 400。推进时钟后仅携带 cursor 翻页仍成功且范围不变；显式范围与 cursor 按时间点比较；返回默认最新页重新生成最近一小时范围并能发现新事件，固定自定义范围不漂移。
6. **并发新事件**：首页后插入头部新事件及与首页末行同 occurred_at 的事件，使用原 cursor 的下一页不重复已返回 id；再插入落入已越过位置的历史补报，分别验证重新查询后：首页排序范围内的补报可见，较深位置补报需重新翻到对应页，已离开默认范围的补报不保证可见、改为覆盖它的显式范围后可查到。不错误声称固定快照。
7. **筛选一致与字面量边界**：Provider/Model 多选、Project UUID+unattributed 组合对明细和速率使用同一条件；共享未归属选择只在 T-058 适配为 `unattributed`，不改 T-055 的 `unassigned`。foreign project UUID 在成员校验后统一 400，不能降为未归属、空结果或返回他空间数据。
8. **跨空间拒绝**：owner/member 可读自己的 workspace；非成员访问路径 workspace 得到 403 且 store 未被调用；A 的会话不能读取 B 的明细或速率，join 也不能带出 B 的项目/来源名称。
9. **字段不支持**：status/duration null、各 Token missing 均显示不可用；0 不显示不可用；project null 显示未归属；不存在插件版本等字段不编造。
10. **加载失败**：首次 5xx/网络失败显示错误与重试、不显示全零；后续轮询失败保留但标记旧 as_of；401/403 走统一处理，400 可回最新页。
11. **筛选过期响应与退出清理**：A 筛选请求被延迟，切换 B 后让 A 最后返回；当前仍只显示 B，cursor 被清空，AbortSignal 传到 fetch。以不同 workspaceID 属性做缓存隔离测试即可，不新增空间切换 UI；退出后迟到响应不能复活面板数据。
12. **轮询与分页**：最新页可见时 5 秒刷新；页面隐藏时速率与明细都停止轮询；页面可见但位于更早页时停止明细的所有自动重取，速率仍可刷新。执行“更早页→隐藏→恢复可见”及网络重连测试，断言明细请求数不增加而速率恢复；返回最新页丢弃旧游标并重取。测试使用现有 Vitest 假计时器 + RTL/MSW 或 QueryObserver，不只测试纯决策函数，不用 sleep 或 UI 自动化。
13. **共享基础复用**：current workspace、401/403 与退出清理由 T-057 测试先行证明，T-058 增加面板集成断言；依赖树没有新增测试框架、Router、Query 或第二请求层。

### 真实仓库检查命令

普通检查：

```sh
GOTOOLCHAIN=local go test -mod=readonly -race -count=1 ./internal/requestdetails ./internal/server
npm --prefix web test
npm --prefix web run typecheck
npm --prefix web run lint
make check
```

数据库集成（仅在两个专用 TEST 变量已由环境安全注入时）：

```sh
GOTOOLCHAIN=local go test -mod=readonly -tags=integration -count=1 -v ./internal/requestdetails ./internal/server
make test-integration
```

执行计划使用测试库中的合成数据与参数化 SQL，经 `EXPLAIN (ANALYZE,BUFFERS)` 记录；不得在开发或生产库跑批量造数。`make check`、`make test-integration` 是仓库现有入口：[Makefile:60-87](../../Makefile#L60-L87)。以上命令均为未来验收，本次规划未执行。

## 11. 执行切分

**单块，不拆。** 两个端点共享筛选、质量和权限语义，面板同时消费它们，拆成并行后端/前端会增加接口协调与 OpenAPI 冲突。

共享基础先行顺序固定：T-054/T-056（已合入）→ T-055 → T-057 共享基础 → T-057 总览页面 → 本任务 OpenAPI → Go 查询与 HTTP → 前端面板。T-055 不是本任务的数据接口前提，但共享 OpenAPI、`server.go`、`main.go` 必须串行；T-057 是身份、请求和挂载的直接前提。T-058 唯一拥有其阶段的明细/速率 OpenAPI 与装配修改，页面块只局部接入已存在 OverviewPage/HomePage；不修改依赖清单、锁文件或 T-057 共享基础。

## 12. 风险、回退与未核实事项

- **新鲜度口径边界**：已确认的窗口口径与速率数据一致，但窗口无事件时无法说明筛选来源最近是否还有历史数据到达；UI 必须显示未知，不能用在线文案掩盖。
- **分页与补报风险**：keyset 不是跨请求快照；已明确可见性边界。如果产品要求严格固定快照，需要另行设计，不在本任务加长期事务或快照导出。
- **筛选性能风险**：现有索引以 workspace+occurred_at 开头，高度选择性项目/模型筛选可能产生残余扫描；无真实执行计划前不承诺 P95，也不预建索引。
- **公开接口批准**：第 5 节两个 GET 及响应格式仍为待用户批准；T-057 的 current workspace/共享导出未批准或未合入时，本任务不可实施。
- **依赖串行风险**：T-055/T-057 的最终代码仍需在开工时核对；不得并行争写 OpenAPI、装配文件或页面入口。
- **测试环境**：Vitest/RTL/MSW 已存在，但本面板的真实取消、假计时器和可见性组合仍须由本任务测试证明；失败时修实现或测试环境，不新增第二套框架。
- **回退**：无数据迁移，回退由主代理撤销本任务实现提交并一起回退前端/API 产物；OpenAPI 同步回退。若后续因 EXPLAIN 新增迁移，回退另按该迁移评审，不在本计划预设 Down 删除线上索引。

## 13. 用户已确认的产品决策

1. **新鲜度范围**：只取当前 60 秒窗口内、符合筛选且已计入速率的事件之 `max(received_at)`；窗口无事件即未知，不采用全部保留明细口径。
2. **默认明细交互**：首屏最近 1 小时；可见最新页 5 秒轮询，查看更早页时暂停明细自动刷新，返回最新页重新查询；页面隐藏时暂停所有轮询。

轮询 vs SSE 不需另问：本计划按已有架构选轮询；除非用户要求亚秒级或无缝推送，否则不扩大范围。

## 14. 依据

外部查证只回答数据库时间/索引顺序和现有轮询取消语义；抓取日期均为 **2026-10-28**。官方文档已回答设计问题，未浅克隆或复制 GitHub 参考实现。

| 事实、采纳或拒绝 | 来源 | 日期与状态 |
|---|---|---|
| `statement_timestamp()` 是语句开始时间，`clock_timestamp()` 会在语句执行中变化；采用单语句 statement_timestamp 固定窗口，不采用会漂移的时钟值 | [PostgreSQL 18 Date/Time Functions](https://www.postgresql.org/docs/18/functions-datetime.html) | 2026-10-28 抓取；已核实一手文档 |
| 多列 ASC B-tree 可反向扫描满足各列同时 DESC；复用现有索引，不新建仅供 DESC 的重复索引 | [PostgreSQL 18 Indexes: Ordering](https://www.postgresql.org/docs/18/indexes-ordering.html) | 2026-10-28 抓取；已核实一手文档 |
| `refetchInterval` 支持定时刷新，默认后台标签页暂停；采用 5 秒、不开 background，不需要 SSE | [TanStack Query Polling](https://tanstack.com/query/latest/docs/framework/react/guides/polling) | 2026-10-28 抓取；已核实当前 latest 文档，T-056 固定版本后须复核 |
| queryFn 可消费 AbortSignal 并传给 fetch；采用 key 隔离 + 取消处理旧筛选/旧 workspace 慢响应 | [TanStack Query Cancellation](https://tanstack.com/query/latest/docs/framework/react/guides/query-cancellation) | 2026-10-28 抓取；已核实当前 latest 文档，实际组合待项目测试 |

本轮未新增外部查证；TanStack Query 5.105.0 已在当前 HEAD 固定。仍未核实：代表性数据量下的执行计划与响应时间、T-055/T-057 最终合入形态，以及本面板测试中的定时器/取消组合。它们均已列为实施前检查，未写成既定事实。
