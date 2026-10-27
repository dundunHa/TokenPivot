# TokenPivot：请求用量上报与存储路线参考研究

> 日期：2026-10-26（America/Los_Angeles）；取证日期：2026-10-27 UTC。  
> 问题：首版在 Go 单体 + PostgreSQL 与专门分析型数据库／额外实时组件之间，怎样保持简单而不堵死扩容？  
> 范围：最多 5 个候选、深读 2 个项目；20 分钟时间盒。只读外部源码与测试，不实施，不作未经证实的性能承诺。

## 结论

1. **“需要分钟／小时／天图表”本身，不构成首版必须引入分析库的证据。** Lago 有真实的 PostgreSQL 原始事件存储、查询时求和和按客户时区分日；同一事件查询抽象可以切换 PostgreSQL／ClickHouse。Langfuse 虽然使用 ClickHouse，但其时间序列查询仍在查询时选择 minute/hour/day，并非必须维护三套预汇总表。对 TokenPivot 可借的是“原始事实可重算、查询入口集中、存储实现可替换”，不是立即照搬某个数据库组合。[E3][E4]

2. **幂等要从业务事件身份进入数据库，不能只靠接收进程内去重或队列任务 ID。** Lago 的 PostgreSQL 唯一键包含 `organization_id, external_subscription_id, transaction_id`；Langfuse 则把接收事件 ID 与 `body.id` 所代表的实体 ID 分开，并按实体归并、异步写入。两者解决的问题并不完全相同：Lago 重复事件可报错，Langfuse 允许同一实体多次更新。TokenPivot 应先确定“同一请求”的稳定身份，以及同 ID 不同内容是拒绝还是更新；请求重试不能产生第二份用量。[E1][E2][E12]

3. **批量接口没有统一答案，但失败语义必须明确。** Langfuse 的兼容接收入口返回 HTTP 207 和逐项 `successes/errors`，成功表示已通过相应入队流程，不等于分析查询已经可见；Lago PostgreSQL 批量接收先校验、批量插入，任一重复导致事务回滚，并按输入下标报告错误。其默认批量上限是可配置的 100，不应当成 TokenPivot 的性能建议。插件补报适合逐项确认，若首版选择整批事务，也必须让插件知道如何处理重复项，不能只返回一个模糊的“部分成功”。[E1][E2]

4. **补报首先是两种时间的语义问题，其次才是后台组件问题。** Lago 原始事件同时保存业务 `timestamp` 与 `created_at`，聚合读取事件时间；其测试显式区分“事件发生于历史时段”和“何时摄入”。查询原始事件重新求和，自然会重新包含成功补入的历史记录；但这不能外推为所有已结算账单／缓存都会自动修正。TokenPivot 可将 `occurred_at` 用于图表与滚动 60 秒，`received_at` 用于采集新鲜度；以后引入汇总表时，才需要明确受影响时段的增量更新或重算。[E3][E10]

5. **租户隔离贯穿认证、写入字段、唯一键与查询条件，而不是单靠前端筛选。** Lago 从 API 凭证取得当前组织，将该组织写入事件，查询再限制组织；Langfuse 从认证范围取得 project，并把 `project_id` 放进事件表和查询过滤。已核实的是这些共享表路径，不是“项目所有路径绝无越权”或“数据库已经启用行级安全”。TokenPivot 的 workspace 必须由接入凭证决定；Project 只是该 workspace 内可筛选维度，不应反过来授予跨 workspace 访问能力。[E6][E7]

6. **额外组件带来的是明确的运维和一致性责任，不能因为参考项目有就全带上。** Langfuse 当前默认 Compose 包含 Web、worker、PostgreSQL、ClickHouse、Redis、MinIO；接收链还包括对象存储、队列、异步归并，迁移中的事件传播另有分区处理与进度记录。Lago 的 PostgreSQL 路径仍会派发后台任务，但 Kafka 生产器在未配置时直接返回；因此“计量项目一定需要 Kafka”不成立。TokenPivot 首版可以不引入消息队列、Redis、对象存储或 WebSocket；是否需要异步汇总，应由确认后的规模和实测查询／摄入指标决定，而不是由参考项目的组件清单决定。[E8][E9][E12]

7. **最值得借鉴的是异常顺序和后端一致性的测试形状，而非“已有测试所以一定正确”。** Lago 测试覆盖批内及既有重复、重复项位于首／中／末、整批回滚、任务入队失败，以及摄入时间边界；PostgreSQL 与 ClickHouse 使用共享事件存储测试。Langfuse 有乱序实体更新和时间桶 SQL 生成测试。此次没有核实到直接证明夏令时边界、分钟加总等于小时／天、以及 TokenPivot 所需的并发重复上报不重计的完整测试；这些仍应成为本项目自己的验收项。[E10][E11]

## 依据

以下均为一手源码或测试；日期为抓取日期。测试“已核实”仅指已阅读相应测试断言，并非本次已运行其全套测试。

| 编号 | 事实 | 来源 URL | 日期 | 状态 |
|---|---|---|---|---|
| E1 | Lago 的组织／订阅／事务唯一键；批量插入遇重复整批回滚、按输入下标给错；上限默认 100 | [唯一索引：structure.sql:12217](https://github.com/getlago/lago-api/blob/0f8679e7bcfe2740c3bbf163cbc56b3819935125/db/structure.sql#L12217)、[CreateBatchService:5–31](https://github.com/getlago/lago-api/blob/0f8679e7bcfe2740c3bbf163cbc56b3819935125/app/services/events/create_batch_service.rb#L5-L31)、[批量事务：75–108](https://github.com/getlago/lago-api/blob/0f8679e7bcfe2740c3bbf163cbc56b3819935125/app/services/events/create_batch_service.rb#L75-L108) | 2026-10-27 | 已核实 |
| E2 | Langfuse 逐项校验，按实体类型和 body.id 归组，上传／入队后生成逐项结果，兼容入口返回 207 | [processEventBatch.ts:101–169](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/packages/shared/src/server/ingestion/processEventBatch.ts#L101-L169)、[归组：179–242](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/packages/shared/src/server/ingestion/processEventBatch.ts#L179-L242)、[存储和入队：256–405](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/packages/shared/src/server/ingestion/processEventBatch.ts#L256-L405)、[结果：430–491](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/packages/shared/src/server/ingestion/processEventBatch.ts#L430-L491)、[HTTP 207：ingestion.ts:235–256](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/web/src/pages/api/public/ingestion.ts#L235-L256) | 2026-10-27 | 已核实 |
| E3 | Lago 原始事件在 PostgreSQL；求和、组合分组及按客户时区分日发生于查询；存储工厂支持两种后端 | [原始表：structure.sql:3401–3417](https://github.com/getlago/lago-api/blob/0f8679e7bcfe2740c3bbf163cbc56b3819935125/db/structure.sql#L3401-L3417)、[sum/grouped_sum：postgres_store.rb:293–323](https://github.com/getlago/lago-api/blob/0f8679e7bcfe2740c3bbf163cbc56b3819935125/app/services/events/stores/postgres_store.rb#L293-L323)、[按时区分日：372–380](https://github.com/getlago/lago-api/blob/0f8679e7bcfe2740c3bbf163cbc56b3819935125/app/services/events/stores/postgres_store.rb#L372-L380)、[store_factory.rb:32–43](https://github.com/getlago/lago-api/blob/0f8679e7bcfe2740c3bbf163cbc56b3819935125/app/services/events/stores/store_factory.rb#L32-L43) | 2026-10-27 | 已核实 |
| E4 | Langfuse 查询构建器按查询粒度选择分钟／小时／天；events_core 仍保存 span 行，相关物化视图是列裁剪／文本截短，不是按时间求和 | [时间粒度：queryBuilder.ts:932–962](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/packages/shared/src/features/query/server/queryBuilder.ts#L932-L962)、[events_core：0040_create_events_core.up.sql:1–47](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/packages/shared/clickhouse/migrations/canonical/0040_create_events_core.up.sql#L1-L47)、[物化视图：0041_create_events_core_mv.up.sql:1–69](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/packages/shared/clickhouse/migrations/canonical/0041_create_events_core_mv.up.sql#L1-L69) | 2026-10-27 | 已核实 |
| E6 | Lago 认证取得组织，写入时使用该组织，原始事件查询限制同一组织 | [base_controller.rb:36–43](https://github.com/getlago/lago-api/blob/0f8679e7bcfe2740c3bbf163cbc56b3819935125/app/controllers/api/base_controller.rb#L36-L43)、[create_batch_service.rb:44–54](https://github.com/getlago/lago-api/blob/0f8679e7bcfe2740c3bbf163cbc56b3819935125/app/services/events/create_batch_service.rb#L44-L54)、[postgres_store.rb:9–25](https://github.com/getlago/lago-api/blob/0f8679e7bcfe2740c3bbf163cbc56b3819935125/app/services/events/stores/postgres_store.rb#L9-L25) | 2026-10-27 | 已核实 |
| E7 | Langfuse 从认证范围使用 projectId，并持久化 project_id、在查询构建器加入 project 过滤 | [processEventBatch.ts:133–147](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/packages/shared/src/server/ingestion/processEventBatch.ts#L133-L147)、[表和主键：0040_create_events_core.up.sql:113–116](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/packages/shared/clickhouse/migrations/canonical/0040_create_events_core.up.sql#L113-L116)、[queryBuilder.ts:626–691](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/packages/shared/src/features/query/server/queryBuilder.ts#L626-L691) | 2026-10-27 | 已核实 |
| E8 | Langfuse 当前默认 Compose 定义六个服务，含四种外部存储服务 | [docker-compose.yml](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/docker-compose.yml#L6-L207) | 2026-10-27 | 已核实 |
| E9 | Lago PostgreSQL 接收分支派发后台任务；Kafka 在未配置服务器／topic 时不发送；不能据此声称所有形态都必须有 Kafka | [create_batch_service.rb:75–84、112–119](https://github.com/getlago/lago-api/blob/0f8679e7bcfe2740c3bbf163cbc56b3819935125/app/services/events/create_batch_service.rb#L75-L119)、[kafka_producer_service.rb:15–20](https://github.com/getlago/lago-api/blob/0f8679e7bcfe2740c3bbf163cbc56b3819935125/app/services/events/kafka_producer_service.rb#L15-L20) | 2026-10-27 | 已核实 |
| E10 | Lago 有重复／回滚／入队失败测试，摄入时间截点测试，以及两种事件后端共享测试；日分组测试存在，但所读测试不足以证明夏令时和跨粒度一致性 | [批量失败断言：create_batch_service_spec.rb:44–51](https://github.com/getlago/lago-api/blob/0f8679e7bcfe2740c3bbf163cbc56b3819935125/spec/services/events/create_batch_service_spec.rb#L44-L51)、[失败和重复场景：92–212](https://github.com/getlago/lago-api/blob/0f8679e7bcfe2740c3bbf163cbc56b3819935125/spec/services/events/create_batch_service_spec.rb#L92-L212)、[摄入截点：an_event_store.rb:1768–1813](https://github.com/getlago/lago-api/blob/0f8679e7bcfe2740c3bbf163cbc56b3819935125/spec/services/events/stores/shared_examples/an_event_store.rb#L1768-L1813)、[PostgreSQL 共享测试：postgres_store_spec.rb:5–8](https://github.com/getlago/lago-api/blob/0f8679e7bcfe2740c3bbf163cbc56b3819935125/spec/services/events/stores/postgres_store_spec.rb#L5-L8)、[ClickHouse 共享测试：clickhouse_store_spec.rb:27](https://github.com/getlago/lago-api/blob/0f8679e7bcfe2740c3bbf163cbc56b3819935125/spec/services/events/stores/clickhouse_store_spec.rb#L27) | 2026-10-27 | 已核实 |
| E11 | Langfuse 测试乱序更新保留较新的字段，并测试时间桶 SQL 与填充步长；后者不是数据库数值一致性测试 | [IngestionService.integration.test.ts:1702–1750](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/worker/src/services/IngestionService/tests/IngestionService.integration.test.ts#L1702-L1750)、[queryBuilder.intervalBucketing.test.ts:13–50](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/packages/shared/src/features/query/server/queryBuilder.intervalBucketing.test.ts#L13-L50) | 2026-10-27 | 已核实 |
| E12 | Langfuse 的事件行使用 ReplacingMergeTree；分区传播任务读取暂存行、写最终事件表并记录处理进度；存在与后台去重、数据新鲜度相关的额外责任 | [events_full：0039_create_events_full.up.sql:112–115](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/packages/shared/clickhouse/migrations/canonical/0039_create_events_full.up.sql#L112-L115)、[handleEventPropagationJob.ts:90–159](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/worker/src/features/eventPropagation/handleEventPropagationJob.ts#L90-L159)、[写入与进度：216–383](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/worker/src/features/eventPropagation/handleEventPropagationJob.ts#L216-L383) | 2026-10-27 | 已核实 |
| E13 | Langfuse 非指定商业目录为 MIT；Lago API 为 AGPLv3；不能把 Langfuse 整仓概括为纯 MIT，也不建议复制 Lago 实现代码 | [Langfuse LICENSE:3–7](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/LICENSE#L3-L7)、[企业许可说明：ee/LICENSE:1–9](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/ee/LICENSE#L1-L9)、[Lago API LICENSE](https://github.com/getlago/lago-api/blob/0f8679e7bcfe2740c3bbf163cbc56b3819935125/LICENSE#L1-L2) | 2026-10-27 | 已核实 |

## 未核实与分歧

- **首版实际规模与性能边界未核实。** 没有给出 PostgreSQL 可支撑多少请求／秒、多少租户或多少事件的数字；本次没有同口径的一手基准，也未运行 TokenPivot 负载测试。
- **日历时区与粒度口径不同。** Lago 的日分组显式调用客户时区转换；Langfuse 所读时间桶函数没有显示同等的客户时区参数。这不是两个实现谁对谁错，不能直接拼接它们的日图口径。夏令时与跨时区数值测试未核实。
- **首版是否预聚合尚不能由外部项目替用户决定。** 已核实上述查询路径直接聚合明细；未穷举两仓库的所有缓存、计费快照与特定业务专用汇总，不能声称它们“全项目完全没有预聚合”。
- **Langfuse 当前处于 v4 路线，兼容上报入口有模式限制。** 所引 [兼容入口](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/web/src/pages/api/public/ingestion.ts#L200-L256) 包含 `events_only` 模式对旧事件的拒绝和弃用提示；本报告借其批量结果设计，不将其描述为所有部署都推荐使用的最新接收入口。
- **部署样例与源码快照不是同一版本承诺。** Lago 根部署仓的 [light 配置](https://github.com/getlago/lago/blob/a1f5650effe7703ade6f82758ec43c2c75c9eca9/deploy/docker-compose.light.yml#L9-L16) 仍固定 `v1.27.1`，不能用来保证本文 API main 快照的最小生产依赖。已确认当前 PostgreSQL 分支存在、Kafka 可不配置；未部署验证整个最小组合。
- **去重不等于任意读路径都具有强一致性。** 本次没有验证 Langfuse 所有查询模式在后台合并之前如何对重复行计数，也没有验证其端到端并发重试的用量绝不重复；不据数据库引擎名称作保证。
- **测试存在不等于本次测试通过。** 只读源码与 CI 定义，未启动其数据库、消息队列或完整测试环境。Lago 的 [测试工作流](https://github.com/getlago/lago-api/blob/0f8679e7bcfe2740c3bbf163cbc56b3819935125/.github/workflows/spec.yml#L96-L106) 与 Langfuse 的 [worker 测试步骤](https://github.com/langfuse/langfuse/blob/7526ede92399d7e04235b581b12470a401aaf5d7/.github/workflows/pipeline.yml#L1142-L1152) 已读，未将其当成当前分支绿灯证明。

## 方法

### 子问题

1. 请求／事件如何幂等、批量接收、报告部分失败？
2. 原始事件与分钟／小时／天聚合在哪里，如何处理补报？
3. tenant/workspace 如何落实到认证、表和查询？
4. 哪些是参考项目实际组件，哪些对首版可能过重？
5. 哪些重复、迟到、时区、跨粒度测试可以借鉴？

### 候选筛选

通过 GitHub Repository Search API 使用英文关键词，分别 `sort=stars&order=desc`；查询加入 `archived:false pushed:>=2025-10-27`，排除归档和过去 12 个月无推送项目。两组结果共 5 个，按 stars 展示如下。stars 仅用于发现候选，不代表方案质量。

| 候选 | stars（抓取时） | 最近推送（UTC） | 许可证（初筛） | 决定 |
|---|---:|---|---|---|
| [langfuse/langfuse](https://github.com/langfuse/langfuse) | 36,554 | 2026-10-27 02:47:26 | API 返回 Other；源码进一步核实为 MIT 核心＋商业目录 | 深读：最贴近 LLM 请求观测 |
| [mlflow/mlflow](https://github.com/mlflow/mlflow) | 28,548 | 2026-10-27 04:01:29 | Apache-2.0 | 不深读：本次更需要计量事件身份、补报证据 |
| [openobserve/openobserve](https://github.com/openobserve/openobserve) | 22,500 | 2026-10-27 04:08:20 | AGPL-3.0 | 不深读：通用观测平台，范围比本任务宽 |
| [getlago/lago](https://github.com/getlago/lago) | 10,714 | 2026-10-26 15:48:27 | AGPL-3.0 | 深读：最贴近可去重计量事件；应用在其 API 拆分仓 |
| [flexprice/flexprice](https://github.com/flexprice/flexprice) | 6,876 | 2026-10-26 22:11:13 | AGPL-3.0 | 不深读：同属计量，受两项目预算限制，优先 Lago |

检索：[llm observability](https://api.github.com/search/repositories?q=llm%20observability%20archived%3Afalse%20pushed%3A%3E%3D2025-10-27&sort=stars&order=desc&per_page=3)、[usage metering](https://api.github.com/search/repositories?q=usage%20metering%20archived%3Afalse%20pushed%3A%3E%3D2025-10-27&sort=stars&order=desc&per_page=2)。这些 URL 是可重放查询，结果随时间变化，上表记录本次抓取值。未展开 Helicone／LiteLLM；这不表示它们不合适。

### 固定源码快照与相似度

- Langfuse：`7526ede92399d7e04235b581b12470a401aaf5d7`。TypeScript 接收与查询代码，和本项目语言部分重合；功能更广、部署更重，不能据其生产架构推出小团队必须拥有同样组件。
- Lago 部署仓：`a1f5650effe7703ade6f82758ec43c2c75c9eca9`；[Lago API](https://github.com/getlago/lago-api) 应用仓：`0f8679e7bcfe2740c3bbf163cbc56b3819935125`。这是同一参考项目的拆仓，不额外计为第三个深读项目。Ruby 与 TokenPivot Go 不同，但计量事件、凭证组织、历史时间语义接近；其订阅计费、PDF、账期等复杂度不应照搬。
- 两项目均浅克隆到 `/tmp` 后读取源码、测试与 CI，而非只读 README；没有复制大段代码。两个项目的实现互为独立参考来源，不能将它们的不同业务语义强行合并。
- 在五个子问题已有可定位的一手事实、剩余缺口可明确标注后停止；未继续搜索社区性能传闻或扩大候选。

## 对 TokenPivot 的决策启示（建议，不是已验证性能结论）

### 可以先确定、不依赖部署规模的部分

- 以请求级原始事件为可重算依据；稳定身份、workspace 来源、发生时间、接收时间和更正语义先确定。只对首次成功接受的事件计数，不能让重试绕过唯一性再更新汇总。
- 上报结果区分“已持久接受”“已存在”“永久无效”“可重试失败”；若允许部分成功，插件只重试需要重试的项。HTTP 状态码本身不能说明每项最终状态。
- 把事件写入与统计查询集中成清楚的模块边界；先只有一个 PostgreSQL 实现即可，不为未来扩展提前建设通用分析框架。
- 最近滚动 60 秒按精确时间区间查询，不能拿自然分钟桶冒充；历史补报不应因为刚收到就冲高实时发生速率。采集新鲜度单独展示接收时间与事件延迟，不靠图表最后一个点推断。

### 需确认规模后再决定的部分

- 若确认首版仍是个人／小团队单机自托管，可把 **Go 单体 + PostgreSQL、明细直接查询聚合** 作为待验证的最简单基线，而不是先引入 ClickHouse、Redis 或 Kafka。
- 如果实测表明需要汇总，先在同库增加可重建的分钟汇总，并明确唯一事件接受与汇总变更的事务／恢复关系；小时／天可以从同一口径生成，不必首版建立三个独立后台流水线。
- 如果后台任务确实有必要，先评估单体内执行和数据库任务记录；采用外部队列前，需要说明它具体解决了什么已观测问题。
- 只有在确认的保留时长、维度组合、并发查询与补报负载下，PostgreSQL 已不能满足约定指标，才比较分析库。衡量项应包括摄入确认延迟、图表查询延迟、查询可见延迟、补报后的修正正确性、备份恢复与运维成本；本报告不虚构切换阈值。

### 借什么形状、拒绝什么复杂度

| 参考 | 可借鉴 | 不照搬 |
|---|---|---|
| Lago | 组织维度唯一事件身份、按发生时间过滤、存储工厂、两后端共享测试 | 订阅／计费／账期模型、收费后处理、PDF、将重复一律视为整批失败的业务选择 |
| Langfuse | 逐项结果、事件与实体身份分离、异步接收与查询可见的状态区分、乱序测试 | 完整 tracing／评测产品、对象存储＋Redis＋worker＋分析库的全套链路、迁移传播任务 |

### 建议交给 Architect 的验收清单

1. 同批重复、跨批重试、并发重复、响应丢失后重试：原始请求数与 Token 总数均不增加；同 ID 不同内容有明确结果。
2. 好项／坏项／重复项混合：返回值与实际持久化逐项一致；进程中断后重试仍正确。
3. 历史补报：修正所属旧分钟／小时／天，不冲高当前滚动 60 秒；如有汇总，重算后与原始明细完全一致。
4. 时区：UTC 与选定的业务时区、跨日／月、夏令时前进和回退；所有边界采用一致的起止包含规则。
5. 粒度：在相同范围和筛选条件下，分钟之和＝小时之和＝天之和＝明细和；不足整桶的边界另作明确测试。
6. 隔离：不同 workspace 可以出现相同来源请求 ID，但不能互读、互相去重或污染统计；伪造 payload 中 workspace 字段无效。
7. 恢复与新鲜度：接收成功但统计暂不可见、后台重试、补报中断；接口与界面不将持久接受误称为聚合完成。
