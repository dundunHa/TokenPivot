# ADR-0005：插件实例与项目标识

状态：项目归属部分为 2026-10-28 用户决策；source 与事件 ID 部分为 T-044 提议，待 T-041 实现 source 创建时复核。关联 T-044、T-045，PI 侧（T-047）沿用。

## 决策

**插件实例（source）**

- source 由 workspace owner 在服务端创建，接入凭证绑定这个 source；插件不自行生成、不上报实例 ID。服务端从凭证得到 `(workspace_id, source_id)`，与 OpenAPI 中“身份只从认证取得”一致。
- 一个凭证对应一个插件实例：同一台机器的同一个 DSH profile 用一个凭证；多台机器或多个 profile 各自申请凭证，不共享。轮换凭证不改变 source，所以历史与重传身份不受影响。
- 不做跨实例合并：两个 source 即使上报同一个 `source_event_id` 也是两条事件（身份含 source_id）。
- `sources.instance_key` 由服务端创建 source 时生成（建议 UUID），只作服务端内部唯一键，展示名由 owner 填写；创建接口与页面属 T-041/T-021，本 ADR 不定义。

**事件 ID**

- DSH：`dsh:msg:<message.id>`、`dsh:compaction:<session id>:<compactionId>`；`dsh:` 前缀区分平台，PI 用自己的前缀。
- 同一实例内 fork 出的会话复制的消息 id 不变，服务端判 duplicate，不重复计数。

**项目**

- 上报的 `project_id` 只能是服务端已有项目的 UUID（OpenAPI 现有约定），插件不创建项目。
- 归属在插件本地配置：「本地目录 → project_id」列表，会话工作目录落在某目录下（最长匹配）即归该项目，否则上报 `null`（未归属）。
- 绝对路径、目录名、git 远端地址都不上报，也不作展示身份；项目的展示名只来自服务端 `projects.name`。`projects.project_key` 由服务端创建项目时生成，不从路径推导。

## 备选与后果

- 不选“插件上报 project_key、服务端自动注册项目”：要改 OpenAPI 与采集逻辑，且路径派生的 key 会把本地目录信息带到服务端。代价是 owner 需要手动配置映射，没配的用量显示为未归属。
- 不选“插件自生成实例 UUID 并上报”：上报的实例 ID 会和凭证决定的 source 产生两套身份，出现不一致时没有可信的一方。
- 用户把同一凭证装到两台机器上时，两边事件会落到同一 source；DSH 的 message.id 是随机 UUID，不会互相覆盖，但看板无法区分两台机器。这属于使用约定，由凭证管理页提示。
