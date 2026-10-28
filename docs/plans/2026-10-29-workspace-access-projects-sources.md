# T-054：空间权限、项目与数据来源管理

- 日期：2026-10-29
- 状态：规划定稿，独立文档评审意见已修正并经主代理核验；待实施授权，本文件不代表功能已实现
- 范围：合并原 T-021，包含项目/来源 CRUD、来源归档与凭证写入并发保护
- 固定决策：保留 `docs/decisions/0004-auth-and-workspace.md` 的认证、成员、Origin 和 Personal 空间边界
- 下游：为 T-055 聚合查询、T-058 明细提供空间资源和权限入口，但不实现二者

## 1. 目标与非目标

### 目标

1. 浏览器对目标空间的每次业务请求都复用唯一成员角色入口 `WorkspaceService.MembershipRole`；owner 可管理，member 只读，非成员无读写权限，数据库故障不能变成非成员。
2. 提供空间内项目、数据来源的创建、列表、修改和归档接口；保留历史项目、来源、事件、分钟聚合和凭证记录，不物理删除、不重新归属。
3. 所有项目/来源 SQL 同时限定资源 ID 与 `workspace_id`；跨空间资源对已授权调用方也表现为不存在。
4. 所有浏览器写操作先执行严格 Origin 检查：缺失、不匹配、`null` 或多个 Origin 均拒绝；读接口不要求 Origin。
5. 补齐来源归档与凭证签发/轮换的竞态：来源归档提交后，不能再签发或轮换出可用凭证；历史行和令牌值不因归档被改写或删除。
6. OpenAPI 接口文档先行，处理器、真实 PostgreSQL、事务与越权测试共同作为验收依据。

### 非目标

- 不实现团队、邀请、成员管理、角色编辑、多空间切换 HTTP API；仍只有 Personal 空间。
- 不实现 T-055 聚合查询、T-058 明细查询或前端页面。
- 不实现项目/来源物理删除、恢复归档、跨空间移动、级联删除、历史重新归属或备份导出。
- 不改变项目既有归档后仍可接收事件的语义。
- 不新增认证方式、外部权限库、通用 JSON 框架、依赖注入框架、消息队列或创建请求幂等键。
- 不改变 DSH/PI 采集的固定凭证认证、1 MiB 请求限制和错误语义，也不让插件请求回退到 Cookie 或空间成员权限。
- 不承诺归档响应返回时，所有归档前已完成认证的采集请求都已结束。

## 2. 现状与证据

| 已有事实 | 证据 | 对本方案的影响 |
|---|---|---|
| 唯一角色查询入口为 `MembershipRole(ctx,userID,workspaceID)(role,found,error)`；非成员是 `found=false`，未知角色和数据库故障是错误 | `internal/workspaces/service.go:48-54`、`:86-103` | 项目、来源、凭证共用这一入口；不得另写角色 SQL |
| 会话中间件只建立浏览器用户身份，不了解目标空间；凭证列表、签发、轮换、吊销已经 owner-only | `internal/server/auth_middleware.go:97-123`、`internal/server/credentials.go:65-110`、`:179-202` | 各处理器先取会话身份，再查目标空间角色；角色判断收口为共享小助手 |
| 写路由先 RequireOrigin 再 RequireSession，读路由只 RequireSession；Origin 缺失或不匹配为 403 | `internal/server/auth_middleware.go:83-123`、`internal/server/credentials.go:65-86` | 新写接口复用现有中间件，保持 Origin→Session→Membership |
| 会话存储/续期故障经 `auth.ErrUnavailable` 返回 503 `UNAVAILABLE`；成员角色与凭证管理数据库故障返回 500 `INTERNAL` | `internal/server/auth_middleware.go:97-123`、`internal/server/auth.go:266-288`、`internal/server/credentials.go:179-202`、`:242-255`、`docs/openapi/openapi.yaml:940-943` | 新接口和测试必须区分两类依赖故障，不能都写成 500 或把数据库故障当成非成员 |
| 四张基础表是 `workspaces`、`projects`、`sources`、`ingest_credentials`，有主键和唯一索引但没有外键；项目/来源唯一约束名分别为 `uk_projects_workspace_id_project_key`、`uk_sources_workspace_id_platform_instance_key` | `db/migrations/00002_foundation_tables.sql:1-47`、`docs/data-model.md:19-23`、`:32-38` | 新增两个小包和来源归档列，不重做表结构；所有关联完整性与 workspace 校验由应用保证 |
| 凭证按 `(workspace_id,source_id)` 归属，摘要列是 `secret_hash`；Issue 只给已有来源创建凭证，不会自动创建项目或来源，Rotate 沿用旧凭证的 source_id，Revoke 只写 `revoked_at` | `db/migrations/00002_foundation_tables.sql:27-46`、`internal/credentials/service.go:127-172` | 新来源由本任务显式创建；归档必须保留凭证，轮换不可改绑来源，签发/轮换需与归档串行 |
| 采集认证按 `secret_hash` 和 `revoked_at` 读取固定 workspace/source；Bearer 失败不回退 Cookie，认证后才进入事件处理 | `internal/credentials/service.go:90-109`、`internal/ingestion/handler.go:95-127`、`:150-161` | 来源归档要撤销 active 凭证；普通采集请求不增加成员查询 |
| 项目归档不改变事件业务身份，归档项目仍通过归属检查；`usage_minute_stats` 保留 workspace/source/project 维度 | `internal/ingestion/postgres.go:101-112`、`db/migrations/00004_usage_minute_stats.sql:6-12`、`docs/data-model.md:32`、`:44` | 不因项目归档修改事件，不重写分钟聚合 |
| 撤销后新认证返回 401；已经认证的请求继续使用所得归属处理，`TouchSource` 可随后更新 `last_received_at` | `internal/credentials/service.go:90-109`、`internal/ingestion/handler.go:118-152`、`internal/ingestion/postgres.go:76-82` | 来源归档沿用同一边界，并明确归档后可能再更新 `last_received_at` |
| 实际历史表为 `usage_events` 与 `usage_minute_stats`；服务角色对前者只有 SELECT/INSERT，对后者有 SELECT/INSERT/UPDATE，均无 DELETE | `db/migrations/00003_usage_events.sql:6-47`、`db/migrations/00004_usage_minute_stats.sql:6-49`、`docs/data-model.md:69`、`:88` | 归档验收必须检查历史仍在，不能通过级联删除“简化” |
| 已有事务内行锁和 READ COMMITTED 设计模式 | `internal/workspaces/postgres.go:61-88`、`docs/decisions/0004-auth-and-workspace.md:23-25` | 归档与凭证写入直接使用 pgx 事务，不引入新事务库 |
| 仓库当前验证入口明确 | `Makefile:62-87` | 只使用已查证的 `make check`、`make test-integration`，不编造 lint/OpenAPI 命令 |

## 3. 权限、Origin 与错误矩阵

### 3.1 浏览器公共顺序

所有新管理处理器按以下顺序执行：

1. 写请求先 `RequireOrigin`；GET 跳过 Origin。
2. `RequireSession` 得到用户 ID；无有效会话返回 401。
3. 解析路径 `workspace_id`，无效 UUID 返回 400。
4. 调用 `requireMember` 或 `requireOwner`，两者内部只能调用 `MembershipRole`：
   - `roleError`：500 `INTERNAL`，不得写成 403；
   - `found=false`：403 `PERMISSION_DENIED`；
   - owner-only 遇到 member：403 `PERMISSION_DENIED`。
5. 通过角色检查后，才解析资源 UUID、请求体并访问目标资源；跨空间资源一律 404，不返回真实 owner、名称或归属空间。

共享助手放在 `internal/server/workspace_access.go`，与 `membershipUserID` 一起从凭证处理器移入；`Credentials` 改为调用同一助手。保留现有 `writeError`、`writeJSON`、`RequireOrigin`、`RequireSession`、`requireEmptyBody`，不另建响应体系。

### 3.2 路由与角色

| 方法与路径 | owner | member | 非成员 | 匿名 | Origin |
|---|---|---|---|---|---|
| `GET /api/v1/workspaces/{workspace_id}/projects` | 200 | 200 | 403 | 401 | 不检查 |
| `POST /api/v1/workspaces/{workspace_id}/projects` | 201 | 403 | 403 | 401 | 严格检查 |
| `PATCH /api/v1/workspaces/{workspace_id}/projects/{project_id}` | 200 | 403 | 403 | 401 | 严格检查 |
| `DELETE /api/v1/workspaces/{workspace_id}/projects/{project_id}` | 204 | 403 | 403 | 401 | 严格检查 |
| `GET /api/v1/workspaces/{workspace_id}/sources` | 200 | 200 | 403 | 401 | 不检查 |
| `POST /api/v1/workspaces/{workspace_id}/sources` | 201 | 403 | 403 | 401 | 严格检查 |
| `PATCH /api/v1/workspaces/{workspace_id}/sources/{source_id}` | 200 | 403 | 403 | 401 | 严格检查 |
| `DELETE /api/v1/workspaces/{workspace_id}/sources/{source_id}` | 204，并撤销 active 凭证 | 403 | 403 | 401 | 严格检查 |
| 现有 `GET .../sources/{source_id}/credentials` | 200 | 403 | 403 | 401 | 不检查 |
| 现有签发、轮换、吊销 | 保持 owner-only；签发/轮换额外要求 active 来源 | 403 | 403 | 401 | 严格检查 |

Origin 缺失、不匹配、`null` 或多个值都为 403；匿名与无 Origin 的写请求仍为 403，因为 Origin 先于会话。所有响应保持 `Cache-Control: no-store`。

### 3.3 错误口径

| 情况 | HTTP / code | 说明 |
|---|---|---|
| 路径 UUID、JSON、字段或空请求体不符合要求 | 400 `INVALID_ARGUMENT` | 使用固定消息，不回显敏感请求值 |
| 请求体超过 16 KiB | 413 `BODY_TOO_LARGE` | 与采集 1 MiB 限制相互独立 |
| 非 JSON Content-Type | 415 `UNSUPPORTED_MEDIA_TYPE` | DELETE 要求空体，不接受 JSON 参数 |
| 无有效会话 | 401 `UNAUTHENTICATED` | 仅在 Origin 通过后到达会话检查 |
| 非成员或 member 执行管理写 | 403 `PERMISSION_DENIED` | 不区分未加入与角色不足 |
| 项目、来源、凭证不存在或属于另一空间 | 404 `NOT_FOUND` | 资源 SQL 必须同时带 workspace_id |
| 项目键、来源身份唯一冲突 | 409 `CONFLICT` | 分别映射两个既有唯一约束，其他 23505 不伪装成业务冲突 |
| 向已归档来源签发/轮换凭证 | 409 `CONFLICT` | 固定消息 `source is archived`；在 owner 校验后返回 |
| 会话存储或续期依赖故障 | 503 `UNAVAILABLE` | 由 RequireSession/writeServiceError 返回；不得降级为匿名、非成员或 500 |
| 成员角色、项目、来源或凭证管理数据库故障 | 500 `INTERNAL` | 不泄露 SQL、约束细节、令牌或跨空间归属；不得写成会话 503 |

OpenAPI 的凭证错误枚举需补 `CONFLICT`；新增管理错误 schema 覆盖上表实际会返回的码，包括 503 `UNAVAILABLE`。每个受会话保护的新路由都要同时声明 500 与 503 响应，并注明 503 仅对应现有会话依赖故障；现有凭证路由继续保留 `CredentialUnavailable`。已归档来源的凭证列表仍返回历史记录，凭证吊销在已归档来源上仍允许执行。

## 4. 接口与数据语义

### 4.1 通用 JSON 规则

项目/来源请求体上限统一为 16 KiB。新增包内小助手复用采集的严格解码原则：拒绝非 UTF-8、未知字段、重复键、尾随 JSON、`null` 字段和错误类型；不为本期重构现有 auth/credentials 解码器。

`project_key`、`name`、`platform`、`instance_key` 均采用当前 ingestion `validLabel` 的可观察规则：1–128 UTF-8 字节、不含控制字符、不允许首尾 Unicode 空白；不做静默 trim 或大小写归一化。`name` 也暂用这一最小规则，避免新增第二套长度口径。平台不新增 DSH/PI 白名单，因为统一采集和表结构允许其他 platform；DSH/PI 自身限制仍由采集接口负责。

### 4.2 项目

- `POST` 请求：`{"project_key":"...","name":"..."}`，两者必填；workspace 只取路径，创建后固定。
- `PATCH` 请求：只允许 `{"name":"..."}`。本期不可修改 `project_key`，避免标识变化影响外部引用；如未来需要，再单独评估历史与展示语义。
- `DELETE`：空体；把 `archived_at` 设为当前时间，保留行。
- 响应字段：`project_id`、`workspace_id`、`project_key`、`name`、`created_at`、`archived_at`。
- `POST` 201 返回完整对象；`PATCH` 200 返回完整对象；列表按 `created_at,id` 升序，空列表为 `[]`，包含已归档项目。

### 4.3 来源

- `POST` 请求：`{"platform":"...","instance_key":"..."}`，两者必填；workspace 只取路径，创建后固定。创建来源不会自动创建凭证，owner 随后调用现有签发接口。
- `PATCH` 请求：只允许 `platform` 和 `instance_key`，至少一个，未提供字段保持不变。二者只修改机器标识；`source_id`、workspace、事件归属、凭证绑定和令牌均不变。插件仍用原令牌，采集凭据继续指向同一 source_id。
- `DELETE`：空体；在同一事务中归档来源并撤销其 active 凭证，成功 204 无响应体。
- 响应字段：`source_id`、`workspace_id`、`platform`、`instance_key`、`created_at`、`last_received_at`、`archived_at`。
- `POST` 201、`PATCH` 200 返回完整对象；列表按 `created_at,id` 升序，空列表为 `[]`，包含已归档来源及其状态。
- 时间输出与凭证一致，使用 RFC3339；可空时间为 `null`。归档时间用 `COALESCE` 保留第一次值。

### 4.4 删除、重复请求与历史

用户在 T-054 决策中选择方案 A：项目/来源 HTTP DELETE 均执行保留历史的归档；项目仅归档，来源归档同时撤销 active 凭证。归档后归档时间不变，重复归档成功且不返回新令牌；本期没有恢复接口。

| 重复/并发情形 | 结果 |
|---|---|
| 重复 GET | 无副作用，包含归档行 |
| 重复 POST 同一 project_key 或来源身份 | 不是幂等创建；一个 201，其余 409，不自动返回已有行 |
| 重复 PATCH 相同值 | 幂等，200 返回当前对象 |
| 重复 DELETE 项目 | 204，第一次归档时间不变 |
| 重复 DELETE 来源 | 204，第一次归档时间不变；已撤销凭证不重置撤销时间 |
| 删除后再创建相同键 | 因既有唯一索引包含归档行，仍 409；不能绕过唯一约束制造两个同键行 |
| 归档已有事件/聚合的项目或来源 | 行、事件、分钟聚合和归属完全保留 |
| 归档仍有凭证的来源 | active 行仅写 `revoked_at`；已吊销行不变；令牌 hash 和 source_id 不变 |
| 归档前已认证的采集请求 | 允许完成并写历史；可能在归档后更新来源 `last_received_at` |
| 归档提交后的新采集认证 | revoked 凭证返回 401；归档前从未签发的新来源也没有可用凭证 |

这些语义必须同步到 `docs/prd.md`、`docs/data-model.md`、`docs/architecture.md` 和 ADR 0004；OpenAPI 明确 DELETE 不是物理删除，避免调用方误以为可重新创建同键资源。

## 5. SQL、事务与关联设计

### 5.1 迁移

新增 `db/migrations/00007_source_archival.sql`：

```sql
ALTER TABLE sources ADD COLUMN archived_at timestamptz;
```

- 可空、无默认值、无回填，旧行表示 active。
- 不加普通索引：本期只有个人空间小列表，归档列不是过滤条件；未来出现按状态过滤和实际慢查询再评估。
- 不改 `projects`、`usage_events`、`usage_minute_stats`、`ingest_credentials`，不新增外键或级联动作。
- `db/migrations/embed.go` 的 `*.sql` 会自动包含新文件，无需改注册代码。
- 迁移编号 00007 已由主代理评审确认可用。

### 5.2 项目/来源服务与存储

新增 `internal/projects` 和 `internal/sources`，沿用现有分层：HTTP 负责认证授权和输入，service 负责业务错误与事务顺序，postgres 文件负责 SQL。ID 使用现有 `pgtype.UUID`，时间使用 `time.Time`/`pgtype.Timestamptz`，不新建通用 ID 或模型包。

核心服务接口：

```go
// projects；ctx 为 context.Context，ID 均为 pgtype.UUID
List(ctx context.Context, workspaceID pgtype.UUID) ([]Project, error)
Create(ctx context.Context, workspaceID pgtype.UUID, projectKey, name string) (Project, error)
Update(ctx context.Context, workspaceID, projectID pgtype.UUID, name string) (Project, error)
Archive(ctx context.Context, workspaceID, projectID pgtype.UUID) error

// sources；使用同一组现有类型
List(ctx context.Context, workspaceID pgtype.UUID) ([]Source, error)
Create(ctx context.Context, workspaceID pgtype.UUID, platform, instanceKey string) (Source, error)
Update(ctx context.Context, workspaceID, sourceID pgtype.UUID, platform, instanceKey *string) (Source, error)
Archive(ctx context.Context, workspaceID, sourceID pgtype.UUID) error
```

各包提供 `NewPostgresStore(pool)`，在 `cmd/tokenpivot/main.go` 与现有 workspace service 一起装配。

项目 SQL：

- List：`WHERE workspace_id=$1 ORDER BY created_at,id`。
- Create：显式写入路径 workspace；`RETURNING` 全字段；仅将 `uk_projects_workspace_id_project_key` 映射为 `ErrConflict`。
- Update：`UPDATE ... WHERE id=$1 AND workspace_id=$2 RETURNING ...`，零行即 `ErrNotFound`。
- Archive：`archived_at=COALESCE(archived_at,now()) WHERE id/workspace`；不存在返回 404，已存在重复执行成功。

来源 List/Create/Update 同理；Create 只映射 `uk_sources_workspace_id_platform_instance_key` 为 `ErrConflict`，Update 遇到同一约束也为 409。Update 动态 SQL 只允许代码内固定列名，值全部参数化，不拼接用户列名。

### 5.3 来源归档事务

`sources` 的归档事务使用显式 `ReadCommitted`，按以下顺序：

1. `SELECT 1 FROM sources WHERE id=$1 AND workspace_id=$2 FOR UPDATE`。无行返回 `ErrNotFound`，此时返回 404，不执行撤销。
2. `UPDATE sources SET archived_at=COALESCE(archived_at,now()) WHERE id=$1 AND workspace_id=$2`。
3. `UPDATE ingest_credentials SET revoked_at=now() WHERE workspace_id=$1 AND source_id=$2 AND revoked_at IS NULL`。
4. 任一步失败均回滚；只有 Commit 成功才向 HTTP 返回 204。Commit 结果未知时返回安全错误，不自动重试、不伪装成功。

事务锁来源行后再更新凭证。归档与凭证签发/轮换使用相同的第一把锁，避免一个事务持来源锁、另一个先改凭证造成归档完成后仍出现 active 凭证。

### 5.4 凭证签发/轮换改造

`internal/credentials/service.go` 和 `postgres.go` 将 Issue/Rotate 的“检查来源 + 插入凭证”放进短事务，不只靠服务层先后两次独立查询：

**Issue**

1. 在事务外生成随机令牌与 hash；失败直接返回安全错误。
2. 开 Read Committed 事务，`SELECT ... FOR UPDATE` 锁定 `(workspace_id,source_id)` 来源并读取 `archived_at`。
3. 来源不存在：`ErrNotFound`；已归档：`ErrSourceArchived`；active 才在事务内插入原固定 workspace/source 的凭证。
4. Commit 成功后返回一次明文令牌。

**Rotate**

1. 事务外生成新令牌/hash。
2. 开事务，先按 `(workspace_id,credential_id)` 读取旧凭证；不存在为 404。
3. 使用旧凭证的 workspace_id/source_id 锁定来源并检查 active；不允许请求体或调用方改绑来源。
4. active 才插入新凭证，Commit 后返回明文；旧凭证是否已吊销不新增限制，保持当前轮换语义。

来源不存在或已归档时不得插入凭证。归档与 Issue/Rotate 竞态只能有两种结果：凭证写入先锁来源，则它先提交、归档随后撤销新凭证；归档先锁来源，则凭证事务等待，归档提交后看到 archived 并失败。两种结果都不能在归档提交后留下 active 凭证。

凭证 List 的普通 `SourceExists` 可继续使用非锁定读取，因为它只是 owner 已授权后的存在性检查；Revoke 不需要来源 active，以便清理历史。Authenticate 不增加来源归档判断，仍只看凭证 hash/revoked_at；归档事务负责撤销，避免把锁定成本加到每个插件事件请求。

### 5.5 必须审计的空间限定语句

实施时用符号引用搜索核对所有新 SQL 和凭证写路径，至少满足：

- projects/sources 所有 List/Get/Update/Archive：`workspace_id` 与资源 ID 同时限定，不能先按 ID 取出后再在内存忽略 workspace。
- source 归档撤销：同时限定 `workspace_id` 和 `source_id`。
- credential Issue/Rotate：来源锁、旧凭证读取和插入都使用已授权 workspace；Rotate 只能沿用旧 source_id。
- ingestion 事件与分钟聚合：继续使用已认证凭证绑定的 workspace/source/project，不读取浏览器 workspace 路径，不回退 Cookie。
- 不因来源归档给事件或分钟聚合增加 UPDATE/DELETE，不把归档项目的 project_id 改成空。

## 6. 现有凭证与采集路径权限覆盖清单

| 路径 | 本期处理 | 必须保持/新增的保证 |
|---|---|---|
| 凭证列表 | 换用共享 owner 助手 | member/非成员不能读；归档来源的历史凭证仍可见 |
| 凭证签发 | 事务锁来源并检查 archived_at | owner-only；archived 为 409；跨空间 source 为 404；无有效令牌产生 |
| 凭证轮换 | 同事务读取旧凭证、锁旧来源、插入 | owner-only；不可改绑 workspace/source；archived 为 409 |
| 凭证吊销 | 保持 owner-only | 已归档来源仍可吊销；重复语义不变 |
| DSH/PI HTTP 采集 | 生产代码不改权限模型 | 无 Cookie/成员/Origin 依赖；有效固定凭证可采，吊销后新请求 401 |
| ingestion `TouchSource` | 不改 | 归档前已认证请求可完成并更新 `last_received_at`；不因此重新激活来源或凭证 |
| 项目归档后的采集 | 不改现有判断 | 仍接受绑定 project_id 的事件 |
| 下游 T-055/T-058 | 本期不实现 | 后续必须使用同一 `requireMember` 和 workspace 限定，不得另起角色 SQL |

## 7. 实施步骤与文件范围

### 第一步：先更新接口文档与决策文本

- `docs/openapi/openapi.yaml`：新增 projects/sources tags、路径、请求/响应 schema、错误码；为凭证 Issue/Rotate 增加 409 与 active 来源说明；所有受会话保护的新路由同时声明管理数据库故障 500 `INTERNAL` 与会话依赖故障 503 `UNAVAILABLE`，并补对应 schema；补充归档、重复请求、历史保留、归档前已认证请求和 16 KiB 规则。
- `docs/decisions/0004-auth-and-workspace.md`：保留现有全部决策，追加本次删除语义、来源归档锁和归档不移动历史的决定。
- 以接口文档作为后续处理器测试的字段和状态码依据；不要先写实现再倒推文档。

### 第二步：迁移与领域服务

- 新增迁移及其集成测试。
- 新增 `internal/projects/{service.go,postgres.go}` 和 `internal/sources/{service.go,postgres.go}`。
- 同期修改 `internal/credentials/{service.go,postgres.go}`，把 Issue/Rotate 与来源归档锁一起完成；不能只上线归档接口而保留无锁凭证写入。
- 保持 `go.mod`、`go.sum` 不变，使用现有 pgx、pq、pgtype。

### 第三步：共享权限与 HTTP 装配

- 新增 `internal/server/workspace_access.go`，移动通用 `pathID`/空体等确有共享需要的助手；凭证处理器改用共享 owner/member 助手。
- 新增 `internal/server/management_json.go`，只承载本期严格 JSON 解码与 16 KiB 限制。
- 新增 `internal/server/projects.go`、`internal/server/sources.go`，实现矩阵中的路由行为。
- 修改 `internal/server/credentials.go`：共享角色助手、来源归档 409 映射；不改变令牌只返回一次的原则。
- 修改 `internal/server/server.go` 的 Options 和 fail-closed 路由注册：只有 Auth、成员服务及对应资源服务都存在时才注册管理路由。
- 修改 `cmd/tokenpivot/main.go` 装配 projects/sources；不新增配置键。

### 第四步：文档同步与历史说明

- `docs/data-model.md`：补 `sources.archived_at`、来源/项目归档与凭证保留规则。
- `docs/architecture.md`：补所有浏览器空间读写共用成员入口、来源归档/凭证事务和 in-flight 边界。
- `docs/prd.md`：把已批准的方案 A 与不可恢复/不可重建同键语义写入需求，避免只存在技术文档中。

### 第五步：测试与发布前检查

按下节映射补齐单元、HTTP、真实 PostgreSQL 和竞态测试，再运行已核实命令。规划阶段未运行这些命令，也未验证实现。

### 最小上线与回退约束（只写计划，不执行部署）

1. **先迁移、后切换。** 先单独应用 00007；可空的新列不要求回填，尚未启用归档时旧版可继续运行。随后进入维护窗口，在启用来源归档前停止所有旧版凭证 Issue/Rotate 写入口，并排空已经进入的旧版签发/轮换请求。可通过短暂停止服务或部署入口路由阻断完成，不为此新增应用配置开关。
2. **新版全部就绪才开放写。** 确认旧版实例已停止、相关请求已排空，再启动包含来源归档及 active 检查/行锁的新实例，最后开放项目/来源管理和凭证写接口。不能让“可归档的新版”和“不检查 archived_at 的旧版凭证写入”同时对外服务。
3. **已有归档后的回退不能恢复旧写入。** 一旦任何来源已归档，不得直接回退到不检查归档的旧版 Issue/Rotate。回退环境必须保留 `sources.archived_at` 与凭证撤销记录，并选择其一：继续停用凭证签发/轮换写入口，或回退到仍包含 active 检查和同事务行锁的构建。不得通过 Down 迁移删除归档列、清除 revoked_at 或重新激活凭证来兼容旧版。
4. 上述顺序是实施后的发布前置条件，不在本规划任务中执行部署，也不要求新增自动化部署系统。

## 8. 测试与验收映射

| 验收点 | 测试位置与可观察结果 |
|---|---|
| 迁移兼容 | `cmd/tokenpivot-migrate/source_archival_integration_test.go`：全迁移后列存在、可空；旧来源 active；记录当前迁移版本 |
| 项目 CRUD | `internal/projects/service_test.go`、`postgres_integration_test.go`：创建/列表/修改/归档、重复键 409 映射、跨空间不存在、归档时间保留 |
| 来源 CRUD | `internal/sources/service_test.go`、`postgres_integration_test.go`：显式创建、双字段修改、唯一冲突、归档行保留、last_received_at 不归零 |
| 来源归档原子性 | service 使用事务替身验证锁→归档→撤销顺序及每步回滚；真实 PostgreSQL 验证 active 凭证被撤销、 revoked 行不重置、任何失败不产生成功结果 |
| 凭证 active 检查 | `internal/credentials/service_test.go`、`postgres_integration_test.go`：active 可签发/轮换，missing 为 not found，archived 拒绝且不插入，Rotate 不改变旧 source_id |
| 归档/签发/轮换竞态 | 真实 PostgreSQL 并发控制两种锁顺序；归档提交后 `revoked_at IS NULL` 的相关凭证数必须为 0，归档后新认证失败 |
| 浏览器角色矩阵 | `internal/server/projects_test.go`、`sources_test.go`：owner/member/非成员/匿名逐路由验证；角色 DB 错误为 500 `INTERNAL`，绝非 403 放行或降级 |
| 依赖故障分层 | 新管理路由和现有凭证路由分别注入会话存储/续期故障与成员/资源数据库故障：前者必须为 503 `UNAVAILABLE`，后者必须为 500 `INTERNAL`；OpenAPI 对应响应与处理器结果一致 |
| Origin 矩阵 | 新管理写和现有凭证写均覆盖缺失、错误、`null`、多 Origin 为 403；GET 无 Origin 可读；匿名写且无 Origin 仍为 403 |
| 跨空间越权 | 对已加入空间使用另一空间的 project/source ID 修改或归档、向跨空间 source 签发、轮换另一空间 credential ID，均为 404；未加入目标空间时先为 403。目标行、凭证和历史不变，响应不含真实归属信息 |
| 请求校验 | 无效 UUID、空体、超 16 KiB、非 JSON、未知/重复字段、非法 UTF-8、控制字符、首尾空白、PATCH 全空均按矩阵返回；拒绝时不执行业务写 |
| 重复请求 | POST 重放 409；PATCH 同值 200；DELETE 重放 204 且第一次 archived_at/revoked_at 不变；归档后同键 POST 仍 409 |
| 凭证 HTTP 回归 | `internal/server/credentials_test.go` 保持 owner-only、明文只返回一次，新增 archived 409；`server_test.go` 覆盖路由 fail-closed |
| 项目历史保留 | 真实 PostgreSQL 预置 `usage_events`/`usage_minute_stats`，项目归档前后行数、workspace/source/project 归属与聚合值一致；归档项目的新事件仍按现有逻辑接收 |
| 来源历史保留 | 归档前后 `usage_events`/`usage_minute_stats` 不删不改；source 与 credential 行仍在，只有 archived_at/revoked_at 按规则变化 |
| 归档前已认证请求 | `internal/ingestion/postgres_integration_test.go` 增加跨包场景：先 Authenticate，再归档来源，再完成已认证 process；事件可写、last_received_at 可更新，但来源仍归档且新令牌认证失败 |
| 装配与全量回归 | `cmd/tokenpivot/main.go` 使用真实服务；`make check` 与带安全测试数据库环境的 `make test-integration` 全绿 |

测试关注真实失败和竞态，不增加伪造的网络重试、归档恢复或团队场景。测试日志和错误断言不得输出实际令牌、数据库口令或 Cookie。

## 9. 执行切分

**单块，不拆。** 来源归档与凭证 Issue/Rotate 必须共享锁顺序并同时交付；项目/来源又共用角色助手、路由装配和同一份 OpenAPI。拆成并行块会增加共享接口和合并协调，却不能安全独立上线。

同一执行块独占以下共享文件：迁移编号、`docs/openapi/openapi.yaml`、`internal/server/server.go`、`cmd/tokenpivot/main.go`、凭证服务/存储和 ADR/模型文档。T-055/T-058 等后续任务在此块完成后基于新接口开发，不与本期并行修改这些文件。

## 10. 已查证的验证命令

```sh
make check
# 在已有安全隔离的测试数据库环境中，按 Makefile 要求注入两个 TEST 数据库变量后：
make test-integration
```

`make check` 已确认包含 Go/前端格式检查、Go vet、前端 lint/test、普通与 uiembed 的 Go race 测试及构建；`make test-integration` 已确认以 `uiembed,integration` 标签串行执行真实 PostgreSQL 集成测试。不得在命令或报告中展开数据库环境变量值。仓库调查中未确认独立 OpenAPI lint 命令，因此不虚构该步骤。

## 11. 主要风险与控制

1. **只改归档、不改凭证写入会产生有效凭证漏洞。** 两者必须同块交付并做两种锁顺序测试。
2. **新旧版混跑会绕过归档检查。** 必须遵守先迁移、停止并排空旧版凭证写、再开放新版归档与写接口的顺序；已有归档后的回退不得恢复无检查的旧写入。
3. **归档响应不等于所有旧采集已停止。** 文档和测试明确只允许归档前已认证请求完成；新认证在归档后失败。
4. **来源机器标识可改会影响运营人员识别。** 保持 source_id、凭证绑定和历史归属稳定；OpenAPI 明确这是标识修改，不是移动来源或换令牌。
5. **归档行仍占唯一键。** 这是保留历史和避免同键歧义的结果；调用方不能通过删除后重建绕过，需在文档和 409 测试中固定。
6. **唯一冲突误映射会泄露或误报。** 只识别 `uk_projects_workspace_id_project_key` 与 `uk_sources_workspace_id_platform_instance_key`，其他数据库错误仍为安全 500。
7. **无分页列表未来可能变大。** 本期 Personal 空间保持最简单全量列表；出现真实规模需求时再新增分页，不能现在引入分页框架。

## 12. 未确认项、假设与依据

### 未确认项

- 无影响方案或验收的未决产品选择：用户已确认归档方案 A；归档与签发/轮换的并发保护是本计划补充的必要安全设计与验收要求，不冒充用户原话。
- 迁移编号 00007 已由主代理评审确认可用；实施本身仍需用户授权。
- 本计划只做仓库调查与文档设计，未运行测试、未连接数据库、未执行部署，也未验证任何新接口已经存在。

### 依据

- 删除语义来自用户对归档方案 A 的确认；现有项目归档保留身份且不回写旧事件见 `docs/data-model.md:32`、`:44`，归档项目仍通过采集归属检查见 `internal/ingestion/postgres.go:101-112`。
- 权限边界见 `docs/decisions/0004-auth-and-workspace.md:7-15`、`docs/architecture.md:73-85`；历史表和应用层关联边界见 `docs/data-model.md:19-23`、`:32-44`、`:46-88`。
- 接口与实现证据见第 2 节已逐条核对的 `file:line`，新路径均在步骤中明确标注为拟新增，不冒充现存代码。
- 锁与事务采用仓库已有 PostgreSQL 行锁、短事务、显式 READ COMMITTED 模式，并由真实数据库竞态测试验收；没有依赖未经核实的外部性能或锁行为断言。
- 本期决策可由仓库代码、现有 ADR 和用户已确认语义回答，因此未进行外部搜索，也没有外部 URL、第三方版本或许可证结论需要引用。
