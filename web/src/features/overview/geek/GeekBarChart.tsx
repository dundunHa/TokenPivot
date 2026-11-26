import { useMemo } from "react";
import {
  Bar,
  BarChart,
  CartesianGrid,
  Cell,
  ReferenceLine,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import { formatInstant } from "../overviewView";
import { fmtAxis, niceTicks } from "./chartScale";
import type { ChartRow, ChartSeries } from "./GeekLineChart";

const AXIS = { fill: "var(--text-muted)", fontSize: 10.5 };

/** 消耗趋势堆叠柱状图：支持单系列聚焦、微圆角顶段、累加中斜纹与高对比悬浮读数。 */
export function GeekBarChart({
  rows,
  series,
  timezone,
  format,
  showSum = false,
  height = 274,
}: {
  rows: ChartRow[];
  series: ChartSeries[];
  timezone: string;
  format: (n: number) => string;
  showSum?: boolean;
  height?: number;
}) {
  const labels = useMemo(() => {
    const first = Date.parse(rows[0]?.at ?? "");
    const last = Date.parse(rows.at(-1)?.at ?? "");
    const short = last - first <= 36 * 3_600_000;
    return new Map(
      rows.map((r) => [
        r.at,
        short && r.label.includes(" ")
          ? (r.label.split(" ")[1] ?? r.label)
          : r.label,
      ]),
    );
  }, [rows]);

  const scale = useMemo(() => {
    let max = 0;
    for (const row of rows) {
      let sum = 0;
      for (const s of series) {
        const v = row[s.key];
        if (typeof v === "number" && v > 0) sum += v;
      }
      if (sum > max) max = sum;
    }
    return niceTicks(max);
  }, [rows, series]);

  return (
    <ResponsiveContainer width="100%" height={height}>
      <BarChart data={rows} margin={{ top: 14, right: 14, bottom: 0, left: 0 }}>
        <defs>
          <pattern
            id="tp-partial-hatch"
            width="6"
            height="6"
            patternUnits="userSpaceOnUse"
            patternTransform="rotate(45)"
          >
            <line
              x1="0"
              y1="0"
              x2="0"
              y2="6"
              stroke="var(--surface)"
              strokeWidth="2"
              strokeOpacity="0.45"
            />
          </pattern>
        </defs>
        <CartesianGrid vertical={false} stroke="var(--grid-line)" />
        <ReferenceLine y={0} stroke="var(--zero-line)" />
        <XAxis
          dataKey="at"
          tick={AXIS}
          tickFormatter={(v: string) => labels.get(v) ?? v}
          axisLine={false}
          tickLine={false}
          minTickGap={40}
          tickMargin={8}
        />
        <YAxis
          tick={AXIS}
          tickFormatter={(v: number) => fmtAxis(v)}
          axisLine={false}
          tickLine={false}
          width={44}
          domain={[0, scale.max]}
          ticks={scale.ticks}
          interval={0}
        />
        <Tooltip
          cursor={{ fill: "var(--text)", fillOpacity: 0.045 }}
          content={
            <BarTooltip
              series={series}
              timezone={timezone}
              format={format}
              showSum={showSum}
            />
          }
        />
        {series.map((s, idx) => {
          const isTop = idx === series.length - 1;
          return (
            <Bar
              key={s.key}
              dataKey={s.key}
              name={s.name}
              stackId="stack"
              fill={s.color}
              stroke="var(--bar-gap-stroke)"
              strokeWidth={1}
              maxBarSize={34}
              radius={isTop ? [4, 4, 0, 0] : [0, 0, 0, 0]}
              isAnimationActive={false}
            >
              {rows.map((row) => (
                <Cell
                  key={row.at}
                  fill={s.color}
                  fillOpacity={row.partial === 1 ? 0.62 : 0.92}
                />
              ))}
            </Bar>
          );
        })}
      </BarChart>
    </ResponsiveContainer>
  );
}

interface TooltipPayload {
  payload?: ChartRow;
}

function BarTooltip({
  active,
  payload,
  series,
  timezone,
  format,
  showSum,
}: {
  active?: boolean;
  payload?: TooltipPayload[];
  series: ChartSeries[];
  timezone: string;
  format: (n: number) => string;
  showSum: boolean;
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
          <b className="geek-num">{format(s.value)}</b>
        </div>
      ))}
      {showSum && items.length > 1 ? (
        <div className="geek-tooltip-row geek-tooltip-sum">
          <span>合计</span>
          <b className="geek-num">
            {format(typeof row.total === "number" ? row.total : sum)}
          </b>
        </div>
      ) : null}
    </div>
  );
}
