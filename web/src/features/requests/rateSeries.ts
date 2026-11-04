import type { AggregateData } from "@/features/overview/overviewApi";
import type {
  ChartRow,
  ChartSeries,
} from "@/features/overview/geek/GeekLineChart";

const MINUTE_MS = 60_000;
/** 当前（未结束）分钟太短时速率抖动很大，不到这么久就先不画。 */
const MIN_PARTIAL_SECONDS = 20;

export interface RateInput {
  key: string;
  name: string;
  color: string;
  dashed?: boolean;
  data: AggregateData | undefined;
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
 * 把分钟粒度的聚合变成 tokens/s 折线数据：整分钟的桶 = 该分钟总量 ÷ 60；
 * 未结束的当前分钟按已过去的秒数折算（不足 20 秒不画）。
 * 聚合不生成空桶：窗口内缺的分钟表示没有事件，按 0 画；桶存在但全部缺失则为空点。
 * 只有已经返回数据的序列才补 0，尚在加载的序列整条留空。
 */
export function buildRateRows(
  inputs: RateInput[],
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
    Math.floor(Date.parse(ref.range.start) / MINUTE_MS) * MINUTE_MS;
  const asOfMs = Date.parse(ref.range.asOf);
  const rows: ChartRow[] = [];
  const byKey = new Map<string, Map<number, number | null>>();
  for (const input of inputs) {
    if (!input.data) continue;
    const m = new Map<number, number | null>();
    for (const p of input.data.points) {
      const at = Date.parse(p.bucketStart);
      const tokens = p.stats.total.value;
      if (tokens === null) {
        m.set(at, null);
        continue;
      }
      if (p.partial) {
        const elapsed = (asOfMs - at) / 1000;
        m.set(
          at,
          elapsed >= MIN_PARTIAL_SECONDS ? Number(tokens) / elapsed : null,
        );
      } else {
        m.set(at, Number(tokens) / 60);
      }
    }
    byKey.set(input.key, m);
  }

  let hasData = false;
  for (let t = startMs; t <= asOfMs; t += MINUTE_MS) {
    const row: ChartRow = {
      at: new Date(t).toISOString(),
      label: hhmm(t, timezone),
    };
    for (const input of inputs) {
      const m = byKey.get(input.key);
      if (!m) {
        row[input.key] = null;
        continue;
      }
      const v = m.has(t)
        ? (m.get(t) ?? null)
        : t === Math.floor(asOfMs / MINUTE_MS) * MINUTE_MS
          ? null
          : 0;
      row[input.key] = v;
      if (v !== null && v > 0) hasData = true;
    }
    rows.push(row);
  }
  return { rows, series, hasData };
}
