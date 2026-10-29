# TODO

<!-- todo-meta: next-id=60 -->

## 里程碑一：采集打通


## 里程碑二：能看数据

- [ ] T-055 {running} {P1} **聚合查询接口：格式定稿 + 趋势 + 汇总 + 排名** 已合入 main；完整 make check、integration 标签 vet 与衔接静态审查通过。按用户决定暂缓真实 PostgreSQL 集成与 EXPLAIN，验收未完成。
  - 架构: docs/architecture.md
  - prd: docs/prd.md
  - 决策: docs/decisions/0003-minute-aggregation-and-retention.md
  - plan: docs/plans/2026-10-29-aggregate-query-api.md
  - 提交: b37a05d
- [ ] T-057 {paused} {P1} **总览页：筛选栏 + 指标区 + 趋势图 + 排名** 用户要求停工，子代理已停止。共享基础改动未提交，保留于 milestone2-supervised-delivery-dev-e0a0 工作区；总览页面未开工，待新规划。
  - 视觉基准: docs/prototypes/dashboard-f.html
  - prd: docs/prd.md
  - plan: docs/plans/2026-10-29-overview-dashboard.md
- [ ] T-058 {paused} {P2} **请求明细：60 秒速率 + 明细查询 + 明细面板** 用户要求停工并准备调整规划；本任务尚未开工，无实现改动。恢复前等待用户确认新范围。
  - prd: docs/prd.md
  - plan: docs/plans/2026-10-29-request-details-and-rate.md
- [ ] T-059 {paused} {P1} **BF 部署：采集服务与仪表盘** 用户要求停工。容器三文件已提交子分支 30be360，未合入、镜像未构建成功；HTTP 配置和部署文档尚未开工，构建方案未改，bf 未变更。
  - plan: docs/plans/2026-10-29-bf-docker-deployment.md
  - 提交: 725061f

## 里程碑三：多人与运维

- [ ] T-018 {P1} **SMTP 邮件：邮箱验证与密码重置** 邮件发送封装、验证链接、重置链接、令牌过期与一次性。
  - 决策: docs/decisions/0004-auth-and-workspace.md
- [ ] T-020 {P2} **成员邀请与管理** owner 邮件邀请、接受邀请、移除成员；邀请交互细节待确认。
  - prd: docs/prd.md §2
- [ ] T-041 {P2} **工作空间设置：项目、接入凭证、成员** 项目、凭证、成员的 owner 管理界面；member 只读。
  - prd: docs/prd.md §9
- [ ] T-047 {P1} **PI 插件：采集、重试与补报** 采集最终用量、转统一格式、缺失字段标缺失、不采集 Prompt/路径/密钥；本地队列重试与断网补报，重传沿用同一事件ID。
  - prd: docs/prd.md §3.2
- [ ] T-029 {P2} **数据源状态：查询与页面** 查询与页面合并：来源平台、实例、版本、最近接收时间、错误；离线检测方式待确认，未实现前不显示“在线”。
  - prd: docs/prd.md §5.5
- [ ] T-039 {P2} **维度详情页** 项目/模型/供应商详情：总量、趋势、其他维度分布，复用总览组件。
  - prd: docs/prd.md §5.3
- [ ] T-013 {P2} **保留期清理任务** 明细按首次接收时间留180天，分钟聚合留13个日历月；清理批量与边界测试。
  - 决策: docs/decisions/0003-minute-aggregation-and-retention.md
- [ ] T-014 {P2} **聚合重建工具** 仅在明细完整覆盖范围内重建；中断可恢复，旧汇总不变。
  - 决策: docs/decisions/0003-minute-aggregation-and-retention.md
- [ ] T-042 {P2} **响应式与动效可访问性** 桌面优先重排，小屏保留核心指标/筛选/趋势/明细；跟随系统 prefers-reduced-motion，不做设置项；状态不只靠颜色。
  - prd: docs/prd.md §8
