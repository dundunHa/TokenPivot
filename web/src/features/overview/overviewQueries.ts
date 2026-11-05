import {
  queryOptions,
  useQueries,
  useQuery,
  useQueryClient,
  type Query,
  type QueryClient,
} from "@tanstack/react-query";
import {
  fetchAggregate,
  fetchProjects,
  fetchRankings,
  type AggregateData,
  type RankingDimension,
  type RankingsData,
} from "./overviewApi";
import { aggregateParams, rankingsParams } from "./overviewApi";
import {
  resolveRange,
  type OverviewFilters,
  type ResolvedRange,
} from "./overviewFilters";

/** 页面可见时的刷新间隔；隐藏页不轮询（refetchIntervalInBackground 默认 false）。 */
export const REFRESH_MS = 10_000;

/**
 * 业务查询键不以 "auth" 开头；meta.userId 标明发起身份，供身份清理识别。
 * 键含用户、空间与全部影响结果的输入（已解析的时间、筛选、时区、粒度等）。
 */
export interface QueryScope {
  userId: string;
  workspaceId: string;
}

/** 键里去掉随分钟滚动的起止时间后的形态；其余输入（筛选、时区、粒度、范围类型）必须相同。 */
function withoutTime(part: unknown): unknown {
  if (typeof part !== "string" || !part.startsWith("start=")) return part;
  const params = new URLSearchParams(part);
  params.delete("start");
  params.delete("end");
  return params.toString();
}

/**
 * 相对时间范围（近 24 小时等）每过一分钟，查询键里的 start 就变一次；键一变，旧数据被丢弃，
 * 整页指标、趋势、排名同时退回加载骨架，页面“整体刷新”一下。这里让新键在加载期间沿用上一份数据，
 * 只有时间滚动（其余输入完全相同）时才沿用；切换筛选、范围类型、粒度、自定义范围仍显示加载态，
 * 不会把另一组条件的数据当作当前结果。
 */
function keepAcrossMinuteRollover<T, K extends readonly unknown[]>(
  key: K,
  range: OverviewFilters["range"],
) {
  const next = JSON.stringify(key.map(withoutTime));
  return (
    previous: T | undefined,
    previousQuery: Query<T, Error, T, K> | undefined,
  ): T | undefined => {
    if (range === "custom" || previous === undefined || !previousQuery) {
      return undefined;
    }
    return JSON.stringify(previousQuery.queryKey.map(withoutTime)) === next
      ? previous
      : undefined;
  };
}

/**
 * useQueries 在键变化时会新建观察者，placeholderData 拿不到上一份结果；这里改为从缓存里找
 * “除起止时间外完全相同”的最近一份已成功数据（只在相对范围滚动分钟时沿用，自定义范围不沿用）。
 */
function cachedSibling<T>(
  client: QueryClient,
  key: readonly unknown[],
  range: OverviewFilters["range"],
): T | undefined {
  if (range === "custom") return undefined;
  const wanted = JSON.stringify(key.map(withoutTime));
  let best: Query | undefined;
  for (const q of client
    .getQueryCache()
    .findAll({ queryKey: key.slice(0, 4) })) {
    if (q.state.data === undefined) continue;
    if (JSON.stringify(q.queryKey.map(withoutTime)) !== wanted) continue;
    if (!best || q.state.dataUpdatedAt > best.state.dataUpdatedAt) best = q;
  }
  return best?.state.data as T | undefined;
}

export function aggregateQuery(
  scope: QueryScope,
  filters: OverviewFilters,
  range: ResolvedRange,
) {
  const query = aggregateParams(filters, range);
  const queryKey = [
    "overview",
    "aggregate",
    scope.userId,
    scope.workspaceId,
    filters.timezone,
    query,
    // 放在末尾：前面各段的位置是既有约定。区分相对范围与自定义范围，避免两个不同的固定范围互相沿用数据。
    filters.range,
  ] as const;
  return queryOptions<AggregateData, Error, AggregateData, typeof queryKey>({
    queryKey,
    queryFn: ({ signal }) => fetchAggregate(scope.workspaceId, query, signal),
    placeholderData: keepAcrossMinuteRollover<AggregateData, typeof queryKey>(
      queryKey,
      filters.range,
    ),
    meta: { userId: scope.userId, workspaceId: scope.workspaceId },
  });
}

export function rankingsQuery(
  scope: QueryScope,
  filters: OverviewFilters,
  range: ResolvedRange,
  groupBy: RankingDimension,
  cursor: string | null,
) {
  const query = rankingsParams(filters, range, groupBy, cursor);
  const queryKey = [
    "overview",
    "rankings",
    scope.userId,
    scope.workspaceId,
    query,
    filters.range,
  ] as const;
  return queryOptions<RankingsData, Error, RankingsData, typeof queryKey>({
    queryKey,
    queryFn: ({ signal }) => fetchRankings(scope.workspaceId, query, signal),
    placeholderData: keepAcrossMinuteRollover<RankingsData, typeof queryKey>(
      queryKey,
      filters.range,
    ),
    meta: { userId: scope.userId, workspaceId: scope.workspaceId },
  });
}

export function projectsQuery(scope: QueryScope) {
  return queryOptions({
    queryKey: [
      "overview",
      "projects",
      scope.userId,
      scope.workspaceId,
    ] as const,
    queryFn: ({ signal }) => fetchProjects(scope.workspaceId, signal),
    meta: { userId: scope.userId, workspaceId: scope.workspaceId },
  });
}

export function useAggregate(
  scope: QueryScope,
  filters: OverviewFilters,
  range: ResolvedRange,
) {
  return useQuery({
    ...aggregateQuery(scope, filters, range),
    refetchInterval: REFRESH_MS,
    // 共享 staleTime 为 Infinity；"always" 使固定范围从隐藏恢复可见时也立即刷新。
    refetchOnWindowFocus: "always",
  });
}

/** 首页（cursor=null）随页面刷新；长尾页范围固定，不轮询。 */
export function useRankings(
  scope: QueryScope,
  filters: OverviewFilters,
  range: ResolvedRange,
  groupBy: RankingDimension,
  cursor: string | null,
) {
  const tail = cursor !== null;
  return useQuery({
    ...rankingsQuery(scope, filters, range, groupBy, cursor),
    refetchInterval: tail ? false : REFRESH_MS,
    refetchOnWindowFocus: tail ? false : "always",
  });
}

export function useProjects(scope: QueryScope) {
  return useQuery(projectsQuery(scope));
}

/** 上一周期（同长度、紧邻当前起点之前）的总量，用于环比；失败时调用方不展示环比。 */
export function usePreviousTotals(
  scope: QueryScope,
  filters: OverviewFilters,
  range: ResolvedRange,
  nowMs: number,
) {
  const length = (range.endMs ?? nowMs) - range.startMs;
  const previous: ResolvedRange = {
    startMs: range.startMs - length,
    endMs: range.startMs,
  };
  return useQuery({
    ...aggregateQuery(scope, { ...filters, granularity: "day" }, previous),
    enabled: length > 0,
    retry: false,
    refetchInterval: REFRESH_MS,
  });
}

/** 按模型 / 供应商拆分趋势：每个分组各取一条带单值筛选的聚合，请求数等于分组数。 */
export function useGroupSeries(
  scope: QueryScope,
  filters: OverviewFilters,
  range: ResolvedRange,
  dimension: "model" | "provider",
  keys: string[],
) {
  const client = useQueryClient();
  return useQueries({
    queries: keys.map((key) => {
      const options = aggregateQuery(
        scope,
        {
          ...filters,
          models: dimension === "model" ? [key] : filters.models,
          providers: dimension === "provider" ? [key] : filters.providers,
        },
        range,
      );
      return {
        ...options,
        // 新观察者没有“上一份结果”，改从缓存取上一分钟的同条件数据，避免整张趋势图闪回骨架。
        placeholderData: () =>
          cachedSibling<AggregateData>(client, options.queryKey, filters.range),
        refetchInterval: REFRESH_MS,
      };
    }),
  });
}

/**
 * 活跃时段热力图：固定取最近 7 天、小时粒度，只带供应商/模型/项目筛选，
 * 不随页面的时间范围与粒度变化（热力图的意义就是看一周的节律）。
 */
export function useWeekActivity(
  scope: QueryScope,
  filters: OverviewFilters,
  nowMs: number,
) {
  const weekFilters: OverviewFilters = {
    ...filters,
    range: "7d",
    granularity: "hour",
    start: null,
    end: null,
  };
  return useQuery({
    ...aggregateQuery(scope, weekFilters, resolveRange(weekFilters, nowMs)),
    refetchInterval: REFRESH_MS * 6,
    retry: false,
  });
}
