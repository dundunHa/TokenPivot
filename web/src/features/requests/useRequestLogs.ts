import { queryOptions, useQuery } from "@tanstack/react-query";
import type { OverviewFilters } from "@/features/overview/overviewFilters";
import type { QueryScope } from "@/features/overview/overviewQueries";
import {
  eventsQueryString,
  type RequestRange,
  fetchRate,
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
    meta: { userId: scope.userId },
  });
}

export function rateQuery(scope: QueryScope, filters: OverviewFilters) {
  const query = filterParams(filters).toString();
  return queryOptions({
    queryKey: [
      "requests",
      "rate",
      scope.userId,
      scope.workspaceId,
      query,
    ] as const,
    queryFn: ({ signal }) => fetchRate(scope.workspaceId, query, signal),
    meta: { userId: scope.userId },
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

/** 速率始终轮询当前 60 秒窗口，与明细翻页无关；隐藏页由 Query 默认暂停。 */
export function useRate(scope: QueryScope, filters: OverviewFilters) {
  return useQuery({
    ...rateQuery(scope, filters),
    refetchInterval: POLL_MS,
    refetchOnWindowFocus: "always",
  });
}
