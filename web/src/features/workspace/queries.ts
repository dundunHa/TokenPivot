import {
  queryOptions,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { handleMutationUnauthenticated } from "@/features/auth/queries";
import {
  createWorkspace,
  fetchUsageOptions,
  fetchWorkspace,
  fetchWorkspaces,
  type Workspace,
} from "./api";

/**
 * 业务查询键不以 "auth" 开头，身份清理（见 auth/queries.ts）据此识别；
 * meta.userId 标明发起请求时的身份，meta.workspaceId 供切换时取消旧空间请求。
 */
export function workspaceListQuery(userId: string) {
  return queryOptions({
    queryKey: ["workspace", "list", userId] as const,
    queryFn: ({ signal }) => fetchWorkspaces(signal),
    meta: { userId },
    // 默认 staleTime 为 Infinity；列表需要在新空间创建后立即可见。
    staleTime: 0,
  });
}

export function workspaceDetailQuery(userId: string, workspaceId: string) {
  return queryOptions({
    queryKey: ["workspace", "detail", userId, workspaceId] as const,
    queryFn: ({ signal }) => fetchWorkspace(workspaceId, signal),
    // 切换验证必须每次走 HTTP，不能被默认 Infinity 或旧缓存挡住。
    staleTime: 0,
    meta: { userId, workspaceId, local403: true },
  });
}

export function usageOptionsQuery(userId: string, workspaceId: string) {
  return queryOptions({
    queryKey: ["workspace", "usage-options", userId, workspaceId] as const,
    queryFn: ({ signal }) => fetchUsageOptions(workspaceId, signal),
    meta: { userId, workspaceId },
  });
}

/** userId 未就绪（未登录或会话确认中）时不发请求。 */
export function useWorkspaceList(userId: string | undefined) {
  return useQuery({
    ...workspaceListQuery(userId ?? ""),
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

const encoder = new TextEncoder();

/** 服务端按 name COLLATE "C"、id 排序；C 排序即 UTF-8 字节序。 */
function compareBytes(left: string, right: string): number {
  const a = encoder.encode(left);
  const b = encoder.encode(right);
  const length = Math.min(a.length, b.length);
  for (let i = 0; i < length; i += 1) {
    const diff = (a[i] ?? 0) - (b[i] ?? 0);
    if (diff !== 0) return diff;
  }
  return a.length - b.length;
}

function compareWorkspaces(left: Workspace, right: Workspace): number {
  const byName = compareBytes(left.name, right.name);
  return byName !== 0
    ? byName
    : compareBytes(left.workspaceId, right.workspaceId);
}

/**
 * 创建成功后先把新空间按服务端顺序写入列表缓存，避免列表重查返回前 URL 选择回落到旧首项；
 * 随后失效列表，以服务端结果为准。401 不在 QueryCache 的处理范围内，这里显式清理。
 */
export function useCreateWorkspace(userId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => createWorkspace(name),
    onError: async (error) => {
      await handleMutationUnauthenticated(client, error, userId);
    },
    onSuccess: async (created) => {
      const key = workspaceListQuery(userId).queryKey;
      client.setQueryData<Workspace[]>(key, (old) => {
        if (!old) return [created];
        if (old.some((w) => w.workspaceId === created.workspaceId)) return old;
        const next = [...old];
        const index = next.findIndex(
          (workspace) => compareWorkspaces(workspace, created) > 0,
        );
        if (index === -1) next.push(created);
        else next.splice(index, 0, created);
        return next;
      });
      await client.invalidateQueries({ queryKey: key });
    },
  });
}
