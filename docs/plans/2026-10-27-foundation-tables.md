# T-008：基础表迁移（workspaces / projects / sources / ingest_credentials）实施计划

> **状态：已完成规划；独立 doc-review 的责任边界修正已落实，待用户另行授权实施。** 原 Q1–Q6 已按当前仓库证据收口；技术方案没有仍需用户拍板的关键事项。开工仍须满足 §7、§9、§12 的环境检查和跨任务协调条件。评审未执行任何迁移或测试。
> 日期：2026-10-27。关联 T-008，ref 中已有 `数据模型: docs/data-model.md`。主代理只提交本文件，并负责更新 TODO；本轮只做规划，不改代码或其他设计文档；主代理按 F2 提交本计划并通过 TODO 工具更新状态。
> 本轮只读了仓库文件并复用已有官方资料及源码依据（§13），**没有连接数据库，没有构建，也没有运行测试**。下文的验收项都是实施要求，不表示已经通过。

## 1. 目标与非目标

目标：新增迁移 `00002`，按[数据模型:30–34](<../data-model.md#L30-L34>)建立四张基础表，包括主键、唯一索引、引用列索引、NOT NULL、默认值、注释，以及服务角色的授权。服务角色是服务进程连接数据库时使用的最小权限账号，即 README 中的 `tokenpivot_app`。用真实 PostgreSQL 运行集成测试，验证表结构、唯一约束的范围、权限和 up/down 行为，并同步数据模型文档和 README。另外，本任务第一次确定 UUID 默认值、标识列排序规则、索引命名和 GRANT 写法，T-009/T-012 将沿用这些写法（[T-009 开工条件:180](<2026-10-27-usage-events-uniqueness.md#L180>)）。

非目标：
- users、成员、会话和 auth_actions 表；usage_events（T-009）；分钟统计（T-012）。T-019 可以新增自己的认证与成员表，但必须复用本任务的 `workspaces`，不得重复建表或替换其主键。
- 任何 Go 业务代码、查询、repository 或 HTTP API。凭证的签发、轮换、撤销由 T-010 实现；令牌格式和摘要算法由 T-007 冻结，T-010 按冻结结果实现。T-008 只提供当前 T-010 计划所需的存储与约束。
- 外键、CHECK、触发器、RLS、级联删除，以及在数据库端校验同一 workspace 的一致性。这些都已定为由应用校验。
- DELETE 授权；孤儿数据（所引用的父行已不存在的子行）的巡检任务（目前没有调度器，由写入方负责校验）；新的 Go 依赖；分区；种子数据。

## 2. 现状（已核实）

| 事实 | 证据 |
|---|---|
| 目前只有一个工具占位迁移，迁移目录下的所有 `*.sql` 都会被内嵌 | [00001_bootstrap.sql:1–6](<../../db/migrations/00001_bootstrap.sql#L1-L6>)、[embed.go:6–9](<../../db/migrations/embed.go#L6-L9>) |
| 编号从 00002 开始；已发布的迁移不能改写；生产环境只做前向修复，即出错时新增迁移来修正，不回退已执行的迁移 | [README:118–120](<../../README.md#L118-L120>)、[T-050 计划:118](<2026-10-27-database-bootstrap.md#L118>) |
| 业务表的授权“随建表的迁移确定”，不预先授予未来所有表的权限 | [README:57](<../../README.md#L57>)、[T-050 计划:77](<2026-10-27-database-bootstrap.md#L77>) |
| 本地的角色和 schema 由管理员手工创建：`tokenpivot_owner` 拥有 schema `tokenpivot`，`tokenpivot_app` 只有 USAGE 权限；测试库使用同名角色 | [README:44–52](<../../README.md#L44-L52>)、[README:168–169](<../../README.md#L168-L169>) |
| 对象名不加 schema 前缀，依靠连接 URL 里的 `search_path`；测试为每个用例建一个随机 schema | [README:81–82](<../../README.md#L81-L82>)、[迁移集成测试:98–125](<../../cmd/tokenpivot-migrate/main_integration_test.go#L98-L125>) |
| 测试辅助函数：`integrationSchema` 校验两个测试 URL 和两个角色，然后建 schema；`testProvider(t, value string, files fs.FS) (*goose.Provider, *sql.DB)` | [:29–127](<../../cmd/tokenpivot-migrate/main_integration_test.go#L29-L127>)、[:129](<../../cmd/tokenpivot-migrate/main_integration_test.go#L129>) |
| **新迁移会让现有测试失败**：矩阵测试写死了“up 后版本为 1、down-one 后为 0”，还断言业务表数量为 0 | [:174–177](<../../cmd/tokenpivot-migrate/main_integration_test.go#L174-L177>)、[:194–197](<../../cmd/tokenpivot-migrate/main_integration_test.go#L194-L197>) |
| 矩阵测试会断言整个过程只用一条连接 | [:189–192](<../../cmd/tokenpivot-migrate/main_integration_test.go#L189-L192>) |
| 被杀进程恢复测试用真实迁移执行 `p.Up`，不断言版本号 | [:350–388](<../../cmd/tokenpivot-migrate/main_integration_test.go#L350-L388>) |
| 为了脱敏，迁移命令出错时会丢弃驱动返回的错误细节（包括 SQLSTATE），`up` 失败时只输出 `up or unlock: database operation failed` | [database.go:17–30](<../../internal/database/database.go#L17-L30>)、[main.go:101–105](<../../cmd/tokenpivot-migrate/main.go#L101-L105>) |
| 迁移连接设置 lock_timeout 5s、statement_timeout 15s；provider 没有开启乱序执行 | [main.go:67–68](<../../cmd/tokenpivot-migrate/main.go#L67-L68>)、[main.go:85–87](<../../cmd/tokenpivot-migrate/main.go#L85-L87>) |
| **凭证存储需求已由当前 T-010 计划固定**：完整令牌的 SHA-256 摘要为 32 字节，`secret_hash` 必须能保存它并有唯一索引，`revoked_at` 可空；认证按摘要等值查找，令牌中不放凭证 ID。同一 source 可以有多条有效凭证，owner 手动停用旧凭证只更新 `revoked_at`，T-010 不自行补迁移 | [T-010 计划:53–69](<2026-10-27-ingestion-credentials.md#L53-L69>)、[:112–135](<2026-10-27-ingestion-credentials.md#L112-L135>)、[:158](<2026-10-27-ingestion-credentials.md#L158>)。旧[数据模型:34](<../data-model.md#L34>)的 text 与公开 ID 定位写法由本任务同步修正 |
| 版本基线：Go 1.26.4，pgx v5.11.0，goose v3.29.0；PG 18.4 **只确认了客户端版本，服务端版本未核实** | [go.mod:3–7](<../../go.mod#L3-L7>)、[README:37–39](<../../README.md#L37-L39>) |
| 检查命令入口 | [Makefile:48–55](<../../Makefile#L48-L55>) format-check、[:61–79](<../../Makefile#L61-L79>) check、[:82–86](<../../Makefile#L82-L86>) test-integration |
| 下游计划状态已经变化：T-009、T-011 已完成只读评审、有条件可执行，T-010 已完成规划；T-009/T-011 都把 T-008 合入列为开工条件，不能再把它们称为未提交草案 | [T-009 状态:1–5](<2026-10-27-usage-events-uniqueness.md#L1-L5>)、[依赖:180–184](<2026-10-27-usage-events-uniqueness.md#L180-L184>)、[T-010 状态:1–5](<2026-10-27-ingestion-credentials.md#L1-L5>)、[T-011 状态:1–5](<2026-10-27-batch-ingestion.md#L1-L5>)、[依赖:33–39](<2026-10-27-batch-ingestion.md#L33-L39>) |

## 3. 已定前提（不再讨论）

主键用 UUID；时间用 `timestamptz`，按 UTC 存储；字符串用 `text`；不用 FK、CHECK、触发器和 RLS，引用关系和同一 workspace 的一致性由应用校验（[数据模型:19](<../data-model.md#L19>)、[ADR-0004:7](<../decisions/0004-auth-and-workspace.md#L7>)）；source 的身份保持稳定，轮换凭证时不变，撤销凭证时不清除防重记录（[数据模型:38](<../data-model.md#L38>)、[ADR-0004:9](<../decisions/0004-auth-and-workspace.md#L9>)）。

## 4. 表设计

通用规则：除了下表中标为可空的三列，所有列都是 NOT NULL；`id` 为 `uuid PRIMARY KEY DEFAULT uuidv7()`；`created_at` 为 `timestamptz NOT NULL DEFAULT now()`；其他列都没有默认值。主键索引使用 PG 默认名 `<表>_pkey`。ingest_credentials 按当前 T-010 计划已经固定的摘要认证与手动轮换需求编写。

| 表 | 列（类型；空值） | 索引 | 说明 |
|---|---|---|---|
| workspaces | id；name text；created_at | 只有主键 | name 不唯一，非空和长度由应用校验 |
| projects | id；workspace_id uuid；project_key text COLLATE "C"；name text；**archived_at timestamptz 可空**；created_at | `uk_projects_workspace_id_project_key` UNIQUE (workspace_id, project_key) | 唯一索引以 workspace_id 开头，所以它同时用作 workspace_id 的引用索引，支持按 workspace 列出项目。**项目归档后 key 仍被占用**（不用部分唯一索引），否则“归档不删除历史身份”就不成立了。archived_at 为 NULL 表示未归档 |
| sources | id；workspace_id uuid；platform text COLLATE "C"；instance_key text COLLATE "C"；**last_received_at timestamptz 可空**；created_at | `uk_sources_workspace_id_platform_instance_key` UNIQUE (workspace_id, platform, instance_key) | 一行代表一个稳定的插件实例，而不是某一次凭证。platform 的取值由 T-007 冻结，由应用校验；instance_key 的生成方式待 T-010/T-043/T-044 确定，不影响表结构。last_received_at 为 NULL 表示从未成功接收过数据 |
| ingest_credentials | id；workspace_id uuid；source_id uuid；**secret_hash bytea**；**revoked_at timestamptz 可空**；created_at | `uk_ingest_credentials_secret_hash` UNIQUE (secret_hash)；`idx_ingest_credentials_workspace_id_source_id` (workspace_id, source_id) | 认证时按 secret_hash 等值查找。secret_hash 存完整令牌的 SHA-256 摘要原始字节，T-010 生成并校验固定的 32 字节长度；本迁移不加 CHECK。唯一索引既支撑这次查找，也保证一个摘要只对应一条凭证。id 用于撤销、列表等管理操作，**它不是秘密**。**source_id 不加唯一约束**：轮换时插入一行新记录，source_id 不变，因此同一个 source 可以有多条凭证。workspace_id 必须等于该 source 所属的 workspace，由 T-010 在签发时校验 |

以下几处与[数据模型:30–34](<../data-model.md#L30-L34>)不同，请主代理审阅，实施时同步写回文档：
1. **projects、sources、ingest_credentials 增加 `created_at`。** 理由：管理页的列表需要按它排序，凭证的签发时间也需要展示和审计；如果以后再补 NOT NULL 列，已有的行只能填上迁移执行时刻这类不真实的值。`updated_at` 目前没有任何读取方，不加。
2. **凭证表建 (workspace_id, source_id) 组合索引，代替数据模型里写的“source_id 索引”。** ADR-0004 要求所有查询都限定 workspace。这个组合索引既支持按 workspace 列出凭证，也支持按 workspace+source 列出，同时满足“每个 `_id` 引用列都有索引”的要求。系统不会单独按 source_id 查询。
3. **UUID 版本定为数据库默认值 `uuidv7()`**（数据模型原文是“待实现时确认”）。
4. **secret_hash 定为 bytea 并加唯一索引，认证按摘要查找**，取代数据模型中的 `secret_hash text` 和“按凭证公开 ID 定位并验证摘要”，与当前 T-010 计划一致。摘要是二进制值，不是业务字符串，所以不适用“字符串用 text”这条前提。

## 5. 本任务确定的跨表约定（T-009/T-012 沿用）

| 约定 | 决定 | 原因与改动代价 |
|---|---|---|
| ID 生成 | `DEFAULT uuidv7()`，写入时用 `RETURNING id` 取回；Go 侧不新增 UUID 依赖 | 主键按时间有序，对 T-009 的大表索引更友好；PG 18 内置这个函数（E1）。代价是**服务端最低版本变成 PG 18**。以后要换，只需 `ALTER ... SET DEFAULT`，已有的 ID 仍然有效，改动代价低 |
| 标识列排序规则 | project_key、platform、instance_key 使用 `COLLATE "C"`；name 列沿用数据库默认值 | 这些列是机器标识。在确定性排序规则下，唯一性本来就是按字节判定的（E2），所以改用 C 不改变唯一性的含义，只是把排序变成字节序，也不再依赖数据库默认的 locale 和系统排序库的版本（E3）。T-009 已对 source_event_id 采用同一写法并核实 E3，本任务沿用，不再把它留作用户问题 |
| 命名 | 唯一索引用 `uk_<表>_<列>`，普通索引用 `idx_<表>_<列>` | 与 T-009 当前计划一致 |
| schema | 对象名不加前缀，依靠 `search_path` | 项目和测试目前都是这样做的。这与 db-postgres skill 默认的“写全前缀”不同，以项目做法为准 |
| 授权 | 在迁移中写 `GRANT SELECT, INSERT, UPDATE ON <表> TO tokenpivot_app;`，**角色名固定** | README 已约定这个角色，迁移不新增环境变量或角色抽象。角色由管理员手工创建；部署登录名不同时，就建一个 NOLOGIN 的 `tokenpivot_app` 角色，再把它授予实际登录角色，由登录角色继承权限。角色不存在时，整个迁移文件回滚，命令以非零码退出，但输出里没有原因（见 §6），需要按 README 自查 |
| 不授予 DELETE、TRUNCATE、REFERENCES、TRIGGER | 归档和撤销都通过 UPDATE 完成，当前 T-010/T-011 也不需要删除这四张表的数据 | 服务无法删除父行，所以写子行时不需要为防止父行被并发删除而加锁。以后出现真实删除需求时，由对应任务新增迁移补 GRANT，并同时处理并发删除；本任务不预留开关 |
| COMMENT ON | 每张表一条；三个可空列写明 NULL 的含义；platform、instance_key、secret_hash 各一条（secret_hash 的注释写明只存摘要、从不存明文令牌）；用英文写，与现有 SQL 和 Go 注释保持一致 | 与 T-009 当前计划一致；字段说明仍以 data-model.md 为准 |

## 6. 迁移 `db/migrations/00002_foundation_tables.sql`

goose 为每个文件开一个事务，所以四张表要么全部建成，要么一张都不建。下面是目标 DDL，COMMENT 只写了一条示例，其余按 §5 补齐：

```sql
-- Foundation tables (T-008). Referential integrity and same-workspace consistency
-- are enforced by the application: no foreign keys, CHECK, triggers or RLS.
-- +goose Up
CREATE TABLE workspaces (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE projects (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    workspace_id uuid NOT NULL,
    project_key text COLLATE "C" NOT NULL,
    name text NOT NULL,
    archived_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uk_projects_workspace_id_project_key ON projects (workspace_id, project_key);
CREATE TABLE sources (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    workspace_id uuid NOT NULL,
    platform text COLLATE "C" NOT NULL,
    instance_key text COLLATE "C" NOT NULL,
    last_received_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uk_sources_workspace_id_platform_instance_key ON sources (workspace_id, platform, instance_key);
CREATE TABLE ingest_credentials (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    workspace_id uuid NOT NULL,
    source_id uuid NOT NULL,
    secret_hash bytea NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uk_ingest_credentials_secret_hash ON ingest_credentials (secret_hash);
CREATE INDEX idx_ingest_credentials_workspace_id_source_id ON ingest_credentials (workspace_id, source_id);
COMMENT ON COLUMN projects.archived_at IS 'NULL means active; otherwise the time the project was archived.';
-- ...remaining COMMENT ON statements per section 5...
GRANT SELECT, INSERT, UPDATE ON workspaces, projects, sources, ingest_credentials TO tokenpivot_app;

-- +goose Down
DROP TABLE ingest_credentials;
DROP TABLE sources;
DROP TABLE projects;
DROP TABLE workspaces;
```

不写 `IF [NOT] EXISTS`：表已经存在或者已经缺失，说明数据库的实际结构和迁移记录对不上，这时应该失败并由人工核对，不能静默跳过。注释字符串不要跨行，以免 goose 按行尾分号切分语句时出错。

**Up 风险**：都是新建空表和索引，表里没有数据，锁的影响可以忽略，能在迁移连接的 lock_timeout 5s / statement_timeout 15s 内完成，不需要非事务迁移，也不需要并发建索引。可能出现的失败都会让整个文件回滚，版本停在 1：服务端低于 PG 18 时 `uuidv7()` 不存在（SQLSTATE 42883）；`tokenpivot_app` 角色不存在（42704）；对象已存在（42P07）。**因为迁移命令为脱敏丢弃了驱动错误，这三种情况的输出都只有 `up or unlock: database operation failed`，看不到 SQLSTATE**（§2）。所以 README 要给出执行 `up` 前的三项自查：`SHOW server_version;` 不低于 18；`SELECT 1 FROM pg_roles WHERE rolname = 'tokenpivot_app';` 能查到结果；00002 尚未执行时，这四张表都不存在。需要原始错误时，查看数据库服务端日志。本任务不改变错误的输出方式，因为脱敏是已有的安全措施。
**合入顺序**：goose 默认遇到缺号的迁移会报错，迁移命令也没有开启乱序执行（E5），所以 **T-008（00002）必须先于 T-009（00003 及以后）合入**，并且不能有并行分支占用 00002。
**Down 风险**：会删除这四张表中的全部数据，且无法恢复，只能在操作者确认过的本地库或测试库上使用（README 已有此规定）；生产环境只做前向修复。T-009 合入后，`down-one` 先撤销的是 T-009；要撤销 00002，必须先撤销它之后的全部迁移。由于没有 FK，测试库里留下的事件行会变成孤儿数据。00002 一旦在任何共享环境执行过，就不能再修改；以后加列（例如 T-010 的标签、last_used_at、到期时间）一律通过新迁移完成。

## 7. 测试与验收

### 7.1 文件与写法
- **修改** [main_integration_test.go](<../../cmd/tokenpivot-migrate/main_integration_test.go>)：
  - 在 `integrationSchema` 现有的服务角色检查处（[:91–97](<../../cmd/tokenpivot-migrate/main_integration_test.go#L91-L97>)）、建 schema 之前，用服务测试连接执行 `SELECT pg_has_role(current_user, 'tokenpivot_app', 'USAGE')`，结果必须为 true，即服务测试账号就是 `tokenpivot_app`，或者不用 SET ROLE 就能直接使用它的权限（E6）。否则以 `test service role must have tokenpivot_app privileges` 失败；如果这个角色根本不存在，查询本身会报错，测试同样直接失败。原因：迁移只给这个固定角色授权，测试环境配置错了的话，后面的用例只会报出难以定位的 42704 或 42501。
  - 在 `TestSingleConnectionMatrix` 中，对 [:156](<../../cmd/tokenpivot-migrate/main_integration_test.go#L156>) 已有的 `p` 调用 `p.ListSources()` 推导版本。这个方法按版本升序返回，不访问数据库，所以不会影响 [:189–192](<../../cmd/tokenpivot-migrate/main_integration_test.go#L189-L192>) 的单连接断言（E4）。`latest` 取最后一项的 `Version`，`previous` 取倒数第二项；少于两项时直接失败。把 [:174–177](<../../cmd/tokenpivot-migrate/main_integration_test.go#L174-L177>) 的期望值改为：status 后为 0，down-one 后为 previous，up 后为 latest。删掉 [:194–197](<../../cmd/tokenpivot-migrate/main_integration_test.go#L194-L197>) 中“业务表数量为 0”的断言，因为从 T-008 起这个前提不再成立；保留 [:207–211](<../../cmd/tokenpivot-migrate/main_integration_test.go#L207-L211>) 的两条服务角色负向检查。这样以后 T-009 和其他迁移都不用再改这里的版本逻辑。
- **新增** `cmd/tokenpivot-migrate/foundation_tables_integration_test.go`（`//go:build integration`，package main）：复用 `integrationSchema`、`testProvider` 和 `migrations.Files`。**用 `p.UpTo(ctx, 2)` 和 `p.DownTo(ctx, 1)` 把测试固定在本迁移的版本上**：`UpTo` 执行到目标版本并包含它，`DownTo` 回退到目标版本但不包含它（E4）。这样以后新增迁移不会影响这些断言。比较 SQLSTATE 时，用 `errors.As` 取出 `*pgconn.PgError` 再比较 `Code`；不加新依赖，不调用 `t.Parallel()`，只使用合成数据（secret_hash 用固定的 32 字节测试值，不使用真实令牌）。

### 7.2 场景
| # | 场景 | 断言 |
|---|---|---|
| 1 | 结构（用迁移账号查系统目录） | `information_schema.columns`：四张表的列集合和 `data_type`（uuid / text / bytea / timestamp with time zone）完全一致，其中 secret_hash 为 bytea；只有 archived_at、last_received_at、revoked_at 可空；id 的默认值包含 `uuidv7()`，created_at 的默认值为 `now()`，其余列没有默认值；三个标识列的 `collation_name='C'`，name 列为 NULL |
| 2 | 索引 | 从 `pg_indexes` / `pg_index` 读出的索引恰好是四个主键加上四个指定索引，列的顺序和是否唯一都与 §4 一致 |
| 3 | 禁止的对象 | 在当前 schema 中：`pg_constraint` 里 contype 为 `f` 或 `c` 的行数为 0（只按这两类过滤，不依赖 NOT NULL 在系统目录中的存储方式）；非内部的 `pg_trigger` 行数为 0；四张表的 `relrowsecurity` 都是 false |
| 4 | 授权 | `information_schema.role_table_grants` 中，grantee 为 `tokenpivot_app` 的记录在四张表上恰好是 SELECT、INSERT、UPDATE |
| 5 | 默认值（用服务角色） | 只插入 name 并 `RETURNING id, created_at`：`uuid_extract_version(id)=7`，created_at 不为 NULL |
| 6 | 项目唯一性的范围 | 插入 (ws1,'k') 成功；再次插入 (ws1,'k') 报 23505；插入 (ws2,'k') 成功；把 (ws1,'k') 归档后再插入 (ws1,'k')，仍报 23505；插入 (ws1,'K') 成功（按字节判等，大小写规范化由应用负责） |
| 7 | source 唯一性的范围 | 重复插入同一个 (ws, platform, instance_key) 报 23505；platform 不同或 ws 不同时可以插入 |
| 8 | 轮换与撤销 | 同一个 source_id 可以插入两条 secret_hash 不同的凭证；用已存在的 secret_hash 再插入一条报 23505；更新第一条的 revoked_at 成功；source 行的 id、workspace_id、platform、instance_key 前后不变；更新 sources.last_received_at 成功 |
| 9 | 权限拒绝 | 服务角色对四张表执行 DELETE、TRUNCATE、`ALTER TABLE` 都报 42501 |
| 10 | 回退 | 执行 `DownTo(1)` 后，四张表的 `to_regclass` 都是 NULL，`GetDBVersion=1`，goose 历史表仍在；之后再次执行 `UpTo(2)` 成功 |
| 11 | 回归 | 修改后的矩阵测试（包括 no change）、事务回滚、争锁、被杀进程恢复等原有用例全部通过 |

### 7.3 命令（都是仓库中已有的命令；本轮没有运行）
```sh
gofmt -l cmd internal db                                   # 期望无输出
GOTOOLCHAIN=local go vet -mod=readonly -tags=integration ./cmd/tokenpivot-migrate/
GOTOOLCHAIN=local go test -mod=readonly -count=1 ./...
# 在专用 _test 库上执行，并独占该库。先在本会话中安全地注入
# TOKENPIVOT_TEST_DATABASE_URL 和 TOKENPIVOT_TEST_APP_DATABASE_URL（只引用变量名，不写出值）：
GOTOOLCHAIN=local go test -mod=readonly -tags=integration -count=1 -p 1 -v ./cmd/tokenpivot-migrate/
make format-check && make check && make test-integration   # 最终的发布前检查（含联合标签）
```
开工前，先在测试库上执行 `SHOW server_version;` 并记录结果；**如果低于 18，就停下来报告**，不要自行改成 `gen_random_uuid()`。

手工验收：先运行 `make build-go`，在本地专用库上只注入 `TOKENPIVOT_MIGRATION_DATABASE_URL`（[README:92](<../../README.md#L92>)），然后依次执行 `./bin/tokenpivot-migrate status`、`up`、`up`、`down-one`、`up`。根据 [main.go:91–122](<../../cmd/tokenpivot-migrate/main.go#L91-L122>) 的日志输出，应该依次看到：第一次 `up` 输出 `applied` 版本 2（全新的库会先输出版本 1）；第二次 `up` 输出 `no change`；`down-one` 输出 `reverted` 版本 2；最后一次 `up` 再次输出 `applied` 版本 2。然后用 `psql \d+ projects` 等命令抽查。注意，`down-one` 会清空本地这四张表。交付时记录真实的 PG 版本、各条命令的退出码，以及没有执行的项目。

## 8. 文档同步（实施者在同一次提交中完成）
- [data-model.md](<../data-model.md>)：第 3 行说明这四张表已由 00002 实现；§2 写入 UUID v7 默认值、PG 18 的最低版本要求、标识列使用 C 排序规则，以及索引命名规则；§3 的四行改为实际的列、索引和服务角色权限，并补上 §4 列出的四处差异。第 38 行同时澄清：当前 sources 没有 `archived_at`，本任务只保证“凭证撤销或项目归档不清除防重记录”；source 生命周期字段由有真实需求的管理任务另起迁移。§4（usage_events）归 T-009 负责，本任务不动。
- [README](<../../README.md>)：修改第 3 行的“当前没有业务表”；修改第 57 行，说明迁移会给固定角色 `tokenpivot_app` 授予表权限、执行 `up` 前这个角色必须存在，以及登录名不同时如何通过 NOLOGIN 角色继承权限；在测试准备一节写明服务测试账号必须是 `tokenpivot_app` 或继承它的权限；写入 §6 的三项执行前自查，并说明命令失败时输出中不包含原因。不列出逐表的授权清单，以迁移文件和 data-model 为准。

## 9. 后续任务的责任边界
- **T-010**：按 T-007 冻结的令牌格式和摘要算法实现 SHA-256 摘要生成；按 `secret_hash` 等值查找的认证查询；撤销判断、owner 手动轮换和旧凭证停用；签发时校验已有 source 属于同一个 workspace。source 创建流程及 instance_key 来源由主代理结合 T-043/T-044 另行协调归属，不视为 T-010 已承接的交付物，也不扩入 T-008；T-007 冻结结果如与当前摘要设计不一致，开工前先协调，不并存两套格式。00002 已提供 32 字节摘要存储、摘要唯一索引、可空 revoked_at、允许同 source 多凭证的结构和组合索引，T-010 不再建凭证表或补这些约束。以后确有新增列需求时另起迁移，不修改已发布的 00002。
- **T-011**：在 workspace 内查找项目；已归档的项目能否继续接收事件（这是产品规则，由 T-007 冻结）；维护 last_received_at（每批数据都会更新 sources 中的同一行，并发高时请求会在这一行上排队等锁，需要自行评估更新频率）；拒绝跨 workspace 访问的测试。**[T-011 计划:37](<2026-10-27-batch-ingestion.md#L37>) 把“同空间关联读取、归档/状态读取规则”写成 T-008 的交付物，是当前的过时表述**：T-008 只交付表结构、约束、授权和列含义，读取代码及规则由 T-010/T-011 自己负责。主代理后续派发 T-011 时须明确以本节边界为准；本次规划提交和 T-008 实施都不改 T-011 文档。
- **T-009/T-012**：沿用 §5 的约定；在 T-008 合入后顺延编号；各自新建 `*_integration_test.go`，不再修改矩阵测试中的版本逻辑。
- **T-019**：直接复用 00002 的 `workspaces.id`，只新增自己负责的用户、成员、会话等表；不得重复创建 workspaces 或另建一套工作空间主键。成员表、用户表，以及项目和空间的管理 API 均不属于 T-008。
- T-013 的清理只涉及事件表和统计表，不需要这四张表的 DELETE 权限。

## 10. 执行切分

**单块，不拆**：由一个 dev 在一个 worktree 中完成。不需要共享的基础层，也不改 go.mod/go.sum、Makefile、`internal/*`、`cmd/tokenpivot/*`、[main.go](<../../cmd/tokenpivot-migrate/main.go>)、[embed.go](<../../db/migrations/embed.go>)、[00001](<../../db/migrations/00001_bootstrap.sql>)、TODO、架构文档或 ADR。

| 文件 | 归属 |
|---|---|
| 新增 `db/migrations/00002_foundation_tables.sql` | T-008，占用编号 00002；主代理负责确保没有并行分支占用同一编号 |
| 新增 `cmd/tokenpivot-migrate/foundation_tables_integration_test.go` | T-008 |
| [main_integration_test.go](<../../cmd/tokenpivot-migrate/main_integration_test.go>) | T-008 先改（§7.1）；T-009 在此基础上做最小的追加，按编号顺序合入 |
| [data-model.md](<../data-model.md>) | T-008 负责第 3 行、§2、§3 和第 38 行的边界澄清；T-009 只改 §4 |
| [README](<../../README.md>) | T-008 |

步骤：① 核对 PG 服务端版本，确认角色已按 README 准备好；② 编写 00002；③ 修改测试辅助函数和矩阵测试，新增测试文件；④ 运行 §7.3 中只针对迁移包的命令；⑤ 同步文档；⑥ 运行完整的发布前检查。

## 11. 待确认与跨计划协调

**用户待确认：无。** 原 Q1/Q2/Q3/Q5 不再当作需要用户批准的产品选择：固定 `tokenpivot_app` 来自 README 的现有部署约定；机器标识使用 C 排序规则与 T-009 当前计划一致；当前任务和下游读取流程没有 DELETE 需求；sources 的 `archived_at` 没有当前读取方，不能仅凭数据模型第 38 行的“source 归档”措辞新增字段。原 Q6 已由当前 T-010 计划固定为“按 32 字节 SHA-256 摘要认证、secret_hash 唯一、可空 revoked_at、允许同 source 多凭证”，不再保留 text/公开 ID 定位的备选方案。

这些是本计划依据仓库证据作出的设计结论，不表示用户另行批准。若主代理或独立 doc-review 找到相反证据，应在 00002 进入任何共享环境前修订本计划；00002 一旦已执行，就只能新增迁移修正。

**主代理协调一项：** 后续派发 T-011 时，明确指出其计划第 37 行对 T-008 交付物的描述已被本计划 §9 取代；本次只提交 T-008 plan，不修改 T-011 文件。这不扩大 T-008 范围，也不阻塞独立 doc-review。

## 12. 风险与未核实
- PG 服务端版本未核实，而 `uuidv7()` 要求服务端为 PG 18 或更高。本轮没有构建、没有连接数据库，也没有运行测试。
- T-010/T-011 尚未实施，00002 是它们共同的 schema 前置；合入前主代理必须按本计划 §9 再核对凭证字段和读取边界，避免下游另建表或补一套认证字段。
- 迁移失败时命令输出不含原因（这是脱敏设计），排障依赖 README 中的自查步骤；本任务不改变这一点。
- 固定角色名对自托管用户来说是一项部署约定，需要在 README 中写清楚。
- C 排序规则会固定机器标识的字节排序与判等行为；应用仍需负责大小写规范化，不能把数据库唯一索引当成规范化机制。E3 已由 T-009 的既有研究核实，本任务不重复外查。
- 测试固定在版本 2 上，前提是 00002 的编号不再变动。如果合入顺序与编号不一致，goose 会报缺号错误。

## 13. 依据（抓取日期 2026-10-27）
| 编号 | 事实 | 来源 | 状态 | 采用情况 |
|---|---|---|---|---|
| E1 | PG 18 内置 `uuidv7()` 和 `uuid_extract_version(uuid) → smallint`，`gen_random_uuid()` 也可用 | [PG 18 UUID 函数](<https://www.postgresql.org/docs/18/functions-uuid.html>) | 已核实（官方文档；未在本仓库的实例上运行） | 用作默认值，并用于测试断言 |
| E2 | 在确定性排序规则下，只有字节序列相同的字符串才判为相等 | [PG 18 Collation](<https://www.postgresql.org/docs/18/collation.html>) | 已核实 | 说明 C 排序规则不改变唯一性的含义 |
| E3 | 操作系统的 locale 数据变化可能导致相关索引损坏 | [PG wiki: Locale data changes](<https://wiki.postgresql.org/wiki/Locale_data_changes>)、[T-009 既有核实:226](<2026-10-27-usage-events-uniqueness.md#L226>) | 已核实（T-009 已阅读全文并记录；本轮未重复外查） | 作为机器标识使用 C 排序规则的辅助理由 |
| E4 | goose v3.29.0：`ListSources` 按版本升序返回；`UpTo` 执行到目标版本（包含）；`DownTo` 回退到目标版本（不包含）；`GetDBVersion` 返回已记录的最高版本；`Source` 包含 `Version int64` | [provider.go](<https://github.com/pressly/goose/blob/v3.29.0/provider.go>)（L182–195、L286–291、L330–338）、[provider_types.go](<https://github.com/pressly/goose/blob/v3.29.0/provider_types.go#L21-L24>) | 已核实（读了源码注释，未运行） | 用于固定版本的测试和矩阵测试的版本推导 |
| E5 | goose 默认遇到缺号（乱序）的迁移会报错，必须显式设置 `WithAllowOutofOrder` 才放行；本仓库的迁移命令没有设置这个选项 | [provider_options.go](<https://github.com/pressly/goose/blob/v3.29.0/provider_options.go#L177-L184>)、[main.go:85–87](<../../cmd/tokenpivot-migrate/main.go#L85-L87>) | 已核实（读了源码，未运行） | 按编号顺序合入 |
| E6 | `pg_has_role(user, role, 'USAGE')` 表示不用 SET ROLE 就能直接使用该角色的权限 | [PG 18 系统信息函数](<https://www.postgresql.org/docs/18/functions-info.html>) | 已核实（官方文档） | 用于测试前置检查 |

术语：服务角色=服务进程连接数据库时使用的最小权限账号 `tokenpivot_app`；孤儿数据=所引用的父行已不存在的子行；前向修复=出错时新增迁移来修正，不回退已执行的迁移。
