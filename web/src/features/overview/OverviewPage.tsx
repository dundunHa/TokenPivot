import { useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { useUsageOptions } from "@/features/workspace/queries";
import { RequestLogPanel } from "@/features/requests/RequestLogPanel";
import { OverviewFilterBar } from "./OverviewFilterBar";
import { OverviewRanking } from "./OverviewRanking";
import { Section } from "./OverviewSection";
import { GeekShell } from "./geek/GeekShell";
import { GeekCommandBar } from "./geek/GeekCommandBar";
import { GeekHeroMetrics } from "./geek/GeekHeroMetrics";
import { GeekTrendChart } from "./geek/GeekTrendChart";
import { GeekBrailleChart } from "./geek/GeekBrailleChart";
import { filtersToCommand } from "./geek/geek-command";
import { GeekDataStatus } from "./geek/GeekDataStatus";
import { GeekRequestLog } from "./geek/GeekRequestLog";
import {
  browserTimezone,
  DEFAULT_OVERVIEW_FILTERS,
  filterIdentity,
  floorMinute,
  parseFilters,
  resolveRange,
  toSearchParams,
  type OverviewFilters,
} from "./overviewFilters";
import { useAggregate, useProjects, type QueryScope } from "./overviewQueries";
import { buildTrend } from "./overviewView";

const TICK_MS = 10_000;

/** 当前时刻（分钟对齐）；页面隐藏时不推进，重新可见时立即更新。 */
function useMinuteNow(): number {
  const [now, setNow] = useState(() => floorMinute(Date.now()));
  useEffect(() => {
    const tick = () => {
      if (document.visibilityState === "visible") {
        setNow(floorMinute(Date.now()));
      }
    };
    const timer = setInterval(tick, TICK_MS);
    document.addEventListener("visibilitychange", tick);
    return () => {
      clearInterval(timer);
      document.removeEventListener("visibilitychange", tick);
    };
  }, []);
  return now;
}

function toggle(list: string[], value: string): string[] {
  return list.includes(value)
    ? list.filter((v) => v !== value)
    : [...list, value];
}

/** 当前空间由工作区容器决定；本页只消费明确的 userId/workspaceId，不再查询 /current。 */
export function OverviewPage({
  userId,
  workspaceId,
  workspaceName,
}: {
  userId: string;
  workspaceId: string;
  workspaceName: string;
}) {
  return (
    <OverviewBody
      scope={{ userId, workspaceId }}
      workspaceName={workspaceName}
    />
  );
}

function OverviewBody({
  scope,
  workspaceName,
}: {
  scope: QueryScope;
  workspaceName: string;
}) {
  const [params, setParams] = useSearchParams();
  const fallbackTz = useMemo(() => browserTimezone(), []);
  const parsed = useMemo(
    () => parseFilters(params, fallbackTz),
    [params, fallbackTz],
  );
  const filters = parsed.ok ? parsed.filters : null;
  const implied = parsed.ok && parsed.timezoneImplied;

  // 首次进入缺时区时，以 replace 语义把解析出的时区写回 URL。
  useEffect(() => {
    if (filters && implied) {
      setParams((prev) => toSearchParams(filters, prev), { replace: true });
    }
  }, [filters, implied, setParams]);

  function update(next: OverviewFilters) {
    setParams((prev) => toSearchParams(next, prev), { replace: true });
  }

  if (!filters) {
    return (
      <div className="flex flex-col gap-2">
        <Alert variant="destructive">
          <AlertDescription>
            链接筛选无效：{parsed.ok ? "" : parsed.reason}
          </AlertDescription>
        </Alert>
        <Button
          variant="outline"
          className="self-start"
          onClick={() =>
            setParams(
              (prev) => toSearchParams(defaults(fallbackTz), stripOwn(prev)),
              { replace: true },
            )
          }
        >
          重置筛选
        </Button>
      </div>
    );
  }
  return (
    <OverviewContent
      scope={scope}
      workspaceName={workspaceName}
      filters={filters}
      update={update}
    />
  );
}

function defaults(timezone: string): OverviewFilters {
  return { ...DEFAULT_OVERVIEW_FILTERS, timezone };
}
function stripOwn(base: URLSearchParams): URLSearchParams {
  const next = new URLSearchParams(base);
  for (const key of [
    "provider",
    "model",
    "project",
    "range",
    "granularity",
    "tz",
    "start",
    "end",
  ]) {
    next.delete(key);
  }
  return next;
}

function OverviewContent({
  scope,
  workspaceName,
  filters,
  update,
}: {
  scope: QueryScope;
  workspaceName: string;
  filters: OverviewFilters;
  update: (next: OverviewFilters) => void;
}) {
  const now = useMinuteNow();
  const range = useMemo(() => resolveRange(filters, now), [filters, now]);
  const aggregate = useAggregate(scope, filters, range);
  const options = useUsageOptions(scope.userId, scope.workspaceId);
  const projects = useProjects(scope);

  const projectNames = useMemo(
    () => new Map((projects.data ?? []).map((p) => [p.projectId, p.name])),
    [projects.data],
  );
  const projectOptions = [
    ...(projects.data ?? []).map((p) => ({
      value: p.projectId,
      label: p.archived ? `${p.name}（已归档）` : p.name,
    })),
    { value: "unassigned", label: "未归属" },
  ];
  const asOption = (v: string) => ({ value: v, label: v });

  const trend = useMemo(
    () => (aggregate.data ? buildTrend(aggregate.data) : null),
    [aggregate.data],
  );
  const tz = filters.timezone;
  const identity = filterIdentity(filters);

  const data = aggregate.data;
  const [showBraille, setShowBraille] = useState(false);

  return (
    <GeekShell
      workspaceName={workspaceName}
      filterSummary={filtersToCommand(filters)}
      rangeSummary={`${filters.range} · ${filters.granularity}`}
    >
      {/* 命令输入条 */}
      <GeekCommandBar
        filters={filters}
        onApply={(updates) => update({ ...filters, ...updates })}
      />

      {/* 可视化筛选栏（保留为可折叠入口） */}
      <details className="geek-filter-details">
        <summary>可视化筛选</summary>
        <div className="geek-filter-body">
          <OverviewFilterBar
            filters={filters}
            providerOptions={(options.data?.providers ?? []).map(asOption)}
            modelOptions={(options.data?.models ?? []).map(asOption)}
            projectOptions={projectOptions}
            optionsError={
              options.isError
                ? "筛选选项加载失败，暂时无法编辑供应商/模型。"
                : null
            }
            projectsError={
              projects.isError ? "项目列表加载失败，暂时无法编辑项目。" : null
            }
            onChange={update}
          />
        </div>
      </details>

      {/* 第一行：总量 hero | 数据状态 */}
      {data ? (
        <div className="geek-split">
          <GeekHeroMetrics totals={data.totals} rate60={null} />
          <GeekDataStatus
            workspaceName={workspaceName}
            timezone={tz}
            lastReceivedAt={null}
            sourceCount={null}
            rangeStart={data.range.start}
            rangeEnd={data.range.end}
            asOf={data.range.asOf}
            retentionCutoff={data.range.retentionCutoff}
            pageUpdatedAt={new Date(aggregate.dataUpdatedAt).toISOString()}
          />
        </div>
      ) : null}

      {/* 第二行：趋势 | 排名 */}
      <div className="geek-split">
        <div className="geek-card">
          <div className="geek-card-head">
            <h2 className="geek-card-title">趋势</h2>
            <div className="geek-segmented">
              <button
                type="button"
                className={`geek-ranking-tab ${!showBraille ? "geek-tab-active" : ""}`}
                aria-pressed={!showBraille}
                onClick={() => setShowBraille(false)}
              >
                面积图
              </button>
              <button
                type="button"
                className={`geek-ranking-tab ${showBraille ? "geek-tab-active" : ""}`}
                aria-pressed={showBraille}
                onClick={() => setShowBraille(true)}
              >
                braille
              </button>
            </div>
          </div>
          <Section title="趋势" query={aggregate} timezone={tz}>
            {data && trend ? (
              trend.points.length === 0 ? (
                <p className="geek-empty geek-dim">
                  {data.totals.requestCount === "0"
                    ? "所选范围与筛选下没有用量。"
                    : "没有可展示的趋势点。"}
                </p>
              ) : showBraille ? (
                <GeekBrailleChart trend={trend} />
              ) : (
                <GeekTrendChart trend={trend} timezone={tz} />
              )
            ) : null}
          </Section>
        </div>

        {/* 排名 — 复用现有 OverviewRanking 保持查询/交互逻辑 */}
        <OverviewRanking
          identity={identity}
          scope={scope}
          filters={filters}
          range={range}
          projectNames={projectNames}
          onToggle={(dim, val) => {
            if (dim === "provider") {
              update({
                ...filters,
                providers: toggle(filters.providers, val),
              });
            } else if (dim === "model") {
              update({ ...filters, models: toggle(filters.models, val) });
            } else if (dim === "project") {
              update({
                ...filters,
                projects: toggle(filters.projects, val),
              });
            }
          }}
        />
      </div>

      {/* 请求明细：整行宽度，容纳 9 列表格 */}
      <GeekRequestLog>
        <RequestLogPanel scope={scope} filters={filters} />
      </GeekRequestLog>
    </GeekShell>
  );
}
