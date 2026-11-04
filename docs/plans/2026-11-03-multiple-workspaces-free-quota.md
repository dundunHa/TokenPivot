# T-064：多工作区与免费用户创建额度

- 日期：2026-11-03
- 状态：需求已确认，计划可实施
- 目标：注册后从零工作区开始，由用户手动创建工作区；同一免费账户最多创建 2 个，并提供创建、列表、切换能力。
- 实施边界：本计划只描述代码、迁移和文档改动；规划阶段不实现代码、不操作数据库。

## 1. 设计结论

1. 工作区没有 `personal/team` 类型，也没有“个人工作区”特殊分支。
2. 每个已登录账户最多成功创建 2 个工作区，首个同样计入额度；额度是当前业务常量 `freeWorkspaceLimit = 2`，不增加套餐、价格或付费配置。
3. 注册事务只创建用户和会话。注册、登录后都通过工作区列表判断零空间状态，由前端强制打开创建 modal。
4. 工作区创建者与成员角色分开：`created_by_user_id` 只负责额度和同账户名称唯一；`workspace_memberships` 仍是请求授权的唯一角色来源。
5. 当前工作区只是前端 URL 选择状态，不写入用户、会话或数据库。切换前用工作区详情接口验证成员资格；每个业务请求仍独立校验成员资格。
6. 保持现有 PostgreSQL 18、应用事务和索引约束方式，不新增 FK、CHECK、trigger、RLS。额度由 `users` 行锁串行化创建，名称由唯一索引兜底。
7. 名称采用**大小写敏感**的精确唯一，不做小写化或 Unicode 规范化；排序采用数据库 `COLLATE "C"` 的字节顺序，再以 UUID 排序保证稳定。

## 2. 现状与冲突

| 现状证据 | 当前行为 | 本次处理 |
|---|---|---|
| `docs/prd.md:36-43` | 租户隔离规则中限定注册自动创建唯一 Personal | 保留隔离要求，改为零空间注册和手动创建 |
| `docs/prd.md:219` | 多空间切换被排除 | 更新为本期范围；`docs/prd.md:230-240` 的跨空间隔离、慢响应验收继续有效 |
| `docs/decisions/0004-auth-and-workspace.md:15,19,25` | 注册事务创建 Personal，当前空间取最早 owner 空间 | 由本次决策取代；`:7,17` 的隔离和前端缓存约束保留 |
| `db/migrations/00002_foundation_tables.sql:4-8`、`docs/data-model.md:30-31` | `workspaces` 只有 `id/name/created_at`，名称不唯一，也没有创建者 | 修改既有迁移，增加创建者及唯一索引 |
| `internal/workspaces/service.go:118-146`、`internal/workspaces/postgres.go:16-25` | `CreatePersonal` 复用或创建最早 owner 空间 | 改为普通手动创建、列表和按 ID 查询 |
| `internal/auth/service.go:119-147,191-219`、`internal/auth/postgres.go:79-95,118-126` | 注册事务依赖 `CreatePersonal` | 删除注册到工作区的写依赖，保留用户和会话原子性 |
| `internal/server/current_workspace.go:32-58`、`docs/openapi/openapi.yaml:861-892` | 只有 `GET /workspaces/current`，零空间返回 500 | 用列表、创建、详情接口替换；空列表是正常状态 |
| `web/src/features/workspace/api.ts:25-39`、`queries.ts:8-29` | 前端只查询 `/current` | 改为列表、创建、详情查询与 mutation |
| `web/src/features/overview/OverviewPage.tsx:62-95` | 页面自行获取唯一当前空间，零空间会进入错误分支 | 由工作区容器决定 `workspaceId`，零空间只展示创建 modal |
| `web/src/features/auth/pages/home-page.tsx:52-53`、`web/src/features/overview/geek/GeekHeroMetrics.tsx:80` | 固定显示 Personal | 移除硬编码，由切换器展示真实名称 |
| `internal/server/workspace_access.go:15-25,47-79` | 所有业务路由统一经 `MembershipRole` 校验 | 保持不变，作为持续约束和回归验收依据 |

## 3. 范围与非范围

### 范围内

- 修改既有 schema 和迁移，按最新结构重建数据库，不做旧数据保留。
- 工作区创建、列表、按 ID 读取（供切换验证）API 及 OpenAPI 文档。
- 注册后零空间、创建 modal、工作区切换器、无空间/重名/超额/慢切换状态。
- 保持所有业务查询按工作区隔离、owner 可管理、member 只读、插件凭证固定到工作区和 source。
- 更新受 schema、注册事务和接口变化影响的测试夹具。

### 非范围

- 删除、重命名、邀请、成员列表或成员管理。
- 付费、套餐、计费、额度配置后台，以及为未来付费预留的扩展框架。
- 工作区类型字段、默认工作区服务端设置、跨账户转移。
- 旧数据回填、升级兼容、数据迁移脚本。
- 新增前端组件库或状态库。
- 修改未提交的原型和依赖文件：`docs/prototypes/index.html`、`docs/todo.md`、`docs/prototypes/dashboard-g.html`、`dashboard-h.html`、`dashboard-i.html`、`dashboard-j.html`、`plugins/dsh/node_modules/`、`plugins/dsh/package-lock.json`。

## 4. 创建、列表、切换接口

### 4.1 接口与表示

| 方法与路径 | 用途 | 成功响应 |
|---|---|---|
| `GET /api/v1/workspaces` | 列出当前会话用户作为成员可访问的工作区 | `200`，JSON 数组；无成员关系时为 `[]` |
| `POST /api/v1/workspaces` | 当前用户手动创建工作区，同时创建 owner 成员关系 | `201`，返回新建工作区 |
| `GET /api/v1/workspaces/{workspaceID}` | 读取切换目标并验证当前用户成员资格 | `200`，返回目标工作区 |

工作区响应保持最小字段：

```json
{
  "workspace_id": "018f2f2a-0000-7000-8000-000000000000",
  "name": "Alpha"
}
```

创建请求：

```json
{
  "name": "Alpha"
}
```

- 列表直接返回数组，与现有项目管理列表风格一致；不增加分页，因为单账户本期最多手动创建 2 个。
- 列表通过 `workspace_memberships.user_id` join `workspaces`，按 `name COLLATE "C" ASC, id ASC` 排序。
- 详情复用 `requireMember`/`MembershipRole` 后再读取指定行；非成员与不存在的工作区都不返回资源内容。
- 移除 `GET /api/v1/workspaces/current` 及 `CurrentWorkspace` schema，前端与接口文档在同一改动中切换，不保留一个会任意选择“最早空间”的兼容层。
- 切换没有服务端会话写入。详情接口成功只表示目标可访问；前端随后修改 URL 中的 `workspace` 参数。

### 4.2 名称规则

服务端在事务外先完成纯输入校验，前端仅做相同规则的即时提示：

1. 使用 Go `strings.TrimSpace` 去除首尾 Unicode 空白，数据库保存修剪后的名称。
2. 修剪后为空返回 `400 INVALID_ARGUMENT`。
3. 长度为 1–80 个 Unicode code point，同时 UTF-8 编码不超过 256 字节；任一超限均为 `400`。
4. 拒绝 Unicode control character；名称中间的普通空白允许。
5. 不做大小写折叠，不做 NFC/NFKC 等规范化。`Alpha` 与 `alpha` 可共存；只有修剪后字节完全相同的名称冲突。
6. 排序完全使用服务端返回顺序，前端不再用 `localeCompare` 重排。

现有 `validManagementLabel` 会拒绝首尾空白而不是修剪（`internal/server/management_json.go:127-142`），不能直接复用为工作区名称规则。抽取严格 JSON 读取的公共部分，保留 project/source 原校验行为，为 workspace 使用独立名称校验。

### 4.3 错误语义

| 情况 | HTTP / code | 前端处理 |
|---|---|---|
| 请求体、JSON 字段、名称格式或 UUID 不合法 | `400 INVALID_ARGUMENT` | 创建时显示字段错误并保留输入；切换时显示目标参数错误 |
| 无有效会话 | `401 UNAUTHENTICATED` | 使用现有认证失效流程 |
| 创建 POST 的 Origin 校验失败 | `403 PERMISSION_DENIED` | 显示无权限；详情 GET 沿用现有读路由，不校验 Origin |
| 详情目标不存在或当前用户非成员 | `403 PERMISSION_DENIED` | 不切换，保留当前空间并提示 |
| 同账户名称重复 | `409 WORKSPACE_NAME_CONFLICT` | modal 保持打开，显示“名称已存在”，保留输入 |
| 已达到 2 个 | `409 WORKSPACE_LIMIT_REACHED` | modal 保持打开，显示额度提示，不再次自动提交 |
| 成员角色或其他数据库完整性/查询失败 | `500 INTERNAL` | 显示可重试错误，不切换或关闭 modal |
| 会话存储后端不可用 | `503 UNAVAILABLE` | 沿用现有暂时不可用提示 |

- 在 OpenAPI 的 management error code 枚举和固定 message 表中增加两个 409 code。
- `POST` 不引入幂等键。按钮 pending 防止重复提交；网络结果未知后的重试由唯一索引阻止重复创建，可能得到名称冲突。
- 唯一冲突只把 `uk_workspaces_created_by_user_id_name` 映射为 `WORKSPACE_NAME_CONFLICT`；成员表等其他唯一冲突仍按内部错误处理。

## 5. 注册与零空间流程

### 后端

1. `RegistrationStore` 删除 `CreatePersonal`，`registrationTxStore` 不再组合 `workspaces.Tx`。
2. 注册仍在原事务内依次 `CreateUser`、`CreateSession`；密码哈希仍在事务外，注册开关行锁和失败回滚规则不变。
3. 注册成功只建立 cookie 会话，不创建工作区或成员关系；`users/sessions` schema 和 `AuthSession` 响应不变。
4. 原注册四阶段失败测试改为用户、会话两阶段；集成断言从 1 工作区/1 成员改为 0/0。

### 前端

1. 注册成功仍导航到 `/`，但首页先查询工作区列表，不渲染任何工作区业务请求。
2. 列表 loading 显示状态；列表 error 显示错误和重试，不把加载失败误判成零空间。
3. 列表为 `[]` 时打开不可关闭的创建 modal：无关闭按钮、Escape/遮罩不关闭；创建成功前不挂载 Overview。
4. 首个创建成功后失效列表查询、选择新 ID、关闭 modal，再加载该工作区业务数据。
5. 已有 1 个或 2 个空间时，切换器提供“创建工作区”入口，此 modal 可取消；超限由服务端 409 最终裁决，前端不把列表长度当作额度来源。

## 6. 最新 schema、迁移与并发保证

### 6.1 修改既有迁移

在 `db/migrations/00002_foundation_tables.sql` 的 `workspaces` 定义中：

```sql
created_by_user_id uuid NOT NULL,
name text COLLATE "C" NOT NULL
```

新增：

```sql
CREATE UNIQUE INDEX uk_workspaces_created_by_user_id_name
    ON workspaces (created_by_user_id, name);
```

- 保留 `id` UUID v7 默认值和 `created_at`。
- `created_by_user_id` 不加 FK；其索引前缀同时支持额度计数。
- 不为额度增加 CHECK 或计数表，不增加 trigger/RLS。
- `workspace_memberships` 结构不变；现有 `(workspace_id,user_id)` 唯一索引继续保证角色唯一，`user_id` 普通索引支持列表。
- 更新 `00006_workspace_memberships.sql` 中 Personal 注释，使其描述手动创建时的 owner 成员关系。
- 所有直接 `INSERT INTO workspaces` 的测试夹具同步提供 `created_by_user_id`，不能使用空 UUID 或随机默认值绕过真实结构。

### 6.2 创建事务

`workspaces.Service` 统一持有角色读取和事务运行依赖，PostgreSQL 实现保留默认 `READ COMMITTED`。每次创建在一个短事务中固定执行：

1. `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`，锁定当前账户；用户不存在则失败且不写入。
2. `SELECT count(*) FROM workspaces WHERE created_by_user_id = $1`；结果大于等于 2 时返回额度错误。
3. 插入 `workspaces(created_by_user_id,name)`，依赖唯一索引兜底同账户重名。
4. 插入 `(workspace_id,user_id,'owner')` 成员关系。
5. 任一步失败全部回滚，事务内不发起网络或其他外部调用。

所有应用创建工作区的路径都经过同一事务和同一锁顺序。移除注册自动创建后，不存在第二条绕过锁的写入路径。不同用户锁不同 `users` 行，可并行创建。

并发成立的理由：

- 同一用户两个创建请求在步骤 1 串行；后获得锁者看到前一个已提交计数，因此最多两个成功。
- 同名校验不是“先查后插”作为唯一防线；最终写入始终受 `(created_by_user_id,name)` 唯一索引约束。
- 工作区行和 owner 成员行同一事务提交，不暴露只有工作区没有成员关系的中间状态。

### 6.3 列表与详情

- 列表只按成员关系返回当前用户可访问空间，不用创建者列代替授权。
- 创建者列不参与运行时角色判定；即使未来出现其他成员关系，owner/member 权限仍只来自 `MembershipRole`。
- 详情先经过现有 `requireMember`，随后按已验证的 workspace ID 查询名称；孤儿成员关系等完整性异常返回 500。

## 7. 会话、角色与跨空间隔离的最小改动

- 不修改 session cookie、session 数据表、登录/登出接口或 `AuthSession`。
- 不修改现有 project/source/credential/request/usage 路由形状；它们继续从路径取得 `workspace_id` 并调用统一成员角色校验。
- owner 继续可写，member 继续只读；创建新空间是账户级动作，任何已登录用户都可创建并成为新空间的 owner，与其在旧空间的 member 角色无关。
- 插件凭证仍固定到工作区和 source；本次不增加凭证移动或共享能力。
- 前端所有 workspace-scoped 查询继续同时包含 `userId`、`workspaceId` 及各自过滤条件；不能用工作区列表的用户级 key 替代业务数据 key。

## 8. 前端交互与慢响应处理

### 8.1 组件归属

- 新增 `WorkspaceHome`：查询列表、解析/选择当前 ID、渲染切换器和创建 modal；列表未就绪或为空时不挂载 Overview。
- 新增 `WorkspaceSwitcher`：展示真实当前名称、列表和创建入口。
- 新增 `WorkspaceCreateDialog`：使用现有 Button/Field/Input/Alert，以条件渲染和 `role="dialog" aria-modal="true"` 实现，不新增依赖。
- `HomePage` 用 `WorkspaceHome` 替换直接挂载 `OverviewPage`，删除静态 Personal 行。
- `OverviewPage` 改为接收明确的 `userId`、`workspaceId` props，不再请求 `/current`；`GeekHeroMetrics` 删除硬编码 Personal，避免复制工作区状态。

### 8.2 URL 选择与查询状态

1. 当前 ID 存在 `?workspace=<uuid>`；过滤参数更新继续保留该参数。
2. URL 无有效成员工作区时，使用服务端排序后的第一项作为当前项，并规范化 URL；空列表不选择。
3. 列表查询 key 为 `["workspace","list",userId]`；详情 key 为 `["workspace","detail",userId,workspaceId]`。
4. 切换详情查询设置局部 403 标记（如 `meta.local403`）：`createAppQueryClient` 对该标记的 403 不执行全局 `dropBusiness`，由切换器就地显示错误；401 及其他业务查询的现有全局清理行为不变。
5. 所有现有 scoped query factory 补齐 `meta.workspaceId`，便于按旧空间取消；原 key 中的过滤条件和 `AbortSignal` 传递保持不变。
6. 点击切换时先取消上一个详情验证请求；详情成功后取消旧 `workspaceId` 的业务查询，再更新 URL。切换期间防止重复选择。
7. 业务区域以 `workspaceId` 作为 React key，切换时卸载旧组件；不使用旧 key 的 `placeholderData`，因此旧空间数据不会作为新空间占位。
8. 旧响应即使晚到，也只能写入包含旧 `workspaceId` 的缓存项，且不得再次设置当前 URL 或当前空间状态。
9. 创建成功后失效 `["workspace","list",userId]`；已有空间的业务缓存不被拿来充当新空间数据。

### 8.3 表单和异常状态

- 提交中禁用输入与提交按钮，防止重复点击。
- 重名、超限、校验失败、500/503 均保留名称输入；只有创建成功才关闭 modal。
- 切换失败保留旧空间和旧 URL；目标详情返回 403 时，旧空间的业务数据、当前 URL 和工作区列表缓存均保持，仅切换器显示局部错误；切换慢时显示 pending，不提前展示目标空间数据。
- A→B→C 快速切换时，后一次取消前一次详情验证，只有最后成功且仍对应当前选择的响应能更新 URL。

## 9. 数据库重建与验证

1. 旧数据明确不保留：实施时选一个新的空 PostgreSQL database/schema，通过现有 `cmd/tokenpivot-migrate` 的 `up` 子命令应用全部修改后的 migration；不在旧库执行回填或升级兼容。
2. 用迁移入口的 `status` 子命令确认全部版本已应用，再启动后端。
3. 实际重建环境的 DSN、服务停止/启动方式由实施环境提供，当前未核实，记为 **TODO(待确认)**；规划阶段不执行数据库命令。
4. 自动验证使用集成测试现有的随机空 schema 和全量 migration 流程，覆盖“最新迁移可从零建立”的要求。
5. 运行 `make test-integration` 与 `make check`；不编造仓库中不存在的 `migrate reset` 命令。

## 10. 文档同步

- `docs/prd.md`：更新注册、多个工作区、2 个额度、名称规则和零空间流程；保留隔离及慢响应验收。
- `docs/data-model.md`：更新 workspaces 列、唯一索引、创建事务、列表关系及“注册唯一写路径”的旧描述。
- `docs/architecture.md`：更新注册调用链、工作区 API、前端 URL 选择和切换取消规则。
- `docs/openapi/openapi.yaml`：新增三个端点、Workspace schema、两个 409 code；删除 `/current` 与旧完整性错误说明。
- `docs/decisions/0004-auth-and-workspace.md`：标记 Personal 自动创建和唯一当前空间条款被 0006 取代，保留仍然有效的隔离条款。
- 新增 `docs/decisions/0006-multiple-workspaces-free-quota.md`：记录本次已确认决策、大小写敏感取舍、行锁与唯一索引理由，以及不做付费框架。

## 11. 测试与逐项验收

| 验收项 | 必须得到的自动化证据 |
|---|---|
| 注册零空间 | service 测试证明写顺序仅 CreateUser→CreateSession；PostgreSQL 集成测试证明 users/sessions 各 1、workspaces/memberships 各 0；cookie 行为不变 |
| 首个创建 | 201 返回修剪后的名称和 ID；数据库恰有 1 行及 owner 成员；列表立即可见且计入额度 |
| 名称规则 | 表驱动覆盖 TrimSpace、全空白、80/81 code point、256/257 UTF-8 字节、control、中间空白；`Alpha`/`alpha` 可共存；规范化不同形式不做额外等价处理 |
| 同账户重名 | 顺序及并发创建同名只能成功 1 次；其余为 `409 WORKSPACE_NAME_CONFLICT`，最终仅 1 行 |
| 最多 2 个 | 同一用户至少 8 个不同名称并发创建，最终恰有 2 个工作区、2 个 owner 成员，其余均为额度错误；不同用户互不占额度 |
| 创建原子性 | 注入工作区插入、成员插入失败，均无部分行残留；非目标唯一冲突不误报为重名 |
| 列表 | 空数组、仅本人成员空间、C 排序和 UUID tie-break；不返回其他账户私有空间 |
| 创建接口鉴权 | 无会话 401；错误 Origin 403；写请求顺序及现有会话异常映射不变 |
| 切换接口 | 成员 200；无会话 401；非法 UUID 400；不存在/非成员 403；角色查询异常 `500 INTERNAL`；会话后端异常 503；详情 GET 不校验 Origin |
| 角色与隔离回归 | 现有 owner 可写/member 只读、非成员 403、跨空间 credentials/source/project/usage/ingestion 测试继续通过 |
| 零空间 UI | 注册落地后列表为空时 modal 出现且不可关闭；未创建前无业务 API 请求；首个创建成功后关闭并选中 |
| 创建异常 UI | 重名、超限、校验失败均保留输入和 modal；pending 期间一次点击只发一个请求；成功后列表失效 |
| 慢切换 UI | 用可控 deferred 响应让 A 请求晚于 B 返回，断言 A 的 signal 被取消、最终只显示 B，A 数据不覆盖 B；快速 A→B→C 只由最后目标更新 URL |
| 切换 403 UI | A→B 的详情验证返回 403 后，A 的 URL、业务数据和空间列表保持，仅显示 B 的局部错误；其他业务 403 的全局清理回归测试继续通过 |
| 查询隔离 | 测试检查 scoped key 含 userId/workspaceId/过滤条件；不使用旧 key 占位；过滤参数变化保留 workspace 参数 |
| 最新 schema | 随机空 schema 跑全量 migration 成功；所有测试夹具提供真实创建者；schema 中无新增 FK/CHECK/trigger/RLS |
| 文档一致 | OpenAPI、PRD、数据模型、架构和 ADR 均不再描述注册自动创建 Personal 或 `/current` 零空间 500 |

最终发布前检查：

```text
make test-integration
make check
```

验收只做代码层和自动化测试，不进行 UI 自动化操作。

## 12. 执行切分（文件不相交）

### A. 共享基础，必须先串行完成

定义最新 schema、领域接口和 OpenAPI，后续所有块从这一版开始：

- `db/migrations/00002_foundation_tables.sql`
- `db/migrations/00006_workspace_memberships.sql`
- `internal/workspaces/service.go`
- `internal/workspaces/postgres.go`
- `internal/workspaces/service_test.go`
- `internal/workspaces/postgres_integration_test.go`
- `docs/openapi/openapi.yaml`

完成标准：迁移从零可建、创建并发/重名/额度集成测试通过，接口 DTO 和错误码固定。

### B. 注册解耦，可在 A 后并行

- `internal/auth/service.go`
- `internal/auth/postgres.go`
- `internal/auth/service_test.go`
- `internal/auth/postgres_integration_test.go`

### C. 后端 HTTP 与装配，可在 A 后并行

- `cmd/tokenpivot/main.go`
- `internal/server/server.go`
- `internal/server/current_workspace.go` → `internal/server/workspaces.go`
- `internal/server/current_workspace_test.go` → `internal/server/workspaces_test.go`
- `internal/server/management_json.go`
- `internal/server/auth_test.go`
- `internal/server/usage_test.go`
- `internal/server/management_test.go`
- `internal/server/credentials_test.go`
- `internal/server/request_details_test.go`

保持 `requireMember`、`requireOwner` 和现有业务 handler 不变；仅调整工作区 service 构造所必需的测试装配。

### D. 前端，可在 A 的 OpenAPI 固定后并行

- `web/src/features/workspace/api.ts`
- `web/src/features/workspace/queries.ts`
- `web/src/features/workspace/WorkspaceHome.tsx`（新增）
- `web/src/features/workspace/WorkspaceSwitcher.tsx`（新增）
- `web/src/features/workspace/WorkspaceCreateDialog.tsx`（新增）
- `web/src/features/workspace/WorkspaceHome.test.tsx`（新增）
- `web/src/features/workspace/workspace-cache.test.tsx`
- `web/src/features/auth/queries.ts`（仅增加切换详情局部 403 分支，保持其他全局清理行为）
- `web/src/features/overview/OverviewPage.tsx`
- `web/src/features/overview/overviewQueries.ts`
- `web/src/features/overview/overview-page.test.tsx`
- `web/src/features/overview/geek/GeekHeroMetrics.tsx`
- `web/src/features/requests/useRequestLogs.ts`
- `web/src/features/requests/request-panel.test.tsx`
- `web/src/features/auth/pages/home-page.tsx`
- `web/src/features/auth/pages/register-page.tsx`
- `web/src/features/auth/auth-flow.test.tsx`
- `web/src/features/auth/session-race.test.tsx`

### E. 跨模块 PostgreSQL 测试夹具，可在 A 后并行

- `internal/projects/postgres_integration_test.go`
- `internal/credentials/postgres_integration_test.go`
- `internal/sources/postgres_integration_test.go`
- `internal/ingestion/postgres_integration_test.go`

这些文件只补齐 `created_by_user_id` seed，不改业务逻辑。

### F. 文档，可与 B–E 并行，最后按实现核对

- `docs/prd.md`
- `docs/data-model.md`
- `docs/architecture.md`
- `docs/decisions/0004-auth-and-workspace.md`
- `docs/decisions/0006-multiple-workspaces-free-quota.md`（新增）

### 集成顺序

1. A 先完成并合入共享基础。
2. B、C、D、E、F 按上述文件边界并行；任何新增共享文件先停下重新划分，避免跨块修改同一文件。
3. 合入后运行完整后端、前端、PostgreSQL 集成测试，重点联调注册→零空间→首建→第二空间→切换→超额。
4. 对照第 11 节逐项核对证据；未执行或未通过的条目明确保留为未验证，不以子任务完成说明代替验收。
