import { describe, expect, it, vi } from "vitest";
import { ApiError, getJson, post, postResult } from "./api";
import { http, HttpResponse, server } from "@/test/server";

describe("shared api", () => {
  it("sends Accept and credentials:include, passes the AbortSignal to fetch, returns unknown json", async () => {
    server.use(http.get("/api/v1/x", () => HttpResponse.json({ ok: 1 })));
    const spy = vi.spyOn(globalThis, "fetch");
    const controller = new AbortController();
    expect(await getJson("/api/v1/x", controller.signal)).toEqual({ ok: 1 });
    const init = spy.mock.calls[0]?.[1];
    spy.mockRestore();
    expect(init?.credentials).toBe("include");
    expect(init?.signal).toBe(controller.signal);
    expect(new Headers(init?.headers).get("Accept")).toBe("application/json");
  });

  it("rejects with the abort error and never wraps it as NETWORK", async () => {
    server.use(
      http.get("/api/v1/slow", async () => {
        await new Promise((r) => setTimeout(r, 200));
        return HttpResponse.json({});
      }),
    );
    const controller = new AbortController();
    const pending = getJson("/api/v1/slow", controller.signal);
    controller.abort();
    const error = await pending.catch((e: unknown) => e);
    expect(error).not.toBeInstanceOf(ApiError);
    expect((error as Error).name).toBe("AbortError");
  });

  it("parses code, details and Retry-After from an error body", async () => {
    server.use(
      http.get("/api/v1/bad", () =>
        HttpResponse.json(
          {
            error: {
              code: "TOO_MANY_POINTS",
              message: "ignored",
              details: { max_points: 1000 },
            },
          },
          { status: 400, headers: { "Retry-After": "7" } },
        ),
      ),
    );
    const error = (await getJson("/api/v1/bad").catch(
      (e: unknown) => e,
    )) as ApiError;
    expect(error).toBeInstanceOf(ApiError);
    expect(error.status).toBe(400);
    expect(error.code).toBe("TOO_MANY_POINTS");
    expect(error.details).toEqual({ max_points: 1000 });
    expect(error.retryAfter).toBe("7");
  });

  it("uses UNKNOWN for a non-json error body and NETWORK for a failed fetch", async () => {
    server.use(
      http.get(
        "/api/v1/html",
        () => new HttpResponse("<b>x</b>", { status: 502 }),
      ),
      http.get("/api/v1/down", () => HttpResponse.error()),
    );
    const unknown = (await getJson("/api/v1/html").catch(
      (e: unknown) => e,
    )) as ApiError;
    expect([unknown.status, unknown.code]).toEqual([502, "UNKNOWN"]);
    const network = (await getJson("/api/v1/down").catch(
      (e: unknown) => e,
    )) as ApiError;
    expect([network.status, network.code]).toEqual([0, "NETWORK"]);
  });

  it("post sends json with Content-Type, or no body and no Content-Type", async () => {
    const seen: { type: string | null; body: string }[] = [];
    server.use(
      http.post("/api/v1/p", async ({ request }) => {
        seen.push({
          type: request.headers.get("Content-Type"),
          body: await request.text(),
        });
        return new HttpResponse(null, { status: 204 });
      }),
    );
    expect(await post("/api/v1/p", { a: 1 })).toBeUndefined();
    const result = await postResult("/api/v1/p");
    expect(result.status).toBe(204);
    expect(seen[0]).toEqual({ type: "application/json", body: '{"a":1}' });
    expect(seen[1]).toEqual({ type: null, body: "" });
  });
});
