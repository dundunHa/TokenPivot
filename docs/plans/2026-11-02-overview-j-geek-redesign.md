# T-062：总览页视觉重做——J 极客重奏 F++ 方案落地 web/

> 状态：**待批准**。
> 日期：2026-11-02。仅本文件属于本次规划交付；提交、TODO 更新与实施授权由主代理负责。
> 视觉原型：[docs/prototypes/dashboard-j.html](../prototypes/dashboard-j.html)（已按 PRD 口径对齐）。
> TODO：T-062（看板「前端」分组，P1）。

---

## 1. 目标、范围与非目标

### 1.1 目标

把已确认的 **J 极客重奏 F++** 原型（琥珀暖黑 + 等宽字体 + braille 光栅化 + 60/40 双栏 + tmux 状态行 + 命令过滤）落地到 `web/src/features/overview/`，替换现有浅色卡片视觉，同时：

- 保留 T-057 已交付的全部查询、筛选、URL 状态、错误处理与数据口径。
- 对齐 PRD §7 统计口径：`total = input + output`，cache 为 input 子集不额外加到 total。
- 补齐 PRD §5.2 顶部 4 指标、§5.2(5) 数据状态、§6 质量标记、§5.2(3) 长尾「其他」、§5.2 点击排名筛选、§7 未完成时段标记。
- 桌面优先响应式重排；遵循 `prefers-reduced-motion`；状态不只靠颜色。

### 1.2 非目标与任务边界

- **不改后端接口、不改数据库、不改认证**：本任务只动前端视觉层与组件结构。现有 `overviewQueries` / `overviewApi` / `overviewFilters` / `useRequestLogs` 的数据契约保持不变。
- **不新增 API**：不引入 latency/cost/budget 相关查询；不为未核实维度新增字段。
- **不实现时延 p50/p90/p99、日成本、预算卡片**（PRD §9 首版不做）。原型里有的保留在原型里，正式前端不实现这三个卡片。
- **不引入新 chart 库**：braille 光栅化用纯函数（参考原型 `brailleRender`），字符直方图用等宽字符块，趋势面积图继续用已有 Recharts。
- **不做多空间切换、不做 CSV 导出、不做成员管理界面**。
- **不重建 T-056 已交付的认证壳、QueryClientProvider、React Router**。
- **T-042**（响应式与动效可访问性）的完整验收不在本任务范围，但本任务保证基本键盘可用、文本状态、不依赖颜色的含义、`prefers-reduced-motion` 基础适配。

---

## 2. 当前 HEAD 已核实事实

| 已核实现状 | 证据 | 结论 |
|---|---|---|
| `OverviewPage` 已串联 FilterBar/Metrics/TrendChart/Ranking/RequestLogPanel | [OverviewPage.tsx:220-278](../../web/src/features/overview/OverviewPage.tsx#L220-L278) | 组件骨架保留，重做视觉层与布局 |
| `OverviewMetrics` 已显示总/输入/输出/请求数 | `OverviewMetrics.tsx`（T-057 交付） | 保留数据消费，改为 J 风格 4 格指标 rail + 大数 hero |
| `OverviewTrendChart` 用 Recharts 面积图 | `OverviewTrendChart.tsx`（T-057 交付） | 保留 Recharts 面积图，配色改琥珀系；braille 作为可选展示模式 |
| `OverviewRanking` 已支持 tab 切换与点击筛选 | `OverviewRanking.tsx`（T-057 交付） | 保留交互，改字符条视觉 + 长尾「其他」行 |
| `RequestLogPanel` 已有日志流 | `RequestLogPanel.tsx`（T-058 交付） | 保留数据，改为终端日志风格 + 质量标记列 |
| `overviewFilters` 已解析 URL 状态 | `overviewFilters.ts`（T-057 交付） | 不改；新增命令输入解析为独立模块 |
| `index.css` 已有语义 token | `web/src/index.css` | 新增 J 专属 token 变量，不改已有变量 |
| `App.tsx` 路由已挂 `OverviewPage` | `web/src/App.tsx` | 不改路由 |

---

## 3. 设计约束（checklist）

### 3.1 视觉（PRD §8）

- [ ] 背景 `#0b0a08`（J 暖黑，比 F 的 `#0a0a0b` 更暖），面板 `#0e0e10`，主文字 `#f4f1ea`，琥珀强调 `#e0a64b`
- [ ] 等宽字体贯穿（`ui-monospace, 'Cascadia Code', 'Fira Code', monospace`）
- [ ] 数字用等宽数字排版（`font-variant-numeric: tabular-nums`）
- [ ] 细线分隔（`1px solid #1e1c18`），不堆叠发光卡片
- [ ] 字符 sparkline、字符条、字符直方图、终端日志流
- [ ] 命令行式顶栏 + 细分隔线；**不要横向滚动事件条**
- [ ] tmux 风格状态行（琥珀底黑字，显示当前空间/筛选/键位提示）

### 3.2 交互

- [ ] 时间窗切换可点（分/时/天）
- [ ] 命令输入条：`provider=` / `model=` / `project=` 过滤，Enter 应用，Esc 清空
- [ ] 键盘：`/` 聚焦命令、`j/k` 滚动、`1/2/3` 切窗口
- [ ] 点击排名项直接筛选，再点取消，active 态高亮
- [ ] hover 读数（趋势图 tooltip）
- [ ] 数字更新动效（count-up）
- [ ] `prefers-reduced-motion` 时关闭 count-up 与闪烁光标

### 3.3 统计口径（PRD §7）

- [ ] `total = input + output`，cache 不额外加到 total
- [ ] 顶部 4 指标：输入 Token / 输出 Token / 请求数 / 最近 60 秒速率
- [ ] 显示工作空间+时区、最后接收时间、覆盖来源
- [ ] 用量质量标记：known/estimated/missing，不只用颜色区分（✓/≈/— 符号）
- [ ] 长尾归「其他」
- [ ] 当前未完成时段 dim 标记
- [ ] 补报按发生时刻回填，不算接收时刻新消耗

---

## 4. 技术选型

| 决策 | 选择 | 理由 |
|---|---|---|
| 框架 | React 19 + TypeScript（现有） | 不重建 |
| 样式 | Tailwind CSS 4 + CSS 变量（现有） | 新增 J token 到 `index.css` |
| 组件 | shadcn/ui（现有） | 复用 Button/Alert/Badge 等 |
| 趋势图 | Recharts（现有） | 面积图保留，配色改琥珀系 |
| braille 光栅化 | 纯函数 `brailleRender(lines, cols, rows)` | 参考原型实现，零依赖 |
| 字符直方图 | 等宽字符块 `█`/`░` | 零依赖，终端气质 |
| 状态管理 | React Query（现有） | 不新增全局状态层 |
| 测试 | Vitest + RTL + MSW（现有） | 组件测试 + 视觉快照 |

---

## 5. 组件切分

### 5.1 新增文件

```text
web/src/features/overview/
  geek/                          # J 风格专属组件
    GeekShell.tsx                # 命令行顶栏 + 整体布局壳
    GeekStatusBar.tsx            # tmux 状态行
    GeekCommandBar.tsx           # 命令输入条（provider=/model=/project=）
    GeekHeroMetrics.tsx          # 大数 hero + 4 格指标 rail
    GeekTrendChart.tsx           # Recharts 面积图（琥珀配色）
    GeekBrailleChart.tsx         # braille 光栅化趋势（可选展示模式）
    GeekRanking.tsx              # 字符条排名 + 长尾「其他」
    GeekRequestLog.tsx           # 终端日志风格请求流
    GeekDataStatus.tsx           # 工作空间+时区+最后接收+覆盖来源
    braille.ts                   # brailleRender 纯函数
    geek-tokens.css              # J 专属 CSS 变量
    geek-command.ts              # 命令解析（provider=/model=/project=）
    geek-utils.ts                # 格式化工具（fmt/fmtMs/fmtPct）
```

### 5.2 修改文件

```text
web/src/features/overview/OverviewPage.tsx    # 引入 GeekShell，替换布局
web/src/index.css                              # 导入 geek-tokens.css
```

### 5.3 不改文件

- `overviewApi.ts` / `overviewFilters.ts` / `overviewQueries.ts` / `overviewView.ts`（数据层不变）
- `OverviewFilterBar.tsx` / `OverviewMetrics.tsx` / `OverviewTrendChart.tsx` / `OverviewRanking.tsx`（保留为旧版，新组件并行存在，由 OverviewPage 切换）
- `web/src/features/requests/`（RequestLogPanel 保留，GeekRequestLog 是新包装）

---

## 6. 数据流

```text
OverviewPage (现有)
  └── GeekShell (新)
        ├── GeekCommandBar (新) ──→ geek-command.ts 解析 ──→ 更新 URL filters
        ├── GeekHeroMetrics (新) ──→ 消费 aggregate.data.totals
        ├── GeekDataStatus (新) ──→ 消费 workspace.name + aggregate.data.range
        ├── GeekTrendChart (新) ──→ 消费 trend（buildTrend 已有）
        ├── GeekBrailleChart (新) ──→ 消费 trend.points → braille.ts
        ├── GeekRanking (新) ──→ 消费 OverviewRanking 的数据与 onToggle
        ├── GeekRequestLog (新) ──→ 包装 RequestLogPanel + 质量标记列
        └── GeekStatusBar (新) ──→ 显示空间/筛选/键位提示
```

现有 `useAggregate` / `useUsageOptions` / `useProjects` / `useRequestLogs` 全部保留，新组件直接消费。

---

## 7. 响应式策略

| 断点 | 布局 | 变化 |
|---|---|---|
| ≥1280px | 60/40 双栏 | 左：hero+trend+ranking；右：dataStatus+requestLog |
| 768–1280px | 单栏堆叠 | 趋势图保持视觉中心；请求流折叠为可展开面板 |
| <768px | 单栏堆叠 | tmux 状态行折叠；命令输入条简化；字符图降级为简化面积图 |

---

## 8. 验收标准

| 标准 | 验证方式 |
|---|---|
| 视觉对齐 J 原型 | 截图对比：琥珀暖黑配色、等宽字体、字符图、tmux 状态行 |
| 统计口径正确 | 单元测试：`total = input + output`，cache 不重复累加 |
| 顶部 4 指标显示 | 组件测试：INPUT/OUTPUT/REQUESTS/RATE 60S 四格存在 |
| 质量标记 | 组件测试：known/estimated/missing 符号渲染 |
| 长尾「其他」 | 组件测试：排名列表包含「其他」行 |
| 点击筛选 | 组件测试：点击排名项更新 URL filters |
| 命令解析 | 单元测试：`provider=openai` 正确解析为 filters |
| 键盘导航 | 组件测试：`/` 聚焦、`j/k` 滚动、`1/2/3` 切换 |
| `prefers-reduced-motion` | 组件测试：count-up 禁用 |
| 类型检查 | `pnpm tsc --noEmit` 通过 |
| 构建 | `pnpm build` 通过 |
| 现有测试 | `pnpm test` 全部通过（不破坏 T-057 测试） |

---

## 9. 执行切分

按文件不相交切 3 个 workstream：

### WS-1：共享基础（设计 token + 工具函数 + 类型）

**文件**：
- `web/src/features/overview/geek/geek-tokens.css`
- `web/src/features/overview/geek/geek-utils.ts`
- `web/src/features/overview/geek/braille.ts`
- `web/src/features/overview/geek/geek-command.ts`
- `web/src/features/overview/geek/braille.test.ts`
- `web/src/features/overview/geek/geek-command.test.ts`

**验收**：braille 纯函数测试通过；命令解析测试通过；CSS 变量正确定义。

### WS-2：左栏组件（hero + trend + ranking）

**文件**：
- `web/src/features/overview/geek/GeekHeroMetrics.tsx`
- `web/src/features/overview/geek/GeekTrendChart.tsx`
- `web/src/features/overview/geek/GeekBrailleChart.tsx`
- `web/src/features/overview/geek/GeekRanking.tsx`
- 对应测试文件

**依赖**：WS-1 已合入（消费 geek-utils/braille）。

**验收**：组件渲染测试通过；视觉截图对齐原型左栏。

### WS-3：右栏组件 + 壳 + 状态行

**文件**：
- `web/src/features/overview/geek/GeekShell.tsx`
- `web/src/features/overview/geek/GeekStatusBar.tsx`
- `web/src/features/overview/geek/GeekCommandBar.tsx`
- `web/src/features/overview/geek/GeekDataStatus.tsx`
- `web/src/features/overview/geek/GeekRequestLog.tsx`
- 对应测试文件
- `web/src/features/overview/OverviewPage.tsx`（引入 GeekShell）
- `web/src/index.css`（导入 geek-tokens.css）

**依赖**：WS-1 已合入（消费 geek-command/geek-utils）。

**验收**：整体页面截图对齐原型；键盘导航测试通过；现有测试全部通过。

### 串行顺序

```
WS-1（共享基础）→ 合入 → WS-2 + WS-3 并行 → 合入 → 联调 → review
```

---

## 10. 风险与开放问题

| 风险 | 缓解 |
|---|---|
| braille 光栅化性能（大量点阵计算） | 限制 cols/rows 上限；`useMemo` 缓存；`prefers-reduced-motion` 时降级为简化面积图 |
| 字符直方图在小屏可读性 | <768px 降级为 Recharts 面积图 |
| 命令输入与现有筛选栏冲突 | 命令输入作为快捷方式，更新同一 URL filters；筛选栏保留为可视化编辑入口 |
| 现有 T-057 测试可能因布局改动失败 | 新组件并行存在，旧组件保留；OverviewPage 切换时确保测试选择器兼容 |

---

## 11. 不做的事

- 多空间切换界面
- 新后端接口
- 时延 p50/p90/p99 卡片
- 日成本卡片
- 预算卡片
- CSV 导出
- 成员管理界面
- 模型别名合并
- 趋势框选联动明细
- 顶部横向滚动事件条
- 1920×1080 固定画布等比缩放

---

## 12. 前置条件

- [ ] 用户批准本 plan
- [ ] T-062 状态更新为 planned
- [ ] 主代理按 F2 提交本 plan 文件
- [ ] 主代理按 D6 创建 worktree
