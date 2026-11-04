import { useId, useMemo, useState } from "react";
import {
  Area,
  AreaChart,
  CartesianGrid,
  Line,
  ComposedChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import type { AggregateData } from "../overviewApi";
import type { OverviewFilters, ResolvedRange } from "../overviewFilters";
import {
  useGroupSeries,
  useRankings,
  type QueryScope,
} from "../overviewQueries";
import { formatInstant, type TrendModel } from "../overviewView";
import { fmt } from "./geek-utils";

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
const PALETTE = ["#e0a64b", "#7aa2ff", "#7fd1a8", "#c792ea", "#f07f6f"];
const GROUP_LIMIT = 5;
const AXIS = { fill: "#857f70", fontSize: 11 };

interface Row {
  at: string;
  label: string;
  [key: string]: number | string | null;
}

interface Series {
  key: string;
  name: string;
  color: string;
}

interface TrendProps {
  scope: QueryScope;
  filters: OverviewFilters;
  range: ResolvedRange;
  data: AggregateData;
  trend: TrendModel;
  timezone: string;
}

/** 趋势卡：总量（输入/输出）与按模型、按提供商的堆叠面积。 */
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

  const stackNote = !grouped ? trend.stackNote : null;

  return (
    <div className="geek-card geek-trend-card">
      <div className="geek-card-head">
        <h2 className="geek-card-title">趋势</h2>
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

      <ul className="geek-legend" aria-label="图例">
        {series.map((s) => (
          <li key={s.key}>
            <i style={{ background: s.color }} />
            {s.name}
          </li>
        ))}
      </ul>

      {!total ? (
        <p className="geek-empty geek-dim">
          {data.totals.requestCount === "0"
            ? "所选范围与筛选下没有用量。"
            : "没有可展示的趋势点。"}
        </p>
      ) : loading ? (
        <div role="status" aria-label="趋势加载中" className="geek-skeleton" />
      ) : failed ? (
        <p role="alert" className="geek-empty geek-err">
          分组趋势加载失败，请稍后重试。
        </p>
      ) : series.length === 0 || rows.length === 0 ? (
        <p className="geek-empty geek-dim">当前筛选下没有分组数据。</p>
      ) : (
        <Chart
          rows={rows}
          series={series}
          stacked={grouped || trend.stackable}
          showTotalLine={!grouped && !trend.stackable}
          timezone={timezone}
        />
      )}
      {stackNote ? (
        <p className="geek-trend-note geek-dim">{stackNote}</p>
      ) : null}
    </div>
  );
}

function Chart({
  rows,
  series,
  stacked,
  showTotalLine,
  timezone,
}: {
  rows: Row[];
  series: Series[];
  stacked: boolean;
  showTotalLine: boolean;
  timezone: string;
}) {
  const id = useId().replace(/:/g, "");
  const Wrapper = showTotalLine ? ComposedChart : AreaChart;
  const labels = useMemo(
    () => new Map(rows.map((r) => [r.at, r.label])),
    [rows],
  );
  return (
    <ResponsiveContainer width="100%" height={300}>
      <Wrapper data={rows} margin={{ top: 8, right: 8, bottom: 0, left: 0 }}>
        <defs>
          {series.map((s) => (
            <linearGradient
              key={s.key}
              id={`${id}-${s.key}`}
              x1="0"
              y1="0"
              x2="0"
              y2="1"
            >
              <stop offset="0%" stopColor={s.color} stopOpacity={0.55} />
              <stop offset="100%" stopColor={s.color} stopOpacity={0.04} />
            </linearGradient>
          ))}
        </defs>
        <CartesianGrid vertical={false} stroke="#26231e" />
        <XAxis
          dataKey="at"
          tick={AXIS}
          tickFormatter={(v: string) => labels.get(v) ?? v}
          axisLine={{ stroke: "#26231e" }}
          tickLine={false}
          minTickGap={48}
        />
        <YAxis
          tick={AXIS}
          tickFormatter={(v: number) => fmt(v)}
          axisLine={false}
          tickLine={false}
          width={48}
        />
        <Tooltip
          cursor={{ stroke: "#857f70", strokeDasharray: "3 3" }}
          content={<TrendTooltip series={series} timezone={timezone} />}
        />
        {series.map((s) => (
          <Area
            key={s.key}
            type="monotone"
            dataKey={s.key}
            name={s.name}
            stackId={stacked ? "1" : undefined}
            stroke={s.color}
            strokeWidth={1.75}
            fill={`url(#${id}-${s.key})`}
            dot={false}
            activeDot={{ r: 3.5, strokeWidth: 0, fill: s.color }}
            connectNulls={false}
            isAnimationActive={false}
          />
        ))}
        {showTotalLine ? (
          <Line
            type="monotone"
            dataKey="total"
            name="总量"
            stroke="#f4f1ea"
            strokeWidth={1}
            strokeDasharray="4 3"
            dot={false}
            isAnimationActive={false}
          />
        ) : null}
      </Wrapper>
    </ResponsiveContainer>
  );
}

interface TooltipPayload {
  dataKey?: string | number;
  value?: number | string | null;
  payload?: Row;
}

function TrendTooltip({
  active,
  payload,
  series,
  timezone,
}: {
  active?: boolean;
  payload?: TooltipPayload[];
  series: Series[];
  timezone: string;
}) {
  const row = payload?.[0]?.payload;
  if (!active || !row) return null;
  const items = series
    .map((s) => ({ ...s, value: row[s.key] }))
    .filter(
      (s): s is typeof s & { value: number } => typeof s.value === "number",
    );
  const sum = items.reduce((a, s) => a + s.value, 0);
  return (
    <div className="geek-tooltip">
      <div className="geek-tooltip-time">{formatInstant(row.at, timezone)}</div>
      {items.map((s) => (
        <div key={s.key} className="geek-tooltip-row">
          <i style={{ background: s.color }} />
          <span>{s.name}</span>
          <b className="geek-num">{fmt(s.value)}</b>
        </div>
      ))}
      <div className="geek-tooltip-row geek-tooltip-sum">
        <span>合计</span>
        <b className="geek-num">
          {typeof row.total === "number" ? fmt(row.total) : fmt(sum)}
        </b>
      </div>
    </div>
  );
}
