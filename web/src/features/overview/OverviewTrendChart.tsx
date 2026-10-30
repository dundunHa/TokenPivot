import {
  Area,
  CartesianGrid,
  ComposedChart,
  Legend,
  Line,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import { formatCount, type TrendModel, type TrendPoint } from "./overviewView";

const INPUT_COLOR = "var(--primary)";
const OUTPUT_COLOR = "var(--success)";
const TOTAL_COLOR = "var(--foreground)";

function TrendTooltip({
  active,
  payload,
}: {
  active?: boolean;
  payload?: readonly { payload?: TrendPoint }[];
}) {
  const point = payload?.[0]?.payload;
  if (!active || !point) return null;
  return (
    <div className="border-border bg-card rounded-md border p-2 text-xs">
      <p>{point.title}</p>
      {point.gap ? null : (
        <>
          <p>
            输入：
            {formatCount(point.input === null ? null : String(point.input))}
          </p>
          <p>
            输出：
            {formatCount(point.output === null ? null : String(point.output))}
          </p>
          <p>
            总量：
            {formatCount(point.total === null ? null : String(point.total))}
          </p>
          {point.partial ? <p>未完成桶（含当前未完成时间）</p> : null}
          {point.unavailable ? <p>该桶用量不可用</p> : null}
          {point.quality ? <p>{point.quality}</p> : null}
        </>
      )}
    </div>
  );
}

/** 面积 + 服务端 total 线；动效关闭；缺口项使所有系列断段。 */
export function OverviewTrendChart({ trend }: { trend: TrendModel }) {
  const stack = trend.stackable ? "tokens" : undefined;
  return (
    <div>
      <ComposedChart
        data={trend.points}
        width={720}
        height={260}
        responsive
        style={{ width: "100%", maxHeight: 260 }}
        accessibilityLayer
      >
        <CartesianGrid stroke="var(--divider)" vertical={false} />
        <XAxis
          dataKey="label"
          stroke="var(--subtle)"
          fontSize={11}
          minTickGap={24}
        />
        <YAxis
          stroke="var(--subtle)"
          fontSize={11}
          width={56}
          tickFormatter={(v: number) => formatCount(String(v))}
        />
        <Tooltip content={TrendTooltip} isAnimationActive={false} />
        <Legend />
        <Area
          dataKey="input"
          name="输入"
          type="monotone"
          stackId={stack}
          stroke={INPUT_COLOR}
          fill={INPUT_COLOR}
          fillOpacity={0.25}
          connectNulls={false}
          isAnimationActive={false}
        />
        <Area
          dataKey="output"
          name="输出"
          type="monotone"
          stackId={stack}
          stroke={OUTPUT_COLOR}
          fill={OUTPUT_COLOR}
          fillOpacity={0.25}
          connectNulls={false}
          isAnimationActive={false}
        />
        <Line
          dataKey="total"
          name={trend.stackable ? "总量（上沿）" : "总量"}
          type="monotone"
          stroke={TOTAL_COLOR}
          strokeDasharray="4 3"
          dot={false}
          connectNulls={false}
          isAnimationActive={false}
        />
      </ComposedChart>
      {trend.stackNote ? (
        <p className="text-muted-foreground mt-2 text-xs">{trend.stackNote}</p>
      ) : null}
      {trend.points.some((p) => p.unavailable) ? (
        <p className="text-muted-foreground mt-1 text-xs">
          部分已完成桶用量不可用（请求数为 0 且指标缺失），不代表完整无事件。
        </p>
      ) : null}
    </div>
  );
}
