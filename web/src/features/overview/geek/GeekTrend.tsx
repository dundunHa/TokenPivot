import { useMemo, useState } from "react";
import type { AggregateData } from "../overviewApi";
import type { OverviewFilters, ResolvedRange } from "../overviewFilters";
import {
  useGroupSeries,
  useRankings,
  type QueryScope,
} from "../overviewQueries";
import { formatCount, type TrendModel } from "../overviewView";
import { SERIES_PALETTE } from "./chartScale";
import { fmt } from "./geek-utils";
import { GeekBarChart } from "./GeekBarChart";
import type { ChartRow, ChartSeries } from "./GeekLineChart";

type MetricTab = "tokens" | "requests" | "cache";
type GroupMode = "io" | "model" | "provider";

const METRICS: { id: MetricTab; label: string }[] = [
  { id: "tokens", label: "Token" },
  { id: "requests", label: "请求数" },
  { id: "cache", label: "缓存 & 推理" },
];

const GROUP_MODES: { id: GroupMode; label: string }[] = [
  { id: "io", label: "输入/输出" },
  { id: "model", label: "按模型" },
  { id: "provider", label: "按供应商" },
];

const GROUP_LIMIT = 5;

interface TrendProps {
  scope: QueryScope;
  filters: OverviewFilters;
  range: ResolvedRange;
  data: AggregateData;
  trend: TrendModel;
  timezone: string;
}

/** 消耗趋势卡：固定堆叠柱状图，支持 Token / 请求数 / 缓存推理 与分组维度切换，支持点击图例聚焦单系列。 */
export function GeekTrend({
  scope,
  filters,
  range,
  data,
  trend,
  timezone,
}: TrendProps) {
  const [metric, setMetric] = useState<MetricTab>("tokens");
  const [groupMode, setGroupMode] = useState<GroupMode>("model");
  const [isolated, setIsolated] = useState<string | null>(null);

  const grouped = metric === "tokens" && groupMode !== "io";
  const rankings = useRankings(
    scope,
    filters,
    range,
    grouped ? groupMode : "model",
    null,
  );
  const groupKeys = useMemo(() => {
    if (!grouped) return [];
    return (rankings.data?.groups ?? [])
      .slice(0, GROUP_LIMIT)
      .map((g) => (groupMode === "model" ? g.key.model : g.key.provider))
      .filter((k): k is string => k !== null);
  }, [grouped, groupMode, rankings.data]);
  const results = useGroupSeries(
    scope,
    filters,
    range,
    groupMode === "provider" ? "provider" : "model",
    grouped ? groupKeys : [],
  );

  const hasPoints = data.points.length > 0 && trend.points.some((p) => !p.gap);
  const loading =
    grouped && (rankings.isPending || results.some((r) => r.isPending));
  const failed =
    grouped && (rankings.isError || results.some((r) => r.isError));

  const labelByAt = useMemo(
    () => new Map(trend.points.map((p) => [p.at, p.label])),
    [trend.points],
  );

  const { rows, series } = useMemo<{
    rows: ChartRow[];
    series: ChartSeries[];
  }>(() => {
    if (metric === "requests") {
      const r: ChartRow[] = data.points.map((p) => ({
        at: p.bucketStart,
        label: labelByAt.get(p.bucketStart) ?? "",
        partial: p.partial ? 1 : 0,
        requests: Number(p.stats.requestCount),
      }));
      return {
        rows: r,
        series: [{ key: "requests", name: "请求数", color: "var(--c-s1)" }],
      };
    }
    if (metric === "cache") {
      const r: ChartRow[] = data.points.map((p) => ({
        at: p.bucketStart,
        label: labelByAt.get(p.bucketStart) ?? "",
        partial: p.partial ? 1 : 0,
        cacheRead: Number(p.stats.cacheRead.value ?? 0),
        reasoning: Number(p.stats.reasoning.value ?? 0),
      }));
      return {
        rows: r,
        series: [
          { key: "cacheRead", name: "缓存读取", color: "var(--c-cache)" },
          { key: "reasoning", name: "推理思考", color: "var(--c-reason)" },
        ],
      };
    }
    if (!grouped) {
      const r: ChartRow[] = data.points.map((p) => ({
        at: p.bucketStart,
        label: labelByAt.get(p.bucketStart) ?? "",
        partial: p.partial ? 1 : 0,
        input: Number(p.stats.input.value ?? 0),
        output: Number(p.stats.output.value ?? 0),
        total:
          p.stats.total.value === null ? null : Number(p.stats.total.value),
      }));
      return {
        rows: r,
        series: [
          { key: "input", name: "输入", color: "var(--c-input)" },
          { key: "output", name: "输出", color: "var(--c-output)" },
        ],
      };
    }
    const sList: ChartSeries[] = groupKeys.map((k, i) => ({
      key: `g${i}`,
      name: k,
      color: SERIES_PALETTE[i % SERIES_PALETTE.length] ?? "var(--c-s1)",
    }));
    const lookups = results.map((res) => {
      const m = new Map<string, number>();
      for (const p of res.data?.points ?? []) {
        m.set(p.bucketStart, Number(p.stats.total.value ?? 0));
      }
      return m;
    });
    let hasOther = false;
    const r: ChartRow[] = data.points.map((p) => {
      const row: ChartRow = {
        at: p.bucketStart,
        label: labelByAt.get(p.bucketStart) ?? "",
        partial: p.partial ? 1 : 0,
      };
      let sum = 0;
      sList.forEach((s, i) => {
        const v = lookups[i]?.get(p.bucketStart) ?? 0;
        row[s.key] = v;
        sum += v;
      });
      const rest = Math.max(0, Number(p.stats.total.value ?? 0) - sum);
      row.other = rest;
      if (rest > 0) hasOther = true;
      return row;
    });
    if (hasOther) {
      sList.push({ key: "other", name: "其他", color: "var(--c-other)" });
    }
    return { rows: r, series: sList };
  }, [metric, grouped, data.points, labelByAt, groupKeys, results]);

  const activeSeries =
    isolated && series.some((s) => s.key === isolated)
      ? series.filter((s) => s.key === isolated)
      : series;

  const formatValue =
    metric === "requests"
      ? (n: number) => `${formatCount(String(Math.round(n)))} 次`
      : fmt;

  return (
    <div className="geek-card geek-trend-card">
      <div className="geek-card-head">
        <h2 className="geek-card-title">消耗趋势</h2>
        <div className="tp-chart-controls">
          <div className="geek-segmented" role="group" aria-label="趋势指标">
            {METRICS.map((m) => (
              <button
                key={m.id}
                type="button"
                className={`geek-ranking-tab ${metric === m.id ? "geek-tab-active" : ""}`}
                aria-pressed={metric === m.id}
                onClick={() => {
                  setMetric(m.id);
                  setIsolated(null);
                }}
              >
                {m.label}
              </button>
            ))}
          </div>
          {metric === "tokens" ? (
            <div className="geek-segmented" role="group" aria-label="趋势维度">
              {GROUP_MODES.map((m) => (
                <button
                  key={m.id}
                  type="button"
                  className={`geek-ranking-tab ${groupMode === m.id ? "geek-tab-active" : ""}`}
                  aria-pressed={groupMode === m.id}
                  onClick={() => {
                    setGroupMode(m.id);
                    setIsolated(null);
                  }}
                >
                  {m.label}
                </button>
              ))}
            </div>
          ) : null}
        </div>
      </div>

      <ul className="geek-legend" aria-label="图例" hidden={!hasPoints}>
        {series.map((s) => {
          const state = isolated
            ? isolated === s.key
              ? "focused"
              : "dimmed"
            : undefined;
          return (
            <li key={s.key}>
              <button
                type="button"
                className="tp-legend-btn"
                data-state={state}
                aria-pressed={isolated === s.key}
                onClick={() =>
                  setIsolated((cur) => (cur === s.key ? null : s.key))
                }
              >
                <span
                  className="tp-swatch tp-swatch-sq"
                  style={{ background: s.color }}
                />
                <span>{s.name}</span>
              </button>
            </li>
          );
        })}
      </ul>

      {!hasPoints ? (
        <p className="geek-empty geek-dim">暂无数据</p>
      ) : loading ? (
        <div role="status" aria-label="趋势加载中" className="geek-skeleton" />
      ) : failed ? (
        <p role="alert" className="geek-empty geek-err">
          分组趋势加载失败，请稍后重试。
        </p>
      ) : activeSeries.length === 0 || rows.length === 0 ? (
        <p className="geek-empty geek-dim">暂无数据</p>
      ) : (
        <GeekBarChart
          rows={rows}
          series={activeSeries}
          timezone={timezone}
          format={formatValue}
          showSum={metric === "tokens"}
        />
      )}
    </div>
  );
}
