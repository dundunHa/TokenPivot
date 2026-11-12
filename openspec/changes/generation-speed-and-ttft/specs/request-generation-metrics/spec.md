# Spec Delta

## Purpose

为普通生成与 compaction 汇总提供口径一致、可表示缺失的请求级生成速度和首 token 延迟，并以独立有效样本汇总近 60 秒表现，避免把总用量或 chunk 数误当成生成速度。

## ADDED Requirements

### Requirement: 客户端观测输出计时

系统 SHALL 以客户端观测时间记录每次 provider 调用尝试的请求开始、首个有效输出和最后有效输出，并明确指标是流式 chunk 估算而非逐 token 精确测量。有效输出 MUST 包括非空文本、非空 reasoning 文本，以及 argumentsDelta 非空或 name 为非空字符串的 tool-call；仅 usage 或 finish 的帧 MUST NOT 视为输出。

#### Scenario: 100 tokens 的标准计时样例

- **WHEN** 请求开始于 0 ms，首个有效输出在 8000 ms，最后有效输出在 10000 ms，最终 provider outputTokens 为 100
- **THEN** TTFT 为 8000 ms，生成速度为 `(100-1)/(10-8)=49.5` tokens/s

#### Scenario: usage 和 finish 不改变输出边界

- **WHEN** usage 帧先于非空 reasoning 到达，随后出现有效 tool-call，最后仅收到 finish 帧
- **THEN** 首个有效输出取 reasoning 时间，最后有效输出取 tool-call 时间，usage 与 finish 时间均不参与计算

### Requirement: 生成速度使用最终用量并允许缺失

系统 SHALL 使用最终 provider outputTokens 作为 N，按 `(N-1)/(t_last_output-t_first_output)` 计算 tokens/s；落库比较与计算时仅 `output_tokens.quality=known` 代表这一最终精确用量，estimated 或 missing MUST NOT 充当 N。N 缺失、N≤1、任一边界缺失或 decode 时长非正时，速度 MUST 为缺失，MUST NOT 使用 chunk 数、0、无穷值或猜测值替代。

#### Scenario: 单个多 token chunk

- **WHEN** 最终 outputTokens 大于 1，但所有输出在同一个 chunk 到达，首末有效输出时间相同
- **THEN** 生成速度为缺失，TTFT 仍可按首个有效输出计算

#### Scenario: 无最终用量

- **WHEN** 已观测到有效输出且 TTFT 有效，但最终 provider outputTokens 缺失
- **THEN** 生成速度为缺失且 TTFT 被保留，系统不得用 chunk 数补算速度

#### Scenario: 只有一个输出 token

- **WHEN** 最终 outputTokens 为 1 且 decode 时长为正
- **THEN** 生成速度为缺失，唯一输出不能形成可测量的逐 token decode 样本

#### Scenario: 最终输出用量仅为估算

- **WHEN** 有效输出边界与 TTFT 完整，但最终 output_tokens 仅为 estimated
- **THEN** 生成速度为缺失且 TTFT 被保留，不把估算值当成最终 provider 精确用量

### Requirement: TTFT 有效性独立于生成速度

系统 SHALL 按 `t_first_output-t_request_start` 计算 TTFT 毫秒数。请求开始或首个有效输出缺失、时间差为负时 TTFT MUST 为缺失；零 TTFT 是有效观测，TTFT 有效性 MUST NOT 依赖 outputTokens 或 decode 时长。

#### Scenario: 时钟回拨

- **WHEN** 首个有效输出时间早于其所属尝试的请求开始时间，或最后有效输出时间早于首个有效输出时间
- **THEN** 负 TTFT 或负 decode 时长对应指标分别为缺失，不得取绝对值或改成 0

#### Scenario: 同一毫秒返回首个输出

- **WHEN** 首个有效输出与请求开始处于同一毫秒
- **THEN** TTFT 为 0 ms，而不是缺失

### Requirement: 重试与并发生成互不串接

系统 SHALL 将计时限制在一次生成上下文和一次 provider 调用尝试内，最终请求记录 MUST 只使用最终成功尝试的开始、输出边界与最终用量；不同 Agent 实例中相同的局部 attempt 标识 MUST NOT 互相关联。

#### Scenario: 失败尝试先于成功重试

- **WHEN** 失败尝试已有首个输出，成功重试重新开始并在之后产生首末输出
- **THEN** 请求指标仅由成功重试计算，TTFT 不包含失败尝试的等待时间，decode 区间也不跨两次尝试

#### Scenario: 两个 Agent 使用相同局部 attempt 标识

- **WHEN** 两个 Agent 实例并发生成且局部 attempt 标识相同
- **THEN** 两个请求分别保留自己的计时，最终事件只通过所属实例与最终事件顺序关联，不互相覆盖

#### Scenario: 已有输出后中断

- **WHEN** 普通请求已有输出与 usage，但最终消息标记 interrupted
- **THEN** 保留既有用量上报，不发布成功请求计时，两项指标均不进入有效样本

#### Scenario: 空工具片段

- **WHEN** tool-call 的 argumentsDelta 为空且 name 缺省或为空字符串
- **THEN** 不改变首末有效输出边界；name-only 或 arguments-only 非空片段仍计为有效输出

### Requirement: 普通请求与 compaction 使用同一指标口径

系统 SHALL 为普通 Assistant 请求和直接流式 compaction 汇总生成相同形状的可选速度与 TTFT；compaction MUST 使用其自身 provider 调用的输出边界，MUST NOT 复用被压缩对话中普通请求的计时。

#### Scenario: compaction 汇总完成

- **WHEN** compaction 直接流产生 reasoning、文本或有效 tool-call，并最终保存 summary
- **THEN** 该 summary 请求按相同公式获得速度与 TTFT，缺失和无效样本规则与普通请求一致

### Requirement: 可空采集与历史兼容

采集接口 SHALL 接受缺省或为 null 的 decode 时长与 TTFT，并以可空原始计时持久化；每请求生成速度由 known 最终 outputTokens 与 decode 时长派生，避免上传和比较浮点派生值。缺省和 null MUST 表示未知；旧记录 MUST 保持缺失，不得回填。新增字段 MUST NOT 改变事件身份、既有最终用量或分钟聚合。

#### Scenario: 旧客户端上报

- **WHEN** 不含新指标字段的旧客户端事件被新服务端接收
- **THEN** 事件按既有规则接受，两个指标保存为缺失，原有用量与防重行为不变

#### Scenario: 查询升级前历史记录

- **WHEN** 请求明细包含升级前已保存的记录
- **THEN** 历史速度与 TTFT 显示为缺失，不显示 0，也不根据原有总 token 和时长反推

### Requirement: 近 60 秒按独立有效样本求算术平均

系统 SHALL 在同一 as_of 截点按 `occurred_at ∈ [as_of-60s,as_of)` 查询当前工作空间原始请求，分别对非空有效速度和非空有效 TTFT 求每请求算术平均并返回各自样本数。系统 MUST NOT 按 token 或时长加权，也 MUST NOT 用总 token 除以总时长代替。

#### Scenario: 两项指标样本集合不同

- **WHEN** 窗口内请求 A 只有 TTFT，请求 B 两项均有效，请求 C 两项均缺失
- **THEN** 速度仅平均 B 且样本数为 1，TTFT 平均 A 与 B 且样本数为 2，C 不进入任一平均值

#### Scenario: 窗口内没有有效样本

- **WHEN** 窗口内没有请求或所有请求的对应指标均缺失
- **THEN** 对应平均值为缺失且样本数为 0，不得展示为 0 tokens/s 或 0 ms

#### Scenario: 补报请求保留原始发生时间与计时

- **WHEN** 插件补报一个仍落在当前滚动窗口内的历史请求，随后又补报一个发生时间早于窗口的请求
- **THEN** 前者按原 occurred_at 和原计时参与后续窗口查询，后者不抬高当前平均值，接收时间不得替代发生时间或输出边界

### Requirement: 请求明细与顶部摘要展示缺失语义

界面 SHALL 展示每请求生成速度 tokens/s 与 TTFT ms，并在顶部展示近 60 秒平均速度、平均 TTFT 及各自有效样本数。缺失 MUST 显示为明确的未知状态，界面 MUST 标注这些值为客户端观测的 chunk 估算。

#### Scenario: 每请求指标部分缺失

- **WHEN** 某请求只有有效 TTFT 而速度缺失
- **THEN** 该请求显示 TTFT，速度显示未知，顶部两项按各自有效样本统计

#### Scenario: 查看滚动窗口摘要

- **WHEN** 用户查看请求日志顶部摘要
- **THEN** 页面能区分平均速度及其样本数、平均 TTFT 及其样本数，并可得知统计窗口和客户端估算口径
