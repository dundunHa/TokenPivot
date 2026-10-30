import { useEffect, useReducer, useRef } from "react";
import {
  CancelledError,
  QueryCache,
  QueryClient,
  queryOptions,
  type Query,
  useIsMutating,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { ApiError } from "@/lib/api";
import { fetchSession, login, logout, register } from "./api";
import { expiresAtMs, type AuthCredentials, type AuthSession } from "./model";

export const sessionKey = ["auth", "session"] as const;
export const writeKey = ["auth", "write"] as const;

/** setTimeout 的最大安全延迟，超过会溢出成立即触发。 */
const MAX_TIMEOUT_MS = 2_147_483_647;

const now = () => Date.now();

/** 业务查询 = 键不属于 "auth" 命名空间的查询；会话查询自成一类。 */
const isBusiness = (query: { queryKey: readonly unknown[] }): boolean =>
  query.queryKey[0] !== "auth";

function sessionUserId(client: QueryClient): string | undefined {
  return client.getQueryData<AuthSession | null>(sessionKey)?.user.id;
}

/** 取消并移除业务缓存（可排除一个查询，如刚失败、需保留错误态的那个）。 */
async function dropBusiness(
  client: QueryClient,
  except?: object,
): Promise<void> {
  const filters = {
    predicate: (query: { queryKey: readonly unknown[] }) =>
      isBusiness(query) && query !== except,
  };
  await client.cancelQueries(filters);
  client.removeQueries(filters);
}

/** 统一的匿名清理：取消会话与业务请求、移除业务缓存，再写入匿名状态。 */
async function clearToAnonymous(client: QueryClient): Promise<void> {
  await Promise.all([
    cancelSession(client),
    client.cancelQueries({ predicate: isBusiness }),
  ]);
  client.removeQueries({ predicate: isBusiness });
  client.setQueryData<AuthSession | null>(sessionKey, null);
}

/**
 * 业务请求的 401/403。只有发起请求时的身份（meta.userId）仍是当前会话身份才处理；
 * 已取消、旧身份或已匿名的迟到失败不产生任何清理副作用。
 */
async function handleBusinessError(
  client: QueryClient,
  error: unknown,
  query: Query<unknown, unknown, unknown>,
): Promise<void> {
  if (!isBusiness(query) || error instanceof CancelledError) return;
  if (!(error instanceof ApiError)) return;
  if (error.status !== 401 && error.status !== 403) return;
  const issuer = query.meta?.userId;
  if (typeof issuer !== "string" || sessionUserId(client) !== issuer) return;
  if (error.status === 401) {
    await clearToAnonymous(client);
    return;
  }
  await dropBusiness(client, query);
  // 失败查询自身保留错误态供界面显示无权限，但旧 data 不得继续可见。
  query.setState({ data: undefined, dataUpdatedAt: 0 });
}

export function createAppQueryClient(): QueryClient {
  const cache = new QueryCache({
    onError: (error, query) => {
      void handleBusinessError(client, error, query);
    },
    onSuccess: (data, query) => {
      // 会话重查确定匿名：同样移除业务缓存，避免旧空间数据残留。
      if (query.queryKey[0] === "auth" && data === null) {
        void dropBusiness(client);
      }
    },
  });
  const client = new QueryClient({
    queryCache: cache,
    defaultOptions: {
      queries: {
        retry: false,
        staleTime: Infinity,
        refetchOnWindowFocus: false,
        refetchOnReconnect: false,
      },
      mutations: { retry: false },
    },
  });
  return client;
}

const sessionQuery = queryOptions({
  queryKey: sessionKey,
  queryFn: ({ signal }) => fetchSession(signal),
});

function cancelSession(client: QueryClient): Promise<void> {
  return client.cancelQueries({ queryKey: sessionKey });
}

/** 先显式取消在途的会话查询，再重新查询；不依赖同 key 请求自动取代。 */
export async function replaceSessionQuery(client: QueryClient): Promise<void> {
  await cancelSession(client);
  await client.refetchQueries({ queryKey: sessionKey });
}

export function useCredentialsMutation(kind: "login" | "register") {
  const client = useQueryClient();
  return useMutation({
    mutationKey: writeKey,
    mutationFn: (credentials: AuthCredentials) =>
      kind === "login" ? login(credentials) : register(credentials),
    onMutate: () => cancelSession(client),
    onSuccess: async (session) => {
      // 先清旧身份的业务缓存，再写入新会话，防止跨账号残留。
      await Promise.all([cancelSession(client), dropBusiness(client)]);
      client.setQueryData<AuthSession | null>(sessionKey, session);
    },
  });
}

export function useLogoutMutation() {
  const client = useQueryClient();
  return useMutation({
    mutationKey: writeKey,
    mutationFn: logout,
    onMutate: async () => {
      await Promise.all([
        cancelSession(client),
        client.cancelQueries({ predicate: isBusiness }),
      ]);
    },
    onSuccess: () => clearToAnonymous(client),
  });
}

export type SessionView =
  | { status: "loading" | "checking" | "anonymous" | "error" }
  | { status: "ready"; session: AuthSession };

export function useAuthSession(): SessionView & {
  retry: () => Promise<void>;
  error: unknown;
} {
  const client = useQueryClient();
  const query = useQuery(sessionQuery);
  const writing = useIsMutating({ mutationKey: writeKey }) > 0;
  const data = query.data;
  const expiresAt = data ? expiresAtMs(data) : undefined;
  // 到期只在渲染时对比当前时间判断；定时器与可见性回调只负责触发重渲染。
  const [, rerender] = useReducer((n: number) => n + 1, 0);
  const expired = expiresAt !== undefined && now() >= expiresAt;
  const refetchedFor = useRef<number | undefined>(undefined);

  useEffect(() => {
    if (expiresAt === undefined) return;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const check = (): boolean => {
      if (now() < expiresAt) return false;
      rerender();
      if (!writing && refetchedFor.current !== expiresAt) {
        refetchedFor.current = expiresAt;
        void replaceSessionQuery(client);
      }
      return true;
    };
    const arm = () => {
      const delay = Math.min(Math.max(expiresAt - now(), 0), MAX_TIMEOUT_MS);
      timer = setTimeout(() => {
        if (!check()) arm();
      }, delay);
    };
    const onVisible = () => {
      if (document.visibilityState === "visible") check();
    };
    arm();
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [client, expiresAt, writing]);

  const retry = () => replaceSessionQuery(client);
  const base = { retry, error: query.error };
  if (data === null) return { ...base, status: "anonymous" };
  if (query.isFetching) {
    return { ...base, status: data === undefined ? "loading" : "checking" };
  }
  if (data === undefined) {
    return { ...base, status: query.isError ? "error" : "loading" };
  }
  if (query.isError) return { ...base, status: "error" };
  if (expired) return { ...base, status: "checking" };
  return { ...base, status: "ready", session: data };
}
