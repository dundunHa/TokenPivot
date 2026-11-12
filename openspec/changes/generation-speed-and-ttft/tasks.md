# Tasks

执行依赖：F 与 B 可并行；C 依赖 B；D 依赖 F+B；E 依赖 C，可与 D 并行；V 依赖全部。F 的宿主写入必须先由主代理取得显式 `/root/work/deepseek-harness` 跨仓执行上下文，本 change 自身不授予该权限。

## 1. F：DSH 宿主公开最终请求计时

- [ ] 1.1 在宿主 `packages/core/agent/src/runtime-types.ts` 增加 `GenerationTiming`、`GenerationTimingNotice` 与 `agent/generation-timing` 声明，在 `packages/core/agent-loop/src/assistant-stream.ts`、`agent.ts` 按 attempt 采集首末有效 delta，并仅在最终 committed `assistant/message` 后按 Agent+seq 发通知；新增 `packages/core/agent-loop/tests/generation-timing.spec.ts`，覆盖标准边界、reasoning/tool-call 的 name-only/arguments-only/二者皆空、usage/finish 排除、0 ms、回拨、失败重试、已有输出和 usage 后取消时不发布计时、多 Agent 相同局部 attemptId、Session 事件先于通知及 Session 持久化形状不变，运行 `pnpm exec vitest run packages/core/agent-loop/tests/generation-timing.spec.ts` 通过。
- [ ] 1.2 在宿主 `packages/compaction/compaction-basic/src/summarizer.ts` 为每次直接 LLM 调用独立采集同一 `GenerationTiming`，在 `region.ts` 将成功 summary 的 seq 通过同一事件同步公开；新增 `packages/compaction/compaction-basic/tests/generation-timing.spec.ts`，覆盖普通 compaction、recover 后仅最终成功调用、无计时 SummaryResult、事件顺序和 summary seq 关联，运行 `pnpm exec vitest run packages/compaction/compaction-basic/tests/generation-timing.spec.ts` 通过。
- [ ] 1.3 在宿主根目录运行 `pnpm run typecheck` 与 `pnpm exec vitest run packages/core/agent-loop packages/compaction/compaction-basic`，确认公开类型、既有普通流和 compaction 回归全部通过，且未新增依赖或修改 Session 持久化文件。

## 2. B：TokenPivot 可空存储与 v1/v2 采集基础

- [ ] 2.1 新增 `db/migrations/00008_generation_metrics.sql`，为 `usage_events` 增加可空无默认值的 `generation_duration_ms`、`ttft_ms` bigint 列及注释，不加索引/CHECK/FK/RLS、不回填；同步 `docs/data-model.md`，在 `internal/ingestion/postgres_integration_test.go` 验证迁移后新行可写、升级前旧行两列为 NULL、既有授权和分钟聚合不变，并在专用 PostgreSQL 测试环境运行 `GOTOOLCHAIN=local go test -mod=readonly -tags=integration -count=1 ./internal/ingestion` 通过。
- [ ] 2.2 在 `internal/ingestion/service.go` 与 `internal/ingestion/postgres.go` 接受两个可缺省、可 null 的非负安全整数，按属性是否出现选择 v1 七元素或 v2 九元素规范化文本并保存原始字段；扩充 `internal/ingestion/handler_test.go`，验证缺省/null/0/合法值、负数/超安全整数/未知字段、v2 字段顺序和标准摘要向量，运行 `GOTOOLCHAIN=local go test -mod=readonly -count=1 ./internal/ingestion` 通过。
- [ ] 2.3 扩大 ExistingEvent 的留存列读取并实现已知 v1/v2 双向共同 v1 投影，未知版本仍返回 `NORMALIZATION_INCOMPATIBLE`；在 handler 与 PostgreSQL 集成测试中覆盖旧 v1 行→v2 重传、v2 行→旧 v1 重传均为 duplicate、原七字段变化为 conflict、同 v2 原始计时变化为 conflict、首行不被更新，运行 2.1 与 2.2 的两条 Go 测试命令通过。
- [ ] 2.4 在 `docs/openapi/openapi.yaml` 一次性写明 ingestion 新字段、服务端按形状选择 v1/v2、九元素数组、跨版本样例，以及 design 规定的 RequestDetail 和 usage-rate 新增可选响应字段与十进制字符串示例；冻结后续 C、E 块使用的响应字段与类型（此处不等待 C 实现）；在 V 块核对全部实现与文档，并运行 `GOTOOLCHAIN=local go test -mod=readonly -count=1 ./internal/ingestion ./internal/requestdetails` 确认现有接口行为无回归。

## 3. C：请求明细与滚动窗口读取

- [ ] 3.1 在 `internal/requestdetails/postgres.go`、`service.go` 增加每请求 `generation_speed_tps` 与 `ttft_ms`，仅用 known outputTokens、N>1、正 decode 时长派生 numeric 速度并舍入 6 位；在 `service_test.go` 和 `postgres_integration_test.go` 验证 100 tokens、TTFT 8000 ms、decode 2000 ms 返回 49.5 tokens/s，及单 chunk 0 decode、N=1、estimated/missing output、历史 NULL、部分 TTFT 有效均不产生伪速度，运行 `GOTOOLCHAIN=local go test -mod=readonly -count=1 ./internal/requestdetails` 和专用 PostgreSQL 下 `GOTOOLCHAIN=local go test -mod=readonly -tags=integration -count=1 ./internal/requestdetails` 通过。
- [ ] 3.2 扩展同一 `usage-rate` 查询，新增 `generation_speed` 与 `ttft` 对象及独立 `sample_count`，对未舍入每请求速度求算术平均后再舍入；保留旧 tokens/min 响应字段，在集成测试中覆盖 A 仅 TTFT、B 两项有效、C 均缺失、无事件/无有效样本、精确 `[as_of-60s,as_of)` 边界、窗口内补报与窗口外补报、provider/model/project 筛选，至少增加两个速度、token 数和时长均不同的有效请求，以区分算术平均与加权平均；确认不按 token/时长加权且 received_at 不替代 occurred_at，运行 3.1 两条命令通过。

## 4. D：DSH 插件关联与可空上报

- [ ] 4.1 在 `plugins/dsh/lib/mapper.js` 增加原始计时到 `generation_duration_ms`、`ttft_ms` 的纯映射：负值/非安全整数/边界缺失为 null，0 保留，两项均缺失时省略字段，至少一项有效时两个键都出现且不上传速度；无有效 usage 但有有效计时时，仍生成事件并将未知用量标为 missing；扩充 `plugins/dsh/test/mapper.test.js` 覆盖标准 2000/8000 ms、单 chunk、回拨、缺边界、旧事件形状及普通/compaction 无 usage 保留 TTFT，运行 `make test-plugin-dsh` 通过。
- [ ] 4.2 在 `plugins/dsh/index.js` 注册全局 `agent/generation-timing` 单 payload 监听器并转交 collector；在 `plugins/dsh/lib/collector.js` 按实际 Session 对象+event.seq 暂存最终事件，核对通知 agent.session 的对象身份，用一次 `queueMicrotask` 收口，旧宿主无通知时按原形状上报、迟到通知不附着；新增 `plugins/dsh/test/collector.test.js` 并补充 `upload.test.js`，覆盖普通/compaction、重试仅最终 seq、多 Agent 并发、通知先后约定、旧宿主、迟到通知、同字符串 session ID 的不同对象互不串接、普通/compaction 无 usage 的 mapper→上传→TTFT 样本链路、v1 缺省与 v2 null/有值实际上传，运行 `make test-plugin-dsh` 通过。

## 5. E：请求日志前端展示

- [ ] 5.1 在 `web/src/features/requests/api.ts` 增加新明细和滚动汇总类型、非负十进制字符串解析，以及旧后端省略新字段时 null+样本数 0 的兼容；新增 `web/src/features/requests/api.test.ts` 并更新 `model.test.ts`，验证合法值、null、缺省、畸形响应和 0 样本不变成数值 0，运行 `npm --prefix web test` 通过。
- [ ] 5.2 在 `model.ts` 仅透传后端口径，在 `RequestLogPanel.tsx` 增加每请求速度/TTFT 两列和顶部近 60 秒两项平均值及各自样本数，替换旧 tokens/min 文案并标明客户端 chunk 估算；更新 `request-panel.test.tsx` 验证 49.5 tokens/s、8000 ms、未知状态、独立样本数和说明文字，运行 `npm --prefix web test` 与 `npm --prefix web run build` 通过，不做 UI 自动化验收。

## 6. V：跨仓与发布前验证

- [ ] 6.1 在宿主根目录重跑 `pnpm run typecheck` 和 `pnpm exec vitest run packages/core/agent-loop packages/compaction/compaction-basic`，在 TokenPivot 根目录运行 `make check`、`make test-plugin-dsh`，确认普通/compaction、重试、多 Agent、旧宿主、解析、构建及全部普通回归通过。
- [ ] 6.2 在既有专用 PostgreSQL 测试环境运行 `make test-integration`，核对端到端验收矩阵：标准 49.5 tokens/s 与 8000 ms、单 chunk/无用量/N=1/estimated、回拨、compaction、历史缺失、v1/v2 双向重传、窗口内/外补报、独立平均样本数；在此逐字段核对 OpenAPI 与 B/C/E 已完成实现一致，避免 B 反向等待 C。任何失败退回所属 F/B/C/D/E 块修复后重跑该块及本组检查。
- [ ] 6.3 按 design 的 Migration Plan 做一次发布前顺序核对：00008→后端/宿主→插件→前端，确认新插件不会先于支持 v2 的后端接收正式流量，回滚先处理含新字段的队列且正常保留新列；将核对结果逐项映射到已通过的混合版本测试，未覆盖项补齐对应自动化测试后再交付。
