import { useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  useCurrentWorkspace,
  useUsageOptions,
} from "@/features/workspace/queries";
import { RequestLogPanel } from "@/features/requests/RequestLogPanel";
import { OverviewFilterBar } from "./OverviewFilterBar";
import { OverviewRanking } from "./OverviewRanking";
import { Section } from "./OverviewSection";
import { describeError, type RankingDimension } from "./overviewApi";
import { GeekShell } from "./geek/GeekShell";
import { GeekCommandBar } from "./geek/GeekCommandBar";
import { GeekHeroMetrics } from "./geek/GeekHeroMetrics";
import { GeekTrendChart } from "./geek/GeekTrendChart";
import { GeekBrailleChart } from "./geek/GeekBrailleChart";

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
import { buildTrend, formatInstant } from "./overviewView";

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

export function OverviewPage({ userId }: { userId: string }) {
  const workspace = useCurrentWorkspace(userId);
  if (workspace.isPending) {
    return (
      <p role="status" className="text-muted-foreground text-sm">
        正在加载空间…
      </p>
    );
  }
  if (workspace.isError) {
    const { kind, message } = describeError(workspace.error);
    return (
      <div className="flex flex-col gap-2">
        <Alert variant="destructive">
          <AlertDescription>{message}</AlertDescription>
        </Alert>
        {kind === "forbidden" ? null : (
          <Button
            variant="outline"
            className="self-start"
            onClick={() => void workspace.refetch()}
          >
            重试
          </Button>
        )}
      </div>
    );
  }
  return (
    <OverviewBody
      scope={{ userId, workspaceId: workspace.data.workspaceId }}
      workspaceName={workspace.data.name}
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

  const onRankingToggle = (dimension: RankingDimension, value: string) => {
    if (dimension === "project") {
      update({ ...filters, projects: toggle(filters.projects, value) });
    } else if (dimension === "provider") {
      update({ ...filters, providers: toggle(filters.providers, value) });
    } else {
      update({ ...filters, models: toggle(filters.models, value) });
    }
  };

  const data = aggregate.data;
  const [showBraille, setShowBraille] = useState(false);

  // 排名数据由 OverviewRanking 内部 query 提供
  const rankingItems = useMemo(() => {
    if (!data) return [];
    return [];
  }, [data]);

  return (
    <GeekShell workspaceName={workspaceName} filterSummary={identity}>
      {/* 命令输入条 */}
      <GeekCommandBar
        filters={filters}
        onApply={(updates) => update({ ...filters, ...updates })}
      />

      {/* 可视化筛选栏（保留为可折叠入口） */}
      <details className="geek-filter-details">
        <summary className="geek-dim" style={{ cursor: "pointer", fontSize: 12 }}>
          ▾ 可视化筛选
        </summary>
        <OverviewFilterBar
          filters={filters}
          providerOptions={(options.data?.providers ?? []).map(asOption)}
          modelOptions={(options.data?.models ?? []).map(asOption)}
          projectOptions={projectOptions}
          optionsError={
            options.isError ? "筛选选项加载失败，暂时无法编辑供应商/模型。" : null
          }
          projectsError={
            projects.isError ? "项目列表加载失败，暂时无法编辑项目。" : null
          }
          onChange={update}
        />
      </details>

      {/* 60/40 双栏 */}
      <div className="geek-workbench">
        {/* 左栏 */}
        <div className="flex flex-col gap-4">
          {/* Hero 指标 */}
          {data ? (
            <GeekHeroMetrics
              totals={data.totals}
              rate60={null}
              timezone={tz}
            />
          ) : null}

          {/* 趋势图 */}
          <Section title="趋势" query={aggregate} timezone={tz}>
            <div className="geek-trend-toggle">
              <button
                className={`geek-ranking-tab ${!showBraille ? "geek-tab-active" : ""}`}
                onClick={() => setShowBraille(false)}
              >
                面积图
              </button>
              <button
                className={`geek-ranking-tab ${showBraille ? "geek-tab-active" : ""}`}
                onClick={() => setShowBraille(true)}
              >
                braille
              </button>
            </div>
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

          {/* 排名 — 复用现有 OverviewRanking 保持查询/交互逻辑 */}
          <div className="geek-ranking-wrapper">
            <OverviewRanking
              identity={identity}
              scope={scope}
              filters={filters}
              range={range}
              projectNames={projectNames}
              onToggle={(dim, val) => {
                if (dim === "provider") {
                  update({ ...filters, providers: toggle(filters.providers, val) });
                } else if (dim === "model") {
                  update({ ...filters, models: toggle(filters.models, val) });
                } else if (dim === "project") {
                  update({ ...filters, projects: toggle(filters.projects, val) });
                }
              }}
            />
          </div>
        </div>

        {/* 右栏 */}
        <div className="flex flex-col gap-4">
          {/* 数据状态 */}
          {data ? (
            <GeekDataStatus
              workspaceName={workspaceName}
              timezone={tz}
              lastReceivedAt={null}
              sourceCount={0}
              rangeStart={data.range.start}
              rangeEnd={data.range.end}
              asOf={data.range.asOf}
              retentionCutoff={data.range.retentionCutoff}
              pageUpdatedAt={new Date(aggregate.dataUpdatedAt).toISOString()}
            />
          ) : null}

          {/* 请求日志 */}
          <GeekRequestLog>
            <RequestLogPanel scope={scope} filters={filters} />
          </GeekRequestLog>
        </div>
      </div>
    </GeekShell>
  );
}
