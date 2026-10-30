import { queryOptions, useQuery } from "@tanstack/react-query";
import {
  fetchAggregate,
  fetchProjects,
  fetchRankings,
  type RankingDimension,
} from "./overviewApi";
import { aggregateParams, rankingsParams } from "./overviewApi";
import type { OverviewFilters, ResolvedRange } from "./overviewFilters";

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

export function aggregateQuery(
  scope: QueryScope,
  filters: OverviewFilters,
  range: ResolvedRange,
) {
  const query = aggregateParams(filters, range);
  return queryOptions({
    queryKey: [
      "overview",
      "aggregate",
      scope.userId,
      scope.workspaceId,
      filters.timezone,
      query,
    ] as const,
    queryFn: ({ signal }) => fetchAggregate(scope.workspaceId, query, signal),
    meta: { userId: scope.userId },
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
  return queryOptions({
    queryKey: [
      "overview",
      "rankings",
      scope.userId,
      scope.workspaceId,
      query,
    ] as const,
    queryFn: ({ signal }) => fetchRankings(scope.workspaceId, query, signal),
    meta: { userId: scope.userId },
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
    meta: { userId: scope.userId },
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
    refetchOnWindowFocus: true,
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
    refetchOnWindowFocus: !tail,
  });
}

export function useProjects(scope: QueryScope) {
  return useQuery(projectsQuery(scope));
}
