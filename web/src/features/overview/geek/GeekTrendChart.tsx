import { useMemo } from "react";
import {
  AreaChart,
  Area,
  XAxis,
  YAxis,
  Tooltip,
  ResponsiveContainer,
  CartesianGrid,
} from "recharts";
import type { TrendModel } from "../overviewView";
import { fmt } from "./geek-utils";

interface GeekTrendChartProps {
  trend: TrendModel;
  timezone: string;
}

/**
 * J 风格趋势图：Recharts 面积图，琥珀配色。
 * PRD §7：输入+输出面积 + 总量线；cache 不额外堆叠。
 */
export function GeekTrendChart({ trend, timezone }: GeekTrendChartProps) {
  const data = useMemo(
    () =>
      trend.points
        .filter((p) => !p.gap)
        .map((p) => ({
          time: p.at,
          input: p.input ?? 0,
          output: p.output ?? 0,
          total: p.total ?? 0,
        })),
    [trend.points],
  );

  if (data.length === 0) {
    return (
      <div className="geek-empty geek-dim">
        所选范围与筛选下没有用量。
      </div>
    );
  }

  return (
    <div className="geek-trend">
      <ResponsiveContainer width="100%" height={280}>
        <AreaChart data={data} margin={{ top: 8, right: 8, bottom: 0, left: 0 }}>
          <defs>
            <linearGradient id="geek-input-grad" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="#e0a64b" stopOpacity={0.3} />
              <stop offset="100%" stopColor="#e0a64b" stopOpacity={0.02} />
            </linearGradient>
            <linearGradient id="geek-output-grad" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="#7aa2ff" stopOpacity={0.2} />
              <stop offset="100%" stopColor="#7aa2ff" stopOpacity={0.02} />
            </linearGradient>
          </defs>
          <CartesianGrid stroke="#1e1c18" strokeDasharray="3 3" />
          <XAxis
            dataKey="time"
            tick={{ fill: "#5c574a", fontSize: 11, fontFamily: "var(--geek-mono)" }}
            tickFormatter={(v: string) => formatTick(v, timezone)}
            axisLine={{ stroke: "#1e1c18" }}
            tickLine={false}
          />
          <YAxis
            tick={{ fill: "#5c574a", fontSize: 11, fontFamily: "var(--geek-mono)" }}
            tickFormatter={(v: number) => fmt(v)}
            axisLine={false}
            tickLine={false}
            width={50}
          />
          <Tooltip
            contentStyle={{
              background: "#14120f",
              border: "1px solid #2a2722",
              borderRadius: 4,
              fontFamily: "var(--geek-mono)",
              fontSize: 12,
              color: "#f4f1ea",
            }}
            labelFormatter={(v) => formatTick(String(v), timezone)}
            formatter={(value, name) => [fmt(Number(value)), String(name)]}
          />
          <Area
            type="monotone"
            dataKey="input"
            stackId="1"
            stroke="#e0a64b"
            strokeWidth={1.5}
            fill="url(#geek-input-grad)"
            name="input"
            dot={false}
            activeDot={{ r: 3, fill: "#e0a64b" }}
          />
          <Area
            type="monotone"
            dataKey="output"
            stackId="1"
            stroke="#7aa2ff"
            strokeWidth={1.5}
            fill="url(#geek-output-grad)"
            name="output"
            dot={false}
            activeDot={{ r: 3, fill: "#7aa2ff" }}
          />
        </AreaChart>
      </ResponsiveContainer>
      <div className="geek-trend-footer geek-dim">
        <span>▪ input ▫ output</span>
        <span>完成时刻计数 · 补报回填发生时段</span>
      </div>
    </div>
  );
}

function formatTick(isoString: string, _timezone: string): string {
  const d = new Date(isoString);
  return [d.getHours(), d.getMinutes()]
    .map((n) => String(n).padStart(2, "0"))
    .join(":");
}
