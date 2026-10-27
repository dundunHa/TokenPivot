# T-010：接入凭证签发、轮换与撤销

> 状态：**草案，未就绪实施**。仅待用户确认「轮换是否自动撤销旧凭证」；其余技术设计按推荐方案展开。
>
> 日期：2026-10-27。规划时分支：`main`。

## 1. 目标

为插件提供独立于浏览器会话的接入凭证，并让 workspace owner 能签发、轮换和撤销凭证：

- 凭证固定绑定创建时的 `workspace_id`、`source_id`，数据库只保存令牌摘要。
- 插件使用 `Authorization: Bearer`，不能以 Cookie 代替插件认证。
- owner 使用浏览器会话管理凭证，状态变更请求必须通过 Origin 校验。
- 轮换不改变 source；撤销不删除 source、事件或防重依据。
- 向 T-011 提供从令牌得到固定 workspace/source 的认证能力。

## 2. 已核实的仓库现状

- `docs/prd.md` §3.3 要求凭证轮换、撤销、owner 管理、插件独立认证；T-041 才负责凭证管理界面。
- `docs/decisions/0004-auth-and-workspace.md` 已决定：工作空间隔离、owner 管理凭证、浏览器会话配合 Origin 检查、插件独立认证且不回退 Cookie。
- `docs/data-model.md` 将 `ingest_credentials` 与 workspace/source 关联，并包含 `secret_hash`、`revoked_at`；凭证和 source 不是同一身份。
- `docs/plans/2026-10-27-batch-ingestion.md` 已把基础表归给 T-008，把采集认证、重传和防重行为归给 T-011/T-009；本任务不能重复承接。
- `docs/todo.md` 中 T-007/T-008 仍在规划，T-009 已规划，T-011 执行中，T-016/T-019 未规划；T-041 才包含凭证 owner 管理界面。
- `cmd/tokenpivot/main.go:27-57` 目前只建立 Web handler、PostgreSQL 连接池并装配 `server.NewHandler`；`internal/server/server.go:14-26` 只有探针与静态前端路由。
- `go.mod:3-8` 使用 Go 1.26.4、pgx v5.11.0、goose v3.29.0；现有标准库已经覆盖随机令牌和摘要，不需要新依赖。
- `docs/openapi/openapi.yaml` 当前不存在；执行中的 T-011 也可能需要接口文档，创建和合并须由主代理协调。

## 3. 范围与非目标

### 范围

1. 凭证签发、轮换、撤销的 owner 管理 API。
2. 高熵随机令牌生成、SHA-256 摘要、一次返回明文、按摘要认证。
3. 凭证管理路径的会话、Origin、owner、workspace/source 隔离检查。
4. 供 T-011 使用的窄认证方法及 PostgreSQL 实现。
5. 单元测试、真实 PostgreSQL 集成测试和与 T-009/T-011 的轮换重传联调验收。
6. 上述 API 的 OpenAPI 接口文档。

### 非目标

- 不建表、不改迁移；`ingest_credentials`、source、workspace 的表结构归 T-007/T-008。
- 不实现 Cookie 会话、用户/成员表或 owner 判断；分别消费 T-016、T-019 的实际能力。
- 不实现采集接口、事件写入或防重约束；归 T-011/T-009。
- 不做凭证管理 UI；归 T-041。
- 不增加凭证 TTL、自动过期、权限 scope、速率限制、审计系统、令牌加密存储或通用幂等框架。
- 不增加认证缓存、第三方令牌库、密码哈希库或新的配置项。

## 4. 开工前置与未就绪处理

1. **T-007/T-008**：已提供实际 workspace/source/credential 模型。`ingest_credentials` 至少能保存固定 `workspace_id`、`source_id`、32 字节 `secret_hash`、可空 `revoked_at`，并有 `secret_hash` 唯一索引用于认证查询。本任务不修改迁移来补这些字段。
2. **T-016**：已在管理路由上提供浏览器会话认证及可信的 request-scoped 用户身份。本任务不自行解析 Cookie。
3. **T-019**：已提供「该用户是否为该 workspace owner」的判断能力。本任务不自行查询用户/成员关系来另造授权规则。
4. **T-011 接口协调**：主代理在实现前确认采集侧是否已有等价认证接口。优先复用实际接口并做窄适配，不能形成两套令牌解析或摘要算法。
5. **OpenAPI 协调**：主代理指定 `docs/openapi/openapi.yaml` 的唯一创建者；若由 T-011 先建，本任务只追加 credentials 路径。

任一前置未交付时，T-010 保持未就绪：可以用 fake 完善本包纯单元测试，但不得用临时生产中间件、临时迁移或模拟 owner 检查绕过前置，也不得宣称端到端验收完成。

## 5. 设计

### 5.1 令牌、摘要与一次展示

- 明文令牌格式：`tpk_` + `crypto/rand.Text()` 的结果。Go 1.26 的该函数提供至少 128 位密码学随机性；解析不硬编码随机部分长度。
- 对**完整令牌**计算 `sha256.Sum256`，把 32 字节摘要写入 `secret_hash`。令牌包含足够随机性，因此不使用 bcrypt/Argon2 等密码慢哈希，也不需要额外 pepper。
- 认证按摘要等值查询，而不是把 credential ID 放进令牌。这样令牌格式不依赖 T-008 尚未确定的主键类型；`secret_hash` 唯一索引同时支撑查询。
- 明文只在签发/轮换成功响应中出现一次。数据库、日志、错误信息及后续任何读取接口均不保存或返回明文；`docs/openapi/openapi.yaml` 也只把 token 标为创建响应字段。
- 不实现「找回旧令牌」。明文响应丢失后只能再次轮换/签发并撤销不再使用的凭证。

### 5.2 管理 API

以下均为建议的 `/api/v1` 路径，成功响应使用直接 JSON 字段，不新建统一响应包装层。错误至少包含 PRD 已约定的 `code`、`message`；若 T-011 已建立相同职责的 server 错误写法则复用。

| 操作 | 方法与路径 | 成功响应 |
|---|---|---|
| 签发 | `POST /api/v1/workspaces/{workspaceID}/sources/{sourceID}/credentials` | `201`：`credential_id`、`workspace_id`、`source_id`、`token` |
| 轮换 | `POST /api/v1/workspaces/{workspaceID}/credentials/{credentialID}/rotate` | `201`：新凭证的上述四个字段 |
| 撤销 | `POST /api/v1/workspaces/{workspaceID}/credentials/{credentialID}/revoke` | `204`，空响应 |

共同规则：

- 三个操作都要求有效浏览器会话、workspace owner；三个写操作都要求 Origin 与配置允许的 Web origin 精确匹配，缺失或不匹配均拒绝。
- 请求体为空，不接受 `workspace_id`、`source_id` 或「是否撤销旧凭证」等额外选项。轮换从旧凭证行读取固定 source，因此客户端不能借轮换移动凭证。
- 未认证：`401 unauthenticated`；Origin 不合法或非 owner：`403 permission_denied`；source/credential 不属于路径 workspace：`404 not_found`，避免泄露其他空间资源。
- 会话、Origin、owner 检查均在写数据库前完成。数据库错误向上返回并映射为服务器错误，不能返回假装成功。
- 管理 API 只认浏览器 owner 会话，接入凭证不能用来管理凭证。

### 5.3 插件认证接口

建议在 `internal/credentials` 提供窄方法（ID 的 Go 类型按 T-008 实际模型调整）：

```go
Authenticate(ctx context.Context, token string) (Credential, error)
```

`Credential` 只暴露采集所需的 credential/workspace/source 标识。认证流程：

1. 校验 `tpk_` 前缀并计算完整令牌摘要；格式错误与凭证不存在、已撤销统一返回 `ErrUnauthenticated`，不泄露具体原因。
2. 以摘要查询凭证；`revoked_at IS NOT NULL` 拒绝。
3. 从数据库行返回 `workspace_id`、`source_id`，绝不采用插件请求体、路径或客户端声明的归属。
4. 数据库不可用返回真实错误，不得回退 Cookie 或匿名身份。

T-011 在插件入口只接受 `Authorization: Bearer <token>`。没有 Bearer、Bearer 无效或已撤销时为 `401 unauthenticated`；即使同时带有效 Cookie 也不能把插件请求改成浏览器用户身份。

### 5.4 轮换语义（推荐，待用户确认）

**推荐：轮换只创建同 source 的新凭证，旧凭证保持有效，owner 更新插件后显式撤销旧凭证。**

理由：

- 新令牌只展示一次，保留旧凭证可避免响应丢失或插件尚未更新时中断采集。
- 不需要记录旧明文，也不需要为「重放创建响应」增加幂等存储。
- 无需为轮换新增 `rotated_from` 字段或长事务；现有 credential 行已能表达多个有效凭证。

该语义下：

- 两个并发轮换会各自创建同 source 的独立有效凭证；它们都能使用，直至分别撤销。这是明确行为，不做隐形去重。
- 签发/轮换不是幂等操作。客户端收到成功前不要自动重放；超时或 5xx 后若重试，可能创建多个有效凭证。不能为了重放响应而保存明文。
- 旧凭证撤销后仅新凭证可用；新旧凭证的 workspace/source 相同，因此防重口径不变。

### 5.5 撤销、并发与生效边界

- 撤销使用单条、按 `credential_id + workspace_id` 限定的条件更新：`revoked_at = COALESCE(revoked_at, now())`。重复撤销保留首次时间并返回相同的 `204`，因此撤销可安全重试。
- 更新影响 0 行时按 `not_found` 处理；不能先查跨空间记录再泄露其存在性。
- 并发撤销由 PostgreSQL 行更新串行化，不需要应用锁。并发签发/轮换是独立插入，不增加「每 source 只能一个有效凭证」的新约束。
- 不增加认证缓存。撤销提交后，之后开始的每次数据库认证都会看到 `revoked_at` 并失败。
- 已在撤销提交前通过认证的采集请求允许结束；撤销不阻塞或回滚该在途请求，也不追溯删除已接受事件。若未来要求「撤销响应返回后绝无任何在途写入」，需要把认证与写入纳入更强的事务协调，不属于本期。
- 撤销只更新凭证行，不更新/删除 source、事件或唯一性依据。之后给同 source 签发新凭证并重传旧请求，仍按原防重规则返回 duplicate/conflict。

## 6. 文件范围与责任

### 新增

- `internal/credentials/service.go`：Credential、错误、令牌生成/摘要、签发/轮换/撤销业务及窄 Store/owner 所需接口。
- `internal/credentials/postgres.go`：基于现有 `*pgxpool.Pool` 的查询与条件更新；不新增通用数据库层。
- `internal/credentials/service_test.go`：令牌、归属、轮换、重试和错误行为。
- `internal/credentials/postgres_integration_test.go`：真实摘要查询、跨空间条件更新、并发撤销及撤销边界。
- `internal/server/credentials.go`：owner 管理 HTTP handler；适配 T-016 会话和 T-019 owner 能力。
- `internal/server/credentials_test.go`：状态码、Origin、Cookie/Bearer 隔离和响应不泄露摘要等 HTTP 行为。

### 修改

- `internal/server/server.go`：只注册上述管理路由，保留 `/api` 不回退静态前端的现有行为。
- `internal/server/server_test.go`：仅在构造参数变化或路由回归需要时调整。
- `cmd/tokenpivot/main.go`：用现有连接池装配 credentials store/service 和实际会话、owner 适配器；main 不写业务逻辑。
- `cmd/tokenpivot/main_test.go`、`cmd/tokenpivot/main_integration_test.go`：只在现有测试因装配签名变化而必须调整时修改。
- `docs/openapi/openapi.yaml`：由主代理协调后新增或追加本计划 API。

### 明确不动

迁移、go.mod/go.sum、采集 handler、防重查询、事件表、前端文件、T-016/T-019 的会话与成员实现。

## 7. 实施顺序

1. 主代理确认第 4 节前置和唯一产品问题，记录 T-008 实际字段/ID 类型、T-016/T-019 调用方式及 T-011 认证接口。
2. 先写 credentials 包测试和窄接口，再实现随机令牌、摘要及 service；不在 handler 写 SQL。
3. 实现 PostgreSQL store：签发前校验 source 属于 workspace；轮换读取旧凭证的固定 source；撤销用单条条件更新；认证按摘要读取归属。
4. 实现 server 管理 handler，接入真实会话、Origin 与 owner 检查，并在 main 装配。
5. 补 OpenAPI 和真实 PostgreSQL 测试，再与 T-011/T-009 联调插件 Bearer、轮换重传和撤销后新凭证重传。
6. 运行 `go test ./...` 与 `go test -race ./...`；真实 PostgreSQL 测试使用仓库既有集成测试环境，不新增秘密输出或静默跳过。

## 8. 验收标准

### 签发与认证

- owner + 合法 Origin 可签发；响应含明文 token，fake/store 中只能观察到 32 字节摘要，摘要与明文不同。
- 无会话、非 owner、缺失/错误 Origin 分别返回上述错误且不写库。
- 用其他 workspace 的 source 签发返回 404，不创建凭证。
- 有效 Bearer 返回数据库固定 workspace/source；错误、格式非法、已撤销令牌均 401。
- 只有 Cookie、或无效 Bearer 与有效 Cookie 同时存在，都不能通过插件认证。

### 轮换、撤销与重试

- 轮换后的新凭证与旧凭证具有完全相同的 workspace/source，客户端不能指定新 source。
- 若用户接受推荐语义：轮换后旧凭证仍可用；显式撤销旧凭证后旧令牌 401、新令牌仍可用。
- 跨 workspace 轮换/撤销返回 404，原凭证和 source 不变。
- 第一次及重复撤销均为 204，`revoked_at` 保持首次值；并发撤销最终只有同一撤销状态，无 500。
- 并发签发/轮换产生独立凭证；测试明确该行为，不能靠先查后插制造竞态。
- 撤销提交后的新认证失败；撤销前已认证的请求允许完成，事件和防重数据保持原样。

### 与 T-009/T-011 联调

- 用旧凭证接受一个带 Idempotency-Key 的请求，轮换后用新凭证原样重传：返回 duplicate，不增加事件。
- 撤销旧凭证后，再给同一 source 签发新凭证并重传同一请求：仍返回 duplicate，证明撤销没有清除防重依据。
- 新凭证不能写往其他 workspace/source；即使请求携带其他归属，也以凭证数据库归属为准。

## 9. 执行切分

**单块，不拆。** 凭证 service、store、管理 handler 和装配共享同一组接口，并行拆分只会增加接口协调。

共享文件限制：`cmd/tokenpivot/main.go`、`internal/server/server.go`、`docs/openapi/openapi.yaml` 由本块修改，但主代理负责与执行中的 T-011 合并和最终装配；本块不改 T-011 handler。无迁移编号、lockfile、生成代码或新依赖需要分配。

## 10. 外部依据

- [Go `crypto/rand.Text`](https://pkg.go.dev/crypto/rand#Text)，2026-10-27，**已通过本地 Go 1.26 工具链 `go doc` 核实**：用于秘密/令牌，至少 128 位随机性。采用它生成令牌，避免自行实现随机编码。
- [Go `crypto/sha256.Sum256`](https://pkg.go.dev/crypto/sha256#Sum256)，2026-10-27，**已通过本地 `go doc` 核实**：返回数据的 SHA-256 摘要。采用标准库保存 32 字节摘要。
- [PostgreSQL Transaction Isolation](https://www.postgresql.org/docs/current/transaction-iso.html) 与 [Data Consistency Checks](https://www.postgresql.org/docs/current/applevel-consistency.html)，2026-10-27，**搜索索引核实，原页受当前网络代理解析限制未直接抓取**：并发 UPDATE 会等待前一更新事务，读提交下后续语句看到已提交结果。仅采用单语句条件更新和明确在途边界；未据此声称可保证更强全局一致性。
- 未引入 GitHub 参考实现：本地设计只需 Go 标准库、现有 pgx 与单表操作，没有需要参考第三方实现的未决技术问题，也避免引入不相符的框架或许可证负担。

## 11. 待确认、假设与主要风险

### 需主代理向用户确认

**轮换是否应自动撤销旧凭证？** 推荐「不自动撤销，插件切换成功后由 owner 显式撤销」。

若用户要求立即失效，计划须改为：单事务锁定旧凭证、确认有效、插入同 source 新凭证并撤销旧凭证；并发轮换只有一个成功，其余返回 `409 conflict`，超时重试不能找回已丢失的新明文。该选择会增加事务边界与中断风险，不能在未确认时擅自采用。

### 技术假设（开工时核实，不需用户回答）

- T-008 能提供摘要唯一索引与必要的固定归属字段；若实际模型不同，先由主代理协调 T-008，不由 T-010 偷加迁移。
- T-016/T-019 最终接口尚未知，本计划只固定所需能力，不虚构其函数签名。
- T-011 执行中的接口尚未核实，最终 Go 签名以主代理协调结果为准。

### 主要风险

- 多任务并行装配可能重复实现插件认证；需先复用 T-011 实际边界。
- 签发/轮换响应丢失无法恢复明文，非幂等重试可能留下未使用凭证；这是只存摘要和一次展示的直接代价，不能通过秘密明文缓存掩盖。
- 撤销不阻断已在途请求；验收和接口文档必须保持这一口径，避免误认为具备全局即时停用。
