import {
  AuthApiError,
  parseAuthErrorCode,
  parseAuthSession,
  type AuthCredentials,
  type AuthSession,
} from "./model";

const BASE = "/api/v1/auth";

async function send(path: string, init: RequestInit): Promise<Response> {
  try {
    return await fetch(path, { ...init, credentials: "same-origin" });
  } catch (cause) {
    if (init.signal?.aborted) throw cause;
    throw new AuthApiError(0, "NETWORK");
  }
}

async function readJson(response: Response): Promise<unknown> {
  try {
    return await response.json();
  } catch {
    return undefined;
  }
}

async function failure(response: Response): Promise<AuthApiError> {
  const code = parseAuthErrorCode(await readJson(response));
  return new AuthApiError(
    response.status,
    code ?? "UNKNOWN",
    response.headers.get("Retry-After") ?? undefined,
  );
}

async function readSession(response: Response): Promise<AuthSession> {
  if (!response.ok) throw await failure(response);
  const session = parseAuthSession(await readJson(response));
  if (session === undefined)
    throw new AuthApiError(response.status, "BAD_RESPONSE");
  return session;
}

function postCredentials(
  path: string,
  credentials: AuthCredentials,
): Promise<Response> {
  return send(`${BASE}/${path}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify({
      email: credentials.email,
      password: credentials.password,
    }),
  });
}

export async function register(
  credentials: AuthCredentials,
): Promise<AuthSession> {
  return readSession(await postCredentials("register", credentials));
}

export async function login(
  credentials: AuthCredentials,
): Promise<AuthSession> {
  return readSession(await postCredentials("login", credentials));
}

/** 仅 401 UNAUTHENTICATED 视为匿名（null）；其他失败一律抛出。 */
export async function fetchSession(
  signal: AbortSignal,
): Promise<AuthSession | null> {
  const response = await send(`${BASE}/session`, {
    method: "GET",
    headers: { Accept: "application/json" },
    signal,
  });
  if (response.status === 401) {
    const error = await failure(response);
    if (error.code === "UNAUTHENTICATED") return null;
    throw error;
  }
  return readSession(response);
}

export async function logout(): Promise<void> {
  const response = await send(`${BASE}/logout`, { method: "POST" });
  if (response.status === 204) return;
  if (!response.ok) throw await failure(response);
  throw new AuthApiError(response.status, "BAD_RESPONSE");
}
