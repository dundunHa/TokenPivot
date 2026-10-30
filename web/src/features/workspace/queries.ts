import { queryOptions, useQuery } from "@tanstack/react-query";
import { fetchCurrentWorkspace, fetchUsageOptions } from "./api";

/**
 * 业务查询键不以 "auth" 开头，身份清理（见 auth/queries.ts）据此识别；
 * meta.userId 标明发起请求时的身份，旧身份的迟到 401/403 因此不会误清新登录。
 */
export function currentWorkspaceQuery(userId: string) {
  return queryOptions({
    queryKey: ["workspace", "current", userId] as const,
    queryFn: ({ signal }) => fetchCurrentWorkspace(signal),
    meta: { userId },
  });
}

export function usageOptionsQuery(userId: string, workspaceId: string) {
  return queryOptions({
    queryKey: ["workspace", "usage-options", userId, workspaceId] as const,
    queryFn: ({ signal }) => fetchUsageOptions(workspaceId, signal),
    meta: { userId },
  });
}

/** userId 未就绪（未登录或会话确认中）时不发请求。 */
export function useCurrentWorkspace(userId: string | undefined) {
  return useQuery({
    ...currentWorkspaceQuery(userId ?? ""),
    enabled: userId !== undefined,
  });
}

/** 真实 workspaceId 未就绪前不发请求。 */
export function useUsageOptions(
  userId: string | undefined,
  workspaceId: string | undefined,
) {
  return useQuery({
    ...usageOptionsQuery(userId ?? "", workspaceId ?? ""),
    enabled: userId !== undefined && workspaceId !== undefined,
  });
}
