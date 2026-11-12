# T-047 PI 用量上报插件实施计划

- 日期：2026-11-05。
- 状态：已按首轮评审修订；SQLite队列与凭证绑定变更经限定只读复核通过（2d1b0b93-dae0-4c3d-90d8-300878b4c868，结论可执行、无必须修项）。待用户批准实施；未提交。
- 需求：`docs/prd.md` §3.2–3.3；接口：`docs/openapi/openapi.yaml`。
- 约束：`docs/architecture.md` §4–5、ADR-0002、ADR-0005。ADR-0005 的 source/事件部分仍有提议状态，不将全文视为已批准决策。

## 1. 用户确认的范围

1. 交付插件代码、测试，并以软链安装到本机 PI；服务地址、接入凭证、项目映射由用户配置。
2. 首版只采集具备请求级最终用量、真实模型归属和完成时间的 assistant 调用。
3. 压缩、缺少完成时间或必要事实的事件暂不采集，明确记录跳过原因与数量；不猜模型、时间或请求数。
4. 代码拟放 `plugins/pi/`；服务端、OpenAPI、DSH 插件保持不变。
5. 不上传 Prompt、回答、路径、工作目录、会话标题或供应商密钥。路径仅本地用于项目匹配。
6. 不扫描历史会话、不修改 PI 上游、不发布 npm 包、不自动部署服务。

## 2. 依据与纠正记录

本机核实版本为 `@earendil-works/pi-coding-agent@1.5.0`。
下文 P 指 `/Users/lxp/.npm-global/lib/node_modules/@earendil-works/pi-coding-agent`；相对证据路径均相对于 P。

| 事实 | 证据 | 验证状态 |
|---|---|---|
| jiti 加载目录型扩展，支持软链、JS/TS 与相对模块 | `dist/core/extensions/loader.js:465-494,626-686`；`docs/extensions.md:123-170` | 子代理源码核实；真实加载待验证 |
| `turn_end` 包含 message 和持久条目 messageEntryId | `dist/core/extensions/types.d.ts:847-854`；`dist/core/agent-session.js:504-529,768-782` | 子代理源码核实；各模式冒烟待验证 |
| 工厂阶段不启动定时器/连接；资源随 session_start/shutdown 起停 | `docs/extensions.md:252-258` | 子代理源码核实 |
| AssistantMessage 无 id，包含 provider/model/responseModel、usage | `node_modules/@earendil-works/pi-ai/dist/types.d.ts:375-405` | 主代理实读核实 |
| timestamp 是开始时间，durationMs 是至响应结束的耗时，deferred 结果可缺耗时 | 同上 `:398-405` | 主代理实读核实 |
| cacheWrite1h 是 cacheWrite 的子集；reasoning 是 output 子集 | 同上 `:285-313` | 主代理实读核实 |
| cacheRead/cacheWrite 在当前类型中为必填，但运行时仍需校验 | 同上 `:285-291` | 主代理实读核实；原草稿的“类型可选”已纠正 |
| session ID 写入 header，加载原会话恢复 header.id；getSessionId 返回它 | `dist/core/session-manager.js:692-725,780-782` | 主代理实读核实 |
| entryId 仅会话内唯一；fork 复制历史条目 | 同上 `:21-29,1386-1432` | 子代理源码核实 |
| 一次压缩可执行两次摘要请求并合并 usage | `dist/core/compaction/compaction.js:693-704` | 主代理实读核实；不映射为单请求 |
| 默认压缩也可解析虚拟模型，ctx.model 不保证实际归属 | `dist/core/agent-session.js:258-271` | 独立评审源码核实 |
| 只接受最终请求用量；不把汇总拆成请求 | 仓库 `docs/architecture.md:37-48` | 主代理实读核实 |
| 同身份同内容 duplicate，不同内容 conflict；逐项事务 | `docs/decisions/0002-ingestion-idempotency-and-batch.md:5-21` | 主代理实读核实 |

此前样本统计只作调研线索，不作为所有供应商的保证，不重复读取私人会话日志。
2026-11-03 数据库往返优化保持接口语义不变，依据 `docs/plans/2026-11-03-ingestion-roundtrip-batching.md:9-25`。

## 3. 事件采集与标识

- 工厂仅注册处理器；session_start 初始化配置、队列和调度；session_shutdown 幂等停止调度并限时等待在途请求与本地持久化。
- 主入口为 turn_end：只处理 assistant 最终结果。pending/deferred 不视为最终结果；error/aborted 只有具备最终事实时才采集。
- 取得同一事件上下文中的 sessionManager.getSessionId() 和 messageEntryId，生成 `pi:msg:<sessionId>:<entryId>`。ID 不包含路径、正文或凭证；对长度/格式按接口校验。
- 两个会话即使 entryId、毫秒时间和 usage 相同，也属于不同事件。禁止以内容 hash 合并真实新调用。
- 入队时冻结完整事件体；相同 ID 再次通知时复用首次冻结内容，不能因配置变化重算旧事件的项目、模型或时间。
- 续聊仅采新通知；fork 不扫描、不重传复制的历史条目。首版不实现历史回填。
- occurred_at = timestamp + durationMs；二者均须为有效有限数值，durationMs 非负。缺少完成时间时跳过，不以开始时间或采集时间替代。
- provider 取 message.provider，model 取有效的 responseModel，否则取有效的 message.model；不以 ctx.model 代替消息事实。
- session_compact 仅增加“不支持请求级压缩统计”的计数；cache_warm 等旁路不纳入首版。
- stderr 输出固定原因码和计数，不输出原始异常、正文、配置值或路径。提供加载/运行状态及跳过计数，明确“assistant 采集”不等于 PI 全部消耗。

## 4. 用量与项目映射

所有数值在运行时校验非负、安全整数及接口上限，缺失不补零。非法值单独计数，禁止静默钳制为合法值。

1. output 取 usage.output；cache read 取 cacheRead；cache write 只取 cacheWrite。cacheWrite1h 不再相加；reasoning 不加到 output 或 total。
2. input 口径以来源数值关系核实：
   - input/output/cacheRead/cacheWrite/total 全部有效，且 total = input + output + cacheRead + cacheWrite：服务端 input 取 input + cacheRead + cacheWrite。
   - total = input + output，且缓存子集有效：服务端 input 直接取 input，避免重复加缓存。
   - 缓存全为已知零时两式等价，不存在歧义。
   - 证据不足时保留来源 input 数值并标 estimated，不将未知缓存当作已知零；缓存字段仍各自 missing。若违反接口子集约束且无可证实口径，跳过并计数，不增大数值以通过校验。
3. 来源 total 有效则保留原数值；独立数值不一致时按接口标 estimated，不取最大值。total 缺失保持 missing；input 的估计质量须保留，避免服务端推导造成虚假 known。
4. 缺失 output 或其他指标按接口三态表达，不构造来源未报告的数值。全缺失/非法组合按接口规则跳过并计数。
5. status：error 映射 error，其余按接口允许值或 null；不把 aborted 宣称成功。
6. 项目按本地规范化目录的路径边界最长匹配，不用裸字符串前缀把 `/foo/bar2` 当 `/foo/bar` 子目录；未匹配为 null。项目 UUID 由服务端预先创建。
7. 格式边界以当前 OpenAPI 的 UsageEvent/UsageObject 为准；测试覆盖 known/estimated/missing 及服务端推导行为。

## 5. 持久队列与多进程

首轮评审否决原先“无锁追加 JSONL、只锁 compact”的方案：rename 前的追加可能留在旧 inode；小于64KiB不保证 fs.appendFile 整行原子；仅以锁龄抢占会同时产生两个写者。

采用 Node 内置 `node:sqlite` 直接存队列，替代 JSON 快照及手写锁；仅支持本机本地文件系统，不放网络盘或同步盘。

- stateDir 内 `queue.sqlite`，`journal_mode=DELETE`、`synchronous=FULL`；无 npm/native addon 依赖，无 WAL、PID文件、锁龄抢占或单独compact。
- 一张绑定元数据表、一张事件表：事件ID为主键，payload不可变，status为pending/dead/overflow。同ID不同内容报固定冲突码，禁止覆盖首次内容。
- 入队、容量处理、死信转换、确认出队和绑定更新均在短 `BEGIN IMMEDIATE` 事务内重读并检查后写入；异常ROLLBACK。事务内无await、无网络。
- 查询批次后结束数据库读操作，锁外HTTP，响应后开短事务，仅删除本批已确认且仍pending的ID。并行上传允许重复HTTP，由服务端accepted/duplicate幂等；不引入claim/租约恢复。
- 死信/overflow通过原记录status变更原子持久化，无跨文件双写；默认10000仅限制pending，超额转overflow并计数，不静默删除全部数据。保留区不自动清空，README说明磁盘增长与人工处理。
- SQLite API同步，使用timeout=0或经过测试的极短等待；SQLITE_BUSY退出事务后异步退避，禁止长同步等待阻塞PI。只有COMMIT成功才算已采集；忙、磁盘满或退出前未提交须明确计数，不宣称成功。
- OS文件锁随进程退出释放，hot journal负责未提交事务回滚；活进程暂停不按时间抢锁，PID复用不参与判断。启动即恢复调度，无新调用也补报。
- stateDir 0700、数据库0600；建库先处理权限与软链检查，DDL事务化。数据库与rollback journal为恢复单元，不单删journal、不在运行中移动或替换数据库。
- 损坏保留数据库及journal并暂停，不覆盖重建；发现非空旧格式状态则暂停并报告需要显式迁移，不忽略旧队列。

依据（2026-11-05限定核实）：[Node 22.19 SQLite API](https://nodejs.org/download/release/v22.19.0/docs/api/sqlite.html)确认DatabaseSync、timeout及同步API，最低版本标Active development；[SQLite locking](https://www.sqlite.org/lockingv3.html)说明文件锁和hot journal恢复。接受内置API成熟度取舍；宿主缺API时明确加载失败，不自动回退手写锁。
子代理临时小测报告：PATH Node v26.11.1、darwin arm64、SQLite3.53.4；4进程400条提交、SIGSTOP活锁报BUSY、SIGKILL回滚未提交数据、401条已提交记录保留、integrity_check=ok、按ID确认不删并发新增。主代理尚未复跑，不能计作插件验收；最低22.19、实际PI宿主、暂停>30秒、满盘/损坏和端到端ACK前kill均留待实施验证。

## 6. 上报与恢复

- POST `/api/v1/ingestion/events`，Bearer 凭证，最多100条且不超过接口1MiB限制，按序处理逐项结果。
- accepted/duplicate 确认出队；conflict/expired/永久 rejected 写死信后出队；FUTURE_EVENT 保留重试。
- retryable、429、5xx、网络错误保留并指数退避，遵守 Retry-After 的秒数与 HTTP-date。
- 400/401/403/415 暂停自动发送，保持队列。低频配置检查（拟30秒）独立于新事件和发送调度；修复配置并完成必要的凭证轮换确认后恢复，无新调用也能唤醒。环境变量变更需重启。
- 413 对半拆批，单条仍超限写死信；未知HTTP状态、畸形200、结果缺失/数量不符、未知逐项状态均保留并退避，不能推断已成功。
- 每次请求有超时；退出等待有明确上限，退出中断只取消网络，不清除未确认事件；后续重传同ID同内容。
- 禁止自动跟随重定向，Authorization 只发往配置端点；日志只含固定错误码、HTTP状态与计数。

## 7. 配置、身份隔离与凭证

配置拟位于 `~/.tokenpivot/pi/config.json`；支持 `TOKENPIVOT_PI_CONFIG` 指定路径，`TOKENPIVOT_PI_TOKEN` 优先于配置 token。

配置键：serverUrl、token、projects[{path,projectId}]、stateDir（默认 `~/.tokenpivot/pi`）、maxQueueItems（10000）、flushDelayMs（2000）、requestTimeoutMs（30000）。示例只放 `tpk_PLACEHOLDER`，不写真实凭证。

- HTTPS；仅回环地址允许HTTP。拒绝 URL 用户信息及不合法地址，不静默丢弃有意义的查询参数。
- 配置包含 token 时要求0600，目录0700；检查软链和既有权限，权限不满足时暂停启用并给固定原因码，不擅自 chmod 软链指向的其它文件。
- 环境变量模式可不在配置中存 token，因此纠正原草稿“PI 只能把 token 落盘”的说法。
- 无服务地址或初始凭证时为“已安装待配置”，不开始新采集，不影响既有队列；配置读取错误不覆盖已持久化数据。
- SQLite元数据保存normalizedServerUrl、acceptedTokenFingerprint、bindingRevision；指纹为SHA-256(`TokenPivotPI/token/v1\0` + 有效Bearer token)完整摘要，不存明文token、不写日志。摘要是单向指纹而非加密，适用于高熵接入token，不宣称抵抗低熵凭证离线猜测。
- 空新库首次有效配置在事务中绑定，绑定缺失但有事件时暂停；队列清空也保留绑定。目的服务只按标准URL规范化，保留scheme/host/port/path/query含义；改变服务必须新stateDir，旧队列不自动转投。
- 启动、入队、发送、响应确认前对比绑定及配置；token指纹变化则暂停该进程新采集与发送，保留队列。环境变量启动读取，变更需重启；独立30秒配置检查处理文件变更，不依赖新事件。
- 拟新增插件命令 `/tokenpivot-confirm-same-source`（尚未实现）：用户先停所有共用stateDir的PI、更新token、重启一个PI；核对新旧token属于同source后执行命令，随后启动其余进程。
- 命令显示不含敏感信息的目的服务标识及“新凭证仍属原source，继续发送旧事件”的确认含义；按观察到的旧指纹+revision执行条件更新，在同一事务写新指纹并revision+1。服务不匹配或条件已变化则保持暂停，禁止后写覆盖。不设置永久confirm=true；B变C仍需再次确认。
- 确认成功立即唤醒flush；确认前崩溃保留旧绑定，确认提交后重启恢复新绑定；旧进程在下一次操作检查时暂停，已经发出的请求无法追回，因此轮换顺序要求先停共用进程。
- source仅由服务端认证决定：本地只检测token字节变化，用户确认是归属断言而非服务端证明。换source使用新stateDir；同token被服务端重绑定也无法本地检测，README明确该边界。本期不新增身份查询接口。

## 8. 文件清单与执行切分

单 dev，一块顺序交付；不拆共享框架，不修改 DSH。

新增 `plugins/pi/`：
- `index.js`：manifest入口、事件注册、加载状态、生命周期。
- `lib/config.js`：配置、权限、地址及身份绑定校验。
- `lib/mapper.js`：请求事实映射、会话命名空间ID、路径边界匹配、跳过原因。
- `lib/queue.js`：SQLite短事务、不可变事件、绑定元数据、死信/overflow；插件实现尚待验证。
- `lib/uploader.js`：批量、响应校验、重试/暂停、超时。
- `lib/collector.js`：采集与恢复调度、运行计数、限时退出。
- `package.json`：ESM、pi.extensions、Node>=22.19、node:test入口；使用内置node:sqlite，无新增npm依赖。
- `test/mapper.test.js`、`test/queue.test.js`、`test/upload.test.js`、`test/lifecycle.test.js`：下节验收。
- `README.md`：安装配置、能力限制、轮换恢复、容量告警、撤回安装。

修改 `Makefile`：新增 test-plugin-pi 的 .PHONY/help/target，与现有 DSH 插件测试入口对称；是否加入总检查以当前 Makefile 核实后明确，不误称已有全量覆盖。
主代理独占计划/TODO整合；开发只交付上述批准范围。实现前建隔离 worktree；本计划未获得提交授权，不自动提交或推送。

## 9. 测试与验收

1. 映射：含缓存/不含缓存/口径未知；cacheWrite1h不重复计算；缺失不补0、total不钳制、质量推导；无duration/无真实归属/pending/deferred跳过；error/aborted完整事实可采；压缩只计跳过；路径边界。
2. 身份：同会话重传同ID同冻结内容；两个会话同entryId/时间/usage仍是两条；fork/续聊不回填历史；配置变化不修改已入队内容。
3. 并发：两写者同时入队、入队与出队交错、双uploader只删除已确认ID；持锁者暂停>30秒仍BUSY；事务提交前后kill恢复；实现没有PID/锁龄抢锁；数据库/journal损坏保留；死信与overflow状态变更原子；SQLITE_BUSY异步退避、满盘明确失败；最低Node版本与实际PI宿主的API加载。
4. 协议：逐项混合结果；FUTURE_EVENT；503/429和Retry-After；413；暂停后无新事件仍可恢复；畸形/未知响应保留；重定向不泄露凭证；超时与限时退出。绑定测试覆盖A→B未确认不发送/不新采集、确认B即flush、B→C再次确认、并发条件更新仅一方成功、旧进程停止、服务变化不可确认覆盖、绑定事务前后kill、无明文token。
5. 使用本地桩验证断网补报，以及“服务端已提交但响应丢失”后原ID重传不重复计数；不停止用户的真实服务。
6. 隐私：队列、死信、overflow和日志不含 token、Prompt、回答、路径及会话标题；json/rpc stdout保持协议可解析。
7. `make test-plugin-pi`通过，原DSH相关测试不回退；整体只读代码评审通过。每项记录实际命令、结果，不用子代理完成消息代替验收。
8. 安装与真实启用分别验收：插件主动加载信号、实际PI事件触发、服务端对应source的单事件与用量核验。没有用户配置则仅报告已安装待启用，不将T-047标done。

## 10. 安装顺序及回滚

1. 实现、测试、代码评审完成；持久稳定的 checkout 中具备已验证代码后，才安装。
2. 审查 `~/.pi/agent/extensions/tokenpivot-pi` 是否存在/是否软链及目标，不覆盖其它安装。
3. 拟建立指向 `/Users/lxp/code-space/gitspace/TokenPivot/plugins/pi` 的目录软链；目标不能是待清理worktree。
4. 用户通过本地配置填地址/凭证/项目映射；助手不读取或输出凭证值。
5. 重启或reload后核验加载信号，分别验证TUI与headless；真实请求产生的事件与服务端记录相符后才记录启用成功。
6. 回滚只将本次创建的链接移入 `~/.Trash/<日期>/`，保留配置、队列和死信；先审查链接再操作。

## 11. 当前待办与非目标

- 用户已确认 assistant-only 首版；原D1–D4有缺陷的备选已移除。
- §5改用SQLite事务与系统文件锁，§7确定指纹检测+一次性显式同source确认；这两处关键变更已通过限定只读复核，现交用户批准实施。
- 真实加载、测试、真实上报均尚未执行；本文只记录计划，不代表功能落地。
- 非目标：压缩请求级采集、cache_warm旁路、历史回填、PI上游修改、服务端接口变更、DSH重构、数据库运维与线上停服。
