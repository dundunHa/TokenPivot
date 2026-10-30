import { ApiError, postResult, requestJson } from "@/lib/api";
import {
  AuthApiError,
  parseAuthSession,
  type AuthCredentials,
  type AuthSession,
} from "./model";

const BASE = "/api/v1/auth";

/** 共享请求层的错误转成认证错误，保持既有 AuthApiError 形状。 */
async function guarded<T>(run: () => Promise<T>): Promise<T> {
  try {
    return await run();
  } catch (cause) {
    if (cause instanceof ApiError) {
      throw new AuthApiError(cause.status, cause.code, cause.retryAfter);
    }
    throw cause;
  }
}

function toSession(status: number, body: unknown): AuthSession {
  const session = parseAuthSession(body);
  if (session === undefined) throw new AuthApiError(status, "BAD_RESPONSE");
  return session;
}

async function postCredentials(
  path: string,
  credentials: AuthCredentials,
): Promise<AuthSession> {
  return guarded(async () => {
    const { status, body } = await postResult(`${BASE}/${path}`, {
      email: credentials.email,
      password: credentials.password,
    });
    return toSession(status, body);
  });
}

export function register(credentials: AuthCredentials): Promise<AuthSession> {
  return postCredentials("register", credentials);
}

export function login(credentials: AuthCredentials): Promise<AuthSession> {
  return postCredentials("login", credentials);
}

/** 仅 401 UNAUTHENTICATED 视为匿名（null）；其他失败一律抛出。 */
export async function fetchSession(
  signal: AbortSignal,
): Promise<AuthSession | null> {
  try {
    return await guarded(async () => {
      const { status, body } = await requestJson(`${BASE}/session`, {
        method: "GET",
        signal,
      });
      return toSession(status, body);
    });
  } catch (error) {
    if (
      error instanceof AuthApiError &&
      error.status === 401 &&
      error.code === "UNAUTHENTICATED"
    ) {
      return null;
    }
    throw error;
  }
}

export async function logout(): Promise<void> {
  const { status } = await guarded(() => postResult(`${BASE}/logout`));
  if (status !== 204) throw new AuthApiError(status, "BAD_RESPONSE");
}
