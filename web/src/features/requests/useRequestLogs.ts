import { queryOptions, useQueries, useQuery } from "@tanstack/react-query";
import type { OverviewFilters } from "@/features/overview/overviewFilters";
import type { QueryScope } from "@/features/overview/overviewQueries";
import {
  eventsQueryString,
  type RequestRange,
  fetchSpeed,
  fetchRequests,
  filterParams,
} from "./api";
import { eventsPollOptions, POLL_MS } from "./model";

/** 键含用户、空间、端点与全部影响结果的输入；meta.userId 供退出时的身份清理识别。 */
export function eventsQuery(
  scope: QueryScope,
  filters: OverviewFilters,
  cursor: string | null,
  range: RequestRange | null,
) {
  const query = eventsQueryString(filters, cursor, range);
  return queryOptions({
    queryKey: [
      "requests",
      "events",
      scope.userId,
      scope.workspaceId,
      query,
    ] as const,
    queryFn: ({ signal }) => fetchRequests(scope.workspaceId, query, signal),
    meta: { userId: scope.userId, workspaceId: scope.workspaceId },
  });
}

export function speedQuery(scope: QueryScope, filters: OverviewFilters) {
  const query = filterParams(filters).toString();
  return queryOptions({
    queryKey: [
      "requests",
      "speed",
      scope.userId,
      scope.workspaceId,
      query,
    ] as const,
    queryFn: ({ signal }) => fetchSpeed(scope.workspaceId, query, signal),
    meta: { userId: scope.userId, workspaceId: scope.workspaceId },
  });
}

export function useRequestEvents(
  scope: QueryScope,
  filters: OverviewFilters,
  cursor: string | null,
  range: RequestRange | null,
) {
  return useQuery({
    ...eventsQuery(scope, filters, cursor, range),
    ...eventsPollOptions(cursor === null),
  });
}

/** 生成速度始终轮询（滚动 60 秒窗口 + 最近 60 分钟序列），与明细翻页无关；隐藏页由 Query 默认暂停。 */
export function useSpeed(
  scope: QueryScope,
  filters: OverviewFilters,
  intervalMs: number = POLL_MS,
) {
  return useQuery({
    ...speedQuery(scope, filters),
    refetchInterval: intervalMs,
    refetchOnWindowFocus: "always",
  });
}

/**
 * 按模型的生成速度：每个模型各取一条带单值模型筛选的结果，请求数等于模型数。
 * 指标卡里的分模型列表与速度趋势共用这些查询（同键同缓存）。
 */
export function useModelSpeeds(
  scope: QueryScope,
  filters: OverviewFilters,
  models: string[],
  intervalMs: number = POLL_MS,
) {
  return useQueries({
    queries: models.map((model) => ({
      ...speedQuery(scope, { ...filters, models: [model] }),
      refetchInterval: intervalMs,
      refetchOnWindowFocus: "always" as const,
    })),
  });
}
