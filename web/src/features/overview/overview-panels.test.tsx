import { describe, expect, it } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
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

function useBackend(
  extra?: Parameters<typeof stats>[0],
  /** 按模型的排行统计覆盖；缺省沿用同一份总量。 */
  byModel?: Record<string, Parameters<typeof stats>[0]>,
) {
  const speedModels: (string | null)[] = [];
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
    http.get(`${BASE}/usage-rate`, () =>
      HttpResponse.json({
        start: "2026-10-28T01:59:00Z",
        end: "2026-10-28T02:00:00Z",
        as_of: "2026-10-28T02:00:00Z",
        event_count: 5,
        window_last_received_at: "2026-10-28T01:59:59Z",
        generation_speed: {
          unit: "tokens/s",
          value: "90.000000",
          sample_count: 3,
        },
        ttft: { unit: "ms", value: "420.000000", sample_count: 4 },
      }),
    ),
    http.get(`${BASE}/usage-speed`, ({ request }) => {
      const model = new URL(request.url).searchParams.get("model");
      speedModels.push(model);
      // 当前窗口合计保持原样；分钟点是服务端算好的均值：m1 = 100，m2 = 50，总体 = 90 tok/s。
      const output =
        model === "m1" ? "10000" : model === "m2" ? "5000" : "9000";
      const minuteSpeed =
        model === "m1"
          ? "100.000000"
          : model === "m2"
            ? "50.000000"
            : "90.000000";
      return HttpResponse.json({
        as_of: "2026-10-28T02:00:30Z",
        series_start: "2026-10-28T01:00:00Z",
        window_seconds: 60,
        unit: "tokens/s",
        current: {
          output_tokens: output,
          duration_ms: "100000",
          event_count: 3,
          measured_count: 3,
          estimated_count: 0,
        },
        points: [
          {
            bucket_start: "2026-10-28T01:58:00Z",
            generation_speed_tps: minuteSpeed,
            sample_count: 3,
          },
        ],
      });
    }),
    http.get(`${BASE}/usage/rankings`, ({ request }) => {
      const by = new URL(request.url).searchParams.get("group_by");
      return HttpResponse.json({
        group_by: by,
        overall: stats(extra),
        groups: ["m1", "m2"].map((model, i) => ({
          key:
            by === "project"
              ? { project_id: `proj-${model}` }
              : { provider: "anthropic", model },
          share: i === 0 ? 0.6 : 0.4,
          stats: stats(byModel?.[model] ?? extra),
        })),
        other: null,
        next_cursor: null,
      });
    }),
  );
  return { speedModels, aggregateGran };
}

describe("overview panels", () => {
  it("shows cache hit rate in the KPI strip and per-model cache hit / 60s speed in the matrix", async () => {
    const seen = useBackend(
      { cacheRead: "600", cacheWrite: "100", reasoning: "50" },
      {
        m1: { cacheRead: "600", cacheWrite: "100" },
        m2: { cacheRead: "200", cacheWrite: "250" },
      },
    );
    const user = userEvent.setup();
    renderApp("/?tz=UTC");

    const cacheKpi = await screen.findByTestId("metric-cache-hit");
    expect(cacheKpi.textContent).toContain("60.0%");
    expect(cacheKpi.textContent).toContain("读 600");
    expect(cacheKpi.textContent).toContain("写 100");

    await user.click(screen.getByRole("tab", { name: "模型" }));
    const ranking = await screen.findByRole("region", { name: "排名" });
    await waitFor(() =>
      expect(
        within(ranking)
          .getAllByTestId("ranking-cache-hit")
          .map((c) => c.textContent),
      ).toEqual(["60%", "20%"]),
    );
    await waitFor(() =>
      expect(
        within(ranking)
          .getAllByTestId("ranking-speed")
          .map((c) => c.textContent?.trim()),
      ).toEqual(["100 tok/s", "50 tok/s"]),
    );
    expect(seen.speedModels).toEqual(expect.arrayContaining(["m1", "m2"]));
  });

  it("shows placeholder dashes when no source reports cache or reasoning", async () => {
    useBackend();
    renderApp("/?tz=UTC");
    const cacheKpi = await screen.findByTestId("metric-cache-hit");
    expect(cacheKpi.textContent).toContain("—");
    const ranking = await screen.findByRole("region", { name: "排名" });
    await waitFor(() =>
      expect(
        within(ranking)
          .getAllByTestId("ranking-cache-hit")
          .every((c) => c.textContent === "—"),
      ).toBe(true),
    );
  });

  it("shows overall generation speed, fastest model and TTFT in the KPI strip", async () => {
    const seen = useBackend();
    renderApp("/?tz=UTC");
    const speedKpi = await screen.findByTestId("metric-speed");
    await waitFor(() => {
      expect(speedKpi.textContent).toContain("90");
      expect(speedKpi.textContent).toContain("最快 m1 100 tok/s");
    });
    const ttftKpi = screen.getByTestId("metric-ttft");
    expect(ttftKpi.textContent).toContain("420");
    expect(seen.speedModels).toEqual(expect.arrayContaining(["m1", "m2"]));
  });

  it("draws the per-minute generation speed trend from the server minute means, for the total and each model", async () => {
    const seen = useBackend();
    renderApp("/?tz=UTC");
    const card = await screen.findByRole("region", { name: "生成速度趋势" });
    // 总体 + 两个模型各取一条速度序列（来自 usage-speed，分钟聚合表没有首末有效输出计时）。
    await waitFor(() =>
      expect(seen.speedModels).toEqual(
        expect.arrayContaining([null, "m1", "m2"]),
      ),
    );
    await waitFor(() =>
      expect(within(card).getByLabelText("图例")).toBeTruthy(),
    );
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
