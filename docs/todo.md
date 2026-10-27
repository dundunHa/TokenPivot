# TODO

<!-- todo-meta: next-id=54 -->

## 里程碑：工程初始化

- [ ] T-053 {P2} **评估并对齐 Node 26 类型声明（@types/node）** web/package.json 的 @types/node 仍为 24.19.2，与 Node 26 运行时主版本不一致；单独评估是否对齐，不在 T-052 内改依赖图。
  - plan: docs/plans/2026-10-27-toolchain-baseline.md

## 里程碑一：采集打通

- [ ] T-011 {P1} **批量采集接口实现** 认证、校验、每项独立事务；逐项返回 accepted/duplicate/conflict/expired，过期判断先于查重；含重传、过期边界、凭证轮换后重传、跨空间写入拒绝测试。
  - 决策: docs/decisions/0002-ingestion-idempotency-and-batch.md
  - 架构: docs/architecture.md
- [ ] T-045 {P1} **DSH 插件：采集、重试与补报** 采集最终用量、转统一格式、缺失字段标缺失、不采集 Prompt/路径/密钥；本地队列重试与断网补报，重传沿用同一事件ID。
  - prd: docs/prd.md §3.2
- [ ] T-009 {P1} **用量明细表与防重唯一约束** 身份=工作空间+插件实例+来源事件ID 唯一索引；普通表即可（约1800万行），不做分区。
  - 数据模型: docs/data-model.md
  - 决策: docs/decisions/0002-ingestion-idempotency-and-batch.md
- [ ] T-044 {P1} **插件实例与项目标识规则** 稳定插件实例ID与项目标识生成；不以绝对路径作展示身份；不做跨实例合并。
  - prd: docs/prd.md §3.3
- [ ] T-043 {P1} **DSH 与 PI 平台能力核实** 核实两平台能拿到的字段、时间语义、事件ID与重复识别能力，结论写成文档，作为后续插件的前置。
  - prd: docs/prd.md §3.2, §11
- [ ] T-007 {P1} **采集接口格式定稿** 定字段、格式版本、批量逐项结果、时钟偏差规则、批量与查询限额；写入接口文档。
  - prd: docs/prd.md §6
  - 决策: docs/decisions/0002-ingestion-idempotency-and-batch.md
- [ ] T-008 {P1} **基础表迁移：工作空间、项目、插件实例、接入凭证** 建表与迁移脚本；source 身份独立于凭证。
  - 数据模型: docs/data-model.md
- [ ] T-010 {P1} **接入凭证签发、轮换与撤销** 凭证决定工作空间归属；轮换不改 source，撤销不清防重依据。
  - prd: docs/prd.md §3.3
  - 决策: docs/decisions/0004-auth-and-workspace.md
- [ ] T-012 {P1} **UTC 分钟聚合写入** 与明细同事务增量写入；区分真实零值、缺失、估算；缓存/推理子集不重复累加。
  - 决策: docs/decisions/0003-minute-aggregation-and-retention.md

## 里程碑二：能看数据

- [ ] T-021 {P1} **项目管理、权限校验与跨空间隔离** 项目增删改仅 owner，member 只读；所有读写路径统一校验工作空间权限；含跨站写请求拦截与越权访问测试。
  - 决策: docs/decisions/0004-auth-and-workspace.md
- [ ] T-024 {P1} **时间序列查询：分钟/小时/天** 按时区自然时段聚合（用 Go 标准时区库，夏令时只作测试用例）；返回实际范围与未完成分钟标记；含三种粒度一致、跨午夜、补报回填测试。
  - 架构: docs/architecture.md
  - 决策: docs/decisions/0003-minute-aggregation-and-retention.md
- [ ] T-016 {P1} **用户、会话与密码登录** 用户表、密码哈希、同源会话Cookie、登录登出、会话过期。
  - 决策: docs/decisions/0004-auth-and-workspace.md
- [ ] T-019 {P1} **工作空间与成员关系：个人/团队、多空间归属** 工作空间表、成员表、owner/member 角色；注册时自动建个人空间。
  - 数据模型: docs/data-model.md
  - 决策: docs/decisions/0004-auth-and-workspace.md
- [ ] T-023 {P1} **查询接口格式定稿** 筛选、分组、粒度、时区参数；点数上限与粒度提示；查询限额。
  - 架构: docs/architecture.md
  - prd: docs/prd.md §5.1
- [ ] T-025 {P1} **汇总指标：总量、输入、输出、请求数** 缓存/推理子集不重复累加；缺失与零值区分；只有汇总时不推算请求数。
  - prd: docs/prd.md §7
- [ ] T-026 {P1} **最近 60 秒滚动速率** 按精确时间从明细算，单位 tokens/min，同时返回数据新鲜度。
  - prd: docs/prd.md §7
- [ ] T-027 {P1} **分组排名与占比** 按 Provider/Model/Project 分组排序，长尾归为“其他”并可展开。
  - prd: docs/prd.md §5.2
- [ ] T-028 {P1} **请求明细分页查询** 仅请求级数据；状态与耗时有则显示；只有汇总的来源显示不可用。
  - prd: docs/prd.md §5.4
- [ ] T-032 {P1} **F·GEEK TERMINAL 设计变量与基础组件** 把 F 原型的配色、等宽字体、边线、状态色落成 Tailwind/CSS 变量；确认组件清单。
  - 视觉基准: docs/prototypes/dashboard-f.html
  - prd: docs/prd.md §8
- [ ] T-033 {P1} **登录注册页与工作空间切换** 登录、注册、验证邮箱、重置密码页面；工作空间切换器；切换后清空上一空间数据并忽略慢响应。
  - prd: docs/prd.md §10
- [ ] T-034 {P1} **全局筛选栏与 URL 状态** Provider/Model/Project 多选、时间范围、粒度、分组；非敏感条件存 URL；粒度被调整时明确提示。
  - prd: docs/prd.md §5.1
- [ ] T-035 {P1} **顶部指标区与数据状态提示** 总 Token、输入、输出、请求数、最近 60 秒；缺失/估算/延迟标记；显示最后更新时间。
  - prd: docs/prd.md §5.2
- [ ] T-036 {P1} **趋势主图** 输入/输出/总量面积图、未完成分钟标记、框选时间段联动明细。
  - prd: docs/prd.md §5.2
  - 视觉基准: docs/prototypes/dashboard-f.html
- [ ] T-037 {P1} **来源排名与用量构成** 按项目/模型/供应商切换排序；字符条与占比；点击排名项直接筛选；“其他”可展开。
  - prd: docs/prd.md §5.2
- [ ] T-038 {P1} **请求明细面板** 日志流式列表、分页；来源不支持的字段显示不可用而非填零。
  - prd: docs/prd.md §5.4

## 里程碑三：多人与运维

- [ ] T-047 {P1} **PI 插件：采集、重试与补报** 采集最终用量、转统一格式、缺失字段标缺失、不采集 Prompt/路径/密钥；本地队列重试与断网补报，重传沿用同一事件ID。
  - prd: docs/prd.md §3.2
- [ ] T-029 {P2} **数据源状态：查询与页面** 查询与页面合并：来源平台、实例、版本、最近接收时间、错误；离线检测方式待确认，未实现前不显示“在线”。
  - prd: docs/prd.md §5.5
- [ ] T-042 {P2} **响应式与动效可访问性** 桌面优先重排，小屏保留核心指标/筛选/趋势/明细；跟随系统 prefers-reduced-motion，不做设置项；状态不只靠颜色。
  - prd: docs/prd.md §8
- [ ] T-017 {P1} **注册与开关：开放注册 / 自托管关闭公开注册** 注册流程与配置开关；首次 owner 初始化方式待确认。
  - prd: docs/prd.md §2
  - 决策: docs/decisions/0004-auth-and-workspace.md
- [ ] T-018 {P1} **SMTP 邮件：邮箱验证与密码重置** 邮件发送封装、验证链接、重置链接、令牌过期与一次性。
  - 决策: docs/decisions/0004-auth-and-workspace.md
- [ ] T-020 {P2} **成员邀请与管理** owner 邮件邀请、接受邀请、移除成员；邀请交互细节待确认。
  - prd: docs/prd.md §2
- [ ] T-041 {P2} **工作空间设置：项目、接入凭证、成员** 项目、凭证、成员的 owner 管理界面；member 只读。
  - prd: docs/prd.md §9
- [ ] T-039 {P2} **维度详情页** 项目/模型/供应商详情：总量、趋势、其他维度分布，复用总览组件。
  - prd: docs/prd.md §5.3
- [ ] T-013 {P2} **保留期清理任务** 明细按首次接收时间留180天，分钟聚合留13个日历月；清理批量与边界测试。
  - 决策: docs/decisions/0003-minute-aggregation-and-retention.md
- [ ] T-014 {P2} **聚合重建工具** 仅在明细完整覆盖范围内重建；中断可恢复，旧汇总不变。
  - 决策: docs/decisions/0003-minute-aggregation-and-retention.md

## 一期

- [ ] T-001 {planning} {P1} **一期总览：Go 采集服务 + React 仪表盘 + DSH/PI 插件** 总览条目不直接执行；推进顺序：工程初始化 → 采集打通 → 能看数据 → 多人与运维。具体目录、依赖与实现简化由后续专业 agent 规划，当前不开始编码。
  - prd: docs/prd.md
  - 原型: docs/prototypes/index.html
  - 视觉基准: docs/prototypes/dashboard-f.html
  - 架构: docs/architecture.md
  - 数据模型: docs/data-model.md
  - 保留决策: docs/decisions/0003-minute-aggregation-and-retention.md

## 采集与存储


## 账号与租户


## 查询与指标


## 仪表盘前端


## DSH/PI 采集插件

## @里程碑：工程初始化
