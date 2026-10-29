import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";

export { http, HttpResponse };
export const server = setupServer();

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
