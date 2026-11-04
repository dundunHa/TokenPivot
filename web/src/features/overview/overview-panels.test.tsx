import { describe, expect, it } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import { renderApp } from "@/test/render";
import { http, HttpResponse, server, sessionBody } from "@/test/server";

const WS = "0198f2b0-0000-7000-8000-0000000000aa";
const BASE = `/api/v1/workspaces/${WS}`;

const metric = (v: string | null) => ({
  value: v,
  known_count: v === null ? "0" : "1",
  estimated_count: "0",
  missing_count: "0",
});
const stats = (extra?: {
  cacheRead?: string | null;
  cacheWrite?: string | null;
  reasoning?: string | null;
}) => ({
  request_count: "7",
  input_tokens: metric("1000"),
  output_tokens: metric("500"),
  total_tokens: metric("1500"),
  cache_read_tokens: metric(extra?.cacheRead ?? null),
  cache_write_tokens: metric(extra?.cacheWrite ?? null),
  reasoning_tokens: metric(extra?.reasoning ?? null),
});

function useBackend(extra?: Parameters<typeof stats>[0]) {
  const rateModels: (string | null)[] = [];
  const aggregateGran: (string | null)[] = [];
  server.use(
    http.get("/api/v1/auth/session", () =>
      HttpResponse.json(sessionBody("a@example.com", "2999-01-01T00:00:00Z")),
    ),
    http.get("/api/v1/workspaces", () =>
      HttpResponse.json([{ workspace_id: WS, name: "Alpha" }]),
    ),
    http.get(`${BASE}/usage/options`, () =>
      HttpResponse.json({ providers: ["anthropic"], models: ["m1", "m2"] }),
    ),
    http.get(`${BASE}/projects`, () => HttpResponse.json([])),
    http.get(`${BASE}/usage/aggregate`, ({ request }) => {
      aggregateGran.push(new URL(request.url).searchParams.get("granularity"));
      return HttpResponse.json({
        range: {
          start: "2026-10-28T00:00:00Z",
          end: "2026-10-28T03:00:00Z",
          as_of: "2026-10-28T03:00:00Z",
          retention_cutoff: "2025-10-28T00:00:00Z",
          timezone: "UTC",
          granularity: "hour",
        },
        totals: stats(extra),
        points: [
          {
            bucket_start: "2026-10-28T01:00:00Z",
            bucket_end: "2026-10-28T02:00:00Z",
            partial: false,
            stats: stats(extra),
          },
          {
            bucket_start: "2026-10-28T02:00:00Z",
            bucket_end: "2026-10-28T03:00:00Z",
            partial: false,
            stats: {
              ...stats(extra),
              total_tokens: metric("4500"),
            },
          },
        ],
      });
    }),
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
    http.get(`${BASE}/usage-rate`, ({ request }) => {
      const model = new URL(request.url).searchParams.get("model");
      rateModels.push(model);
      const value = model === "m1" ? "6000" : model === "m2" ? "3000" : "9000";
      return HttpResponse.json({
        start: "2026-10-28T01:59:00Z",
        end: "2026-10-28T02:00:00Z",
        as_of: "2026-10-28T02:00:00Z",
        window_seconds: 60,
        unit: "tokens/min",
        total_tokens: {
          value,
          event_count: 3,
          known_count: 3,
          estimated_count: 0,
          missing_count: 0,
        },
        window_last_received_at: "2026-10-28T01:59:59Z",
      });
    }),
    http.get(`${BASE}/usage/rankings`, ({ request }) => {
      const by = new URL(request.url).searchParams.get("group_by");
      return HttpResponse.json({
        group_by: by,
        overall: stats(extra),
        groups: ["m1", "m2"].map((model, i) => ({
          key:
            by === "model"
              ? { provider: "anthropic", model }
              : { provider: "anthropic", model },
          share: i === 0 ? 0.6 : 0.4,
          stats: stats(extra),
        })),
        other: null,
        next_cursor: null,
      });
    }),
  );
  return { rateModels, aggregateGran };
}

describe("overview panels", () => {
  it("shows cache hit rate and the cache / reasoning subsets", async () => {
    useBackend({ cacheRead: "600", cacheWrite: "100", reasoning: "50" });
    renderApp("/?tz=UTC");
    expect((await screen.findByTestId("cache-hit")).textContent).toBe("60%");
    expect(screen.getByTestId("comp-cache-read").textContent).toContain(
      "占输入 60%",
    );
    expect(screen.getByTestId("comp-cache-write").textContent).toContain(
      "占输入 10%",
    );
    expect(screen.getByTestId("comp-reasoning").textContent).toContain(
      "占输出 10%",
    );
  });

  it("says there is no data when no source reports cache or reasoning", async () => {
    useBackend();
    renderApp("/?tz=UTC");
    const card = await screen.findByRole("region", { name: "缓存与推理" });
    expect(within(card).getByText("暂无数据")).toBeTruthy();
    expect(screen.queryByTestId("cache-hit")).toBeNull();
  });

  it("lists the top models by live rate in tokens/s, highest first", async () => {
    const seen = useBackend();
    renderApp("/?tz=UTC");
    const rows = await screen.findAllByTestId("model-rate");
    await waitFor(() =>
      expect(
        screen.getAllByTestId("model-rate").map((r) => r.textContent),
      ).toEqual([expect.stringContaining("m1"), expect.stringContaining("m2")]),
    );
    expect(rows).toHaveLength(2);
    const texts = screen.getAllByTestId("model-rate").map((r) => r.textContent);
    expect(texts[0]).toContain("100");
    expect(texts[1]).toContain("50");
    expect(seen.rateModels).toEqual(expect.arrayContaining(["m1", "m2"]));
  });

  it("draws the weekly activity heatmap with its peak", async () => {
    const seen = useBackend();
    renderApp("/?tz=UTC");
    expect((await screen.findByTestId("heat-peak")).textContent).toContain(
      "02:00",
    );
    expect(screen.getByTestId("heatmap")).toBeTruthy();
    // 热力图固定用小时粒度，与页面所选粒度无关。
    expect(seen.aggregateGran).toContain("hour");
  });
});
