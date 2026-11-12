import { describe, expect, it } from "vitest";
import type { SpeedData, SpeedMinute } from "./api";
import { buildSpeedRows } from "./speedSeries";

const minute = (
  at: string,
  output: string | null,
  duration: string | null,
): SpeedMinute => ({
  bucketStart: at,
  outputTokens: output,
  durationMs: duration,
  measuredCount: output === null ? 0 : 1,
  estimatedCount: 0,
});
const data = (points: SpeedMinute[]): SpeedData => ({
  asOf: "2026-11-04T11:00:30Z",
  seriesStart: "2026-11-04T10:57:00Z",
  current: {
    outputTokens: null,
    durationMs: null,
    eventCount: 0,
    measuredCount: 0,
    estimatedCount: 0,
  },
  points,
});
const total = (d: SpeedData | undefined) => ({
  key: "total",
  name: "总体",
  color: "#fff",
  data: d,
});

describe("buildSpeedRows", () => {
  it("turns minute sums into output ÷ request time and leaves silent minutes empty, not zero", () => {
    const { rows, hasData } = buildSpeedRows(
      [
        total(
          data([
            minute("2026-11-04T10:58:00Z", "6000", "20000"), // 300 tok/s
            minute("2026-11-04T11:00:00Z", "500", "2000"), // 250 tok/s
          ]),
        ),
      ],
      "UTC",
    );
    const by = Object.fromEntries(rows.map((r) => [r.label, r.total]));
    expect(rows.map((r) => r.label)).toEqual([
      "10:57",
      "10:58",
      "10:59",
      "11:00",
    ]);
    expect(by["10:57"]).toBeNull();
    expect(by["10:58"]).toBe(300);
    expect(by["10:59"]).toBeNull();
    expect(by["11:00"]).toBe(250);
    expect(hasData).toBe(true);
  });

  it("keeps a series that is still loading empty and reports no data when nothing is measurable", () => {
    const { rows, hasData } = buildSpeedRows(
      [
        total(data([])),
        { key: "m:a", name: "a", color: "#000", data: undefined },
      ],
      "UTC",
    );
    expect(rows.every((r) => r["m:a"] === null)).toBe(true);
    expect(hasData).toBe(false);
  });

  it("ignores a minute whose request time is zero instead of dividing by it", () => {
    const { rows, hasData } = buildSpeedRows(
      [total(data([minute("2026-11-04T10:58:00Z", "10", "0")]))],
      "UTC",
    );
    expect(rows.find((r) => r.label === "10:58")?.total).toBeNull();
    expect(hasData).toBe(false);
  });

  it("returns nothing before any series has loaded", () => {
    expect(buildSpeedRows([total(undefined)], "UTC")).toMatchObject({
      rows: [],
      hasData: false,
    });
  });

  it("labels minutes in the requested time zone", () => {
    const { rows } = buildSpeedRows(
      [total(data([minute("2026-11-04T11:00:00Z", "60", "1000")]))],
      "Asia/Shanghai",
    );
    expect(rows.at(-1)?.label).toBe("19:00");
  });
});
