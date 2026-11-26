import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useSearchParams } from "react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { useUsageOptions } from "@/features/workspace/queries";
import { OverviewFilterBar } from "./OverviewFilterBar";
import { OverviewRanking } from "./OverviewRanking";
import { Section } from "./OverviewSection";
import { GeekHeatmap } from "./geek/GeekHeatmap";
import { GeekKpis, type FastestModelInfo } from "./geek/GeekKpis";
import { GeekTrend } from "./geek/GeekTrend";
import { useTopModels } from "@/features/requests/modelRates";
import { formatSpeed, speedOf } from "@/features/requests/model";
import { SpeedTrend } from "@/features/requests/SpeedTrend";
import { useModelSpeeds, useRate } from "@/features/requests/useRequestLogs";
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
  useWeekActivity,
  useProjects,
  type QueryScope,
} from "./overviewQueries";
import { useRefreshMs } from "./refreshInterval";
import { buildTrend, formatInstant } from "./overviewView";

const TICK_MS = 10_000;

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

export function OverviewPage({
  userId,
  workspaceId,
}: {
  userId: string;
  workspaceId: string;
}) {
  return <OverviewBody scope={{ userId, workspaceId }} />;
}

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
  const refreshMs = useRefreshMs();
  const range = useMemo(() => resolveRange(filters, now), [filters, now]);
  const aggregate = useAggregate(scope, filters, range);
  const options = useUsageOptions(scope.userId, scope.workspaceId);
  const projects = useProjects(scope, refreshMs);
  const rate = useRate(scope, filters);
  const { models: topModels } = useTopModels(scope, filters, range);
  const modelSpeedQueries = useModelSpeeds(
    scope,
    filters,
    topModels,
    refreshMs,
  );

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
  const week = useWeekActivity(scope, filters, now);

  const { modelSpeeds, fastestModel } = useMemo(() => {
    const map = new Map<string, string>();
    let best: { model: string; speed: number; text: string } | null = null;
    topModels.forEach((model, i) => {
      const current = modelSpeedQueries[i]?.data?.current;
      const v = current ? speedOf(current) : null;
      if (v === null) return;
      const estimated = (current?.estimatedCount ?? 0) > 0;
      const text = `${formatSpeed(v)}${estimated ? " ≈" : ""}`;
      map.set(model, text);
      if (!best || v > best.speed) {
        best = { model, speed: v, text };
      }
    });
    const fastest: FastestModelInfo | null = best
      ? {
          model: (best as { model: string; text: string }).model,
          speedText: (best as { model: string; text: string }).text,
        }
      : null;
    return { modelSpeeds: map, fastestModel: fastest };
  }, [topModels, modelSpeedQueries]);

  const rangeText = data
    ? `统计范围 ${formatInstant(data.range.start, tz)} → ${formatInstant(data.range.end, tz)} · ${tz}`
    : null;

  const qualityText = useMemo(() => {
    if (!data) return null;
    const known = Number(data.totals.total.known);
    const est = Number(data.totals.total.estimated);
    const miss = Number(data.totals.total.missing);
    const total = known + est + miss;
    if (total <= 0) return null;
    const knownPct = ((known / total) * 100).toFixed(1);
    const estPct =
      est > 0 ? ` · ${((est / total) * 100).toFixed(1)}% 估算` : "";
    return `✓ ${knownPct}% 已知${estPct}`;
  }, [data]);

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
        rangeText={rangeText}
        qualityText={qualityText}
        onChange={update}
      />

      {data ? (
        <GeekKpis
          data={data}
          previous={previous.data?.totals}
          rate={rate.data}
          fastestModel={fastestModel}
        />
      ) : null}

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

      <OverviewRanking
        identity={identity}
        scope={scope}
        filters={filters}
        range={range}
        projectNames={projectNames}
        modelSpeeds={modelSpeeds}
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

      <div className="geek-split">
        <SpeedTrend
          scope={scope}
          filters={filters}
          range={range}
          intervalMs={refreshMs}
        />
        {week.data ? <GeekHeatmap data={week.data} timezone={tz} /> : null}
      </div>
    </div>
  );
}
