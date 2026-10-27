# T-010：接入凭证签发、轮换与撤销

> 状态：**已完成规划**。实施仍需另行授权，并须满足第 4 节前置；本地验收与最终联调验收分别报告，不预先宣称通过。
>
> 用户已确认：由 owner 配置新 API Key，旧 Key 手动停用；轮换不自动撤销旧凭证。
>
> 日期：2026-10-27。规划时分支：`main`。

## 1. 目标

为插件提供独立于浏览器会话的接入凭证，并让 workspace owner 能列出、签发、轮换和撤销凭证：

- 凭证固定绑定创建时的 `workspace_id`、`source_id`，数据库只保存令牌摘要。
- 插件使用 `Authorization: Bearer`，不能以 Cookie 代替插件认证。
- owner 使用浏览器会话管理凭证，状态变更请求必须通过 Origin 校验。
- 轮换不改变 source；撤销不删除 source、事件或防重依据。
- 向 T-011 提供从令牌得到固定 workspace/source 的认证能力。

## 2. 已核实的仓库现状

- `docs/prd.md` §3.3 要求凭证轮换、撤销、owner 管理、插件独立认证；T-041 才负责凭证管理界面。
- `docs/decisions/0004-auth-and-workspace.md` 已决定：工作空间隔离、owner 管理凭证、浏览器会话配合 Origin 检查、插件独立认证且不回退 Cookie。
- `docs/data-model.md` 将 `ingest_credentials` 与 workspace/source 关联，并包含 `secret_hash`、`revoked_at`；凭证和 source 不是同一身份。当前模型中的 `secret_hash` 为 `text`，认证查询使用公开 credential ID，不能视为「32 字节摘要存储、摘要唯一索引、按摘要认证」已经交付；这些仍是 T-008 的协调前置。
- `docs/plans/2026-10-27-batch-ingestion.md` 已把基础表归给 T-008，把采集认证、重传和防重行为归给 T-011/T-009；T-007 负责令牌与摘要格式而非建表，本任务不重复承接其他任务范围。
- `docs/todo.md` 中 T-007/T-008 仍在规划，T-009 已规划，T-011 执行中，T-016/T-019 未规划；T-041 才包含凭证 owner 管理界面。
- `cmd/tokenpivot/main.go:27-57` 目前只建立 Web handler、PostgreSQL 连接池并装配 `server.NewHandler`；`internal/server/server.go:14-26` 只有探针与静态前端路由。
- `go.mod:3-8` 使用 Go 1.26.4、pgx v5.11.0、goose v3.29.0；现有标准库已经覆盖随机令牌和摘要，不需要新依赖。
- `docs/openapi/openapi.yaml` 当前不存在；执行中的 T-011 也可能需要接口文档，创建和合并须由主代理协调。

## 3. 范围与非目标

### 范围

1. 限定 workspace/source 的凭证元数据列表，以及凭证签发、轮换、撤销的 owner 管理 API。
2. 按 T-007 确定的格式生成高熵随机令牌、计算 SHA-256 摘要、一次返回明文、按摘要认证。
3. 凭证管理路径的会话、Origin、owner、workspace/source 隔离检查。
4. 供 T-011 使用的窄认证方法及 PostgreSQL 实现。
5. 单元测试、真实 PostgreSQL 集成测试、T-010 本地验收，以及交付后与 T-009/T-011 的轮换重传联合验收。
6. 上述 API 的 OpenAPI 接口文档。

### 非目标

- 不建表、不改迁移；`ingest_credentials`、source、workspace 的表结构归 T-008。令牌与摘要格式归 T-007，本任务不另造第二套格式。
- 不实现 Cookie 会话、用户/成员表、owner 判断、可信 Web origin 配置或通用 Origin 校验器；分别消费 T-016、T-019 的实际能力。
- 不实现采集接口、事件写入或防重约束；归 T-011/T-009。
- 不做凭证管理 UI；归 T-041。
- 不增加凭证 TTL、自动过期、权限 scope、速率限制、审计系统、令牌加密存储或通用幂等框架。
- 不增加认证或元数据列表缓存、第三方令牌库、密码哈希库或新的配置项。
- 不修改 T-011 或其他任务的计划文件。

## 4. 开工前置与未满足处理

1. **T-007 格式协调**：T-007 负责确定令牌与摘要格式，不负责建表。开工时其结论须与本计划第 5.1 节的 `tpk_` 令牌、完整令牌 SHA-256 摘要口径一致；若不一致，由主代理先协调格式，T-010 不私自并存两套格式。
2. **T-008 表与索引**：T-008 提供实际 workspace/source/credential 表。现有 `secret_hash text` 与公开 credential ID 查询不是本设计所需能力；`ingest_credentials` 至少须能保存固定 `workspace_id`、`source_id`、32 字节 `secret_hash`、可空 `revoked_at`，并有 `secret_hash` 唯一索引用于按摘要认证。本任务不修改迁移来补这些字段或索引。
3. **T-016 会话与 Origin 能力**：已在管理路由上提供浏览器会话认证及可信的 request-scoped 用户身份，同时提供可信 Web origin 的配置来源和可复用的精确 Origin 校验能力。本任务不自行解析 Cookie，不新增 origin 配置，也禁止临时信任请求 `Host` 或把 `Host` 当作可信 Web origin。
4. **T-019 owner 能力**：已提供「该用户是否为该 workspace owner」的判断能力。本任务不自行查询用户/成员关系来另造授权规则。
5. **T-011 接口与现状协调**：主代理在 T-010 开工前确认采集侧是否已有等价认证接口，以及 `cmd/tokenpivot/main.go`、`internal/server/server.go`、`docs/openapi/openapi.yaml` 的最新改动。优先复用实际认证接口并做窄适配，不能形成两套令牌解析或摘要算法；若 T-011 已改共享文件，先基于其现状协调，不覆盖或回退其改动。
6. **OpenAPI 协调**：主代理指定 `docs/openapi/openapi.yaml` 的唯一创建者；若 T-011 已先建，本任务在其现有内容上追加 credentials 路径。后续共享文件修改严格按第 9 节串行进行。

任一前置未交付时，不得开始对应生产实现：可以用 fake 完善本包纯单元测试，但不得用临时生产中间件、临时迁移、临时信任 `Host` 或模拟 owner 检查绕过前置，也不得宣称本地或端到端验收完成。

## 5. 设计

### 5.1 令牌、摘要与一次展示

- 明文令牌格式：`tpk_` + `crypto/rand.Text()` 的结果。Go 1.26 的该函数提供至少 128 位密码学随机性；解析不硬编码随机部分长度。
- 对**完整令牌**计算 `sha256.Sum256`，把 32 字节摘要写入 `secret_hash`。令牌包含足够随机性，因此不使用 bcrypt/Argon2 等密码慢哈希，也不需要额外 pepper。
- 认证按摘要等值查询，而不是把 credential ID 放进令牌。这样令牌格式不依赖 T-008 尚未确定的主键类型；`secret_hash` 唯一索引同时支撑查询。
- 明文只在签发/轮换成功响应中出现一次。数据库仅保存摘要、不保存明文；日志、错误信息及后续任何读取接口均不记录或返回明文或摘要；`docs/openapi/openapi.yaml` 也只把 token 标为创建响应字段。
- 不实现「找回旧令牌」。签发或轮换响应丢失后，owner 通过第 5.2 节的列表 API 获取该 workspace/source 下的新 credential ID 及撤销状态，再撤销未使用或重复的凭证；也可再次轮换/签发。列表不能恢复明文。

### 5.2 管理 API

以下均为 `/api/v1` 路径，成功响应使用直接 JSON 字段，不新建统一响应包装层。错误至少包含 PRD 已约定的 `code`、`message`；若 T-011 已建立相同职责的 server 错误写法则复用。

| 操作 | 方法与路径 | 成功响应 |
|---|---|---|
| 列表 | `GET /api/v1/workspaces/{workspaceID}/sources/{sourceID}/credentials` | `200`：`credentials` 数组；每项含 `credential_id`、`workspace_id`、`source_id`、可空 `revoked_at` |
| 签发 | `POST /api/v1/workspaces/{workspaceID}/sources/{sourceID}/credentials` | `201`：`credential_id`、`workspace_id`、`source_id`、`token` |
| 轮换 | `POST /api/v1/workspaces/{workspaceID}/credentials/{credentialID}/rotate` | `201`：新凭证的上述四个字段 |
| 撤销 | `POST /api/v1/workspaces/{workspaceID}/credentials/{credentialID}/revoke` | `204`，空响应 |

共同规则：

- 四个操作都要求有效浏览器会话和 workspace owner。签发、轮换、撤销三个写操作还要求 Origin 与 T-016 配置的可信 Web origin 精确匹配，缺失或不匹配均拒绝；列表 GET 不把写操作 Origin 校验作为准入条件，但会话与 owner 检查不能省略。
- Origin 校验复用 T-016 能力；任何管理路径都不得根据请求 `Host` 临时建立信任。
- 列表返回路径 source 下包括已撤销凭证在内的元数据，以 `revoked_at` 表达撤销状态；不返回 `token`、`secret_hash` 或任何明文/摘要派生值，不增加列表缓存。
- 签发、轮换、撤销请求体为空，不接受 `workspace_id`、`source_id` 或「是否撤销旧凭证」等额外选项。轮换从旧凭证行读取固定 source，因此客户端不能借轮换移动凭证。
- 未认证：`401 unauthenticated`；写操作 Origin 不合法或非 owner：`403 permission_denied`；source/credential 不属于路径 workspace：`404 not_found`，避免泄露其他空间资源。
- 会话、适用的 Origin、owner 检查均在写数据库前完成。数据库错误向上返回并映射为服务器错误，不能返回假装成功。
- 管理 API 只认浏览器 owner 会话，接入凭证不能用来管理凭证。

### 5.3 插件认证接口

在 `internal/credentials` 提供窄方法（ID 的 Go 类型按 T-008 实际模型调整）：

```go
Authenticate(ctx context.Context, token string) (Credential, error)
```

`Credential` 只暴露采集所需的 credential/workspace/source 标识。认证流程：

1. 校验 `tpk_` 前缀并计算完整令牌摘要；格式错误与凭证不存在、已撤销统一返回 `ErrUnauthenticated`，不泄露具体原因。
2. 以摘要查询凭证；`revoked_at IS NOT NULL` 拒绝。
3. 从数据库行返回 `workspace_id`、`source_id`，绝不采用插件请求体、路径或客户端声明的归属。
4. 数据库不可用返回真实错误，不得回退 Cookie 或匿名身份。

T-011 在插件入口只接受 `Authorization: Bearer <token>`。没有 Bearer、Bearer 无效或已撤销时为 `401 unauthenticated`；即使同时带有效 Cookie 也不能把插件请求改成浏览器用户身份。

### 5.4 轮换语义（已确认）

**轮换只创建同 source 的新凭证，旧凭证保持其原有撤销状态，owner 更新插件后显式撤销旧凭证。**

理由：

- 新令牌只展示一次，保留有效的旧凭证可避免响应丢失或插件尚未更新时中断采集。
- 不需要记录旧明文，也不需要为「重放创建响应」增加幂等存储。
- 无需为轮换新增 `rotated_from` 字段或长事务；现有 credential 行已能表达多个有效凭证。

该语义下：

- owner 可基于有效或已撤销的旧 credential 的固定归属生成新 credential。轮换只读取旧行的 workspace/source 并插入新行，不复活旧凭证、不清除其 `revoked_at`，旧 credential ID 仍保持撤销。
- 两个并发轮换会各自创建同 source 的独立有效凭证；它们都能使用，直至分别撤销。这是明确行为，不做隐形去重。
- 签发/轮换不是幂等操作。客户端收到成功前不要自动重放；超时或 5xx 后若重试，可能创建多个有效凭证。不能为了重放响应而保存明文；响应丢失通过列表元数据并撤销多余凭证处理。
- 旧凭证撤销后仅新凭证可用；新旧凭证的 workspace/source 相同，因此防重口径不变。

### 5.5 撤销、并发与生效边界

- 撤销使用单条、按 `credential_id + workspace_id` 限定的条件更新：`revoked_at = COALESCE(revoked_at, now())`。重复撤销保留首次时间并返回相同的 `204`，因此撤销可安全重试。
- 更新影响 0 行时按 `not_found` 处理；不能先查跨空间记录再泄露其存在性。
- 并发撤销由 PostgreSQL 行更新串行化，不需要应用锁。并发签发/轮换是独立插入，不增加「每 source 只能一个有效凭证」的新约束。
- 撤销与轮换竞争只影响旧 credential ID 的撤销状态，新凭证始终作为独立行创建，二者无需锁：轮换先提交、撤销后提交时，新凭证有效而旧凭证随后撤销；撤销先提交、轮换后提交时，轮换仍读取旧行固定归属并创建有效新凭证，旧行保持撤销。
- 不增加认证缓存。撤销提交后，之后开始的每次数据库认证都会看到 `revoked_at` 并失败。
- 已在撤销提交前通过认证的采集请求允许结束；撤销不阻塞或回滚该在途请求，也不追溯删除已接受事件。若未来要求「撤销响应返回后绝无任何在途写入」，需要把认证与写入纳入更强的事务协调，不属于本期。
- 撤销只更新凭证行，不更新/删除 source、事件或唯一性依据。之后给同 source 签发新凭证并重传旧请求，仍按原防重规则返回 duplicate/conflict。

## 6. 文件范围与责任

### 新增

- `internal/credentials/service.go`：Credential、错误、令牌生成/摘要、元数据列表、签发/轮换/撤销业务及窄 Store/owner 所需接口。
- `internal/credentials/postgres.go`：基于现有 `*pgxpool.Pool` 的列表查询、摘要查询、插入与条件更新；不新增通用数据库层。
- `internal/credentials/service_test.go`：令牌、列表元数据、响应丢失后按 ID 撤销、归属、基于已撤销旧凭证轮换、重试和错误行为。
- `internal/credentials/postgres_integration_test.go`：使用 `integration` build tag 和专用测试数据库，覆盖真实摘要查询、列表归属与跨空间隔离、跨空间条件更新、并发撤销、撤销/轮换两种提交顺序及撤销边界。
- `internal/server/credentials.go`：列表及写操作的 owner 管理 HTTP handler；适配 T-016 会话与 Origin 能力和 T-019 owner 能力。
- `internal/server/credentials_test.go`：列表会话/owner、列表不要求写操作 Origin、跨空间不可读、写操作 Origin、状态码、Cookie/Bearer 隔离、响应丢失后列出 ID 再撤销，以及响应不泄露明文之外的秘密或摘要等 HTTP 行为。

### 修改

- `internal/server/server.go`：只注册上述列表与管理路由，保留 `/api` 不回退静态前端的现有行为。
- `internal/server/server_test.go`：仅在构造参数变化或路由回归需要时调整。
- `cmd/tokenpivot/main.go`：用现有连接池装配 credentials store/service 和实际会话、Origin、owner 适配器；main 不写业务逻辑。
- `cmd/tokenpivot/main_test.go`、`cmd/tokenpivot/main_integration_test.go`：只在现有测试因装配签名变化而必须调整时修改；新增真实 PostgreSQL 场景使用 `integration` build tag 和专用测试数据库。
- `docs/openapi/openapi.yaml`：由主代理协调后新增或追加本计划的列表、签发、轮换、撤销 API；列表响应只含元数据，不含明文或摘要。

### 明确不动

迁移、go.mod/go.sum、采集 handler、防重查询、事件表、前端文件、T-016/T-019 的会话与成员实现，以及其他任务的计划文件。

## 7. 实施顺序

1. 主代理确认第 4 节前置，记录 T-007 格式结论、T-008 实际字段/ID 类型、T-016 会话与可信 Origin 能力、T-019 调用方式及 T-011 认证接口和共享文件现状；前置不满足则不开始对应生产实现。
2. 先写 credentials 包测试和窄接口，再实现随机令牌、摘要及 service；列表只返回元数据，不在 handler 写 SQL。
3. 实现 PostgreSQL store：列表按 workspace/source 限定归属；签发前校验 source 属于 workspace；轮换读取有效或已撤销旧凭证的固定 source；撤销用单条条件更新；认证按摘要读取归属。
4. 实现 server 列表和管理 handler，接入真实会话、Origin 与 owner 检查，并在 main 装配；补齐列表、响应丢失和跨空间隔离测试。
5. 完成 T-010 的 OpenAPI、单元测试、真实 PostgreSQL 测试和管理 API/认证能力本地验收，先交付这一基础能力。运行 `go test ./...`、`go test -race ./...` 和显式的 `make test-integration`（主代理已核实 `Makefile:81-86`）；新增 PostgreSQL 测试使用 `integration` build tag 与专用测试数据库，缺环境或专用库时报告集成验收未完成，不静默跳过。
6. T-010 完成上述共享文件修改和本地交付后，T-011 再接入 Bearer 认证并完成其采集侧重传改动；随后与 T-009 联合验收轮换、撤销和重传。联合验收不阻塞第 5 步基础认证能力交付，但最终联调必须实际完成并单独报告结果，未完成时不得宣称已通过。

## 8. 验收标准

### 签发与认证

- owner + 合法 Origin 可签发；响应含明文 token，fake/store 中只能观察到 32 字节摘要，摘要与明文不同。
- 无会话、非 owner、缺失/错误 Origin 分别返回上述错误且不写库。
- 用其他 workspace 的 source 签发返回 404，不创建凭证。
- 有效 Bearer 返回数据库固定 workspace/source；错误、格式非法、已撤销令牌均 401。
- 只有 Cookie、或无效 Bearer 与有效 Cookie 同时存在，都不能通过插件认证。

### 元数据列表与响应丢失

- 有效会话 + owner 在不提供写操作 Origin 时可列表；无会话或非 owner 分别返回 401/403，且不能读取元数据。
- 列表只返回路径 workspace/source 下的 credential ID、固定归属和 `revoked_at`，包括已撤销项；任何响应、日志或错误均不含明文、`secret_hash` 或其派生值。
- workspace/source 路径不匹配或尝试读取其他空间 source 时返回 404，不返回跨空间凭证元数据。
- 模拟签发/轮换成功响应丢失后，owner 能通过同 workspace/source 列表取得新 credential ID，再撤销该未使用凭证；撤销后列表显示其撤销状态，对应令牌认证失败。
- 列表不创建或修改凭证，不使用缓存，也不增加通用幂等机制。

### 轮换、撤销与重试

- 轮换后的新凭证与旧凭证具有完全相同的 workspace/source，客户端不能指定新 source。
- 轮换后原本有效的旧凭证仍可用；显式撤销旧凭证后旧令牌 401、新令牌仍可用。
- owner 可基于已撤销旧 credential 的固定归属轮换出有效新凭证；旧 credential ID 不变且仍撤销，`revoked_at` 不被清除。
- 撤销/轮换覆盖两种提交顺序：先轮换后撤销、先撤销后轮换；两种结果均为新凭证有效、旧凭证撤销，无需应用锁。
- 跨 workspace 列表/轮换/撤销按上述规则返回 404，原凭证和 source 不变。
- 第一次及重复撤销均为 204，`revoked_at` 保持首次值；并发撤销最终只有同一撤销状态，无 500。
- 并发签发/轮换产生独立凭证；测试明确该行为，不能靠先查后插制造竞态。
- 撤销提交后的新认证失败；撤销前已认证的请求允许完成，事件和防重数据保持原样。

### 测试与交付边界

- 新增真实 PostgreSQL 测试均带 `integration` build tag，使用专用测试数据库，并由 `make test-integration` 显式执行；缺环境时报集成验收未完成，不能以跳过代替通过。
- T-010 的认证能力与管理 API 可在联合验收前完成本地交付，但交付报告只声明实际运行通过的检查。
- 最终完成以 T-011 接入和下列联合验收实际通过为准；未执行或未通过时明确保留未完成状态。

### 与 T-009/T-011 联合验收

- 用旧凭证接受一个带 Idempotency-Key 的请求，轮换后用新凭证原样重传：返回 duplicate，不增加事件。
- 撤销旧凭证后，再给同一 source 签发新凭证并重传同一请求：仍返回 duplicate，证明撤销没有清除防重依据。
- 新凭证不能写往其他 workspace/source；即使请求携带其他归属，也以凭证数据库归属为准。

## 9. 执行切分

**T-010 单块，不拆。** 凭证 service、store、管理 handler 和装配共享同一组接口，块内并行拆分只会增加接口协调。

跨任务必须串行：T-010 先完成 `cmd/tokenpivot/main.go`、`internal/server/server.go`、`docs/openapi/openapi.yaml` 的修改与本地验收，T-011 再基于该版本接入认证并修改同一批共享文件；不得同时改这些文件。主代理负责开工前核对 T-011 现状、协调串行顺序和最终装配；若 T-011 已改动，保留并协调其现状，不覆盖、不回退，也不修改另一计划。本块不改 T-011 handler。无迁移编号、lockfile、生成代码或新依赖需要分配。

## 10. 外部依据

- [Go `crypto/rand.Text`](https://pkg.go.dev/crypto/rand#Text)，2026-10-27，**已通过本地 Go 1.26 工具链 `go doc` 核实**：用于秘密/令牌，至少 128 位随机性。采用它生成令牌，避免自行实现随机编码。
- [Go `crypto/sha256.Sum256`](https://pkg.go.dev/crypto/sha256#Sum256)，2026-10-27，**已通过本地 `go doc` 核实**：返回数据的 SHA-256 摘要。采用标准库保存 32 字节摘要。
- [PostgreSQL Transaction Isolation](https://www.postgresql.org/docs/current/transaction-iso.html) 与 [Data Consistency Checks](https://www.postgresql.org/docs/current/applevel-consistency.html)，2026-10-27，**搜索索引核实，原页受当前网络代理解析限制未直接抓取**：并发 UPDATE 会等待前一更新事务，读提交下后续语句看到已提交结果。仅采用单语句条件更新和明确在途边界；未据此声称可保证更强全局一致性。
- 未引入 GitHub 参考实现：本地设计只需 Go 标准库、现有 pgx 与单表操作，没有需要参考第三方实现的未决技术问题，也避免引入不相符的框架或许可证负担。

## 11. 已确认决策、开工协调与主要风险

### 已确认决策

- 用户选择自己配置新 API Key，旧 Key 由 owner 手动停用；因此轮换不自动撤销旧凭证，不采用轮换事务内同时撤销旧凭证的方案。
- 为处理一次展示响应丢失，提供 owner 限定 workspace/source 的元数据列表；只列出 ID、归属和撤销状态，不恢复明文，不引入 UI、缓存或通用幂等。

### 开工协调（不需用户再次确认）

- T-007 负责格式、T-008 负责建表；现有 `secret_hash text` 和公开 credential ID 查询不能直接承接本设计，须先由主代理协调 T-008 交付摘要存储与唯一索引。
- T-016/T-019 最终接口尚未知，本计划只固定所需会话、可信 Origin、owner 能力，不虚构其函数签名。
- T-011 执行中的接口和共享文件现状须在开工时核实，最终 Go 签名与接入点以主代理协调结果为准。

### 主要风险

- 多任务并行装配可能重复实现插件认证或覆盖共享文件；须先复用 T-011 实际边界，并严格按 T-010 先改、T-011 后接入的顺序执行。
- 签发/轮换响应丢失无法恢复明文，非幂等重试可能留下未使用凭证；列表只降低定位多余 credential ID 的风险，owner 仍需识别并撤销，不能通过秘密明文缓存掩盖。
- 撤销不阻断已在途请求；验收和接口文档必须保持这一口径，避免误认为具备全局即时停用。
- 本地验收先于联合重传验收；若只完成前者就宣称整体联调通过，会掩盖 T-011 接入或防重行为问题。
