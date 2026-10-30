import { describe, expect, it } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
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
    http.get("/api/v1/workspaces/current", () =>
      HttpResponse.json({ workspace_id: WS, name: "Personal" }),
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
      calls.push(new URL(request.url));
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

describe("overview page", () => {
  it("shows the four metrics from the server and queries with the URL timezone", async () => {
    const calls = useBackend();
    renderApp("/?tz=UTC");
    const total = await screen.findByTestId("metric-total");
    expect(total.textContent).toContain("120");
    expect(screen.getByTestId("metric-requests").textContent).toContain("7");
    expect(calls[0]?.searchParams.get("timezone")).toBe("UTC");
    expect(calls[0]?.searchParams.get("granularity")).toBe("hour");
    expect(calls[0]?.searchParams.has("provider")).toBe(false);
  });

  it("does not send a query for an invalid link and offers a reset", async () => {
    const calls = useBackend();
    const user = userEvent.setup();
    renderApp("/?range=bogus&tz=UTC");
    await screen.findByText(/链接筛选无效/);
    expect(calls).toHaveLength(0);
    await user.click(screen.getByRole("button", { name: "重置筛选" }));
    await screen.findByTestId("metric-total");
    expect(calls).toHaveLength(1);
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

  it("shows actionable text for TOO_MANY_POINTS without changing granularity", async () => {
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
    renderApp("/?tz=UTC&granularity=minute&range=7d");
    await screen.findAllByText(/趋势点数超过上限 1000.*天/);
    expect(
      calls.every((c) => c.searchParams.get("granularity") === "minute"),
    ).toBe(true);
  });
});
