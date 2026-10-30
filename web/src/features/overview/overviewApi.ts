import { ApiError, getJson } from "@/lib/api";
import { type OverviewFilters, type ResolvedRange } from "./overviewFilters";

/** 唯一读取聚合 / 排名 / 项目列表响应结构的位置：unknown 在此收窄，形状不对按数据错误抛出。 */

export interface MetricStat {
  /** 十进制字符串；null = 缺失（不等于零）。 */
  value: string | null;
  known: string;
  estimated: string;
  missing: string;
}

export interface UsageStats {
  requestCount: string;
  input: MetricStat;
  output: MetricStat;
  total: MetricStat;
}

export interface AggregatePoint {
  bucketStart: string;
  bucketEnd: string;
  partial: boolean;
  stats: UsageStats;
}

export interface AggregateData {
  range: {
    start: string;
    end: string;
    asOf: string;
    retentionCutoff: string;
    timezone: string;
    granularity: string;
  };
  totals: UsageStats;
  points: AggregatePoint[];
}

export type RankingDimension = "project" | "provider" | "model";

export interface RankingKey {
  projectId: string | null;
  provider: string | null;
  model: string | null;
}

export interface RankingGroup {
  key: RankingKey;
  share: number | null;
  stats: UsageStats;
}

export interface RankingsData {
  groupBy: RankingDimension;
  overall: UsageStats;
  groups: RankingGroup[];
  other: { share: number | null; stats: UsageStats } | null;
  nextCursor: string | null;
}

export interface ProjectItem {
  projectId: string;
  name: string;
  archived: boolean;
}

export const RANKING_LIMIT = 10;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function badResponse(): ApiError {
  return new ApiError(200, "BAD_RESPONSE");
}

const DECIMAL = /^(0|[1-9][0-9]*)$/;

function decimal(value: unknown): string {
  if (typeof value !== "string" || !DECIMAL.test(value)) throw badResponse();
  return value;
}

function text(value: unknown): string {
  if (typeof value !== "string") throw badResponse();
  return value;
}

function record(value: unknown): Record<string, unknown> {
  if (!isRecord(value)) throw badResponse();
  return value;
}

function metric(value: unknown): MetricStat {
  const r = record(value);
  return {
    value: r.value === null ? null : decimal(r.value),
    known: decimal(r.known_count),
    estimated: decimal(r.estimated_count),
    missing: decimal(r.missing_count),
  };
}

function stats(value: unknown): UsageStats {
  const r = record(value);
  return {
    requestCount: decimal(r.request_count),
    input: metric(r.input_tokens),
    output: metric(r.output_tokens),
    total: metric(r.total_tokens),
  };
}

function share(value: unknown): number | null {
  if (value === null) return null;
  if (typeof value !== "number" || !Number.isFinite(value)) throw badResponse();
  return value;
}

function parseAggregate(body: unknown): AggregateData {
  const r = record(body);
  const range = record(r.range);
  if (!Array.isArray(r.points)) throw badResponse();
  return {
    range: {
      start: text(range.start),
      end: text(range.end),
      asOf: text(range.as_of),
      retentionCutoff: text(range.retention_cutoff),
      timezone: text(range.timezone),
      granularity: text(range.granularity),
    },
    totals: stats(r.totals),
    points: r.points.map((p: unknown) => {
      const point = record(p);
      if (typeof point.partial !== "boolean") throw badResponse();
      return {
        bucketStart: text(point.bucket_start),
        bucketEnd: text(point.bucket_end),
        partial: point.partial,
        stats: stats(point.stats),
      };
    }),
  };
}

function parseRankings(body: unknown): RankingsData {
  const r = record(body);
  const groupBy = r.group_by;
  if (groupBy !== "project" && groupBy !== "provider" && groupBy !== "model") {
    throw badResponse();
  }
  if (!Array.isArray(r.groups)) throw badResponse();
  const other = r.other === null ? null : record(r.other);
  return {
    groupBy,
    overall: stats(r.overall),
    groups: r.groups.map((g: unknown) => {
      const group = record(g);
      const key = record(group.key);
      const projectId = key.project_id;
      if (projectId !== undefined && projectId !== null) text(projectId);
      return {
        key: {
          projectId: typeof projectId === "string" ? projectId : null,
          provider: typeof key.provider === "string" ? key.provider : null,
          model: typeof key.model === "string" ? key.model : null,
        },
        share: share(group.share),
        stats: stats(group.stats),
      };
    }),
    other: other
      ? { share: share(other.share), stats: stats(other.stats) }
      : null,
    nextCursor: r.next_cursor === null ? null : text(r.next_cursor),
  };
}

function parseProjects(body: unknown): ProjectItem[] {
  if (!Array.isArray(body)) throw badResponse();
  return body.map((p: unknown) => {
    const project = record(p);
    return {
      projectId: text(project.project_id),
      name: text(project.name),
      archived: project.archived_at !== null,
    };
  });
}

function toIso(ms: number): string {
  return new Date(ms).toISOString().replace(/\.000Z$/, "Z");
}

/** 时间范围 + 筛选的公共查询参数；start 必填，end 省略表示服务端当前时刻。 */
export function rangeParams(
  filters: OverviewFilters,
  range: ResolvedRange,
): URLSearchParams {
  const params = new URLSearchParams();
  params.set("start", toIso(range.startMs));
  if (range.endMs !== null) params.set("end", toIso(range.endMs));
  for (const v of filters.providers) params.append("provider", v);
  for (const v of filters.models) params.append("model", v);
  for (const v of filters.projects) params.append("project", v);
  return params;
}

function base(workspaceId: string): string {
  return `/api/v1/workspaces/${encodeURIComponent(workspaceId)}`;
}

export function aggregateParams(
  filters: OverviewFilters,
  range: ResolvedRange,
): string {
  const params = rangeParams(filters, range);
  params.set("timezone", filters.timezone);
  params.set("granularity", filters.granularity);
  return params.toString();
}

export function rankingsParams(
  filters: OverviewFilters,
  range: ResolvedRange,
  groupBy: RankingDimension,
  cursor: string | null,
): string {
  const params = rangeParams(filters, range);
  params.set("group_by", groupBy);
  params.set("limit", String(RANKING_LIMIT));
  if (cursor) params.set("cursor", cursor);
  return params.toString();
}

export async function fetchAggregate(
  workspaceId: string,
  query: string,
  signal: AbortSignal,
): Promise<AggregateData> {
  return parseAggregate(
    await getJson(`${base(workspaceId)}/usage/aggregate?${query}`, signal),
  );
}

export async function fetchRankings(
  workspaceId: string,
  query: string,
  signal: AbortSignal,
): Promise<RankingsData> {
  return parseRankings(
    await getJson(`${base(workspaceId)}/usage/rankings?${query}`, signal),
  );
}

export async function fetchProjects(
  workspaceId: string,
  signal: AbortSignal,
): Promise<ProjectItem[]> {
  return parseProjects(await getJson(`${base(workspaceId)}/projects`, signal));
}

export type ErrorKind = "forbidden" | "filter" | "service" | "data";

/** 把请求错误转成可展示的类别与文案；不静默改筛选或粒度，只给出接口提供的建议。 */
export function describeError(error: unknown): {
  kind: ErrorKind;
  message: string;
} {
  if (!(error instanceof ApiError)) {
    return { kind: "service", message: "请求失败，请重试。" };
  }
  if (error.status === 403) {
    return { kind: "forbidden", message: "无权限访问该空间的用量数据。" };
  }
  if (error.code === "BAD_RESPONSE") {
    return { kind: "data", message: "响应数据格式不符合预期，已忽略。" };
  }
  const details = isRecord(error.details) ? error.details : {};
  if (error.status === 400) {
    if (error.code === "TOO_MANY_POINTS") {
      const max =
        typeof details.max_points === "number" ? details.max_points : null;
      const hint = details.suggested_granularity;
      const advice =
        hint === "hour" || hint === "day"
          ? `可改用「${hint === "hour" ? "小时" : "天"}」粒度`
          : "只能缩短时间范围";
      return {
        kind: "filter",
        message: `趋势点数超过上限${max === null ? "" : ` ${max}`}，${advice}。`,
      };
    }
    if (error.code === "OUTSIDE_RETENTION") {
      const cutoff =
        typeof details.retention_cutoff === "string"
          ? details.retention_cutoff
          : null;
      return {
        kind: "filter",
        message: `起点早于数据保留截点${cutoff ? `（${cutoff}）` : ""}，请缩短时间范围。`,
      };
    }
    return { kind: "filter", message: "筛选条件无效，请调整后重试。" };
  }
  if (error.status === 0) {
    return { kind: "service", message: "网络异常，请检查连接后重试。" };
  }
  return { kind: "service", message: "服务暂不可用，请稍后重试。" };
}
