import { ApiError, getJson } from "@/lib/api";
import type { OverviewFilters } from "@/features/overview/overviewFilters";

/** 请求明细与 60 秒速率：唯一读取 usage/events、usage/rate 响应结构的位置。 */

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

export interface RateData {
  start: string;
  end: string;
  asOf: string;
  /** 十进制字符串；null = 无可用值（无事件或全部缺失），不是零。 */
  value: string | null;
  eventCount: number;
  knownCount: number;
  estimatedCount: number;
  missingCount: number;
  windowLastReceivedAt: string | null;
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

const DECIMAL = /^(0|[1-9][0-9]*)$/;

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

/** 首页不带 cursor 与时间范围（服务端取最近 1 小时）；翻页只带 cursor（范围在 cursor 内）。 */
export function eventsQueryString(
  filters: OverviewFilters,
  cursor: string | null,
): string {
  const params = filterParams(filters);
  params.set("limit", String(PAGE_SIZE));
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
    await getJson(`${base(workspaceId)}/events?${query}`, signal),
  );
}

export async function fetchRate(
  workspaceId: string,
  query: string,
  signal: AbortSignal,
): Promise<RateData> {
  return parseRate(await getJson(`${base(workspaceId)}/rate?${query}`, signal));
}
