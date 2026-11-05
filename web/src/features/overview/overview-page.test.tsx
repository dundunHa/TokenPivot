import { afterEach, describe, expect, it, vi } from "vitest";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/test/render";
import {
  errorResponse,
  http,
  HttpResponse,
  server,
  sessionBody,
} from "@/test/server";

const WS = "0198f2b0-0000-7000-8000-0000000000aa";
const PROJECT = "0198f2b0-0000-7000-8000-0000000000bb";
const SESSION = "/api/v1/auth/session";
const BASE = `/api/v1/workspaces/${WS}`;

const metric = (v: string | null) => ({
  value: v,
  known_count: v === null ? "0" : "1",
  estimated_count: "0",
  missing_count: "0",
});
const stats = (
  i: string | null,
  o: string | null,
  t: string | null,
  r: string,
) => ({
  request_count: r,
  input_tokens: metric(i),
  output_tokens: metric(o),
  total_tokens: metric(t),
});

function useBackend(opts?: { aggregate?: () => Response | Promise<Response> }) {
  const calls: URL[] = [];
  server.use(
    http.get(SESSION, () =>
      HttpResponse.json(sessionBody("a@example.com", "2999-01-01T00:00:00Z")),
    ),
    http.get("/api/v1/workspaces", () =>
      HttpResponse.json([{ workspace_id: WS, name: "Alpha" }]),
    ),
    http.get(`${BASE}/usage/options`, () =>
      HttpResponse.json({ providers: ["anthropic"], models: ["m1"] }),
    ),
    http.get(`${BASE}/projects`, () =>
      HttpResponse.json([
        { project_id: PROJECT, name: "Alpha", archived_at: null },
      ]),
    ),
    http.get(`${BASE}/usage/aggregate`, ({ request }) => {
      // “上一周期”环比查询固定用 day 粒度；这里只记录当前周期的聚合请求。
      const url = new URL(request.url);
      if (url.searchParams.get("granularity") !== "day") calls.push(url);
      return (
        opts?.aggregate?.() ??
        HttpResponse.json({
          range: {
            start: "2026-10-28T00:00:00Z",
            end: "2026-10-28T02:00:00Z",
            as_of: "2026-10-28T02:00:00Z",
            retention_cutoff: "2025-10-28T00:00:00Z",
            timezone: "UTC",
            granularity: "hour",
          },
          totals: stats("100", "20", "120", "7"),
          points: [
            {
              bucket_start: "2026-10-28T00:00:00Z",
              bucket_end: "2026-10-28T01:00:00Z",
              partial: false,
              stats: stats("100", "20", "120", "7"),
            },
          ],
        })
      );
    }),
    // 请求明细面板的默认空响应；本文件不断言它们。
    http.get(`${BASE}/usage-events`, () =>
      HttpResponse.json({
        items: [],
        next_cursor: null,
        has_more: false,
        start: "2026-10-28T01:00:00Z",
        end: "2026-10-28T02:00:00Z",
        as_of: "2026-10-28T02:00:00Z",
        retention_cutoff: "2026-04-01T00:00:00Z",
      }),
    ),
    http.get(`${BASE}/usage-speed`, () =>
      HttpResponse.json({
        as_of: "2026-10-28T02:00:00Z",
        series_start: "2026-10-28T01:00:00Z",
        window_seconds: 60,
        unit: "tokens/s",
        current: {
          output_tokens: null,
          duration_ms: null,
          event_count: 0,
          measured_count: 0,
          estimated_count: 0,
        },
        points: [],
      }),
    ),
    http.get(`${BASE}/usage/rankings`, ({ request }) => {
      const by = new URL(request.url).searchParams.get("group_by");
      return HttpResponse.json({
        group_by: by,
        overall: stats("100", "20", "120", "7"),
        groups: [
          {
            key:
              by === "project"
                ? { project_id: PROJECT }
                : { provider: "anthropic", model: "m1" },
            share: 1,
            stats: stats("100", "20", "120", "7"),
          },
        ],
        other: null,
        next_cursor: null,
      });
    }),
  );
  return calls;
}

/** 热力图（小时）与默认趋势之外的固定粒度另发聚合；这里只看主趋势的请求。 */
const mainCalls = (calls: URL[]) =>
  calls.filter(
    (c) =>
      !["hour", "minute"].includes(c.searchParams.get("granularity") ?? ""),
  );

/** 默认趋势请求：分钟粒度、约 24 小时、不带模型筛选（区别于按模型的分组序列）。 */
const defaultTrendCalls = (calls: URL[]) =>
  calls.filter(
    (c) =>
      c.searchParams.get("granularity") === "minute" &&
      !c.searchParams.has("model") &&
      Date.now() - Date.parse(c.searchParams.get("start") ?? "") >
        23 * 3_600_000,
  );

afterEach(() => {
  vi.useRealTimers();
});

describe("overview page", () => {
  it("shows the four metrics from the server and queries with the URL timezone", async () => {
    const calls = useBackend();
    renderApp("/?tz=UTC");
    const total = await screen.findByTestId("metric-total");
    expect(total.textContent).toContain("120");
    expect(screen.getByTestId("metric-requests").textContent).toContain("7");
    // 默认：近 24 小时、按分钟。
    const trend = defaultTrendCalls(calls);
    expect(trend.length).toBeGreaterThan(0);
    expect(trend[0]?.searchParams.get("timezone")).toBe("UTC");
    expect(trend[0]?.searchParams.has("provider")).toBe(false);
  });

  it("does not send a query for an invalid link and offers a reset", async () => {
    const calls = useBackend();
    const user = userEvent.setup();
    renderApp("/?range=bogus&tz=UTC");
    await screen.findByText(/链接筛选无效/);
    expect(calls).toHaveLength(0);
    await user.click(screen.getByRole("button", { name: "重置筛选" }));
    await screen.findByTestId("metric-total");
    expect(defaultTrendCalls(calls)).toHaveLength(1);
  });

  it("writes the browser timezone back to the URL when missing", async () => {
    useBackend();
    renderApp("/");
    await screen.findByTestId("metric-total");
    await waitFor(() =>
      expect(screen.getByTestId("location").textContent).toContain("/|"),
    );
  });

  it("clicking a model ranking row toggles only the Model filter", async () => {
    const calls = useBackend();
    const user = userEvent.setup();
    renderApp("/?tz=UTC");
    await screen.findByTestId("metric-total");
    await user.click(screen.getByRole("tab", { name: "模型" }));
    await user.click(await screen.findByRole("button", { name: /m1/ }));
    await waitFor(() => {
      const last = calls[calls.length - 1];
      expect(last?.searchParams.getAll("model")).toEqual(["m1"]);
      expect(last?.searchParams.has("provider")).toBe(false);
    });
    expect(screen.getByRole("button", { name: "移除 模型 m1" })).toBeTruthy();
  });

  it("keeps the model tab after a model click and collapses the long tail", async () => {
    useBackend();
    server.use(
      http.get(`${BASE}/usage/rankings`, ({ request }) => {
        const u = new URL(request.url).searchParams;
        const by = u.get("group_by");
        const tail = u.get("cursor") !== null;
        return HttpResponse.json({
          group_by: by,
          overall: stats("100", "20", "120", "7"),
          groups: [
            {
              key: tail
                ? { provider: "p", model: "tail-model" }
                : { provider: "anthropic", model: "m1" },
              share: 0.5,
              stats: stats("100", "20", "120", "7"),
            },
          ],
          other: tail ? null : { share: 0.5, stats: stats("1", "1", "2", "1") },
          next_cursor: tail ? null : "c1",
        });
      }),
    );
    const user = userEvent.setup();
    renderApp("/?tz=UTC");
    await screen.findByTestId("metric-total");
    await user.click(screen.getByRole("tab", { name: "模型" }));
    await user.click(await screen.findByRole("button", { name: /其他/ }));
    await screen.findByRole("button", { name: /tail-model/ });
    await user.click(screen.getByRole("button", { name: /^m1/ }));
    await screen.findByRole("button", { name: "移除 模型 m1" });
    expect(
      screen.getByRole("tab", { name: "模型", selected: true }),
    ).toBeTruthy();
    // 等新筛选的排名加载完成（m1 行变为已筛选），再确认长尾已收起。
    await screen.findByRole("button", { name: /^m1/, pressed: true });
    expect(screen.queryByRole("button", { name: /tail-model/ })).toBeNull();
    expect(
      screen
        .getByRole("button", { name: /其他/ })
        .getAttribute("aria-expanded"),
    ).toBe("false");
  });

  it("keeps the request log off the overview and opens it from the nav with filters kept", async () => {
    useBackend();
    const user = userEvent.setup();
    renderApp("/?tz=UTC&provider=anthropic");
    await screen.findByTestId("metric-total");
    expect(screen.queryByRole("region", { name: "请求明细" })).toBeNull();
    await user.click(screen.getByRole("link", { name: "请求明细" }));
    await screen.findByRole("region", { name: "请求明细" });
    expect(screen.getByTestId("location").textContent).toContain("/requests");
    expect(screen.queryByTestId("metric-total")).toBeNull();
    expect(
      screen.getByRole("button", { name: "移除 供应商 anthropic" }),
    ).toBeTruthy();
    await user.click(screen.getByRole("link", { name: "总览" }));
    await screen.findByTestId("metric-total");
  });

  it("shows a reset for an invalid link on the requests page too", async () => {
    useBackend();
    renderApp("/requests?range=bogus&tz=UTC");
    await screen.findByText(/链接筛选无效/);
    expect(screen.getByRole("button", { name: "重置筛选" })).toBeTruthy();
  });

  it("refetches immediately when the page becomes visible again (fixed range, infinite staleTime)", async () => {
    const calls = useBackend();
    renderApp(
      "/?tz=UTC&range=custom&start=2026-10-28T00:00:00Z&end=2026-10-28T02:00:00Z",
    );
    await screen.findByTestId("metric-total");
    const before = calls.length;
    const set = (state: string) => {
      Object.defineProperty(document, "visibilityState", {
        configurable: true,
        get: () => state,
      });
      window.dispatchEvent(new Event("visibilitychange"));
    };
    try {
      set("hidden");
      set("visible");
      await waitFor(() => expect(calls.length).toBeGreaterThan(before));
    } finally {
      Reflect.deleteProperty(document, "visibilityState");
    }
  });

  it("keeps the numbers and charts on screen while a relative range rolls over to the next minute", async () => {
    // 假定时器必须在渲染前装上，页面里的 10 秒定时器才会被它推进。
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
    const calls = useBackend();
    renderApp("/?tz=UTC");
    expect((await screen.findByTestId("metric-total")).textContent).toContain(
      "120",
    );
    await screen.findByRole("region", { name: "排名" });
    const firstStart = defaultTrendCalls(calls)[0]?.searchParams.get("start");

    // 下一分钟的查询（start 不同）一直不返回：旧数据必须继续显示，不能退回加载骨架。
    let release: () => void = () => undefined;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    server.use(
      http.get(`${BASE}/usage/aggregate`, async () => {
        await gate;
        return HttpResponse.json({});
      }),
      http.get(`${BASE}/usage/rankings`, async () => {
        await gate;
        return HttpResponse.json({});
      }),
    );
    try {
      vi.setSystemTime(Date.now() + 61_000);
      await act(() => vi.advanceTimersByTimeAsync(10_000));
      await act(() => new Promise<void>((resolve) => setTimeout(resolve, 80)));
      // 新分钟的请求已发出（start 变了）但还没返回。
      const starts = new Set(calls.map((c) => c.searchParams.get("start")));
      expect(starts.size).toBeGreaterThan(1);
      expect(firstStart).toBeTruthy();
      expect(screen.getByTestId("metric-total").textContent).toContain("120");
      expect(
        screen
          .queryAllByRole("status")
          .map((e) => e.getAttribute("aria-label")),
      ).toEqual([]);
      const ranking = screen.getByRole("region", { name: "排名" });
      expect(within(ranking).queryByRole("status")).toBeNull();
    } finally {
      release();
    }
  });

  it("still shows the loading state when the filters change, not the previous result", async () => {
    useBackend();
    const user = userEvent.setup();
    renderApp("/?tz=UTC");
    await screen.findByTestId("metric-total");
    let release: () => void = () => undefined;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    server.use(
      http.get(`${BASE}/usage/aggregate`, async () => {
        await gate;
        return HttpResponse.json({});
      }),
    );
    try {
      await user.click(screen.getByRole("button", { name: "最近七天" }));
      await screen.findByRole("status", { name: "趋势加载中" });
      expect(screen.queryByTestId("metric-total")).toBeNull();
    } finally {
      release();
    }
  });

  it("a failing region does not wipe the others", async () => {
    useBackend();
    server.use(
      http.get(`${BASE}/usage/rankings`, () => errorResponse(500, "INTERNAL")),
    );
    renderApp("/?tz=UTC");
    expect((await screen.findByTestId("metric-total")).textContent).toContain(
      "120",
    );
    const ranking = await screen.findByRole("region", { name: "排名" });
    await within(ranking).findByText(/服务暂不可用/);
  });

  it("shows actionable text for TOO_MANY_POINTS without changing a fitting explicit granularity", async () => {
    const calls = useBackend({
      aggregate: () =>
        HttpResponse.json(
          {
            error: {
              code: "TOO_MANY_POINTS",
              message: "x",
              details: { max_points: 1000, suggested_granularity: "day" },
            },
          },
          { status: 400 },
        ),
    });
    renderApp("/?tz=UTC&granularity=hour&range=today");
    await screen.findAllByText(/趋势点数超过上限 1000.*天/);
    // 显式的“小时”在今天范围内放得下，主趋势不改粒度（其余是固定粒度的附带查询）。
    const hourly = calls.filter(
      (c) => c.searchParams.get("granularity") === "hour",
    );
    expect(hourly.length).toBeGreaterThan(0);
    expect(
      calls.some((c) => c.searchParams.get("granularity") === "auto"),
    ).toBe(false);
  });

  it("lets the server choose when an explicit granularity is too fine for the range", async () => {
    const calls = useBackend();
    renderApp("/?tz=UTC&granularity=minute&range=7d");
    await screen.findByTestId("metric-total");
    expect(mainCalls(calls).length).toBeGreaterThan(0);
    expect(
      mainCalls(calls).every(
        (c) => c.searchParams.get("granularity") === "auto",
      ),
    ).toBe(true);
    expect(screen.queryByText(/趋势点数超过上限/)).toBeNull();
  });
});
