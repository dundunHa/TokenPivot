import { describe, expect, it } from "vitest";
import type { AggregatePoint, MetricStat } from "../overviewApi";
import { buildHeatmap } from "./heatmap";

const metric = (value: string | null): MetricStat => ({
  value,
  known: value === null ? "0" : "1",
  estimated: "0",
  missing: "0",
});
const point = (
  startIso: string,
  tokens: string | null,
  requests = "1",
): AggregatePoint => ({
  bucketStart: startIso,
  bucketEnd: startIso,
  partial: false,
  stats: {
    requestCount: requests,
    input: metric(null),
    output: metric(null),
    total: metric(tokens),
    cacheRead: metric(null),
    cacheWrite: metric(null),
    reasoning: metric(null),
  },
});

describe("buildHeatmap", () => {
  it("places hourly buckets by local date and hour and finds the peak", () => {
    const map = buildHeatmap(
      [
        point("2026-11-03T14:00:00Z", "100"),
        point("2026-11-03T15:00:00Z", "300", "4"),
        point("2026-11-04T01:00:00Z", "200"),
      ],
      "Asia/Shanghai",
    );
    // 上海 UTC+8：14:00Z=22:00、15:00Z=23:00（11-03），01:00Z=09:00（11-04）。
    expect(map.days.map((d) => d.key)).toEqual(["2026-11-03", "2026-11-04"]);
    expect(map.days[0]?.cells[22]?.tokens).toBe(100);
    expect(map.days[0]?.cells[23]).toEqual({ tokens: 300, requests: 4 });
    expect(map.days[1]?.cells[9]?.tokens).toBe(200);
    expect(map.max).toBe(300);
    expect(map.peak).toMatchObject({ hour: 23, tokens: 300 });
    expect(map.days[0]?.label).toContain("11-03");
  });

  it("keeps missing hours empty and a known zero distinct from no data", () => {
    const map = buildHeatmap(
      [point("2026-11-03T10:00:00Z", "0"), point("2026-11-03T11:00:00Z", null)],
      "UTC",
    );
    const cells = map.days[0]?.cells ?? [];
    expect(cells[10]?.tokens).toBe(0);
    expect(cells[11]?.tokens).toBeNull();
    expect(cells[12]?.tokens).toBeNull();
    expect(map.max).toBe(0);
    expect(map.peak).toBeNull();
  });

  it("sums a repeated daylight-saving hour into one cell and keeps only 7 days", () => {
    // 美国东部 2026-11-01 回拨：本地 01:00 出现两次。
    const dst = buildHeatmap(
      [
        point("2026-11-01T05:00:00Z", "10"),
        point("2026-11-01T06:00:00Z", "20"),
      ],
      "America/New_York",
    );
    expect(dst.days[0]?.cells[1]?.tokens).toBe(30);

    const many = buildHeatmap(
      Array.from({ length: 9 }, (_, i) =>
        point(`2026-11-0${i + 1}T12:00:00Z`, "5"),
      ),
      "UTC",
    );
    expect(many.days).toHaveLength(7);
    expect(many.days[0]?.key).toBe("2026-11-03");
  });

  it("returns an empty matrix for no points", () => {
    expect(buildHeatmap([], "UTC")).toEqual({ days: [], max: 0, peak: null });
  });
});
