import { afterEach, describe, expect, it, vi } from "vitest";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/test/render";
import {
  deferred,
  errorResponse,
  http,
  HttpResponse,
  server,
  sessionBody,
} from "@/test/server";

const WS = "0198f2b0-0000-7000-8000-0000000000aa";
const PROJECT = "0198f2b0-0000-7000-8000-0000000000bb";
const BASE = `/api/v1/workspaces/${WS}`;
const RANGE = {
  start: "2026-10-28T11:00:00Z",
  end: "2026-10-28T12:00:00Z",
  as_of: "2026-10-28T12:00:00Z",
  retention_cutoff: "2026-04-01T12:00:00Z",
};

const tok = (value: number | null, quality = "known") => ({ value, quality });
const item = (id: string, over: Record<string, unknown> = {}) => ({
  id,
  occurred_at: "2026-10-28T11:59:00Z",
  received_at: "2026-10-28T11:59:01Z",
  provider: "anthropic",
  model: "m1",
  project: null,
  source: { id: "s1", platform: "dsh", instance_key: "laptop" },
  input_tokens: tok(100),
  output_tokens: tok(null, "missing"),
  total_tokens: tok(0),
  status: null,
  duration_ms: null,
  ...over,
});
const page = (
  items: unknown[],
  next: string | null = null,
  over: Record<string, unknown> = {},
) => ({
  items,
  next_cursor: next,
  has_more: next !== null,
  ...RANGE,
  ...over,
});
const rateBody = (over: Record<string, unknown> = {}) => ({
  start: "2026-10-28T11:59:00Z",
  end: "2026-10-28T12:00:00Z",
  as_of: "2026-10-28T12:00:00Z",
  window_seconds: 60,
  unit: "tokens/min",
  total_tokens: {
    value: "12000",
    event_count: 5,
    known_count: 3,
    estimated_count: 1,
    missing_count: 1,
  },
  window_last_received_at: "2026-10-28T11:59:59Z",
  ...over,
});

interface Backend {
  events: URL[];
  rates: URL[];
}

function useBackend(opts?: {
  events?: (url: URL) => Response | Promise<Response>;
  rate?: (url: URL) => Response | Promise<Response>;
}): Backend {
  const seen: Backend = { events: [], rates: [] };
  const metric = {
    value: "1",
    known_count: "1",
    estimated_count: "0",
    missing_count: "0",
  };
  const stats = {
    request_count: "1",
    input_tokens: metric,
    output_tokens: metric,
    total_tokens: metric,
  };
  server.use(
    http.get("/api/v1/auth/session", () =>
      HttpResponse.json(sessionBody("a@example.com", "2999-01-01T00:00:00Z")),
    ),
    http.get("/api/v1/workspaces/current", () =>
      HttpResponse.json({ workspace_id: WS, name: "Personal" }),
    ),
    http.get(`${BASE}/usage/options`, () =>
      HttpResponse.json({ providers: ["anthropic", "other"], models: ["m1"] }),
    ),
    http.get(`${BASE}/projects`, () =>
      HttpResponse.json([
        { project_id: PROJECT, name: "Alpha", archived_at: null },
      ]),
    ),
    http.get(`${BASE}/usage/aggregate`, () =>
      HttpResponse.json({
        range: {
          ...RANGE,
          retention_cutoff: RANGE.retention_cutoff,
          timezone: "UTC",
          granularity: "hour",
        },
        totals: stats,
        points: [],
      }),
    ),
    http.get(`${BASE}/usage/rankings`, ({ request }) =>
      HttpResponse.json({
        group_by: new URL(request.url).searchParams.get("group_by"),
        overall: stats,
        groups: [],
        other: null,
        next_cursor: null,
      }),
    ),
    http.get(`${BASE}/usage/events`, ({ request }) => {
      const url = new URL(request.url);
      seen.events.push(url);
      return opts?.events?.(url) ?? HttpResponse.json(page([item("e1")]));
    }),
    http.get(`${BASE}/usage/rate`, ({ request }) => {
      const url = new URL(request.url);
      seen.rates.push(url);
      return opts?.rate?.(url) ?? HttpResponse.json(rateBody());
    }),
  );
  return seen;
}

/** 推进被伪造的 setInterval，再用真实 setTimeout 等 MSW 响应落地。 */
async function tick(ms: number) {
  await act(() => vi.advanceTimersByTimeAsync(ms));
  await act(() => new Promise<void>((resolve) => setTimeout(resolve, 60)));
}

function setVisibility(state: string) {
  Object.defineProperty(document, "visibilityState", {
    configurable: true,
    get: () => state,
  });
  document.dispatchEvent(new Event("visibilitychange"));
}

afterEach(() => {
  vi.useRealTimers();
  Reflect.deleteProperty(document, "visibilityState");
});

describe("request log panel", () => {
  it("renders rows with unavailable, zero and missing fields apart and rate text", async () => {
    useBackend();
    renderApp("/?tz=UTC");
    const row = await screen.findByTestId("request-row");
    expect(row.textContent).toContain("100");
    expect(within(row).getByText("不可用")).toBeTruthy();
    expect(within(row).getByText("0")).toBeTruthy();
    expect(within(row).getByText("状态不可用")).toBeTruthy();
    expect(within(row).getByText("耗时不详")).toBeTruthy();
    expect(within(row).getByText("未归属")).toBeTruthy();
    expect((await screen.findByTestId("rate-value")).textContent).toBe(
      "12,000",
    );
    expect(screen.getByTestId("rate-notes").textContent).toContain(
      "含估算 1 条",
    );
    expect(screen.getByTestId("rate-notes").textContent).toContain("部分覆盖");
    expect(screen.getByTestId("rate-fresh").textContent).toContain(
      "窗口内最后接收",
    );
  });

  it("distinguishes no events and unavailable usage in the rate", async () => {
    useBackend({
      rate: () =>
        HttpResponse.json(
          rateBody({
            total_tokens: {
              value: null,
              event_count: 0,
              known_count: 0,
              estimated_count: 0,
              missing_count: 0,
            },
            window_last_received_at: null,
          }),
        ),
      events: () => HttpResponse.json(page([])),
    });
    renderApp("/?tz=UTC");
    expect((await screen.findByTestId("rate-value")).textContent).toBe(
      "窗口内无事件",
    );
    expect(screen.getByTestId("rate-fresh").textContent).toContain(
      "接收新鲜度未知",
    );
    await screen.findByText("当前保留明细中没有匹配的请求。");
  });

  it("sends filters with unattributed mapping and no range, and pages by cursor", async () => {
    const seen = useBackend({
      events: (url) =>
        HttpResponse.json(
          url.searchParams.get("cursor") === "c1"
            ? page([item("e2")], null)
            : page([item("e1")], "c1"),
        ),
    });
    const user = userEvent.setup();
    renderApp("/?tz=UTC&provider=anthropic&project=unassigned");
    await screen.findByTestId("request-row");
    const first = seen.events[0];
    expect(first?.searchParams.getAll("provider")).toEqual(["anthropic"]);
    expect(first?.searchParams.getAll("project")).toEqual(["unattributed"]);
    expect(first?.searchParams.has("start")).toBe(false);
    expect(first?.searchParams.get("limit")).toBe("50");
    expect(seen.rates[0]?.searchParams.getAll("project")).toEqual([
      "unattributed",
    ]);
    expect(seen.rates[0]?.searchParams.has("limit")).toBe(false);

    await user.click(screen.getByRole("button", { name: "下一页" }));
    await waitFor(() =>
      expect(seen.events.at(-1)?.searchParams.get("cursor")).toBe("c1"),
    );
    expect(seen.events.at(-1)?.searchParams.has("start")).toBe(false);
    await screen.findByText(/更早页，不自动刷新/);
    expect(
      (screen.getByRole("button", { name: "下一页" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    await user.click(screen.getByRole("button", { name: "返回最新页" }));
    await waitFor(() => expect(screen.queryByText(/更早页/)).toBeNull());
    expect(seen.events.at(-1)?.searchParams.has("cursor")).toBe(false);
    expect(screen.getByRole("button", { name: "上一页" })).toHaveProperty(
      "disabled",
      true,
    );
  });

  it("resets the cursor when a filter changes", async () => {
    const seen = useBackend({
      events: (url) =>
        HttpResponse.json(
          url.searchParams.get("cursor")
            ? page([item("e2")])
            : page([item("e1")], "c1"),
        ),
    });
    const user = userEvent.setup();
    renderApp("/?tz=UTC&model=m1");
    await screen.findByTestId("request-row");
    await user.click(screen.getByRole("button", { name: "下一页" }));
    await screen.findByText(/更早页/);
    await user.click(screen.getByRole("button", { name: "移除 模型 m1" }));
    await screen.findByText(/最新页每 5 秒刷新/);
    const last = seen.events.at(-1);
    expect(last?.searchParams.has("cursor")).toBe(false);
    expect(last?.searchParams.has("model")).toBe(false);
    expect(seen.rates.at(-1)?.searchParams.has("model")).toBe(false);
  });

  it("shows first-load errors with retry for rows and rate", async () => {
    let fail = true;
    useBackend({
      events: () =>
        fail
          ? errorResponse(500, "INTERNAL")
          : HttpResponse.json(page([item("e1")])),
      rate: () =>
        fail ? errorResponse(500, "INTERNAL") : HttpResponse.json(rateBody()),
    });
    const user = userEvent.setup();
    renderApp("/?tz=UTC");
    const list = await screen.findByRole("region", { name: "明细列表" });
    await within(list).findByText(/服务暂不可用/);
    await screen.findByText(/速率：服务暂不可用/);
    fail = false;
    await user.click(within(list).getByRole("button", { name: "重试" }));
    await screen.findByTestId("request-row");
    await user.click(screen.getByRole("button", { name: "重试速率" }));
    await screen.findByTestId("rate-value");
  });

  it("keeps the previous rows and says so when a poll fails", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    let fail = false;
    const seen = useBackend({
      events: () =>
        fail
          ? errorResponse(500, "INTERNAL")
          : HttpResponse.json(page([item("e1")])),
      rate: () =>
        fail ? errorResponse(500, "INTERNAL") : HttpResponse.json(rateBody()),
    });
    renderApp("/?tz=UTC");
    await screen.findByTestId("request-row");
    await screen.findByTestId("rate-value");
    fail = true;
    const before = seen.events.length;
    await tick(5_000);
    expect(seen.events.length).toBeGreaterThan(before);
    await screen.findByText(/^更新失败，展示截至/);
    await screen.findByText(/速率更新失败，展示截至/);
    expect(screen.getByTestId("request-row")).toBeTruthy();
    expect(screen.getByTestId("rate-value").textContent).toBe("12,000");
  });

  it("polls both every 5 seconds on the latest page and only the rate on an older page", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    const seen = useBackend({
      events: (url) =>
        HttpResponse.json(
          url.searchParams.get("cursor")
            ? page([item("e2")])
            : page([item("e1")], "c1"),
        ),
    });
    renderApp("/?tz=UTC");
    await screen.findByTestId("request-row");
    await screen.findByTestId("rate-value");
    const e0 = seen.events.length;
    const r0 = seen.rates.length;
    await tick(5_000);
    expect(seen.events.length).toBe(e0 + 1);
    expect(seen.rates.length).toBe(r0 + 1);

    const next = screen.getByRole("button", { name: "下一页" });
    await act(async () => {
      next.click();
    });
    await screen.findByText(/更早页/);
    const e1 = seen.events.length;
    const r1 = seen.rates.length;
    await tick(5_000);
    expect(seen.rates.length).toBe(r1 + 1);
    await tick(10_000);
    expect(seen.rates.length).toBeGreaterThan(r1 + 1);
    expect(seen.events.length).toBe(e1);
  });

  it("pauses polling while the page is hidden", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    const seen = useBackend();
    renderApp("/?tz=UTC");
    await screen.findByTestId("request-row");
    await screen.findByTestId("rate-value");
    act(() => setVisibility("hidden"));
    const e = seen.events.length;
    const r = seen.rates.length;
    await tick(20_000);
    expect(seen.events.length).toBe(e);
    expect(seen.rates.length).toBe(r);
  });

  it("ignores a late response for the previous filter", async () => {
    const slow = deferred<void>();
    const seen = useBackend({
      events: async (url) => {
        if (url.searchParams.getAll("provider").join() === "anthropic") {
          await slow.promise;
          return HttpResponse.json(page([item("old")]));
        }
        return HttpResponse.json(page([item("new", { model: "fresh-model" })]));
      },
    });
    const user = userEvent.setup();
    renderApp("/?tz=UTC&provider=anthropic");
    await waitFor(() => expect(seen.events.length).toBe(1));
    await user.click(
      await screen.findByRole("button", { name: "移除 供应商 anthropic" }),
    );
    await screen.findByText(/anthropic \/ fresh-model/);
    await act(async () => {
      slow.resolve();
      await Promise.resolve();
    });
    expect(screen.getAllByTestId("request-row")).toHaveLength(1);
    expect(screen.getByText(/fresh-model/)).toBeTruthy();
  });

  it("drops the request cache for the signed-out identity", async () => {
    useBackend();
    server.use(
      http.post(
        "/api/v1/auth/logout",
        () => new HttpResponse(null, { status: 204 }),
      ),
    );
    const user = userEvent.setup();
    const client = renderApp("/?tz=UTC");
    await screen.findByTestId("request-row");
    expect(
      client.getQueryCache().findAll({ queryKey: ["requests"] }).length,
    ).toBeGreaterThan(0);
    await user.click(screen.getByRole("button", { name: "登出" }));
    await waitFor(() =>
      expect(
        client.getQueryCache().findAll({ queryKey: ["requests"] }),
      ).toHaveLength(0),
    );
  });
});
