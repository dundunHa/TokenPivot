# T-009：用量明细表与防重唯一约束实施计划

> 日期：2026-10-27。TODO：T-009。状态：**已完成只读评审并落实修正；有条件可执行，§8 开工条件尚未满足，不授权编码。**
> 本次只写本文件；未改代码、迁移、TODO 或其他文档，未连接数据库、未运行测试。仓库根目录没有 AGENTS.md（已列根目录核实），约定取自 README 与 docs。

## 1. 目标与非目标

目标：新增一条 goose 迁移，建普通表 `usage_events`（不可变的请求级最终用量事件），用数据库唯一约束 `(workspace_id, source_id, source_event_id)`（下称“身份键”）仲裁重复；持久化 duplicate/conflict 判定所需的比较依据和首次 `received_at`；区分缺失与零；服务角色（服务进程连库所用的最小权限账号）只拿 SELECT、INSERT；用真实 PG 测试证明这些数据库层性质，并同步 `docs/data-model.md` §4。

非目标：HTTP、认证、批量流程、过期判断、时间采样、duplicate/conflict 业务判定与错误映射（T-011/T-007/T-010）；分钟聚合（T-012）；清理任务及其 DELETE 授权（T-013）；重建（T-014）；查询接口及筛选索引（T-024/T-026/T-028）；workspace/source/project/credential 表（T-008）；分区、外键、CHECK、触发器、RLS；新 Go 依赖或 Go 存储包；保存原始报文或凭证 ID；任何吞吐、容量承诺。

## 2. 现状与证据（本轮读取）

| 事实 | 证据与影响 |
|---|---|
| 迁移只有工具占位，无业务表 | [bootstrap:1–6](<../../db/migrations/00001_bootstrap.sql#L1-L6>)；[embed.go:6–9](<../../db/migrations/embed.go#L6-L9>) 内嵌该目录全部 `*.sql`，新迁移自动打包，测试夹具不得放入此目录。 |
| 编号与发布规则 | [README:118](<../../README.md#L118>)：后续从 00002 编号，已在共享环境执行的文件不能改写；[README:120](<../../README.md#L120>)：逐文件事务，生产前向修复、不自动 down。 |
| 迁移执行约束 | [migrate main:67–71](<../../cmd/tokenpivot-migrate/main.go#L67-L71>) 单连接、`lock_timeout=5s`、`statement_timeout=15s`；[main:85–87](<../../cmd/tokenpivot-migrate/main.go#L85-L87>) 未开启乱序迁移（goose 默认遇缺号报错，E8）。 |
| 授权由迁移决定，写法未定 | [README:44–45](<../../README.md#L44-L45>) 本地示例角色 `tokenpivot_owner`/`tokenpivot_app`；[README:57](<../../README.md#L57>)“业务表授权由后续迁移确定，不预授未来所有表权限”。第一张业务表（T-008）将首次确定授权写法。 |
| 设计已定、物理未定 | [数据模型:40–56](<../data-model.md#L40-L56>) 字段建议与身份键；[:58](<../data-model.md#L58>) 候选索引；[:94](<../data-model.md#L94>) “精确索引须在实施计划确认”；[:19](<../data-model.md#L19>) 不用外键/CHECK/触发器/RLS；[:96](<../data-model.md#L96>) 文档须与迁移同步。 |
| 业务规则 | [ADR-0002:7–9](<../decisions/0002-ingestion-idempotency-and-batch.md#L7-L9>) 身份、duplicate/conflict、不静默覆盖、接收时间不参与比较；[:21](<../decisions/0002-ingestion-idempotency-and-batch.md#L21>) 改身份或规范化哈希须版本化迁移。 |
| T-011 对本任务的期待 | [T-011 计划:38](<2026-10-27-batch-ingestion.md#L38>)：普通表、身份键、规范化版本及比较依据、首次 received_at、所需权限，业务 SQL 归属待定；[:52](<2026-10-27-batch-ingestion.md#L52>) 其 `internal/ingestion` postgres 文件负责参数化 SQL；[:79–86](<2026-10-27-batch-ingestion.md#L79-L86>) `INSERT … ON CONFLICT DO NOTHING` 加下一条 SELECT；[:84](<2026-10-27-batch-ingestion.md#L84>) 提到“需要时用行共享锁”，与本计划权限设计冲突（§4.4、Q1）。 |
| 测试库安全写法已存在 | [migrate 集成测试:29–127](<../../cmd/tokenpivot-migrate/main_integration_test.go#L29-L127>) `integrationSchema`（两 TEST 变量、回环 IP、`_test` 库、角色检查、随机 `t050_` schema、归属核对后清理）；[:129–150](<../../cmd/tokenpivot-migrate/main_integration_test.go#L129-L150>) `testProvider`；[:305–326](<../../cmd/tokenpivot-migrate/main_integration_test.go#L305-L326>) 用 ticker 加期限轮询 pg_locks 观察等待。同类 helper 另有 [database 包副本](<../../internal/database/database_integration_test.go#L18-L116>) 和 [服务入口测试副本](<../../cmd/tokenpivot/main_integration_test.go#L185>)。 |
| 现有迁移测试写死“无业务表” | [:174–180](<../../cmd/tokenpivot-migrate/main_integration_test.go#L174-L180>) up 后版本恒为 1、down-one 后为 0；[:194–197](<../../cmd/tokenpivot-migrate/main_integration_test.go#L194-L197>) 断言业务表数为 0；[:207–211](<../../cmd/tokenpivot-migrate/main_integration_test.go#L207-L211>) 服务角色不能建表或写迁移历史。T-008 合入时必改，本任务在其结果上追加。 |
| 工具链 | [go.mod:3–7](<../../go.mod#L3-L7>) Go 1.26.4、pgx v5.11.0、goose v3.29.0，无 UUID 库；[README:37](<../../README.md#L37>) 本地验收 PG 18.4；[Makefile:82–86](<../../Makefile#L82-L86>) `test-integration` 用 `uiembed,integration` 标签、`-p 1`。 |
| 错误脱敏边界 | [database:17–30](<../../internal/database/database.go#L17-L30>) `SafeError` 丢弃驱动细节；本任务不新增错误分类。 |

2026-10-27 `todo list`：T-007、T-008、T-010、T-012 为规划中且 docs/plans 下无其计划文件；T-011 显示执行中，但其计划写明前置未满足。本文只对其他任务提条件，不替其定稿。

## 3. 已定前提（不重开）

普通表、不分区；身份键为三字段；事件不可变，首次 received_at 不续期；数据库不用外键、CHECK、触发器、RLS（主代理转达的用户决定，与 [数据模型:19](<../data-model.md#L19>)、[:56](<../data-model.md#L56>) 一致）。

## 4. 表设计

### 4.1 列

| 列 | 类型/空值 | 说明与责任 |
|---|---|---|
| id | uuid NOT NULL，主键 | 内部身份，也是游标次序键。生成方式（应用生成或 `DEFAULT uuidv7()`，PG 18 内置，E7）沿用 T-008 约定，本任务不另定。 |
| workspace_id | uuid NOT NULL | 只取自 T-010 认证结果；无外键。 |
| source_id | uuid NOT NULL | 稳定插件实例，不是凭证 ID。 |
| source_event_id | text COLLATE "C" NOT NULL | 不透明 ID，逐字节相等。编码后最大字节数、字符集由 T-007 冻结、T-011 入口校验；T-009 开工前须核实该上限与两个 UUID 组成的实际唯一索引项兼容。具体上限本次未核实，不能仅限制字符数或靠数据库写入报错兜底。 |
| project_id | uuid 可空 | NULL 只表示未归属；非法或跨空间 ID 由 T-011 拒绝，不得降为 NULL。 |
| provider、model | text NOT NULL | T-007 规范化后的原身份。 |
| occurred_at | timestamptz NOT NULL，无默认值 | 精度由 T-007 冻结，不得细于 timestamptz 的 1 微秒（E6）；摘要与落库必须使用同一可无损表示的时间值。超精度输入是拒绝、舍入还是截断，由 T-007 冻结并提供窗口边界及重传样例，本任务不代定。 |
| received_at | timestamptz NOT NULL，**无默认值** | 首次成功接受的服务端时间，由 T-011 按 T-007 冻结的采样点显式传入，不让数据库默默选时钟。须保证 occurred_at ≤ received_at（[T-011 计划:93](<2026-10-27-batch-ingestion.md#L93>)），数据库不校验。 |
| normalization_version | text NOT NULL | 取值由 T-007 定。 |
| payload_hash | bytea NOT NULL | 规范化业务内容的原始摘要字节，算法与长度由 T-007 定（版本隐含算法）。数据模型原写 text，改为 bytea：消除十六进制大小写等表示歧义、按字节比较，并同步改文档。不含 received_at、凭证、id、传输字段顺序。 |
| 各指标值 `<metric>`（候选 input/output/total/cache_read/cache_write/reasoning_tokens） | bigint 可空 | NULL＝缺失，0＝已知零，不设默认值。最终清单与 total 语义由 T-007 定。 |
| 各指标质量 `<metric>_quality` | text NOT NULL | 每个指标一列，词表由 T-007 冻结（数据模型建议 known/estimated/missing）并写进 COMMENT。值为 NULL 当且仅当质量为缺失，由 T-011 校验。 |
| status、duration_ms | text / bigint 可空 | 仅当 T-007 v1 格式包含时建列；NULL＝来源不可得。 |

不加 created_at/updated_at：received_at 已是写入时间，行不可变，无更新时间可言。不存原始报文或 jsonb：比较集合已落成列，也避免意外留存未声明字段。

### 4.2 约束与索引

| 对象 | 定义 | 理由 |
|---|---|---|
| usage_events_pkey | PRIMARY KEY (id) | — |
| uk_usage_events_workspace_id_source_id_source_event_id | 建表时 UNIQUE (workspace_id, source_id, source_event_id) | 身份键和 ON CONFLICT 的仲裁对象；三列均 NOT NULL，不涉及 NULL 唯一性。前缀 (workspace_id, source_id) 同时覆盖 source 引用访问。 |
| idx_usage_events_workspace_id_occurred_at_id | (workspace_id, occurred_at, id) | 已确认的访问路径：明细游标（T-028）、60 秒速率（T-026）、重建范围（T-014），见 [数据模型:58](<../data-model.md#L58>)。 |
| idx_usage_events_received_at | (received_at) | 按首次接收保留清理（[数据模型:60](<../data-model.md#L60>)），供 T-013；其分批方式若需别的形状，由 T-013 替换。 |

- 两条读/清理索引随建表一起建：此时表为空，可在迁移事务内直接建；以后在有数据的表上补建，要按 db-postgres 迁移规范另行评估锁级别。不对写入开销作承诺，T-011 按 T-007 限额验证时再观察。
- 不建单独 source_id 索引：已被身份键前缀覆盖，且租户访问都带 workspace_id（[数据模型:19](<../data-model.md#L19>)）。不建 project_id 索引：项目只归档不删除、不级联（[数据模型:32](<../data-model.md#L32>)、[:38](<../data-model.md#L38>)），没有“删父行前检查”的路径；按项目筛选的组合索引由查询任务按 EXPLAIN 加（[数据模型:58](<../data-model.md#L58>)）。这偏离 db-postgres skill“所有 _id 列建索引”的默认，理由写进 data-model.md。
- `source_event_id` 用 COLLATE "C"：确定性排序规则下相等就是字节相同（E4），相等语义不变；C 排序不受操作系统 locale 数据升级影响，而 libc locale 变化可能让 B-tree 索引顺序失效，放进本应拒绝的重复数据（E5）。README 建库语句未指定 locale（[README:46](<../../README.md#L46>)），所以不依赖库默认值。
- 名称用 skill 默认的 `uk_`/`idx_` 前缀；T-008 若已定其他命名约定，以 T-008 为准。
- 不分区；无外键、CHECK、触发器、RLS；存储参数用默认值。
- COMMENT ON：表（不可变最终事件、服务角色仅 SELECT/INSERT）、project_id、received_at、payload_hash、normalization_version、各质量列词表及 NULL 含义。goose 以“行内最后一个词以 `;` 结尾”判定语句结束，遇 `--` 停止扫描（E9），所以注释文本写成单行，不含 ASCII 分号结尾，也不含 `--`。

### 4.3 比较依据持久化

- 持久化三样：normalization_version、payload_hash、T-007 比较集合中的**每个字段**（按冻结精度成列）。
- 同版本摘要比较及跨版本比较规则由 T-007 冻结、T-011 执行。类型化列不保证能按新规则重建：旧规范化可能丢失时间精度、原始名称或字段，新增可空列不能恢复这些信息。不得仅凭版本不同或按新规则重算不同就判 conflict。
- 开工条件：T-007 的比较集合必须是本表列的子集，并提供基于实际留存信息的跨版本比较规则和样例，覆盖有损变化。无法重建时须明确兼容限制及旧版本重传如何保持原判定；若与既定不可破坏旧摘要判定要求冲突，先报告并解决，不自行降级。不预建双哈希框架、不保存原始报文。未来新增字段须经新迁移与兼容评审，不以补可空列宣称兼容完成。
- 数据库不校验摘要与列是否一致（无 CHECK/触发器）；T-011 必须从同一个规范化结果计算摘要和列值，并由其测试覆盖。

### 4.4 权限与不可变

- 服务角色只授表级 `SELECT, INSERT`，没有其他表权限（UPDATE、DELETE、TRUNCATE、REFERENCES、TRIGGER 等）。GRANT 写在本迁移里，被授权角色的写法沿用 T-008 冻结的方式；本任务不新增角色名配置。
- 效果（E2、E3）：`INSERT … ON CONFLICT DO NOTHING RETURNING id` 所需的 INSERT 与 SELECT 都具备；`UPDATE`、`DELETE`、`ON CONFLICT DO UPDATE`、`SELECT … FOR SHARE/FOR UPDATE` 对服务角色都会因权限不足失败（预期 SQLSTATE 42501，以测试为准）。即使 SQL 写错，服务角色也无法覆盖内容或续期 received_at。
- 对 T-011 的影响：[T-011 计划:84](<2026-10-27-batch-ingestion.md#L84>) 的“必要时行共享锁”在此权限下不可用。推荐 T-011 不锁本表的行：行对服务角色不可变；duplicate/conflict 不写任何数据；唯一的删除方是 T-013 清理，且只能删已出窗口的行（[数据模型:60](<../data-model.md#L60>)、[T-011 计划:93](<2026-10-27-batch-ingestion.md#L93>)），读完后行被清理不改变正确结论。见 Q1。
- 清理所需 DELETE 由 T-013 在自己的迁移里授予执行清理的角色。
- 无 RLS 时的租户隔离：服务对本表的每条 SQL 都带认证得到的 workspace_id（身份查询再带 source_id、source_event_id）；写路径由 T-011、读路径由 T-021 等任务负责并测试。本任务只证明身份键按空间、来源独立。

## 5. SQL 与校验责任归属

| 内容 | 归属 |
|---|---|
| DDL、约束与索引名、COMMENT、GRANT、data-model.md §4 同步、数据库层测试 | T-009 |
| 生产 `INSERT … ON CONFLICT (workspace_id, source_id, source_event_id) DO NOTHING RETURNING id`、下一条身份 SELECT、事务生命周期、时间采样与窗口复核、duplicate/conflict 判定、错误映射与脱敏日志 | T-011，放在其 `internal/ingestion` postgres 文件（[T-011 计划:52](<2026-10-27-batch-ingestion.md#L52>)、[:170](<2026-10-27-batch-ingestion.md#L170>)） |
| 指标清单、质量词表、比较集合、规范化/摘要/版本、时间精度与采样点、ID 长度与字符集、provider/model 规范 | T-007 |
| workspace/source 来源与凭证规则；项目同空间校验 | T-010；T-008 的表加 T-011 的校验 |
| 分钟表 SQL；清理 SQL 与 DELETE 授权；读查询及筛选索引 | T-012；T-013；T-024/T-026/T-028 |

本表对生产 SQL 的约定（写进 data-model.md §4）：冲突目标只用这三列推断，冲突动作只用 DO NOTHING；身份查询三列齐全；不对本表加行锁；只有身份键竞争走 duplicate/conflict 路径，其他 23505 属存储失败（[T-011 计划:86](<2026-10-27-batch-ingestion.md#L86>)）。

应用层校验（数据库无 CHECK/外键，由 T-011 在入口做一次，存储层不重复）：非负与范围、NULL⇔缺失配对、质量词表、ID 长度与字符集、provider/model 非空与规范、项目属于同一空间、摘要长度、occurred_at ≤ received_at。

否决的备选：由 T-009 提供 Go 存储包（接收调用者 pgx.Tx 的插入/查询函数）。好处是 SQL 贴近 DDL；代价是为两条语句新建包和导出类型，且依赖尚未冻结的字段，与 T-011 已规划的 postgres 文件重叠；“不覆盖”已由权限层保证，与 SQL 由谁写无关。日后要改，只需把两条语句挪进包，不动表结构，代价低。

## 6. 迁移

- 文件：`db/migrations/<N>_usage_events.sql`，N 为实施时 main 上最大编号加一（T-008 合入后），五位数字。合入顺序必须与编号顺序一致：goose 默认遇缺号报错（E8），本项目未开乱序。
- Up（goose 单文件事务）：CREATE TABLE（含主键、身份键）→ 两条 CREATE INDEX → COMMENT ON → GRANT。对象名不加 schema 前缀、依赖 search_path，这是项目现状（测试使用随机 schema），偏离 skill 的“写全 schema 前缀”。空表 DDL 不重写数据，预期在迁移命令 5s 锁等待、15s 语句期限内完成，以场景 11 实测为准，不需要非事务迁移。
- Down：`DROP TABLE usage_events;`，只供开发与测试的 down-one；在有数据的库上执行会销毁全部明细，生产只前向修复（[README:120](<../../README.md#L120>)），不在数据库侧加防护逻辑。

示意（非最终 DDL：其余指标按 T-007 清单成对追加；id 默认值与 GRANT 对象按 T-008；COMMENT 略）：

```sql
-- +goose Up
CREATE TABLE usage_events (
    id uuid NOT NULL,
    workspace_id uuid NOT NULL,
    source_id uuid NOT NULL,
    source_event_id text COLLATE "C" NOT NULL,
    project_id uuid,
    provider text NOT NULL,
    model text NOT NULL,
    occurred_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL,
    normalization_version text NOT NULL,
    payload_hash bytea NOT NULL,
    input_tokens bigint,
    input_tokens_quality text NOT NULL,
    CONSTRAINT usage_events_pkey PRIMARY KEY (id),
    CONSTRAINT uk_usage_events_workspace_id_source_id_source_event_id UNIQUE (workspace_id, source_id, source_event_id)
);
CREATE INDEX idx_usage_events_workspace_id_occurred_at_id ON usage_events (workspace_id, occurred_at, id);
CREATE INDEX idx_usage_events_received_at ON usage_events (received_at);
GRANT SELECT, INSERT ON usage_events TO <T-008 约定的服务角色>;

-- +goose Down
DROP TABLE usage_events;
```

## 7. 测试与验收

### 7.1 位置与测试库安全

- 新建 `cmd/tokenpivot-migrate/usage_events_integration_test.go`（`//go:build integration`，package main），复用同包 `integrationSchema` 与 `testProvider`：以迁移 owner 应用真实 `migrations.Files`，再用 `TOKENPIVOT_TEST_APP_DATABASE_URL` 的服务角色执行被测语句。不新增 helper 副本或测试变量；不为本任务抽共享 helper 包，也不另立无当前需求的重构任务（Q4）。
- 沿用 [README:163–176](<../../README.md#L163-L176>)：专用实例、回环 IP、`_test` 库；迁移 owner 非超级用户且无 CREATEROLE，服务角色无建库/建对象权限，密码认证；随机 `t050_` schema，归属核对后才清理；缺变量直接失败、不 skip；`-p 1`、独占运行；不打印 URL、不读环境文件、不连业务库。
- 只用合成数据：workspace/source 用随机 UUID（无外键，不依赖 T-008 的数据行）。
- 并发协调：先取等待方的 `pg_backend_pid()`，轮询 `pg_locks` 中该 pid 存在 `granted=false` 的锁，确认它已阻塞（同 [:305–326](<../../cmd/tokenpivot-migrate/main_integration_test.go#L305-L326>) 的 ticker 加期限写法），不用 sleep 猜调度；每个 goroutine 都有期限并被回收。

### 7.2 场景与断言

| # | 场景 | 必须断言 |
|---|---|---|
| 1 | 结构 | relkind 为普通表（非分区）；列类型、空值、默认值（received_at 与指标列无默认值）与 §4.1 一致；主键与身份键的列及顺序准确；source_event_id 排序规则为 C；索引集合恰为 §4.2 四个；外键与 CHECK 约束数为 0（不按白名单断言其他约束类型）；无非内部触发器；未启用 RLS。 |
| 2 | 权限 | 服务角色有 SELECT、INSERT，无其他表权限；以服务角色执行 UPDATE、DELETE、TRUNCATE、ON CONFLICT DO UPDATE、SELECT FOR SHARE 均因权限不足失败；原有 [:207–211](<../../cmd/tokenpivot-migrate/main_integration_test.go#L207-L211>) 的负向检查仍通过。 |
| 3 | 三元唯一 | 首次插入返回 1 行；同身份、任意内容再插（DO NOTHING）返回 0 行；表中仅 1 行。 |
| 4 | 空间/来源独立 | 同一 source_event_id 在 (w2,s1)、(w1,s2) 下各自插入成功；`evt-A` 与 `evt-a` 是不同身份（逐字节相等）。 |
| 5 | 不覆盖 | 同身份、不同内容、更晚 received_at 的第二次插入后，原行所有列（含 id、payload_hash、normalization_version、received_at）逐列不变。 |
| 6 | 并发·提交 | 事务 A 插入未提交，同身份的事务 B 被观察到阻塞；A 提交后 B 返回 0 行，B 的下一条 SELECT 能读到 A 的行（E1 在数据库层的体现）；最终 1 行。 |
| 7 | 并发·回滚 | A 回滚后 B 返回 1 行并提交；最终 1 行，内容为 B 的。 |
| 8 | 并发·多方 | 若干（例如 8 个）独立连接经 channel 同时放行，以不同内容插同一身份并提交：所有 RETURNING 合计恰 1 行；表中 1 行，内容等于胜出者。 |
| 9 | 缺失与零 | 指标 NULL＋缺失、0＋已知经 pgx 读回仍分别为 NULL 与 0，质量值不变；省略指标列不会被补 0。 |
| 10 | 时间 | occurred_at、received_at 的微秒值原样往返；省略 received_at 的插入因 NOT NULL 失败（数据库不代选时钟）。 |
| 10a | 身份长度边界 | 按冻结的编码后最大字节数构造合法 source_event_id，覆盖不可压缩内容及字符集允许时的多字节样例；与两个 UUID 组成的实际唯一索引项必须成功写入并正确去重。超限入口拒绝由 T-011 验收。 |
| 11 | 迁移 | 更新 TestSingleConnectionMatrix 期望：up 后为最新版本且有 usage_events；down-one 只移除本表、回到上一版本，T-008 的表保留；再次 up 恢复；重复 up 记录 no change。其他使用真实迁移的测试（如 [:350–388](<../../cmd/tokenpivot-migrate/main_integration_test.go#L350-L388>)）仍通过。 |

以上只证明数据库层性质，**不能当作** duplicate/conflict/expired/accepted 的业务全链路证明：测试 SQL 不是 T-011 的生产 SQL；内容比较、过期优先、同事务分钟增量、提交后才 accepted，由 T-011 按其 [§8](<2026-10-27-batch-ingestion.md#L126-L162>) 用真实 PG 另行验收。

### 7.3 命令（已核实存在，本次未运行）

```sh
GOTOOLCHAIN=local go test -mod=readonly -race -count=1 ./...
GOTOOLCHAIN=local go vet -mod=readonly ./...
make check
# 仅在专用 _test 库、两个 TEST 变量已安全注入且独占时执行：
make test-integration
```

依据 [Makefile:61–86](<../../Makefile#L61-L86>)。交付时记录退出码、Go 与 PG 版本、普通/联合标签结果和未通过项。

## 8. 开工条件

1. **T-007 已冻结并合入**：指标清单与 total 语义、质量词表、v1 是否含 status/duration、比较集合（须为本表列的子集）、normalization_version 取值、摘要算法与字节长度、occurred_at 精度（不细于 1 微秒）及超精度处理/边界样例、source_event_id 编码后最大字节数与字符集、provider/model 规范化。另须提供基于实际留存信息、覆盖有损变化的跨版本比较规则和样例；不能重建的兼容限制须明确且满足旧版本重传判定要求。T-009 开工前核实最大合法字节数与两个 UUID 组成的实际唯一索引项兼容，不猜数据库极限。T-043/T-044 的结论经 T-007 消费。
2. **T-008 已合入**：最新迁移编号；UUID 主键生成约定；服务角色授权写法（含集成测试中服务角色如何拿到授权）；约束与索引命名约定；有业务表后 TestSingleConnectionMatrix 的断言写法。
3. 专用 PG 测试库可用；用户另行授权实施。

不依赖 T-010/T-011/T-012/T-013 完成。若要 T-009 先于 T-008，本任务就得替 T-008 定授权写法和 ID 约定，属范围变更，须先修订本文（不推荐）。若比较集合含不落列的字段，或 T-008 的授权写法无法表达“仅 SELECT、INSERT”，停下报告，不自行变通。

## 9. 执行切分

**单块，不拆。** 一个实施者在开工条件满足后的 main HEAD 上完成。

| 文件 | 归属与操作 |
|---|---|
| 新增 `db/migrations/<N>_usage_events.sql` | T-009；编号在实施时占用，主代理保证没有并行分支占同号，并按编号顺序合入。 |
| 新增 `cmd/tokenpivot-migrate/usage_events_integration_test.go` | T-009。 |
| [main_integration_test.go](<../../cmd/tokenpivot-migrate/main_integration_test.go>) | 只改 TestSingleConnectionMatrix 的版本与表集合期望；T-008 先改，本任务在其结果上最小追加，不重构 helper。 |
| [data-model.md](<../data-model.md>) | 只改 §4（列、索引、权限、比较依据、不建 project_id 索引的理由），并在 §7 第 94 行补一句“明细首版为普通表、不分区”；其他节归各自任务。 |
| README | 默认不改；仅当 T-008 建立了逐表授权清单时追加 usage_events 一行。 |
| 不改 | go.mod/go.sum、Makefile、`internal/`、各 main.go、web、ADR、其他计划、docs/todo.md。 |

顺序：核对开工条件与真实文件 → 写迁移 → 写测试 → 同步文档 → 跑 §7.3 → 主代理只读评审。

## 10. 待确认、风险与评审重点

评审后采纳的局部工程选择（不作为额外用户确认关卡）：

- **Q1 权限与行锁**：服务角色仅 SELECT/INSERT，不对事件表使用行锁；沿用 T-011 查询后窗口复核，以及仍有效但无行时返回可重试结果。T-011 开工时须将其计划:84 的可选行锁与此权限约定协调，本轮不修改另一计划。只有出现已证实必须持锁的需求，才重新评估权限，不默认放开 UPDATE。
- **Q2 COLLATE "C"**：source_event_id 采用。T-008/T-012 的 text 唯一键是否同样处理，由各自任务评估，本文不代定。
- **Q3 索引集合**：随空表建 (workspace_id, occurred_at, id) 和 (received_at)，不预建 project_id 筛选索引；后续查询任务按真实访问路径评估。
- **Q4 测试 helper**：复用 migrate 包现有 helper，不抽共享包、不为现有副本另立重构任务。

只读评审指出的跨版本重建保证、擅定时间截断、索引字节长度条件均已在 §4、§7、§8 修正。计划可有条件交付；实际前置未冻结，不代表可立即编码。

风险：T-007 迟迟不冻结会阻塞本任务；摘要与列不一致只能靠 T-011 的测试发现；down-one 会销毁数据；约 1800 万行（[数据模型:94](<../data-model.md#L94>)）下的表与索引体积、写入与清理开销均未实测，不作承诺。T-011 若已实际开工，须确认它没有自建明细表或占用迁移编号。

评审重点：身份键三列及顺序；received_at 无默认值；摘要用 bytea；比较集合是列的子集；服务角色仅 SELECT/INSERT；COLLATE "C"；不建 project_id 索引的理由；测试没有越界宣称业务全链路。

## 11. 依据

先用已有研究：[参考研究:11](<../research/2026-10-26-tokenpivot-ingestion-storage-reference.md#L11>) 记录了 Lago 把租户与事务 ID 放进 PostgreSQL 唯一键，支持“幂等靠数据库唯一键”的既定做法。身份键是已定决策，未再做 GitHub 调查。新增核实只针对会改变 DDL、权限或测试的事实；网页用 curl 抓取，日期均为 2026-10-27。

| 编号 | 事实 | 来源 | 状态 | 采用 |
|---|---|---|---|---|
| E1 | Read Committed 下 ON CONFLICT DO NOTHING 可能因不可见的并发提交而不插入，须用下一条语句再读 | [PG 18 事务隔离](<https://www.postgresql.org/docs/18/transaction-iso.html>)（沿用 T-011 计划 E1） | 已核实（T-011 规划时） | 场景 6 |
| E2 | INSERT 需 INSERT 权限；ON CONFLICT DO UPDATE 另需 UPDATE；所有 ON CONFLICT 形式都需要对冲突目标等被读列有 SELECT；RETURNING 需 SELECT | [PG 18 INSERT](<https://www.postgresql.org/docs/18/sql-insert.html>) | 已核实 | 授 SELECT＋INSERT，场景 2 |
| E3 | FOR NO KEY UPDATE、FOR UPDATE、FOR SHARE、FOR KEY SHARE 需 UPDATE 权限 | [PG 18 SELECT](<https://www.postgresql.org/docs/18/sql-select.html>) | 已核实 | Q1 |
| E4 | 确定性排序规则只把字节序列相同的字符串视为相等 | [PG 18 Collation](<https://www.postgresql.org/docs/18/collation.html>) | 已核实 | COLLATE "C" 不改相等语义 |
| E5 | libc locale 数据变化可使 B-tree 索引顺序失效、插入本应拒绝的重复；C/POSIX 不受影响 | [PostgreSQL Wiki：Locale data changes](<https://wiki.postgresql.org/wiki/Locale_data_changes>) | 已核实（官方 wiki 正文；各发行版受影响情况未核实） | COLLATE "C" |
| E6 | timestamptz 分辨率为 1 微秒 | [PG 18 日期时间类型](<https://www.postgresql.org/docs/18/datatype-datetime.html>) | 已核实 | 精度条件，场景 10 |
| E7 | PG 18 内置 uuidv7() | [PG 18 UUID 函数](<https://www.postgresql.org/docs/18/functions-uuid.html>) | 已核实 | 仅供 T-008 选择，本任务不定 |
| E8 | goose v3.29.0 默认遇缺失（乱序）迁移报错，`WithAllowOutofOrder` 才放开 | 本机模块缓存 goose v3.29.0 `provider_options.go:177–186`（与 go.mod:7 同版本） | 已核实（读源码，未运行） | 按编号顺序合入 |
| E9 | goose SQL 解析以行内最后一个词以 `;` 结尾判定语句结束，遇 `--` 停止扫描 | 同上 `internal/sqlparser/parser.go:375–394`，调用处 `:247`、`:253` | 已核实（读源码，未运行） | COMMENT 写法 |

未核实：B-tree 单个索引项的大小上限；PG 18 是否把 NOT NULL 也登记为约束行（测试只断言外键/CHECK 数为 0，不按白名单断言，以免受此影响）；42501 以外的具体报错文本。本次未运行任何测试、未连接数据库。

术语：身份键＝唯一约束 (workspace_id, source_id, source_event_id)；服务角色＝服务进程连库所用的最小权限账号（README 示例为 tokenpivot_app）；比较依据＝判定 duplicate/conflict 所需的已存数据（规范化版本、摘要、比较字段列）。
