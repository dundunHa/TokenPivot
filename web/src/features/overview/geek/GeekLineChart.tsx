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
  /** 辅助线（如总量）：更细的虚线，不画发光与末端标记。 */
  dashed?: boolean;
}

const AXIS = { fill: "#857f70", fontSize: 11 };
const GRID = "#26231e";
const BG = "#0e0e10";

/** 最后一个有值的点：实心点 + 一圈柔光，表示“当前”。 */
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
    <g>
      <circle cx={cx} cy={cy} r={7} fill={color} opacity={0.18} />
      <circle
        cx={cx}
        cy={cy}
        r={3.5}
        fill={color}
        stroke={BG}
        strokeWidth={1.5}
      />
    </g>
  );
}

/** 趋势与速率共用的折线图：好读的纵轴刻度、紧凑横轴、发光线条与“当前”标记。 */
export function GeekLineChart({
  rows,
  series,
  timezone,
  format,
  showSum = false,
  height = 300,
  connectNulls = false,
}: {
  rows: ChartRow[];
  series: ChartSeries[];
  timezone: string;
  format: (n: number) => string;
  /** 悬浮提示末尾是否追加“合计”（各线之和或行内 total）。 */
  showSum?: boolean;
  height?: number;
  /** 空值连线跨过（速度趋势：没有请求的分钟没有速度，不是 0）。 */
  connectNulls?: boolean;
}) {
  // 跨度不超过 36 小时时横轴只留“时:分”，日期交给悬浮提示，标签更干净。
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
        <CartesianGrid vertical={false} stroke={GRID} strokeDasharray="2 5" />
        <ReferenceLine y={0} stroke="#34302a" />
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
          width={44}
          domain={[0, scale.max]}
          ticks={scale.ticks}
          interval={0}
        />
        <Tooltip
          cursor={{
            stroke: "#a09a8c",
            strokeDasharray: "3 4",
            strokeOpacity: 0.6,
          }}
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
            strokeWidth={s.dashed ? 1.25 : 2.25}
            strokeDasharray={s.dashed ? "4 4" : undefined}
            strokeLinecap="round"
            strokeLinejoin="round"
            style={
              s.dashed
                ? undefined
                : { filter: `drop-shadow(0 0 5px ${s.color}59)` }
            }
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
            activeDot={{ r: 4.5, fill: s.color, stroke: BG, strokeWidth: 2 }}
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
