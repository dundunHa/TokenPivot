import type { AggregatePoint } from "../overviewApi";

/** 热力图的一格：某本地日期的某个本地小时。 */
export interface HeatCell {
  /** token 总量；null = 该小时没有可用数据（无事件或全部缺失）。 */
  tokens: number | null;
  requests: number;
}

export interface HeatDay {
  /** 本地日期键 YYYY-MM-DD，用于排序与定位。 */
  key: string;
  /** 展示标签，如“周二 11-04”。 */
  label: string;
  cells: HeatCell[];
}

export interface Heatmap {
  days: HeatDay[];
  /** 全部格子中的最大 token 量；没有任何可用数据时为 0。 */
  max: number;
  peak: { day: string; hour: number; tokens: number } | null;
}

const MAX_DAYS = 7;
const partsCache = new Map<string, Intl.DateTimeFormat>();

function formatter(
  timezone: string,
  locale: string,
  options: Intl.DateTimeFormatOptions,
): Intl.DateTimeFormat {
  const id = `${timezone}|${locale}|${JSON.stringify(options)}`;
  let f = partsCache.get(id);
  if (!f) {
    f = new Intl.DateTimeFormat(locale, {
      timeZone: timezone,
      hourCycle: "h23",
      ...options,
    });
    partsCache.set(id, f);
  }
  return f;
}

function part(
  parts: Intl.DateTimeFormatPart[],
  type: Intl.DateTimeFormatPartTypes,
): string {
  return parts.find((p) => p.type === type)?.value ?? "";
}

/**
 * 把小时桶按所选时区折成“日期 × 小时”矩阵：只按桶起点的本地日期与小时归格，
 * 不假定一天 24 个桶（夏令时的重复小时累加进同一格，缺失小时保持为空）。
 * 只保留最近 7 个本地日期。
 */
export function buildHeatmap(
  points: AggregatePoint[],
  timezone: string,
): Heatmap {
  const days = new Map<string, HeatDay>();
  for (const p of points) {
    const ms = Date.parse(p.bucketStart);
    if (Number.isNaN(ms)) continue;
    const at = new Date(ms);
    const ymd = formatter(timezone, "en-CA", {
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
    }).formatToParts(at);
    const key = `${part(ymd, "year")}-${part(ymd, "month")}-${part(ymd, "day")}`;
    const hour = Number(part(ymd, "hour")) % 24;
    let day = days.get(key);
    if (!day) {
      const weekday = formatter(timezone, "zh-CN", {
        weekday: "short",
      }).format(at);
      day = {
        key,
        label: `${weekday} ${part(ymd, "month")}-${part(ymd, "day")}`,
        cells: Array.from({ length: 24 }, () => ({
          tokens: null,
          requests: 0,
        })),
      };
      days.set(key, day);
    }
    const cell = day.cells[hour];
    if (!cell) continue;
    const value = p.stats.total.value;
    if (value !== null) cell.tokens = (cell.tokens ?? 0) + Number(value);
    cell.requests += Number(p.stats.requestCount);
  }
  const sorted = [...days.values()]
    .sort((a, b) => a.key.localeCompare(b.key))
    .slice(-MAX_DAYS);
  let max = 0;
  let peak: Heatmap["peak"] = null;
  for (const day of sorted) {
    day.cells.forEach((cell, hour) => {
      if (cell.tokens !== null && cell.tokens > max) {
        max = cell.tokens;
        peak = { day: day.label, hour, tokens: cell.tokens };
      }
    });
  }
  return { days: sorted, max, peak };
}
