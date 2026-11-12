import { ApiError, getJson } from "@/lib/api";
import type { OverviewFilters } from "@/features/overview/overviewFilters";

/** 请求明细与 60 秒速率：唯一读取 usage-events、usage-rate 响应结构的位置。 */

export type Quality = "known" | "estimated" | "missing";

export interface TokenField {
  /** 仅 quality=missing 时为 null。 */
  value: number | null;
  quality: Quality;
}

export interface RequestRow {
  id: string;
  occurredAt: string;
  receivedAt: string;
  provider: string;
  model: string;
  project: { id: string; key: string; name: string } | null;
  source: { platform: string; instanceKey: string };
  input: TokenField;
  output: TokenField;
  total: TokenField;
  status: string | null;
  durationMs: number | null;
  /** 十进制字符串（最多 6 位小数）；null = 缺失，不是 0。 */
  generationSpeedTps: string | null;
  /** 整数毫秒；null = 缺失，0 是真实观测值。 */
  ttftMs: number | null;
}

export interface RequestPage {
  items: RequestRow[];
  nextCursor: string | null;
  hasMore: boolean;
  start: string;
  end: string;
  asOf: string;
  retentionCutoff: string;
}

/** 一项窗口平均：值缺失（无有效样本）与真实的 0 用 null / 十进制字符串区分。 */
export interface RateSample<U extends string> {
  unit: U;
  /** 十进制字符串（最多 6 位小数）；null = 无有效样本，不是 0。 */
  value: string | null;
  /** 该项自己的有效样本数，与另一项独立。 */
  sampleCount: number;
}

export interface RateData {
  start: string;
  end: string;
  asOf: string;
  /** 旧 tokens/min 字段：仅保留给旧前端，页面不再展示。 */
  value: string | null;
  eventCount: number;
  knownCount: number;
  estimatedCount: number;
  missingCount: number;
  windowLastReceivedAt: string | null;
  /** 近 60 秒平均生成速度；旧后端省略时为 null 值、0 样本。 */
  generationSpeed: RateSample<"tokens/s">;
  /** 近 60 秒平均 TTFT；旧后端省略时为 null 值、0 样本。 */
  ttft: RateSample<"ms">;
}

export const PAGE_SIZE = 50;

function badResponse(): ApiError {
  return new ApiError(200, "BAD_RESPONSE");
}

function record(value: unknown): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw badResponse();
  }
  return value as Record<string, unknown>;
}

function text(value: unknown): string {
  if (typeof value !== "string") throw badResponse();
  return value;
}

function count(value: unknown): number {
  if (typeof value !== "number" || !Number.isInteger(value) || value < 0) {
    throw badResponse();
  }
  return value;
}

function nullableText(value: unknown): string | null {
  return value === null ? null : text(value);
}

/** 非负安全整数；null 原样返回，供可空字段复用。 */
function nullableSafeInteger(value: unknown): number | null {
  if (value === null) return null;
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0) {
    throw badResponse();
  }
  return value;
}

const DECIMAL = /^(0|[1-9][0-9]*)$/;
/** 后端口径：非负十进制字符串，最多 6 位小数；不用 JSON 浮点。 */
const METRIC_DECIMAL = /^(0|[1-9][0-9]*)(\.[0-9]{1,6})?$/;

function metricDecimal(value: unknown): string {
  if (typeof value !== "string" || !METRIC_DECIMAL.test(value)) {
    throw badResponse();
  }
  return value;
}

/** 明细行的可选新字段：旧后端省略该键、新后端给 null 都按缺失处理。 */
function optionalMetricDecimal(value: unknown): string | null {
  return value === undefined || value === null ? null : metricDecimal(value);
}

function optionalSafeInteger(value: unknown): number | null {
  return value === undefined ? null : nullableSafeInteger(value);
}

/** 必填的非负安全整数：指标对象必带样本数，null 与缺失都是畸形响应。 */
function safeCount(value: unknown): number {
  const parsed = nullableSafeInteger(value);
  if (parsed === null) throw badResponse();
  return parsed;
}

/**
 * 旧后端会整块省略新增对象：省略按「无有效样本」处理（值 null、样本数 0），
 * 而不是报错或当成 0。显式 null 对象不是约定的形状，按畸形响应拒绝。
 */
function rateSample<U extends string>(value: unknown, unit: U): RateSample<U> {
  if (value === undefined) return { unit, value: null, sampleCount: 0 };
  const r = record(value);
  if (text(r.unit) !== unit) throw badResponse();
  return {
    unit,
    value: r.value === null ? null : metricDecimal(r.value),
    sampleCount: safeCount(r.sample_count),
  };
}

function token(value: unknown): TokenField {
  const r = record(value);
  const q = r.quality;
  if (q !== "known" && q !== "estimated" && q !== "missing") {
    throw badResponse();
  }
  if (q === "missing") {
    if (r.value !== null) throw badResponse();
    return { value: null, quality: q };
  }
  return { value: count(r.value), quality: q };
}

function row(value: unknown): RequestRow {
  const r = record(value);
  const project = r.project === null ? null : record(r.project);
  const source = record(r.source);
  return {
    id: text(r.id),
    occurredAt: text(r.occurred_at),
    receivedAt: text(r.received_at),
    provider: text(r.provider),
    model: text(r.model),
    project: project
      ? {
          id: text(project.id),
          key: text(project.key),
          name: text(project.name),
        }
      : null,
    source: {
      platform: text(source.platform),
      instanceKey: text(source.instance_key),
    },
    input: token(r.input_tokens),
    output: token(r.output_tokens),
    total: token(r.total_tokens),
    status: nullableText(r.status),
    durationMs: r.duration_ms === null ? null : count(r.duration_ms),
    generationSpeedTps: optionalMetricDecimal(r.generation_speed_tps),
    ttftMs: optionalSafeInteger(r.ttft_ms),
  };
}

function parsePage(body: unknown): RequestPage {
  const r = record(body);
  if (!Array.isArray(r.items) || typeof r.has_more !== "boolean") {
    throw badResponse();
  }
  return {
    items: r.items.map(row),
    nextCursor: nullableText(r.next_cursor),
    hasMore: r.has_more,
    start: text(r.start),
    end: text(r.end),
    asOf: text(r.as_of),
    retentionCutoff: text(r.retention_cutoff),
  };
}

function parseRate(body: unknown): RateData {
  const r = record(body);
  const total = record(r.total_tokens);
  const value = total.value;
  if (value !== null && (typeof value !== "string" || !DECIMAL.test(value))) {
    throw badResponse();
  }
  return {
    start: text(r.start),
    end: text(r.end),
    asOf: text(r.as_of),
    value,
    eventCount: count(total.event_count),
    knownCount: count(total.known_count),
    estimatedCount: count(total.estimated_count),
    missingCount: count(total.missing_count),
    windowLastReceivedAt: nullableText(r.window_last_received_at),
    generationSpeed: rateSample(r.generation_speed, "tokens/s"),
    ttft: rateSample(r.ttft, "ms"),
  };
}

/** 共享“未归属”选择在此映射为本接口的 unattributed。 */
export function filterParams(filters: OverviewFilters): URLSearchParams {
  const params = new URLSearchParams();
  for (const v of filters.providers) params.append("provider", v);
  for (const v of filters.models) params.append("model", v);
  for (const v of filters.projects) {
    params.append("project", v === "unassigned" ? "unattributed" : v);
  }
  return params;
}

/** 面板独立的明细时间范围；null 表示服务端默认的最近 1 小时（随轮询滚动）。 */
export interface RequestRange {
  start: string;
  end: string;
}

/**
 * 默认范围的首页不带 start/end（服务端取最近 1 小时），其翻页只带 cursor（范围在
 * cursor 内）；显式范围的每一页都带同一组 start/end，翻页不漂移。
 */
export function eventsQueryString(
  filters: OverviewFilters,
  cursor: string | null,
  range: RequestRange | null,
): string {
  const params = filterParams(filters);
  params.set("limit", String(PAGE_SIZE));
  if (range) {
    params.set("start", range.start);
    params.set("end", range.end);
  }
  if (cursor) params.set("cursor", cursor);
  return params.toString();
}

function base(workspaceId: string): string {
  return `/api/v1/workspaces/${encodeURIComponent(workspaceId)}/usage`;
}

export async function fetchRequests(
  workspaceId: string,
  query: string,
  signal: AbortSignal,
): Promise<RequestPage> {
  return parsePage(
    await getJson(`${base(workspaceId)}-events?${query}`, signal),
  );
}

export async function fetchRate(
  workspaceId: string,
  query: string,
  signal: AbortSignal,
): Promise<RateData> {
  return parseRate(await getJson(`${base(workspaceId)}-rate?${query}`, signal));
}
