import { useEffect, useReducer, useRef } from "react";
import {
  QueryClient,
  queryOptions,
  useIsMutating,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { fetchSession, login, logout, register } from "./api";
import { expiresAtMs, type AuthCredentials, type AuthSession } from "./model";

export const sessionKey = ["auth", "session"] as const;
export const writeKey = ["auth", "write"] as const;

/** setTimeout 的最大安全延迟，超过会溢出成立即触发。 */
const MAX_TIMEOUT_MS = 2_147_483_647;

const now = () => Date.now();

export function createAppQueryClient(): QueryClient {
  return new QueryClient({
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
      await cancelSession(client);
      client.setQueryData<AuthSession | null>(sessionKey, session);
    },
  });
}

export function useLogoutMutation() {
  const client = useQueryClient();
  return useMutation({
    mutationKey: writeKey,
    mutationFn: logout,
    onMutate: () => cancelSession(client),
    onSuccess: async () => {
      await cancelSession(client);
      client.setQueryData<AuthSession | null>(sessionKey, null);
    },
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
