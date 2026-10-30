import type {
  AggregateData,
  AggregatePoint,
  MetricStat,
  RankingGroup,
  UsageStats,
} from "./overviewApi";

/** 展示层：格式化、质量文案、趋势/排名视图模型。不重新计算后端口径。 */

export const MISSING = "—";

/** 十进制字符串 → 展示文本；< 1000 原样，其余 K/M/B 紧凑。精度只用于展示。 */
export function formatCount(value: string | null): string {
  if (value === null) return MISSING;
  if (value.length <= 3) return value;
  const n = Number(value);
  const units: [number, string][] = [
    [1e12, "T"],
    [1e9, "B"],
    [1e6, "M"],
    [1e3, "K"],
  ];
  for (const [size, unit] of units) {
    if (n >= size) {
      const scaled = n / size;
      return `${scaled >= 100 ? scaled.toFixed(0) : scaled.toFixed(2)}${unit}`;
    }
  }
  return value;
}

/** 完整数值（千分位），用于 title / tooltip。 */
export function formatExact(value: string | null): string {
  if (value === null) return MISSING;
  return value.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}

export function formatShare(share: number | null): string {
  return share === null ? MISSING : `${(share * 100).toFixed(1)}%`;
}

export type MetricQuality = "known" | "estimated" | "partial" | "missing";

/** 缺失（value=null）≠ 零；有估算或有缺失事件时标明。 */
export function metricQuality(stat: MetricStat): MetricQuality {
  if (stat.value === null) return "missing";
  if (stat.missing !== "0") return "partial";
  if (stat.estimated !== "0") return "estimated";
  return "known";
}

export function qualityNote(stat: MetricStat): string | null {
  switch (metricQuality(stat)) {
    case "missing":
      return "缺失/不可用";
    case "partial":
      return `部分数据（${stat.missing} 个事件缺失）`;
    case "estimated":
      return "含估算";
    case "known":
      return null;
  }
}

export interface MetricCard {
  id: "total" | "input" | "output" | "requests";
  label: string;
  value: string | null;
  exact: string;
  note: string | null;
}

export function buildMetricCards(totals: UsageStats): MetricCard[] {
  const token = (
    id: "total" | "input" | "output",
    label: string,
    s: MetricStat,
  ) => ({
    id,
    label,
    value: s.value,
    exact: formatExact(s.value),
    note: qualityNote(s),
  });
  return [
    token("total", "总 Token", totals.total),
    token("input", "输入 Token", totals.input),
    token("output", "输出 Token", totals.output),
    {
      id: "requests",
      label: "请求数",
      value: totals.requestCount,
      exact: formatExact(totals.requestCount),
      note: null,
    },
  ];
}

const partsCache = new Map<string, Intl.DateTimeFormat>();

function formatter(
  timezone: string,
  key: string,
  options: Intl.DateTimeFormatOptions,
): Intl.DateTimeFormat {
  const id = `${timezone}|${key}`;
  let f = partsCache.get(id);
  if (!f) {
    f = new Intl.DateTimeFormat("en-CA", {
      timeZone: timezone,
      hourCycle: "h23",
      ...options,
    });
    partsCache.set(id, f);
  }
  return f;
}

function pad(n: number): string {
  return String(n).padStart(2, "0");
}

/** 所选时区下的本地时间 + UTC 偏移，如 `2026-10-28 09:00 +08:00`（含秒时用 withSeconds）。 */
export function formatInstant(
  iso: string,
  timezone: string,
  withSeconds = false,
): string {
  const ms = Date.parse(iso);
  if (Number.isNaN(ms)) return MISSING;
  const parts = formatter(timezone, withSeconds ? "s" : "m", {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    ...(withSeconds ? { second: "2-digit" } : {}),
  }).formatToParts(new Date(ms));
  const get = (t: string) => parts.find((p) => p.type === t)?.value ?? "";
  const local = `${get("year")}-${get("month")}-${get("day")} ${get("hour")}:${get("minute")}${withSeconds ? `:${get("second")}` : ""}`;
  return `${local} ${offsetLabel(timezone, ms)}`;
}

export function offsetLabel(timezone: string, ms: number): string {
  const w = formatter(timezone, "w", {
    year: "numeric",
    month: "numeric",
    day: "numeric",
    hour: "numeric",
    minute: "numeric",
    second: "numeric",
  }).formatToParts(new Date(Math.floor(ms / 1000) * 1000));
  const n = (t: string) => Number(w.find((p) => p.type === t)?.value ?? 0);
  const wall = Date.UTC(
    n("year"),
    n("month") - 1,
    n("day"),
    n("hour"),
    n("minute"),
    n("second"),
  );
  const off = Math.round((wall - Math.floor(ms / 1000) * 1000) / 60_000);
  const sign = off < 0 ? "-" : "+";
  const abs = Math.abs(off);
  return `${sign}${pad(Math.floor(abs / 60))}:${pad(abs % 60)}`;
}

/** 横轴短标签；按桶实际起点和所选时区，不假定每天 24 小时。 */
export function bucketLabel(
  iso: string,
  timezone: string,
  granularity: string,
): string {
  const ms = Date.parse(iso);
  if (granularity === "day") {
    const p = formatter(timezone, "d", {
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
    }).formatToParts(new Date(ms));
    const g = (t: string) => p.find((x) => x.type === t)?.value ?? "";
    return `${g("month")}-${g("day")}`;
  }
  const p = formatter(timezone, "h", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  }).formatToParts(new Date(ms));
  const g = (t: string) => p.find((x) => x.type === t)?.value ?? "";
  return `${g("month")}-${g("day")} ${g("hour")}:${g("minute")}`;
}

export interface TrendPoint {
  /** 桶的唯一标识（UTC 起点）；缺口项为其前一桶终点。 */
  key: string;
  label: string;
  /** 该点对应的 UTC 时刻（缺口项为前一桶终点）。 */
  at: string;
  title: string;
  input: number | null;
  output: number | null;
  total: number | null;
  gap: boolean;
  partial: boolean;
  /** 已完成桶 request_count=0 且指标全缺失。 */
  unavailable: boolean;
  quality: string | null;
}

export interface TrendModel {
  points: TrendPoint[];
  /** 仅当输入/输出均为完整已知值且 total 等于两者之和时堆叠。 */
  stackable: boolean;
  stackNote: string | null;
}

function num(stat: MetricStat): number | null {
  return stat.value === null ? null : Number(stat.value);
}

function pointQuality(p: AggregatePoint): string | null {
  const notes = [
    ["输入", p.stats.input],
    ["输出", p.stats.output],
    ["总量", p.stats.total],
  ].flatMap(([name, stat]) => {
    const note = qualityNote(stat as MetricStat);
    return note ? [`${name as string}：${note}`] : [];
  });
  return notes.length > 0 ? notes.join("；") : null;
}

function sumMatches(p: AggregatePoint): boolean {
  const { input, output, total } = p.stats;
  if (input.value === null || output.value === null || total.value === null) {
    return false;
  }
  return BigInt(input.value) + BigInt(output.value) === BigInt(total.value);
}

function fullyKnown(stat: MetricStat): boolean {
  return metricQuality(stat) === "known";
}

/**
 * 稀疏桶 → 图表序列。服务端不生成空桶：相邻两点不连续时插入 null 缺口项，
 * 使所有系列断段且不填零；真实返回的零保持零。
 */
export function buildTrend(data: AggregateData): TrendModel {
  const { timezone, granularity } = data.range;
  const points: TrendPoint[] = [];
  let stackable = data.points.length > 0;
  let stackNote: string | null = null;
  let previous: AggregatePoint | null = null;
  for (const p of data.points) {
    if (previous && previous.bucketEnd !== p.bucketStart) {
      points.push({
        key: `gap:${previous.bucketEnd}`,
        label: bucketLabel(previous.bucketEnd, timezone, granularity),
        at: previous.bucketEnd,
        title: "该区间服务端未返回数据",
        input: null,
        output: null,
        total: null,
        gap: true,
        partial: false,
        unavailable: false,
        quality: null,
      });
    }
    const unavailable =
      !p.partial &&
      p.stats.requestCount === "0" &&
      p.stats.input.value === null &&
      p.stats.output.value === null &&
      p.stats.total.value === null;
    if (
      !fullyKnown(p.stats.input) ||
      !fullyKnown(p.stats.output) ||
      !fullyKnown(p.stats.total) ||
      !sumMatches(p)
    ) {
      if (stackable) {
        stackNote =
          "部分桶的输入/输出缺失、含估算或与总量不一致，面积不堆叠，总量线取自服务端。";
      }
      stackable = false;
    }
    points.push({
      key: p.bucketStart,
      label: bucketLabel(p.bucketStart, timezone, granularity),
      at: p.bucketStart,
      title: `${formatInstant(p.bucketStart, timezone)} → ${formatInstant(p.bucketEnd, timezone)}`,
      input: num(p.stats.input),
      output: num(p.stats.output),
      total: num(p.stats.total),
      gap: false,
      partial: p.partial,
      unavailable,
      quality: pointQuality(p),
    });
    previous = p;
  }
  if (granularity !== "day") {
    // DST 回拨使本地时间重复：重名标签附 UTC 偏移区分，桶与 UTC 口径不变。
    const counts = new Map<string, number>();
    for (const p of points) counts.set(p.label, (counts.get(p.label) ?? 0) + 1);
    for (const p of points) {
      if ((counts.get(p.label) ?? 0) > 1) {
        p.label = `${p.label} ${offsetLabel(timezone, Date.parse(p.at))}`;
      }
    }
  }
  return { points, stackable, stackNote };
}

export interface RankingRow {
  id: string;
  /** 点击切换的筛选值（项目 ID / unassigned / provider / model）。 */
  filterValue: string;
  label: string;
  sublabel: string | null;
  share: number | null;
  total: string | null;
  note: string | null;
}

export function rankingRow(
  group: RankingGroup,
  dimension: "project" | "provider" | "model",
  projectNames: Map<string, string>,
): RankingRow {
  const total = group.stats.total.value;
  const note = qualityNote(group.stats.total);
  if (dimension === "project") {
    const id = group.key.projectId;
    return {
      id: id ?? "unassigned",
      filterValue: id ?? "unassigned",
      label: id === null ? "未归属" : (projectNames.get(id) ?? id),
      sublabel: null,
      share: group.share,
      total,
      note,
    };
  }
  if (dimension === "provider") {
    const provider = group.key.provider ?? "";
    return {
      id: provider,
      filterValue: provider,
      label: provider,
      sublabel: null,
      share: group.share,
      total,
      note,
    };
  }
  const provider = group.key.provider ?? "";
  const model = group.key.model ?? "";
  return {
    id: `${provider}\u0000${model}`,
    filterValue: model,
    label: model,
    sublabel: provider,
    share: group.share,
    total,
    note,
  };
}

/** 字符条（原型风格）：share 缺失时不画。 */
export function glyphBar(share: number | null, width = 20): string | null {
  if (share === null) return null;
  const filled = Math.max(0, Math.min(width, Math.round(share * width)));
  return "█".repeat(filled) + "░".repeat(width - filled);
}
