import { useMemo, useState } from "react";
import type { AggregateData } from "../overviewApi";
import type { OverviewFilters, ResolvedRange } from "../overviewFilters";
import {
  useGroupSeries,
  useRankings,
  type QueryScope,
} from "../overviewQueries";
import type { TrendModel } from "../overviewView";
import { SERIES_PALETTE } from "./chartScale";
import { fmt } from "./geek-utils";
import {
  GeekLineChart,
  type ChartRow,
  type ChartSeries,
} from "./GeekLineChart";

type Mode = "total" | "model" | "provider";

const MODES: { id: Mode; label: string }[] = [
  { id: "total", label: "总量" },
  { id: "model", label: "按模型" },
  { id: "provider", label: "按提供商" },
];

const INPUT_COLOR = "#e0a64b";
const OUTPUT_COLOR = "#7aa2ff";
const OTHER_COLOR = "#6a655a";
/** 分组调色：与琥珀主色相邻但可区分，深色背景下对比度足够。 */
const PALETTE = SERIES_PALETTE;
const GROUP_LIMIT = 5;

type Row = ChartRow;
type Series = ChartSeries;

interface TrendProps {
  scope: QueryScope;
  filters: OverviewFilters;
  range: ResolvedRange;
  data: AggregateData;
  trend: TrendModel;
  timezone: string;
}

const GRAN_NAME: Record<string, string> = {
  minute: "分钟",
  hour: "小时",
  day: "天",
};

/** 趋势卡：总量（输入/输出）与按模型、按提供商的折线。 */
export function GeekTrend({
  scope,
  filters,
  range,
  data,
  trend,
  timezone,
}: TrendProps) {
  const [mode, setMode] = useState<Mode>("total");
  const grouped = mode !== "total";
  const rankings = useRankings(
    scope,
    filters,
    range,
    grouped ? mode : "model",
    null,
  );
  const groupKeys = useMemo(() => {
    if (!grouped) return [];
    return (rankings.data?.groups ?? [])
      .slice(0, GROUP_LIMIT)
      .map((g) => (mode === "model" ? g.key.model : g.key.provider))
      .filter((k): k is string => k !== null);
  }, [grouped, mode, rankings.data]);
  const results = useGroupSeries(
    scope,
    filters,
    range,
    mode === "provider" ? "provider" : "model",
    grouped ? groupKeys : [],
  );

  const total = trend.points.some((p) => !p.gap);
  const loading =
    grouped && (rankings.isPending || results.some((r) => r.isPending));
  const failed =
    grouped && (rankings.isError || results.some((r) => r.isError));

  const { rows, series } = useMemo(() => {
    if (!grouped) {
      const rows: Row[] = trend.points.map((p) => ({
        at: p.at,
        label: p.label,
        input: p.input,
        output: p.output,
        total: p.total,
      }));
      const series: Series[] = [
        { key: "input", name: "输入", color: INPUT_COLOR },
        { key: "output", name: "输出", color: OUTPUT_COLOR },
      ];
      return { rows, series };
    }
    const series: Series[] = groupKeys.map((k, i) => ({
      key: `g${i}`,
      name: k,
      color: PALETTE[i % PALETTE.length] ?? INPUT_COLOR,
    }));
    const lookups = results.map((r) => {
      const m = new Map<string, number>();
      for (const p of r.data?.points ?? []) {
        m.set(p.bucketStart, Number(p.stats.total.value ?? 0));
      }
      return m;
    });
    let hasOther = false;
    const rows: Row[] = data.points.map((p) => {
      const row: Row = {
        at: p.bucketStart,
        label: trend.points.find((t) => t.at === p.bucketStart)?.label ?? "",
      };
      let sum = 0;
      series.forEach((s, i) => {
        const v = lookups[i]?.get(p.bucketStart) ?? 0;
        row[s.key] = v;
        sum += v;
      });
      const rest = Math.max(0, Number(p.stats.total.value ?? 0) - sum);
      row.other = rest;
      if (rest > 0) hasOther = true;
      return row;
    });
    if (hasOther)
      series.push({ key: "other", name: "其他", color: OTHER_COLOR });
    return { rows, series };
  }, [grouped, trend.points, data.points, groupKeys, results]);

  // 实际粒度以服务端回传为准：自动模式下它由范围决定；显式粒度过细时服务端也会改用自动。
  const actual = data.range.granularity;
  const label = GRAN_NAME[actual];
  const overridden =
    filters.granularity !== "auto" && actual !== filters.granularity;
  const granularityText = label
    ? `按${label}${overridden ? "（范围较长，已自动调整）" : ""}`
    : null;

  // 输入/输出不完整或含估算时，总量虚线取自服务端，提示读者以它为准。
  const showTotalLine = !grouped && !trend.stackable;
  const chartSeries: Series[] = showTotalLine
    ? [
        ...series,
        { key: "total", name: "总量", color: "#f4f1ea", dashed: true },
      ]
    : series;
  const note =
    total && showTotalLine
      ? "部分时段的输入/输出缺失、含估算或与总量不一致，虚线为服务端总量。"
      : null;

  return (
    <div className="geek-card geek-trend-card">
      <div className="geek-card-head">
        <h2 className="geek-card-title">
          趋势
          {granularityText ? (
            <span className="geek-card-sub" data-testid="trend-granularity">
              {granularityText}
            </span>
          ) : null}
        </h2>
        <div className="geek-segmented" role="group" aria-label="趋势维度">
          {MODES.map((m) => (
            <button
              key={m.id}
              type="button"
              className={`geek-ranking-tab ${mode === m.id ? "geek-tab-active" : ""}`}
              aria-pressed={mode === m.id}
              onClick={() => setMode(m.id)}
            >
              {m.label}
            </button>
          ))}
        </div>
      </div>

      <ul className="geek-legend" aria-label="图例" hidden={!total}>
        {series.map((s) => (
          <li key={s.key}>
            <i style={{ background: s.color }} />
            {s.name}
          </li>
        ))}
      </ul>

      {!total ? (
        <p className="geek-empty geek-dim">
          {data.totals.requestCount === "0" ? "暂无数据" : "暂无数据"}
        </p>
      ) : loading ? (
        <div role="status" aria-label="趋势加载中" className="geek-skeleton" />
      ) : failed ? (
        <p role="alert" className="geek-empty geek-err">
          分组趋势加载失败，请稍后重试。
        </p>
      ) : series.length === 0 || rows.length === 0 ? (
        <p className="geek-empty geek-dim">暂无数据</p>
      ) : (
        <GeekLineChart
          rows={rows}
          series={chartSeries}
          timezone={timezone}
          format={fmt}
          showSum
        />
      )}
      {note ? <p className="geek-trend-note geek-dim">{note}</p> : null}
    </div>
  );
}
