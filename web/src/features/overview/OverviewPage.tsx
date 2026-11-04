import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useSearchParams } from "react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { useUsageOptions } from "@/features/workspace/queries";
import { OverviewFilterBar } from "./OverviewFilterBar";
import { OverviewRanking } from "./OverviewRanking";
import { Section } from "./OverviewSection";
import { GeekKpis } from "./geek/GeekKpis";
import { GeekTrend } from "./geek/GeekTrend";
import { RateKpi } from "@/features/requests/RateKpi";
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
import {
  useAggregate,
  usePreviousTotals,
  useProjects,
  type QueryScope,
} from "./overviewQueries";
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
}: {
  userId: string;
  workspaceId: string;
}) {
  return <OverviewBody scope={{ userId, workspaceId }} />;
}

/**
 * 解析 URL 筛选：缺时区时写回、非法时给出重置入口；合法时把筛选和更新函数交给子树。
 * 总览与请求明细页共用，保证两页的筛选语义一致。
 */
export function FiltersGate({
  children,
}: {
  children: (
    filters: OverviewFilters,
    update: (next: OverviewFilters) => void,
  ) => ReactNode;
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
  return <>{children(filters, update)}</>;
}

function OverviewBody({ scope }: { scope: QueryScope }) {
  return (
    <FiltersGate>
      {(filters, update) => (
        <OverviewContent scope={scope} filters={filters} update={update} />
      )}
    </FiltersGate>
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
  filters,
  update,
}: {
  scope: QueryScope;
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
  const previous = usePreviousTotals(scope, filters, range, now);

  return (
    <div className="geek-main">
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

      {data ? (
        <GeekKpis
          data={data}
          previous={previous.data?.totals}
          rate={<RateKpi scope={scope} filters={filters} />}
        />
      ) : null}

      <div className="geek-split">
        <Section title="趋势" query={aggregate} timezone={tz}>
          {data && trend ? (
            <GeekTrend
              scope={scope}
              filters={filters}
              range={range}
              data={data}
              trend={trend}
              timezone={tz}
            />
          ) : null}
        </Section>

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
    </div>
  );
}
