import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";

export { http, HttpResponse };

/**
 * 默认 handlers：未显式 mock 的测试也拿到真实的默认响应。
 * 飞书授权默认按“本实例未配置飞书应用凭证”返回，测试要用飞书入口时再 `server.use` 覆盖。
 */
export const server = setupServer(
  http.get("/api/v1/auth/feishu/authorize", () =>
    HttpResponse.json({
      enabled: false,
      authorize_url: null,
      expires_in_seconds: 0,
    }),
  ),
);

export function deferred<T = void>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

export function sessionBody(email: string, expiresAt: string) {
  return {
    user: { id: "0198f2b0-0000-7000-8000-000000000001", email },
    expires_at: expiresAt,
  };
}

export function errorResponse(
  status: number,
  code: string,
  headers?: Record<string, string>,
) {
  return HttpResponse.json(
    { error: { code, message: "ignored" } },
    { status, headers },
  );
}
