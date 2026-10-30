/** 同源业务请求的共享实现：Cookie、Accept、AbortSignal、unknown JSON 与安全的错误解析。 */

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly details: unknown;
  readonly retryAfter: string | undefined;

  constructor(
    status: number,
    code: string,
    details?: unknown,
    retryAfter?: string,
  ) {
    super(code);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.details = details;
    this.retryAfter = retryAfter;
  }
}

export interface ApiResult {
  status: number;
  /** 空响应体（如 204）或非 JSON 为 undefined。 */
  body: unknown;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

async function readJson(response: Response): Promise<unknown> {
  try {
    return await response.json();
  } catch {
    return undefined;
  }
}

function failure(response: Response, body: unknown): ApiError {
  const error = isRecord(body) && isRecord(body.error) ? body.error : undefined;
  const code = typeof error?.code === "string" ? error.code : "UNKNOWN";
  return new ApiError(
    response.status,
    code,
    error?.details,
    response.headers.get("Retry-After") ?? undefined,
  );
}

/** 发出请求；非 2xx 抛 ApiError，网络失败为 status 0 / NETWORK，主动中止原样抛出。 */
export async function requestJson(
  path: string,
  init: RequestInit,
): Promise<ApiResult> {
  let response: Response;
  try {
    response = await fetch(path, {
      ...init,
      credentials: "include",
      headers: { Accept: "application/json", ...init.headers },
    });
  } catch (cause) {
    if (init.signal?.aborted) throw cause;
    throw new ApiError(0, "NETWORK");
  }
  const body = await readJson(response);
  if (!response.ok) throw failure(response, body);
  return { status: response.status, body };
}

export async function getJson(
  path: string,
  signal?: AbortSignal,
): Promise<unknown> {
  return (await requestJson(path, { method: "GET", signal })).body;
}

/** body 省略时不带请求体与 Content-Type。 */
export async function post(
  path: string,
  body?: unknown,
  signal?: AbortSignal,
): Promise<unknown> {
  return (await postResult(path, body, signal)).body;
}

/** 与 post 相同，但保留状态码，供需要区分 204 的调用方使用。 */
export function postResult(
  path: string,
  body?: unknown,
  signal?: AbortSignal,
): Promise<ApiResult> {
  if (body === undefined) return requestJson(path, { method: "POST", signal });
  return requestJson(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
    signal,
  });
}
