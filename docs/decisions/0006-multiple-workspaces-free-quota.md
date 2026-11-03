# ADR-0006：多工作空间与免费创建额度

状态：2026-11-03 用户决策，关联 T-064；取代 [ADR-0004](0004-auth-and-workspace.md) 中“注册自动创建唯一 `Personal` 个人空间”“个人空间是本期唯一空间、不提供多空间切换”“当前空间取最早 owner 空间”“账号四部分写入”四条。本文记录已确认的目标规则，不声称接口或界面已运行通过。

## 决策

**范围与额度**

- 工作空间没有 personal/team 类型，也没有“个人空间”特殊分支；注册事务只创建用户和首个会话，不创建工作空间或成员关系。
- 每个账户最多成功创建 2 个工作空间，首个同样计入；额度是服务端业务常量 `freeWorkspaceLimit = 2`，不引入套餐、价格、计费、额度配置后台或为付费预留的扩展框架。
- 本期不交付工作空间删除、重命名、成员列表、邀请、移除或角色变更。

**创建、列表与当前空间**

- `POST /api/v1/workspaces` 创建空间并在同一事务写入 `owner` 成员关系；`GET /api/v1/workspaces` 按成员关系列出当前用户可访问的空间，空列表返回 `[]`；`GET /api/v1/workspaces/{workspaceID}` 供切换前校验成员资格。`GET /api/v1/workspaces/current` 及其 `CurrentWorkspace` schema 移除。
- `workspaces.created_by_user_id` 只用于额度计数和同账户名称唯一，不参与运行时角色判定；owner/member 权限只来自 `workspace_memberships` 的 `MembershipRole`。
- 当前工作空间只存在于前端 URL 的 `?workspace=<uuid>`，服务端不保存用户、会话或数据库级别的默认空间；切换成功不写会话，只改 URL。
- 列表按 `workspace_memberships.user_id` join `workspaces`，以 `name COLLATE "C" ASC, id ASC` 排序，不分页；详情对不存在与非成员统一返回 `403`，不泄露资源是否存在。

**名称规则**

- 服务端在事务外 `TrimSpace` 后校验：1–80 个 Unicode code point、UTF-8 不超过 256 字节、不含 Unicode control character；名称中间普通空白允许。
- 大小写敏感，不做小写化、不做 NFC/NFKC 等 Unicode 规范化；`Alpha` 与 `alpha` 可共存，只有修剪后字节完全相同才冲突。
- 数据库保存修剪后的值，列使用 `COLLATE "C"`；判等与排序都按字节，前端不再用 `localeCompare` 重排。

**并发与数据完整性**

- 修改既有迁移 00002，`workspaces` 增加 `created_by_user_id uuid NOT NULL`、`name text COLLATE "C" NOT NULL` 和唯一索引 `uk_workspaces_created_by_user_id_name (created_by_user_id, name)`；不加 FK、CHECK、trigger 或 RLS，不建额度计数表。
- 创建在默认 `READ COMMITTED` 的短事务内固定执行：`users` 行 `FOR UPDATE` → `count(*) FROM workspaces WHERE created_by_user_id = $1` → 插入空间 → 插入 owner 成员关系；计数达到 2 返回 `409 WORKSPACE_LIMIT_REACHED`，任一步失败整体回滚。
- 同账户并发创建被 `users` 行锁串行化，后到者能看到前者已提交的计数；重名由唯一索引兜底，只有该索引冲突映射为 `409 WORKSPACE_NAME_CONFLICT`，其他唯一冲突仍按内部错误处理。不同账户锁不同行，可并行创建。

**前端**

- 列表为空时强制打开不可关闭的创建弹窗，创建成功前不挂载总览、不发起工作空间业务请求；已有空间时切换器提供可取消的创建入口，超额以服务端 `409` 为准。
- 切换前用详情接口以 `staleTime: 0` 每次 HTTP 重新校验成员资格；点击切换先取消上一个详情请求，成功后取消旧 `workspaceId` 的业务查询再更新 URL。
- 所有 scoped query key 同时包含 `userId`、`workspaceId` 与过滤条件并带 `meta.workspaceId`；业务区域以 `workspaceId` 为 React key，不使用旧 key 的占位数据，迟到响应只能写入旧空间缓存。
- 切换目标详情返回 403 时只用局部标记显示切换器错误，保留旧空间 URL、业务数据与列表缓存，不触发全局业务清理；创建 mutation 的 401 仍走现有用户身份校验后的认证清理。

## 备选与后果

- **不选大小写折叠或 Unicode 规范化**：小写化会不可逆合并 `Alpha`/`alpha`，规范化还要定义 NFC/NFKC 与各语言的等价规则，并与展示名预期冲突。代价是用户可能建出仅大小写不同的近似名，界面只按字面重复提示，不做“相似名称”拦截；这是可接受的取舍。
- **不选数据库默认排序**：`COLLATE "C"` 的字节序不是人类语言排序（例如大写、重音、非拉丁字符的顺序不符合阅读习惯），但换来与判等一致、跨 locale 与排序库版本稳定的顺序；排序是列表的固定服务端顺序，前端不再重排。
- **不选“先查后插”作为唯一防线**：并发下两个请求会同时通过预检；唯一索引是最终仲裁，预检只用于友好报错。
- **不选计数表、CHECK、trigger 或 RLS**：额度上限是业务规则而非数据不变量，计数表引入需要维护的第二份状态，CHECK 无法跨行计数，trigger 与 RLS 超出本项目既有的“应用层校验 + 索引约束”方式。`users` 行锁加实时 count 已满足 2 个额度，且无需清理计数。
- **不选 Serializable 隔离**：可串行化需要新增序列化失败的重试策略；同账户行锁已经把创建串行化，不同账户仍可并行，`READ COMMITTED` 足够。
- **不选注册时自动创建空间**：自动创建让用户无法选择名称，也让首个空间绕开额度与名称唯一语义；零空间加强制创建弹窗让首个空间与后续创建走同一事务和同一校验。
- **不选服务端保存当前空间**：切换会新增会话或用户行的写路径，多标签页还会互相覆盖；URL 选择加每次详情校验保持无状态，且每个业务请求仍独立校验成员资格。
- **不做付费框架**：当前没有计费需求，提前抽象套餐会把额度与价格、配额、账单耦合；将来调整额度或引入套餐需要改常量并重新审查并发测试，这是明确的后续成本。
- **迁移与部署后果**：本次直接修改既有迁移并从空库重建，不做旧库回填或兼容升级；已部署库需要重建，具体 DSN、停服与执行方式为 TODO(待确认)。本文不声称生产环境已生效，也不修改部署文档。

验收与逐项证据见 [T-064 计划](../plans/2026-11-03-multiple-workspaces-free-quota.md) 第 11 节；接口字段与错误码以 [OpenAPI 接口文档](../openapi/openapi.yaml) 为准。
