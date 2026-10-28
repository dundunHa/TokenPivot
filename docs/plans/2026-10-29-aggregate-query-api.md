# T-055：聚合查询接口（筛选、趋势与分组排名）

> 日期：2026-10-29。状态：**已规划；不授权实施**。
>
> 合并原 T-023/T-024/T-025/T-027。依赖 T-054 的空间权限与项目/来源管理边界；60 秒速率和请求明细归 T-058，UI 归 T-057。本文只规划接口文档、查询服务、HTTP 接入与必要查询索引，不授权实现。

## 1. 已确认查询限额

用户已确认以下首版数值，实施时直接写入 OpenAPI 与入口校验，不再另定一套：

| 项 | 已确认值 | 说明 |
|---|---:|---|
| 趋势返回点数上限 `MAX_POINTS` | **1000** | 超过时返回 `TOO_MANY_POINTS`，附上限和满足限制的最细建议粒度；不是性能承诺。 |
| 排名每页默认/最大条数 | **10 / 50** | 首屏返回主要来源与“其他”，展开时逐页读取。 |

`docs/openapi/openapi.yaml:401-407` 已先冻结 13 个日历月、实际保留截点、`[start,end)`、1000 点及超限建议粒度要求，本计划逐项落实。筛选值数量上限属于入口安全限制，定为每维最多 50 个不同值。

## 2. 已核实现状

### 2.1 数据、统计与保留边界

- `db/migrations/00004_usage_minute_stats.sql:6-43` 已建 `usage_minute_stats`：键为 `(workspace_id,bucket_start,source_id,project_id,provider,model)`，`project_id=NULL` 表示未归属；每事件累加 `event_count`，六类 token 各保存 sum 与 known/estimated/missing 三类计数。
- `db/migrations/00004_usage_minute_stats.sql:40-49` 当前只有唯一约束自带索引，前缀是 `(workspace_id,bucket_start)`；服务角色已有 SELECT。没有查询专用组合索引。
- `internal/aggregation/minute.go:133-177` 已把发生时刻转换成 UTC 分钟起点，不读会话时区；`:97-131` 分开累加六类指标，total 不由缓存或推理子集叠加。本任务复用这些已落库数据，不改写入 SQL。
- `docs/data-model.md:75-88` 规定 sum 是 known 与 estimated 之和，missing 不贡献值；真实零由 known 计数识别，全部缺失必须显示不可用；查询索引由查询任务按 EXPLAIN 决定。
- `docs/architecture.md:54-65` 规定分钟保留 13 个日历月、接口回传精确截止边界，趋势与排名共同读取分钟表；统一 `[start,end)`，历史秒级范围不得静默取整，当前未完成分钟须标记并返回统计截点，夏令时重复小时不能合并。
- `docs/decisions/0003-minute-aggregation-and-retention.md:7-15` 已确认 13 个日历月和按完整分钟清理，但没有给出“当前时刻减 13 个月”的月末、闰日或分钟计算算法。
- `docs/data-model.md:105-109` 与 `docs/plans/2026-10-27-utc-minute-aggregation.md:15-20,250-256` 只确定保留原则和后续清理边界，**没有已实现或可复用的清理截点算法**。第 5.1 节因此定义一个统一算法，供本任务与 T-013 清理实现共同使用，不声称它已存在。
- `docs/prd.md:97-115` 要求总量、输入、输出、请求数、分钟/小时/天趋势及项目/模型/供应商排名；同名模型跨供应商分别统计。`docs/prd.md:151-180` 要求缓存/推理不重复计数、缺失不等于零。

### 2.2 HTTP、认证与装配

- `internal/server/auth_middleware.go:97-123` 的 `RequireSession` 已统一完成 Cookie 会话认证、续期和身份注入；`internal/server/auth_middleware.go:54-59` 的 `UserFromContext` 是 handler 读取用户身份的唯一入口。
- `internal/workspaces/service.go:23-27` 已定义 owner 可管理、member 可读取；`:86-103` 的 `MembershipRole` 返回成员角色且不会把数据库故障当成非成员。本任务只复用这个现有读取接口，不假定 T-054 未来的 HTTP 路径或服务方法。
- `internal/server/credentials.go:179-203` 展示现有“会话用户 + 路径 workspace + 成员角色”的授权顺序；查询接口应采用同一身份来源，但读权限允许 owner 与 member，不能照抄 owner-only 判断。
- `internal/server/auth.go:45-54,251-289` 已有统一 `{error:{code,message}}`、JSON 输出及安全错误边界；`internal/database/database.go:17-29` 只保留取消/超时身份，普通数据库错误不能伪分类成未认证或空结果。
- `internal/server/server.go:28-44,80-82` 当前只注册认证、凭证与采集接口；`cmd/tokenpivot/main.go:46-58` 是现有装配入口。当前没有已注册的用量查询 HTTP 接口。
- `docs/openapi/openapi.yaml:9-15` 明确当前文档尚未包含查询接口；本任务须先把本计划的格式写入该文件，再实现代码。单事件采集的 JSON 数值格式不属于本任务，保持不变。

## 3. 目标与非目标

### 3.1 目标

1. 提供工作空间内的只读聚合接口：同一组 Provider、Model、Project 多选筛选同时作用于总量、趋势和排名。
2. 返回输入、输出、总量、缓存读、缓存写、推理六类 token 及请求数；每项 token 同时返回值与三类质量计数，聚合整数在 JSON 中不失真。
3. 支持 UTC 分钟及按指定时区的自然小时、自然日趋势；支持当前未完成桶、普通及半小时夏令时切换和明确点数限制。
4. 支持按 project、provider、model 排名，返回主要分组、占总量比例、聚合“其他”，并可用游标展开“其他”。
5. 每个请求都重新执行会话、工作空间读权限和当前保留截点检查，所有 SQL 都限定获准的 workspace，响应不缓存。

### 3.2 非目标

- 不实现滚动 60 秒速率、请求明细、明细游标或原始事件查询（T-058）。
- 不实现页面、图表、筛选 URL 状态、项目/来源选择器或展示名映射（T-057/T-054）。
- 不新增项目/来源管理接口，不假定或调用尚未确定的 T-054 接口；排名只返回稳定身份，项目显示名由 UI 从 T-054 的合法数据来源取得。
- 不新增表、物化视图、缓存、消息队列、异步任务、新存储系统、新配置项或第三方依赖；不改分钟写入 UPSERT，不实现保留清理。
- 不做费用、预算、延迟分位数、模型别名合并、跨工作空间汇总或跨请求数据快照。
- 不改变单事件采集的整数 JSON 输入/输出格式，不承诺尚未测得的并发量、响应时间或“任何筛选都走索引”。

## 4. T-054 依赖与停止条件

T-055 的数据源与 HTTP 格式可以独立确定，但公开接口交付受以下边界约束：

1. **读权限冻结前停止公开路由接入。** 沿用现有 `RoleMember` 的既定含义，允许 owner/member 读取、非成员与未知角色拒绝。若 T-054 尚未确认该规则，或准备替换 `MembershipRole`，只能先做纯时间计算和独立 SQL 测试；不得先上线一套临时权限判断。
2. **不等待、不猜测 T-054 的 HTTP 响应。** T-055 不调用项目/来源管理端点，不返回项目展示名，不复制其列表逻辑。若 UI 需要名称，T-057 按 T-054 实际交付接口组合数据。
3. **身份语义变化时停止。** 若 T-054 改变 project ID、未归属 NULL 或 workspace 归属语义，须先复核本计划 SQL 和游标格式；不得另建“查询专用项目身份”。
4. **跨空间资源不做存在性探测。** 获准查询某 workspace 后，筛选其他 workspace 的 project UUID 只得到本 workspace 内的空结果；不加入 metadata 查询来区分“存在但越权”和“不存在”。

## 5. 接口文档设计（拟新增）

以下路径均为**拟新增**，不是现有接口。方法均为 GET，使用 `cookieAuth`，不要求 `Origin`；所有成功与失败响应带 `Cache-Control: no-store`。

- `GET /api/v1/workspaces/{workspaceID}/usage/aggregate`：总量 + 趋势。
- `GET /api/v1/workspaces/{workspaceID}/usage/rankings`：分组排名 + 其他展开。

### 5.1 公共参数与统一保留截点

| 参数 | 位置 | 必填 | 规则 |
|---|---|---|---|
| `workspaceID` | path | 是 | UUID；只用于授权与 SQL 租户条件，不从请求接收 user ID。 |
| `start` | query | 是 | 带偏移的 RFC 3339 时刻，转换为 UTC instant；必须分钟对齐（秒与 fractional second 都为 0）。 |
| `end` | query | 否 | 带偏移且分钟对齐的 RFC 3339 时刻；省略表示本次请求采样一次的服务端当前时刻 `as_of`，用于包含当前未完成分钟。 |
| `provider` | query，可重复 | 否 | 精确、区分大小写的 provider；同维多值 OR。空值拒绝，最多 50 个不同值。 |
| `model` | query，可重复 | 否 | 精确、区分大小写的 model；同维多值 OR。空值拒绝，最多 50 个不同值。 |
| `project` | query，可重复 | 否 | UUID 或精确字面量 `unassigned`；同维 OR，UUID 与未归属可组合，最多 50 个不同值。 |

共同规则：

- 不同维度之间 AND；未出现的维度不筛选。重复值先去重，未知 query 参数、单值参数重复出现、非法 UTF-8/编码、空值均返回 `400 INVALID_ARGUMENT`。
- 有效范围是 `[start,end)`，且 `start < end`；显式 `end` 不得晚于 `as_of`，不得静默截断未来范围。要查询当前分钟必须省略 `end`，而不是提交带秒的“现在”。
- 有效上界 `effectiveEnd` 是显式 `end`；`end` 省略时则是本次请求只采样一次的统计截点 `as_of`。入口校验、SQL 上界与响应 `range.end` 共用 effectiveEnd，省略时 `range.as_of` 也是同一值。响应中的实际时间一律转换成带 `Z` 的 RFC 3339 UTC。
- 历史 `start`/`end` 带非零秒或微秒直接拒绝，不做 floor/ceil。小时/天也允许分钟对齐的非整点范围：先按原始 `[start,end)` 过滤，再归入预计算桶；被范围截断的边缘桶标 `partial=true`，因此同一范围换粒度不会改变总量。

**两个端点共用以下单调不减的 13 个日历月算法。** 对当前时刻 `now` 先取 `u=now.UTC()`，再把年月向过去移动 13 个月得到目标年月；原日在目标月存在时沿用日、时、分，秒归零；不存在时统一取目标月下一月 1 日 00:00 UTC：

```text
if u.Day() <= daysIn(targetYear, targetMonth):
    retention_cutoff = UTC(targetYear, targetMonth, u.Day(), u.Hour(), u.Minute(), 0)
else:
    retention_cutoff = UTC(firstDayOfNextMonth(targetYear, targetMonth), 00:00)
```

- 目标月天数通过“目标月 1 日 → 下一月 1 日 → 前一天”计算；不得直接对月末日期使用 Go `AddDate(0,-13,0)`，也不得夹到短月末后每天重新沿用时分，否则截点可能倒退，重新允许查询已清理范围。
- 例：`2027-03-28T23:59:59Z` 的截点是 `2026-02-28T23:59:00Z`，`2027-03-29T00:00:00Z` 起到 3 月末的截点均为 `2026-03-01T00:00:00Z`；`2029-03-31T00:00:30Z` 的截点是 `2028-03-01T00:00:00Z`。短月溢出日不承诺保留目标月末的额外部分；截点只按 UTC 计算，与展示 timezone 无关。
- `start == retention_cutoff` 合法；`start < retention_cutoff` 返回 `400 OUTSIDE_RETENTION`，不执行聚合 SQL。错误 details 带当前 `retention_cutoff`，成功响应的 `range.retention_cutoff` 也返回同一值。
- 首屏用本次当前时刻计算截点。排名后续页仍复用游标中的原 `as_of` 固定查询上界，但必须用**当页新的当前时刻**重新计算并校验保留截点；原 start 若已早于新截点则返回 `OUTSIDE_RETENTION`，不得用旧 `as_of` 延长保留期。
- 本算法是拟新增的共享定义，由 `internal/aggregation` 提供一个小函数及测试，T-013 清理实现必须复用，且只能清理 `bucket_start < retention_cutoff` 的行，等于截点的行保留；本任务不实现清理，也不声称当前已有清理截点代码。

### 5.2 `aggregate` 专有参数与点数限制

| 参数 | 必填 | 规则 |
|---|---|---|
| `timezone` | 是 | IANA 时区名称，例如 `UTC`、`America/Los_Angeles`、`Australia/Lord_Howe`；空字符串、`Local`、固定偏移字符串拒绝。它只决定小时/天桶，不改变 UTC 分钟存储。 |
| `granularity` | 是 | `minute`、`hour`、`day` 之一。三种粒度共用第 5.3 节预计算边界，不在 SQL 另用一套分桶算法。 |

- 在访问聚合 SQL 前，用有效范围和 Go `time.Location` 预计算会相交的桶边界。枚举到 **MAX_POINTS+1（1001）个桶立即早停**，不得先完整枚举超长范围再计数。
- 超过 1000 返回 `400 TOO_MANY_POINTS`，不执行聚合 SQL，不截断 points，也不静默改变 granularity。
- 该错误的 `error.details` 固定包含 `max_points:1000` 和 `suggested_granularity`：从比请求粒度更粗的候选中，按 hour → day 的次序复用同一边界枚举器，选择第一个满足 1000 点的粒度；每个候选同样在 1001 个桶早停。没有满足者时 `suggested_granularity=null`，message 明确提示缩短范围。
- 示例：`{"error":{"code":"TOO_MANY_POINTS","message":"time range has too many points","details":{"max_points":1000,"suggested_granularity":"hour"}}}`。details 不是可选日志文本，OpenAPI 必须表达该结构。
- 不生成缺失时间桶，不做零填充；SQL 只返回筛选后有数据的桶。

### 5.3 单一时间桶边界算法（拟新增）

**核心取舍：不再让 Go 计算一套边界、PostgreSQL `date_trunc` 另算一套。** Go 负责生成唯一边界，SQL 只按这些边界归桶；因此 bucket_start、bucket_end、partial、点数和实际聚合都来自同一份定义，也不假定自然小时固定为 3600 秒。

算法以 UTC 分钟为最小精度，使用 `LoadLocation` 取得 Location，再用 `t.In(loc)` 把同一 instant 映射到所选时区：

| 粒度 | 桶键 `bucketKey(t)` | 边界含义 |
|---|---|---|
| minute | UTC 分钟起点 | 直接使用现有分钟边界。 |
| hour | 本地年月日、本地小时、该 instant 的 UTC offset | 本地小时或 offset 任一变化即进入新桶；重复小时因 offset 不同而不合并，半小时 offset 切换可在非整点形成新边界。 |
| day | 本地年月日 | 本地日期变化才进入新桶；同一天内 offset 变化不拆分自然日。 |

可实施步骤：

1. 从 start 所在桶开始：向过去探测，找到进入当前 `bucketKey` 的第一个 UTC 分钟，作为真实 `bucket_start`；不要用本地歧义时间直接 `time.Date` 猜 offset。
2. 从当前桶向未来寻找下一个 `bucketKey` 变化的第一个 UTC 分钟，得到真实 `bucket_end`。hour 以分钟探测；day 可先以小时探测、再在跨日期前一小时内细化到分钟。minute 直接加 1 分钟。
3. 重复步骤 2，按 `bucket_start < effectiveEnd` 产生桶；为最后一个桶也算出真实 bucket_end。达到 1001 个桶立即返回超限，不继续扫描剩余范围。
4. `partial = bucket_start < start OR bucket_end > effectiveEnd`；end 省略时 effectiveEnd 就是 as_of，所以当前未完成 minute/hour/day 自然为 true。partial 只表示时间覆盖，不表示指标质量。
5. 将每个桶的序号、bucket_start、bucket_end 作为参数化 `timestamptz` 数组传给 SQL，以 `unnest` 形成边界表。分钟行同时满足原始 `[start,end)` 过滤和 `row.bucket_start >= boundary.start AND row.bucket_start < boundary.end`，再按边界序号汇总。
6. 响应直接使用这些预计算边界，不让数据库重新推导。空桶仍被过滤，不填零。

该定义明确覆盖：普通前进造成的小时跳过、普通回拨造成的重复小时、`Australia/Lord_Howe` 的 30 分钟 offset 变化，以及 23/25 小时或半小时偏移的自然日。它是有意选择的“带 offset 的小时段”口径，不把同一本地小时的不同 offset 合并。

### 5.4 `rankings` 专有参数

| 参数 | 必填 | 规则 |
|---|---|---|
| `group_by` | 是 | `project`、`provider`、`model`。`model` 的分组键实际是 `(provider,model)`，避免同名模型跨供应商被合并。 |
| `limit` | 否 | 默认 **10**、最大 **50**；第一页与后续页必须相同。 |
| `cursor` | 否 | 上一响应的 `next_cursor`；展开“其他”时传入。 |

- 排名固定按各组 `total_tokens` 的可展示 sum 降序，值相同再按稳定分组键升序；project 的 NULL 键使用固定 `NULLS LAST` 次序，provider/model 使用 `COLLATE "C"` 保证游标次序不受数据库 locale 影响。首版不增加排序参数。
- 游标是不透明 base64url JSON，包含格式版本、规范化查询（含 workspaceID）的 SHA-256、原 `as_of` 和页偏移；不增加签名密钥。它只承担分页定位，**不是授权凭据**，每页仍执行会话、workspace 权限和当页保留截点检查。
- 传 `cursor` 时，客户端须重复原筛选、`group_by`、`limit` 与时间参数；服务端重算规范化查询摘要比对，不一致或游标格式非法均返回 `400 INVALID_ARGUMENT`。游标内 `as_of` 也须通过 start/end/未来时间校验，不能借伪造游标绕过范围检查。
- 第一页返回前 N 组与“第 N+1 组起全部剩余组”的 `other`。下一页返回接下来 N 组与“本页之后全部剩余组”的新 `other`；UI 用新 groups + 新 other 替换旧 other。无剩余组时 `other=null`、`next_cursor=null`。
- 分钟桶会被历史补报修改，当前表没有可用于跨请求快照的版本。游标固定查询与统计上界，但**不冻结分组值或排名**；分页期间发生回填时可能出现重排，不承诺跨页无重复/无遗漏。静态数据下必须严格无重复、无遗漏；需要最新首屏时重新请求第一页。跨请求快照若以后成为真实需求，需另行设计数据版本，不在本任务加存储。

### 5.5 指标与响应结构

**所有聚合值和计数在 JSON 中都使用十进制字符串，只有缺失的 MetricStats.value 使用 null；share 仍用浮点 JSON number。** 这样总量、请求数和质量计数超过 JavaScript 的 `2^53-1` 安全整数范围时也不会被浏览器静默舍入。

`MetricStats`（六类 token 共用）：

```json
{
  "value": "120",
  "known_count": "1",
  "estimated_count": "0",
  "missing_count": "1"
}
```

- 字符串为无符号规范十进制：`0` 或首个数字 1–9、后续数字 0–9；不允许前导零、小数点、指数或正负号。OpenAPI 用 string + pattern 表达，不能标成 integer。
- `value = 所有 known/estimated 输入值之和`；`known_count + estimated_count == 0` 时 `value=null`，即使 sum 的物理增量为 0。已知 0 或估算 0 都返回 `value="0"`，由相应计数区分。
- `UsageStats` 包含十进制字符串 `request_count` 与 `input_tokens`、`output_tokens`、`total_tokens`、`cache_read_tokens`、`cache_write_tokens`、`reasoning_tokens` 六个 `MetricStats`。三类质量计数也全部是十进制字符串。
- `request_count` 是分钟表 `event_count` 之和，即已接受请求级最终事件数；上报重传不增加，全部 token 缺失的事件仍计入。不把汇总级事件伪造成请求。
- `total_tokens` 直接汇总存储的 total，不用 input+output 重算；cache_read/cache_write/reasoning 仅作为独立子集返回，绝不加入 total。
- 无行时 `request_count="0"`，六指标均为 `value=null` 且三类计数为 `"0"`；points/groups 返回 `[]`，不是 null。
- 以上只适用于本任务新增的聚合响应。单事件采集请求/响应继续保持既有 JSON integer，不做字符串化迁移。

`aggregate` 响应样例：

```json
{
  "range": {
    "start": "2026-10-28T00:00:00Z",
    "end": "2026-10-28T01:30:00.123456Z",
    "as_of": "2026-10-28T01:30:00.123456Z",
    "retention_cutoff": "2025-09-28T01:30:00Z",
    "timezone": "UTC",
    "granularity": "hour"
  },
  "totals": {
    "request_count": "2",
    "input_tokens": {"value":"100","known_count":"1","estimated_count":"0","missing_count":"1"},
    "output_tokens": {"value":"20","known_count":"1","estimated_count":"0","missing_count":"1"},
    "total_tokens": {"value":"120","known_count":"1","estimated_count":"0","missing_count":"1"},
    "cache_read_tokens": {"value":"60","known_count":"1","estimated_count":"0","missing_count":"1"},
    "cache_write_tokens": {"value":null,"known_count":"0","estimated_count":"0","missing_count":"2"},
    "reasoning_tokens": {"value":null,"known_count":"0","estimated_count":"0","missing_count":"2"}
  },
  "points": [
    {
      "bucket_start": "2026-10-28T01:00:00Z",
      "bucket_end": "2026-10-28T02:00:00Z",
      "partial": true,
      "stats": {
        "request_count": "2",
        "input_tokens": {"value":"100","known_count":"1","estimated_count":"0","missing_count":"1"},
        "output_tokens": {"value":"20","known_count":"1","estimated_count":"0","missing_count":"1"},
        "total_tokens": {"value":"120","known_count":"1","estimated_count":"0","missing_count":"1"},
        "cache_read_tokens": {"value":"60","known_count":"1","estimated_count":"0","missing_count":"1"},
        "cache_write_tokens": {"value":null,"known_count":"0","estimated_count":"0","missing_count":"2"},
        "reasoning_tokens": {"value":null,"known_count":"0","estimated_count":"0","missing_count":"2"}
      }
    }
  ]
}
```

- points 按 bucket_start 升序。bucket_start/bucket_end 是唯一识别 instant 的 UTC 时间，不只返回本地小时文本。
- `partial=true` 仅表示该桶被请求 start/effectiveEnd 截断；历史已结束桶也可能因后续补报改变，字段不宣称“数据永不再变”。
- bucket_end 是第 5.3 节算出的下一真实边界，不能对 hour 固定加 1 小时、对 day 固定加 24 小时。

`rankings` 响应形状：

```json
{
  "range": {
    "start": "2026-10-28T00:00:00Z",
    "end": "2026-10-28T01:30:00.123456Z",
    "as_of": "2026-10-28T01:30:00.123456Z",
    "retention_cutoff": "2025-09-28T01:30:00Z"
  },
  "group_by": "model",
  "overall": {"request_count":"2","input_tokens":{"value":"100","known_count":"1","estimated_count":"0","missing_count":"1"},"output_tokens":{"value":"20","known_count":"1","estimated_count":"0","missing_count":"1"},"total_tokens":{"value":"120","known_count":"1","estimated_count":"0","missing_count":"1"},"cache_read_tokens":{"value":"60","known_count":"1","estimated_count":"0","missing_count":"1"},"cache_write_tokens":{"value":null,"known_count":"0","estimated_count":"0","missing_count":"2"},"reasoning_tokens":{"value":null,"known_count":"0","estimated_count":"0","missing_count":"2"}},
  "groups": [
    {
      "key": {"provider":"example-provider","model":"example-model"},
      "share": 1.0,
      "stats": {"request_count":"2","input_tokens":{"value":"100","known_count":"1","estimated_count":"0","missing_count":"1"},"output_tokens":{"value":"20","known_count":"1","estimated_count":"0","missing_count":"1"},"total_tokens":{"value":"120","known_count":"1","estimated_count":"0","missing_count":"1"},"cache_read_tokens":{"value":"60","known_count":"1","estimated_count":"0","missing_count":"1"},"cache_write_tokens":{"value":null,"known_count":"0","estimated_count":"0","missing_count":"2"},"reasoning_tokens":{"value":null,"known_count":"0","estimated_count":"0","missing_count":"2"}}
    }
  ],
  "other": null,
  "next_cursor": null
}
```

- project 键为 `{"project_id":"uuid"}` 或 `{"project_id":null}`；provider 键为 `{"provider":"..."}`；model 键如上包含 provider 与 model。
- `share` 是该组 total_tokens 可展示 sum ÷ `overall.total_tokens` 可展示 sum，范围 0–1；它不是账单比例，部分缺失/估算仍由 stats 明示。overall 分母为 0 或 null、或该组 value 为 null 时 `share=null`，不得把 0/0 或缺失写成 0%。share 是展示用浮点近似值，聚合整数本身不得先转 float。
- `other` 没有 key，包含与 groups 相同的 share/stats；每一页 groups 与 other 合计等于本页起剩余值，overall 始终包含所有组。

### 5.6 错误与权限

| HTTP | code | 情况与 details |
|---:|---|---|
| 400 | `INVALID_ARGUMENT` | 参数、UUID、时区、范围、分钟对齐、筛选数量或游标非法；不执行聚合查询。 |
| 400 | `OUTSIDE_RETENTION` | start 早于当次保留截点，含过期分页游标；details 为 `{"retention_cutoff":"..."}`。 |
| 400 | `TOO_MANY_POINTS` | 趋势相交桶数超过 1000；details 包含数值 `max_points=1000` 和取值为 hour、day 或 null 的 `suggested_granularity`，null 表示只能缩短范围。 |
| 401 | `UNAUTHENTICATED` | 无有效浏览器会话；沿用会话中间件。 |
| 403 | `PERMISSION_DENIED` | 非该 workspace 成员，或 T-054 冻结的角色不具备读权限；不返回数据。 |
| 500 | `INTERNAL` | 成员查询、聚合 SQL、十进制格式化或其他非预期故障；不返回伪空结果。 |
| 503 | `UNAVAILABLE` | 会话续期等已有认证中间件判定的依赖不可用；本任务不重新分类 `database.SafeError` 已抹平的数据库错误。 |

授权顺序：RequireSession → 解析 workspaceID → MembershipRole → 参数/游标/当次保留截点校验 → 聚合 SQL。这样未认证请求不探测 workspace；权限失败不执行聚合。已获准后的空结果与越权筛选 ID 不泄露其他 workspace 元数据。

## 6. 查询与数据流设计（拟新增）

### 6.1 共用过滤

1. HTTP adapter 只负责严格解析 query、调用授权和写出响应；service 负责规范化筛选、时间/保留截点/边界/游标及指标映射；Postgres store 负责参数化 SQL。
2. 每条 SQL 都包含 `workspace_id = $workspaceID AND bucket_start >= $start AND bucket_start < $effectiveEnd`。workspaceID 来自已授权路径，start 已通过当次保留截点检查。
3. provider/model/project 使用固定 SQL 片段与参数数组；project 的 `unassigned` 只映射为 `project_id IS NULL`。group_by 只能从代码内枚举选择完整表达式，客户端字符串绝不拼进 SQL。
4. 趋势先应用原始分钟范围与筛选，再按第 5.3 节边界表归桶；不读取 `usage_events`，不按 received_at 过滤。

### 6.2 总量与趋势一条语句

采用一条 SELECT：以参数化 `unnest` 边界表关联过滤后的分钟行，内层按边界序号汇总，外层对桶结果使用窗口 sum 得到 overall totals。每一行同时携带本桶 stats 与重复的整体 stats，Go 只取首行的整体值；空结果由 service 构造空 UsageStats。

这样同一 SQL 语句快照内 `totals == sum(points)`，避免两条独立查询之间补报造成不一致。不要求与另一个 rankings 请求共享快照。SQL 不使用 `date_trunc` 推导另一套桶，也不依赖连接/session TimeZone。

### 6.3 排名、overall 与其他一条语句

一条 SELECT 内依次完成：过滤与分组求和 → 计算稳定排序及 overall 窗口合计 → 根据已验证游标偏移选择本页 → 聚合本页之后的 other。响应最多 N 个 groups + 1 个 other，不把全部分组先读入 Go。

overall 必须在本页过滤之前计算，保证后续页的 share 分母仍是整个筛选范围。游标偏移只用于分页，不允许客户端直接提交 offset。

### 6.4 数值与失败

- PostgreSQL `sum(bigint)` 返回 numeric。store 无损扫描 numeric，并直接格式化为第 5.5 节规范十进制字符串；**不再转换 int64，也不设 int64 聚合上限**。超过 int64 或浏览器安全整数范围的合法聚合值仍须原样返回。
- share 只在完整 numeric 和值算出后转换展示用浮点，不能用浮点累加 token 或质量计数。
- 所有数据库错误经 `database.SafeError` 返回；handler 在请求边界记录一次安全错误并返回 500。取消/超时继续沿 context 传递，不重试、不另开连接、不把错误写成空数据。
- 沿用现有 HTTP 与连接池超时，不新增查询专用配置。点数限制不是 SQL 执行时间保证。

### 6.5 索引策略

先用现有 `(workspace_id,bucket_start,...)` 唯一索引和接近期望规模的数据执行 `EXPLAIN (ANALYZE, BUFFERS)`，覆盖无筛选、project 筛选、provider+model 筛选、长范围排名。只有观察到真实瓶颈才新增最小前向索引迁移；候选必须按实际查询确定，不预建 source/project/provider/model 的所有排列。

若确需索引：由主代理分配迁移编号，评估锁与写入影响，同步 `docs/data-model.md` §5，并保留 EXPLAIN 前后证据。代码回滚不要求删索引；共享环境不自动 Down。没有证据时本任务零迁移。

## 7. 拟改文件与责任

| 路径 | 状态与责任 |
|---|---|
| `docs/openapi/openapi.yaml` | 修改：先加入两个 GET 路径、参数、保留截点、十进制字符串 schema、响应样例与 details 错误结构，更新文档范围说明。 |
| `internal/aggregation/retention.go` | 新增：第 5.1 节唯一 UTC 13 日历月截点函数；不写数据库、不实现清理，供 T-013 复用。 |
| `internal/aggregation/retention_test.go` | 新增：月末、闰日、连续溢出日、截点单调不减、分钟取整与边界相等测试。 |
| `internal/aggregation/minute.go` | 只最小更新包注释以涵盖保留截点；不改 ApplyRequest、bucketStart 或 UPSERT。 |
| `internal/usage/service.go` | 新增：查询类型、严格规范化、第 5.3 节唯一边界枚举、点数/建议粒度、游标、指标映射；不 import server。 |
| `internal/usage/postgres.go` | 新增：参数化边界数组、趋势与排名 SQL、窗口合计、numeric 十进制格式化、SafeError。 |
| `internal/usage/service_test.go` | 新增：参数、保留截点接入、普通 DST、Lord Howe、桶边界、早停、建议粒度、游标和大整数字符串测试。 |
| `internal/usage/postgres_integration_test.go` | 新增：真实 PG 按预计算边界归桶、过滤、汇总、排名、other、空结果和错误路径。 |
| `internal/server/usage.go` | 新增：两个 GET handler、复用会话/成员服务与统一错误信封、no-store。 |
| `internal/server/usage_test.go` | 新增：路由、解析、权限、保留错误、响应 details 与字符串字段测试；数据库正确性不以 mock 代替。 |
| `internal/server/server.go` | 最小修改 Options 与路由注册；缺依赖时不注册，不能失败开放。 |
| `cmd/tokenpivot/main.go` | 最小修改 usage service/store 装配，复用现有 pool 与 workspaceService。 |
| `db/migrations/NNNNN_*.sql`、`docs/data-model.md` | **条件修改**：仅 EXPLAIN 证明需要索引时；主代理协调编号，不预设文件名或编号。 |
| T-054/T-057/T-058 文件、`go.mod`、锁文件 | 本任务不改。 |

现有文件行号见第 2 节；本节除明确标注“修改”者外均为拟新增路径。

## 8. 有序实施步骤

0. 主代理确认 T-054 的 workspace 读权限与项目身份边界已满足第 4 节。未满足时停止，不以临时接口替代；查询限额已由用户确认，无需再问。
1. 先修改 `docs/openapi/openapi.yaml`，冻结两个路径、参数、响应、质量、保留截点、十进制字符串与错误 details；核对所有样例可由 schema 表达，单事件采集 schema 不变。
2. 在 aggregation 新增纯函数 RetentionCutoff 及月末/闰日/连续溢出日/单调不减/分钟测试；在 usage service 实现严格解析、第 5.3 节边界枚举、1001 早停、建议粒度与游标。先完成纯 Go 测试，不引入通用查询框架。
3. 实现 Postgres 趋势一条语句与排名一条语句；趋势只消费预计算边界。先使用现有索引，SQL 覆盖六指标、request_count、overall、other、稳定排序及 numeric 字符串格式化。
4. 增加真实 PostgreSQL 集成测试，直接建立分钟统计测试数据，验证存储层结果；不得用 owner 权限绕过应用角色，不用 mock 宣称 SQL 正确。普通 DST 与 Lord Howe 的边界由纯 Go 测试验证，真实 PG 验证它严格按传入边界归桶。
5. 实现 server adapter 并接入 `server.go` 与 `main.go`；复用 RequireSession、MembershipRole、no-store 和统一错误。T-054 权限未就绪时不得执行本步公开接入。
6. 在代表性数据上执行 EXPLAIN；记录真实 PG 版本、数据量、查询形状与计划。只有证据支持时才增加第 6.5 节的条件迁移，并复测写入与查询。
7. 运行下列现有发布前检查，不新增脚本；规划阶段不执行集成测试：

```bash
GOTOOLCHAIN=local go test -mod=readonly -race -count=1 ./...
GOTOOLCHAIN=local go vet -mod=readonly ./...
make check
```

真实数据库验收只在已授权、无人并行使用的专用 `_test` 数据库执行：

```bash
make test-integration
```

## 9. 验收矩阵

| 类别 | 场景 | 可观察预期 |
|---|---|---|
| 认证 | 无/失效 Cookie；有效会话但非成员 | 分别 401、403；无聚合 SQL 与数据；响应 no-store。 |
| 授权 | owner、member、未知角色；每页重新检查 | 按 T-054 冻结读规则放行或拒绝；游标不能绕过权限；数据库故障不是非成员。 |
| 租户 | SQL 行含两个 workspace；筛选其他 workspace project UUID | 只统计路径获准 workspace；越权 UUID 不泄露存在性，可得到本空间空结果。 |
| 参数 | 未知参数、单值重复、空筛选、超过每维 50、非法 UUID/时区 | 400 INVALID_ARGUMENT，无数据库聚合，不回显输入值。 |
| 范围 | start≥end、未来 end、秒/微秒非零的历史边界 | 明确 400，不静默取整或截断未来。 |
| 保留月末 | 2027-03-31、2029-03-31 减 13 个日历月；平年/闰年及连续溢出日跨日 | 溢出日分别统一得到 2026-03-01T00:00Z、2028-03-01T00:00Z；有效日保留 UTC 时分并秒归零；连续测试断言 now₂≥now₁ 时 cutoff₂≥cutoff₁，不重新开放已清理范围。 |
| 保留边界 | start==cutoff、start=cutoff 前 1 分钟 | 前者允许，后者 400 OUTSIDE_RETENTION 且 details 返回精确 cutoff，无聚合 SQL。 |
| 分页保留 | 原 start 在翻页时已早于新 cutoff | 即使旧游标 as_of 合法也返回 OUTSIDE_RETENTION；不能沿旧 as_of 多查一页过期数据。 |
| 当前桶 | end 省略且包含当前分钟 | 实际 end/as_of 相同且只采样一次；当前 minute/hour/day 均 partial=true。 |
| 边缘桶 | hour/day 使用非整点但分钟对齐的范围 | 先过滤原始范围；首/尾被截断桶 partial=true；换粒度 totals 不变。 |
| 普通夏令时 | America/New_York 春季前进、秋季回拨 | 跳过小时不生成空点；重复 01:00 以不同 offset/UTC 边界区分；自然日分别为 23/25 小时。 |
| 半小时夏令时 | Australia/Lord_Howe 2026 年前进与回拨 | offset 变化产生真实非整点小时边界；可出现 30 分钟小时段，bucket_end 不固定 +1h，不错误合并不同 offset。 |
| 单一边界 | 同一请求的点数、响应边界、partial 与 SQL 聚合 | 全部来自同一预计算边界数组；PG 不另跑 date_trunc 得出不同结果。 |
| 点数 | 恰好 1000、第 1001 个桶 | 前者允许；后者在枚举 1001 时立即停，400 TOO_MANY_POINTS，不执行 SQL、不继续枚举余下范围。 |
| 超限建议 | 纯 Go 测试用可控小上限构造 hour 可满足、仅 day 可满足、day 也不满足 | 分别建议 hour、day、null；选择满足上限且最接近请求粒度的更粗粒度，null 时提示缩短范围；真实 1000 上限错误响应必须带同样的 details，不静默改粒度。 |
| 分钟趋势 | 同一 instant 不同时区表示；分钟边界 | 分钟桶相同；范围始终 [start,end)，展示 timezone 不改变 UTC 分钟归属。 |
| 空桶 | 范围内部分分钟/小时/天无行 | 不生成点、不填零；已有真实零桶仍返回。 |
| 质量 | known=0、estimated=0、全部 missing、部分 missing | 前两者 value="0" 且计数可区分；全 missing value=null；部分 missing 返回已知/估算和及 missing_count。 |
| 大整数 | value、request_count、三类计数超过 2^53-1，及超过 int64 | JSON 十进制字符串逐位准确保留，无浏览器浮点失真、无 int64 溢出拒绝；schema 不是 integer。 |
| 单事件兼容 | 既有采集请求与逐项响应 | 仍使用既有 JSON integer 与校验，本任务不改其格式。 |
| 请求数 | 全部 token 缺失事件、上报重传后的分钟数据 | event_count 仍计请求；查询本身不再次去重或翻倍；summary 不进入数据。 |
| 子集 | input=100/output=20/total=120/cache_read=60；reasoning=8 | total 仍 120，不因 cache 变 180；output 仍 20，不因 reasoning 变 28。 |
| 一致性 | 同一 aggregate 请求多桶 | totals 等于 points 各项之和，request_count 与三类计数也一致。 |
| 筛选 | provider/model/project 多选，跨维组合，unassigned+UUID | 同维 OR、跨维 AND；同时作用于 totals、points、overall、groups、other。 |
| 模型分组 | 同名 model 属于两个 provider | group_by=model 返回两个不同 `(provider,model)` 键，不合并。 |
| 排名 | total 相同、全 missing 与真实零并存 | total numeric sum 降序，稳定键决定次序；缺失 value=null 不伪装真实零，组仍保留。 |
| 占比 | 正常分母、0/0、overall 全缺失、单组全缺失 | 正常时 share 为 0–1 浮点数；无法定义时 share=null；聚合值不先转 float。 |
| 其他 | 组数 0、≤N、N+1、多页静态数据 | other 为 null 或准确聚合余组；每页无重复/无遗漏，最后 next_cursor=null；overall 始终为全部组。 |
| 游标 | 改筛选/limit/group_by/workspace、伪造格式、原 end 省略 | 非法或查询变化 400；权限与保留期重新检查；合法后续页复用原统计上界但不复用旧保留截点。 |
| 并发补报 | 分页间同范围排名被修改 | 文档不宣称快照；首屏重新请求可取得新排序；不得把游标当成数据版本。 |
| 空结果 | aggregate/rankings 无匹配行 | totals/overall request_count="0"、指标 value=null/count="0"；points/groups=[]，other/cursor=null。 |
| 故障 | 成员查询失败、SQL 失败、取消/超时 | 不返回伪空数据；安全日志无 SQL/参数；无自动重试。 |
| 查询计划 | 代表性规模四类查询 | 留存 EXPLAIN 证据；无证据不加索引，有证据才做最小迁移并复测。 |

## 10. 执行切分

**单块，不拆。** 两个端点共享参数规范化、保留截点、MetricStats/UsageStats、游标、SQL 过滤与权限路径；趋势边界又同时影响校验、SQL 和响应，拆成并行块会产生临时接口与共享文件冲突。

- 本块拥有第 7 节所有新增文件及 OpenAPI、`server.go`、`main.go` 的最小修改；aggregation 仅新增共享截点函数及包注释，不接触写入逻辑。
- 主代理协调 T-054 依赖与条件迁移编号；若无需索引，没有共享迁移、生成代码、依赖锁文件或注册表需要二次合并。
- T-013 后续复用同一 RetentionCutoff，不复制月末算法；T-057 只消费已发布接口；T-058 独立读取明细，不改本块 SQL。

## 11. 风险与回滚

- **分页期间回填**：分钟表没有版本，跨页排名可能重排。首版明确不承诺快照，静态分页仍须正确；不得用增加缓存或版本表的方式在本任务“顺手修复”。
- **时区数据差异**：边界只由 Go Location 计算，运行环境 tzdata 可用性与版本未核实。测试必须写死普通 DST 和 Lord Howe 的期望 UTC 边界；若环境缺少时区数据则停止并报告，不能退回服务器本地时区。PostgreSQL 不再独立解释展示时区，因此没有 Go/PG 两套分桶结果不一致的问题。
- **边界枚举成本**：hour 以分钟探测、day 需要细化跨日边界，必须按 1001 桶早停并用测试证明不会扫描整个超长范围；1000 点限制不代表响应时间承诺。
- **高基数与长范围**：点数上限只限制趋势桶数，不消除排名分组排序成本；并发查询与响应时间目标仍待真实环境确定。EXPLAIN 前不得宣称性能达标。
- **部分质量占比**：share 基于可展示 known/estimated sum，不等于完整账单；响应必须携带质量计数，UI 不得隐藏部分缺失。share 是浮点展示值，不能用于反推精确 token。
- **回滚**：纯只读代码可直接回滚路由与装配，分钟表及写入不受影响，无需数据补偿。共享截点纯函数不影响数据；条件索引是附加对象，代码回滚可保留，共享环境不自动 Down。

## 12. 依据

| 编号 | 事实、采用或拒绝的结论 | 来源 | 日期/核实状态 |
|---|---|---|---|
| E1 | PostgreSQL 18 三参数 `date_trunc` 能在指定时区截断 timestamptz，但若 Go 另算 bucket_end/点数就会形成两套边界。计划**拒绝**把它与本 Go 算法混用，SQL 只消费预计算边界；不断言普通或半小时 DST 下 date_trunc 不符合本项目口径。 | [PostgreSQL 18 Date/Time Functions §9.9.2](https://www.postgresql.org/docs/18/functions-datetime.html#FUNCTIONS-DATETIME-TRUNC) | 2026-10-28 抓取官方正文，已核实函数语义；本修订不再依赖它完成项目分桶。 |
| E2 | Go `LoadLocation` 按 IANA 名称加载 Location；`Time.In` 保持同一 instant、只改变展示 Location。采用 UTC instant 探测本地桶键，避免直接构造歧义本地时间。 | [Go `time.LoadLocation`](https://pkg.go.dev/time#LoadLocation)、[Go `Time.In`](https://pkg.go.dev/time#Time.In) | 2026-10-28 抓取官方正文，已核实；运行环境 tzdata 可用性与版本待核实。 |
| E3 | 统一分钟来源、[start,end)、历史分钟对齐、当前未完成桶、夏令时唯一 instant、13 日历月与精确截点、1000 点和最细建议粒度。 | `docs/architecture.md:50-65`、`docs/decisions/0003-minute-aggregation-and-retention.md:7-15`、`docs/openapi/openapi.yaml:401-407` | 仓库已确认文档，本轮已读；月末/闰日截点算法在既有文档中未定，由本计划补齐。 |
| E4 | 指标 sum/质量计数、真实零/缺失、子集关系与 event_count 请求数口径。 | `docs/data-model.md:75-88`、`internal/aggregation/minute.go:97-131` | 仓库文档与实际代码，本轮已读；未另做外部参考实现调查。 |
| E5 | PostgreSQL 18 `sum(bigint)` 返回 numeric，空集 sum 返回 NULL。采用无损 numeric 扫描、规范十进制字符串与空结果构造，不再转 int64。 | [PostgreSQL 18 Aggregate Functions](https://www.postgresql.org/docs/18/functions-aggregate.html#FUNCTIONS-AGGREGATE-TABLE) | 2026-10-28 抓取官方正文，已核实；项目大整数响应仍须测试。 |
| E6 | IANA tzdb 的 Lord Howe 规则在 2008 年后使用 `0:30` 夏令时偏移，证明自然小时不能假定固定 3600 秒。采用 offset 感知桶键与真实下一边界，并增加前进/回拨测试。 | [IANA tzdb `australasia`](https://data.iana.org/time-zones/tzdb/australasia) | 2026-10-28 抓取官方 tzdb 正文，已核实规则；项目环境的具体边界结果待纯 Go 测试。 |

## 13. 未决与非阻塞核实项

- 查询限额已全部确认：MAX_POINTS=1000、排名默认 10/最大 50；无剩余用户产品决策。
- TODO(待确认)：T-054 最终 workspace 读权限规则及项目身份语义；未满足第 4 节停止条件前不得公开接入。
- TODO(待确认)：Go 运行环境的时区数据与专用集成环境；PostgreSQL 不再独立计算展示时区边界，但实际版本仍须记录。
- TODO(待确认)：代表性数据量、并发查询、响应时间目标及是否需要查询索引；不以规划量代替 EXPLAIN。
- T-013 尚未实现清理；本计划只定义并交付共享截点函数，不能据此宣称清理已经使用同一截点。
- 60 秒速率、明细、UI、保留清理和跨请求数据版本均不在本任务内，不应作为本任务验收的隐性扩展。
