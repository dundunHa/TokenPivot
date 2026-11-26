# T-071：概览生成速度趋势采用首末有效输出计时

## 范围

- 仅改概览「生成速度趋势」的总体与分模型分钟点及其说明。顶部速度卡、分模型当前值、请求明细与滚动 60 秒均值、Token 用量、采集、数据库结构、权限与路由版本保持原样。
- 分钟点沿用现有 UTC 自然分钟、`occurred_at`、左闭右开窗口、筛选和查询频率。显示时区和跨空档连线保持原样；缺样本的分钟仍是空值，不画成 0，不按请求总耗时补算。

## 实施方案

沿用 `/api/v1/workspaces/{workspaceID}/usage-speed`。同一查询复用请求明细的 `speedValid`、`speedExpr`：已知输出 token 数大于 1、生成时长为正时，单请求速度为 `(输出 token 数 − 1) × 1000 ÷ generation_duration_ms`。时长是首个有效输出到最后一个有效输出的时间差，不含最后输出后等待完成信号的时间。按 UTC 分钟对未舍入的每请求速度求算术平均，最后舍入六位。只返回有有效样本的分钟。

`points` 每项为 `bucket_start`、非空十进制字符串 `generation_speed_tps`、正整数 `sample_count`。不兼容旧点格式、不增加路由。`current` 的计算、覆盖计数及响应格式保持原样。前端只解析并显示服务端分钟均值。同步 OpenAPI 和 PRD；前后端从同一固定版本部署。

## 执行切分

单块，不拆。唯一实施代理使用隔离分支，修改以下范围：

- `internal/requestdetails/postgres.go`、`internal/requestdetails/service.go`：查询与分钟点的响应映射。
- `internal/requestdetails/postgres_integration_test.go`：真实 PostgreSQL 行为回归。
- `web/src/features/requests/api.ts`、`web/src/features/requests/speedSeries.ts`、`web/src/features/requests/SpeedTrend.tsx`：解析、图表值和文案。
- `web/src/features/requests/api.test.ts`、`web/src/features/requests/speedSeries.test.ts`、`web/src/features/overview/overview-panels.test.tsx`：解析与图表回归。
- `docs/openapi/openapi.yaml`、`docs/prd.md`：更新分钟点定义、示例及趋势口径，不改现有滚动速度定义。

## 验收

1. 同一分钟两个请求分别为 101 token / 2 秒和 11 token / 1 秒，速度 50、10，点值为 `30.000000`，样本数为 2。将一个请求的总耗时设为 100 秒仍得 30，能区分旧公式和生成时长加权平均。另用现有 `0.9999996`、`1.0000011` 样本断言先平均再舍入为 `1.000000`，不是 `1.000001`。
2. 缺计时、生成时间为零、输出少于两个 token、估算或缺失输出不进趋势；没有有效样本的分钟留空，整条无样本显示空状态。不从 `duration_ms` 推算。
3. UTC 桶、窗口边界、补报按 `occurred_at`、模型/供应商/项目筛选正确；同一组有效请求与请求明细滚动均值一致。旧 `current` 值及覆盖计数回归保持不变；前端继续显示原顶部卡和分模型列表。
4. 运行仓库的 `make check` 和 `make test-integration`，集成测试仅使用同一专用回环 `_test` 数据库的两种角色。前端测试覆盖解析错误、分钟空值和概览联动。只读评审核对最终差异。
5. 合入主分支后从干净固定版本部署到已确认的当前 TokenPivot 环境；记录执行 ID、运行版本和健康结果，核对部署后的趋势页面与受保护接口，不把没有真实样本当零。部署不改变采集配置或其他环境。
