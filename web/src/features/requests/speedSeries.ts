import type {
  ChartRow,
  ChartSeries,
} from "@/features/overview/geek/GeekLineChart";
import type { SpeedData } from "./api";
import { speedOf } from "./model";

const MINUTE_MS = 60_000;

export interface SpeedInput {
  key: string;
  name: string;
  color: string;
  dashed?: boolean;
  data: SpeedData | undefined;
}

const labelFormat = new Map<string, Intl.DateTimeFormat>();
function hhmm(ms: number, timezone: string): string {
  let f = labelFormat.get(timezone);
  if (!f) {
    f = new Intl.DateTimeFormat("en-GB", {
      timeZone: timezone,
      hour: "2-digit",
      minute: "2-digit",
      hourCycle: "h23",
    });
    labelFormat.set(timezone, f);
  }
  return f.format(new Date(ms));
}

/**
 * 把每分钟的“输出 ÷ 耗时”变成 tokens/s 折线数据，横轴是 UTC 分钟（按所选时区显示时:分）。
 * 没有可测量请求的分钟留空（不是 0）：模型没在跑时没有“速度”，图上连线跨过空档。
 * 尚在加载的序列整条留空。
 */
export function buildSpeedRows(
  inputs: SpeedInput[],
  timezone: string,
): { rows: ChartRow[]; series: ChartSeries[]; hasData: boolean } {
  const ref = inputs.find((i) => i.data)?.data;
  const series: ChartSeries[] = inputs.map(({ key, name, color, dashed }) => ({
    key,
    name,
    color,
    dashed,
  }));
  if (!ref) return { rows: [], series, hasData: false };

  const startMs =
    Math.floor(Date.parse(ref.seriesStart) / MINUTE_MS) * MINUTE_MS;
  const endMs = Math.floor(Date.parse(ref.asOf) / MINUTE_MS) * MINUTE_MS;
  const bySeries = new Map<string, Map<number, number | null>>();
  for (const input of inputs) {
    if (!input.data) continue;
    const m = new Map<number, number | null>();
    for (const p of input.data.points) {
      m.set(Date.parse(p.bucketStart), speedOf(p));
    }
    bySeries.set(input.key, m);
  }

  let hasData = false;
  const rows: ChartRow[] = [];
  for (let t = startMs; t <= endMs; t += MINUTE_MS) {
    const row: ChartRow = {
      at: new Date(t).toISOString(),
      label: hhmm(t, timezone),
    };
    for (const input of inputs) {
      const v = bySeries.get(input.key)?.get(t) ?? null;
      row[input.key] = v;
      if (v !== null) hasData = true;
    }
    rows.push(row);
  }
  return { rows, series, hasData };
}
