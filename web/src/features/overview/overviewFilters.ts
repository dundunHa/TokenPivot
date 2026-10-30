/** 总览筛选：URL 严格解析/序列化与时间范围计算。不依赖 React。 */

export const RANGE_PRESETS = [
  "1h",
  "today",
  "yesterday",
  "7d",
  "custom",
] as const;
export type RangePreset = (typeof RANGE_PRESETS)[number];
export const GRANULARITIES = ["minute", "hour", "day"] as const;
export type Granularity = (typeof GRANULARITIES)[number];

export interface OverviewFilters {
  providers: string[];
  models: string[];
  projects: string[];
  range: RangePreset;
  granularity: Granularity;
  timezone: string;
  /** 仅 range=custom：带偏移的 RFC 3339 分钟时间。 */
  start: string | null;
  end: string | null;
}

/** 默认值只在此处：今天 + 小时，空选择表示全部。时区由调用方补入。 */
export const DEFAULT_OVERVIEW_FILTERS: Omit<OverviewFilters, "timezone"> = {
  providers: [],
  models: [],
  projects: [],
  range: "today",
  granularity: "hour",
  start: null,
  end: null,
};

export type ParseResult =
  | { ok: true; filters: OverviewFilters; timezoneImplied: boolean }
  | { ok: false; reason: string };

const MAX_VALUES = 50;
const MAX_LENGTH = 256;
const MINUTE_MS = 60_000;
const PROJECT_PATTERN =
  /^(unassigned|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$/i;
const OWN_KEYS = [
  "provider",
  "model",
  "project",
  "range",
  "granularity",
  "tz",
  "start",
  "end",
] as const;

export function browserTimezone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone;
}

const formatters = new Map<string, Intl.DateTimeFormat>();

function wallFormatter(timezone: string): Intl.DateTimeFormat {
  let f = formatters.get(timezone);
  if (!f) {
    f = new Intl.DateTimeFormat("en-US", {
      timeZone: timezone,
      hourCycle: "h23",
      year: "numeric",
      month: "numeric",
      day: "numeric",
      hour: "numeric",
      minute: "numeric",
      second: "numeric",
    });
    formatters.set(timezone, f);
  }
  return f;
}

function wallParts(timezone: string, ms: number) {
  const out: Record<string, number> = {};
  for (const p of wallFormatter(timezone).formatToParts(new Date(ms))) {
    if (p.type !== "literal") out[p.type] = Number(p.value);
  }
  return out as {
    year: number;
    month: number;
    day: number;
    hour: number;
    minute: number;
    second: number;
  };
}

/** timezone 在该时刻的 UTC 偏移（毫秒，东为正）。 */
export function tzOffsetMs(timezone: string, ms: number): number {
  const whole = Math.floor(ms / 1000) * 1000;
  const w = wallParts(timezone, whole);
  return (
    Date.UTC(w.year, w.month - 1, w.day, w.hour, w.minute, w.second) - whole
  );
}

export function isValidTimezone(timezone: string): boolean {
  if (timezone === "" || /^[+-]\d/.test(timezone)) return false;
  try {
    new Intl.DateTimeFormat("en-US", { timeZone: timezone });
    return timezone.toLowerCase() !== "local";
  } catch {
    return false;
  }
}

/** timezone 当前自然日 00:00 的 UTC 毫秒。 */
export function startOfLocalDay(timezone: string, ms: number): number {
  const w = wallParts(timezone, ms);
  const wall = Date.UTC(w.year, w.month - 1, w.day);
  const guess = wall - tzOffsetMs(timezone, ms);
  return wall - tzOffsetMs(timezone, guess);
}

const RFC3339 =
  /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(\.\d+)?(Z|[+-]\d{2}:\d{2})$/;

export interface ParsedInstant {
  ms: number;
  offsetMs: number;
  /** 秒与小数秒是否都为 0。 */
  aligned: boolean;
}

/** 解析带偏移的 RFC 3339；日期不存在（如 2 月 30 日）或格式不符返回 null。 */
export function parseInstant(text: string): ParsedInstant | null {
  const m = RFC3339.exec(text);
  if (!m) return null;
  const [, y, mo, d, h, mi, s, frac, off] = m;
  const nums = [y, mo, d, h, mi, s].map(Number) as [
    number,
    number,
    number,
    number,
    number,
    number,
  ];
  const [year, month, day, hour, minute, second] = nums;
  if (hour > 23 || minute > 59 || second > 59) return null;
  const wall = Date.UTC(year, month - 1, day, hour, minute, second);
  const check = new Date(wall);
  if (check.getUTCMonth() !== month - 1 || check.getUTCDate() !== day) {
    return null;
  }
  let offsetMs = 0;
  if (off !== "Z" && off !== undefined) {
    const oh = Number(off.slice(1, 3));
    const om = Number(off.slice(4, 6));
    if (oh > 23 || om > 59) return null;
    offsetMs = (oh * 60 + om) * MINUTE_MS * (off.startsWith("-") ? -1 : 1);
  }
  const fractionZero = frac === undefined || /^\.0+$/.test(frac);
  return {
    ms: wall - offsetMs,
    offsetMs,
    aligned: second === 0 && fractionZero,
  };
}

/** 自定义范围的错误说明；合法返回 null。偏移须与所选时区在该时刻一致。 */
export function validateCustomRange(
  start: string,
  end: string,
  timezone: string,
): string | null {
  const a = parseInstant(start);
  const b = parseInstant(end);
  if (!a || !b) {
    return "时间格式无效，需要带偏移的 RFC 3339，如 2026-10-28T09:00:00+08:00";
  }
  if (!a.aligned || !b.aligned) return "时间须精确到分钟（秒与小数秒为 0）";
  if (
    tzOffsetMs(timezone, a.ms) !== a.offsetMs ||
    tzOffsetMs(timezone, b.ms) !== b.offsetMs
  ) {
    return `时间偏移与时区 ${timezone} 在该时刻的偏移不一致（可能是不存在的本地时间）`;
  }
  if (a.ms >= b.ms) return "起点必须早于终点";
  return null;
}

function sortedUnique(values: string[]): string[] {
  return [...new Set(values)].sort();
}

function parseMulti(
  params: URLSearchParams,
  key: "provider" | "model" | "project",
): string[] | string {
  const raw = params.getAll(key);
  const values = key === "project" ? raw.map((v) => v.toLowerCase()) : raw;
  for (const v of values) {
    if (v === "" || v.length > MAX_LENGTH)
      return `参数 ${key} 含空值或过长的值`;
    if (key === "project" && !PROJECT_PATTERN.test(v)) {
      return "参数 project 须为项目 UUID 或 unassigned";
    }
  }
  const unique = sortedUnique(values);
  if (unique.length > MAX_VALUES) return `参数 ${key} 超过 ${MAX_VALUES} 个`;
  return unique;
}

function single(
  params: URLSearchParams,
  key: string,
): string | null | undefined {
  const all = params.getAll(key);
  if (all.length > 1) return undefined;
  return all[0] ?? null;
}

/** 严格解析：非法输入返回原因，调用方不得发出查询，也不得静默改值。 */
export function parseFilters(
  params: URLSearchParams,
  fallbackTimezone: string,
): ParseResult {
  const values = {} as Record<"provider" | "model" | "project", string[]>;
  for (const key of ["provider", "model", "project"] as const) {
    const parsed = parseMulti(params, key);
    if (typeof parsed === "string") return { ok: false, reason: parsed };
    values[key] = parsed;
  }
  const raw: Record<string, string | null> = {};
  for (const key of ["range", "granularity", "tz", "start", "end"]) {
    const v = single(params, key);
    if (v === undefined) return { ok: false, reason: `参数 ${key} 重复` };
    raw[key] = v;
  }
  const range = raw.range ?? DEFAULT_OVERVIEW_FILTERS.range;
  if (!(RANGE_PRESETS as readonly string[]).includes(range)) {
    return { ok: false, reason: "未知的时间范围" };
  }
  const granularity = raw.granularity ?? DEFAULT_OVERVIEW_FILTERS.granularity;
  if (!(GRANULARITIES as readonly string[]).includes(granularity)) {
    return { ok: false, reason: "未知的粒度" };
  }
  const timezoneImplied = raw.tz === null;
  const timezone = raw.tz ?? fallbackTimezone;
  if (!isValidTimezone(timezone)) return { ok: false, reason: "时区无效" };
  let start: string | null = null;
  let end: string | null = null;
  if (range === "custom") {
    if (raw.start === null || raw.end === null) {
      return { ok: false, reason: "自定义范围需要 start 与 end" };
    }
    const problem = validateCustomRange(
      raw.start ?? "",
      raw.end ?? "",
      timezone,
    );
    if (problem) return { ok: false, reason: problem };
    start = raw.start ?? null;
    end = raw.end ?? null;
  } else if (raw.start !== null || raw.end !== null) {
    return { ok: false, reason: "start/end 仅用于自定义范围" };
  }
  return {
    ok: true,
    timezoneImplied,
    filters: {
      providers: values.provider,
      models: values.model,
      projects: values.project,
      range: range as RangePreset,
      granularity: granularity as Granularity,
      timezone,
      start,
      end,
    },
  };
}

/** 写回 URL：只替换总览自身参数，其余原样保留；默认值省略，时区始终写入。 */
export function toSearchParams(
  filters: OverviewFilters,
  base: URLSearchParams,
): URLSearchParams {
  const next = new URLSearchParams(base);
  for (const key of OWN_KEYS) next.delete(key);
  for (const v of sortedUnique(filters.providers)) next.append("provider", v);
  for (const v of sortedUnique(filters.models)) next.append("model", v);
  for (const v of sortedUnique(filters.projects)) next.append("project", v);
  if (filters.range !== DEFAULT_OVERVIEW_FILTERS.range) {
    next.set("range", filters.range);
  }
  if (filters.granularity !== DEFAULT_OVERVIEW_FILTERS.granularity) {
    next.set("granularity", filters.granularity);
  }
  next.set("tz", filters.timezone);
  if (filters.range === "custom" && filters.start && filters.end) {
    next.set("start", filters.start);
    next.set("end", filters.end);
  }
  return next;
}

export function floorMinute(ms: number): number {
  return Math.floor(ms / MINUTE_MS) * MINUTE_MS;
}

export interface ResolvedRange {
  startMs: number;
  /** 省略表示由服务端采样当前时刻（含未完成分钟）。 */
  endMs: number | null;
}

/** 预设按 nowMs 解析为 [start,end)；URL 只保存预设，不保存解析结果。 */
export function resolveRange(
  filters: OverviewFilters,
  nowMs: number,
): ResolvedRange {
  const now = floorMinute(nowMs);
  switch (filters.range) {
    case "1h":
      return { startMs: now - 60 * MINUTE_MS, endMs: null };
    case "7d":
      return { startMs: now - 7 * 24 * 60 * MINUTE_MS, endMs: null };
    case "today":
      return { startMs: startOfLocalDay(filters.timezone, now), endMs: null };
    case "yesterday": {
      const endMs = startOfLocalDay(filters.timezone, now);
      return { startMs: startOfLocalDay(filters.timezone, endMs - 1), endMs };
    }
    case "custom": {
      const a = parseInstant(filters.start ?? "");
      const b = parseInstant(filters.end ?? "");
      return { startMs: a?.ms ?? now, endMs: b?.ms ?? now };
    }
  }
}

/** 影响结果但不含解析时间的稳定标识（用于长尾分页范围）。 */
export function filterIdentity(filters: OverviewFilters): string {
  return JSON.stringify([
    filters.providers,
    filters.models,
    filters.projects,
    filters.range,
    filters.granularity,
    filters.timezone,
    filters.start,
    filters.end,
  ]);
}
