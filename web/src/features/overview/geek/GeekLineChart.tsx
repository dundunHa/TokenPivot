import { useMemo } from "react";
import {
  CartesianGrid,
  Line,
  LineChart,
  ReferenceLine,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import { formatInstant } from "../overviewView";
import { fmtAxis, niceTicks } from "./chartScale";

export interface ChartRow {
  at: string;
  label: string;
  [key: string]: number | string | null;
}

export interface ChartSeries {
  key: string;
  name: string;
  color: string;
  /** 辅助线（如总体）：更细的虚线，不画末端标记。 */
  dashed?: boolean;
}

const AXIS = { fill: "var(--text-muted)", fontSize: 10.5 };

function LastDot({
  cx,
  cy,
  index,
  lastIndex,
  color,
}: {
  cx?: number;
  cy?: number;
  index?: number;
  lastIndex: number;
  color: string;
}) {
  if (index !== lastIndex || cx === undefined || cy === undefined) {
    return <g />;
  }
  return (
    <circle
      cx={cx}
      cy={cy}
      r={3}
      fill={color}
      stroke="var(--surface)"
      strokeWidth={1.5}
    />
  );
}

/** 速度趋势折线图：纯净无混色单调折线、主题自适应网格与高对比悬浮读数。 */
export function GeekLineChart({
  rows,
  series,
  timezone,
  format,
  showSum = false,
  height = 222,
  connectNulls = false,
}: {
  rows: ChartRow[];
  series: ChartSeries[];
  timezone: string;
  format: (n: number) => string;
  showSum?: boolean;
  height?: number;
  connectNulls?: boolean;
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
      for (const s of series) {
        const v = row[s.key];
        if (typeof v === "number" && v > max) max = v;
      }
    }
    return niceTicks(max);
  }, [rows, series]);
  const lastIndex = useMemo(() => {
    const out = new Map<string, number>();
    for (const s of series) {
      for (let i = rows.length - 1; i >= 0; i -= 1) {
        if (typeof rows[i]?.[s.key] === "number") {
          out.set(s.key, i);
          break;
        }
      }
    }
    return out;
  }, [rows, series]);

  return (
    <ResponsiveContainer width="100%" height={height}>
      <LineChart
        data={rows}
        margin={{ top: 12, right: 14, bottom: 0, left: 0 }}
      >
        <CartesianGrid vertical={false} stroke="var(--grid-line)" />
        <ReferenceLine y={0} stroke="var(--zero-line)" />
        <XAxis
          dataKey="at"
          tick={AXIS}
          tickFormatter={(v: string) => labels.get(v) ?? v}
          axisLine={false}
          tickLine={false}
          minTickGap={56}
          tickMargin={8}
        />
        <YAxis
          tick={AXIS}
          tickFormatter={(v: number) => fmtAxis(v)}
          axisLine={false}
          tickLine={false}
          width={40}
          domain={[0, scale.max]}
          ticks={scale.ticks}
          interval={0}
        />
        <Tooltip
          cursor={{ stroke: "var(--border-strong)", strokeWidth: 1 }}
          content={
            <ChartTooltip
              series={series}
              timezone={timezone}
              format={format}
              showSum={showSum}
            />
          }
        />
        {series.map((s) => (
          <Line
            key={s.key}
            type="monotone"
            dataKey={s.key}
            name={s.name}
            stroke={s.color}
            strokeWidth={s.dashed ? 1.4 : 1.85}
            strokeDasharray={s.dashed ? "4 4" : undefined}
            strokeLinecap="round"
            strokeLinejoin="round"
            dot={
              s.dashed ? (
                false
              ) : (
                <LastDot
                  lastIndex={lastIndex.get(s.key) ?? -1}
                  color={s.color}
                />
              )
            }
            activeDot={{
              r: 3.8,
              fill: s.color,
              stroke: "var(--surface)",
              strokeWidth: 1.8,
            }}
            connectNulls={connectNulls}
            isAnimationActive={false}
          />
        ))}
      </LineChart>
    </ResponsiveContainer>
  );
}

interface TooltipPayload {
  payload?: ChartRow;
}

function ChartTooltip({
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
    .filter((s) => !s.dashed)
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
      {showSum ? (
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
