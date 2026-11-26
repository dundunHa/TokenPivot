import { describe, expect, it } from "vitest";
import type { SpeedData, SpeedMinute } from "./api";
import { buildSpeedRows } from "./speedSeries";

const minute = (at: string, speed: string, samples = 1): SpeedMinute => ({
  bucketStart: at,
  generationSpeedTps: speed,
  sampleCount: samples,
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
  it("plots the server minute mean as is and leaves silent minutes empty, not zero", () => {
    const { rows, hasData } = buildSpeedRows(
      [
        total(
          data([
            minute("2026-11-04T10:58:00Z", "300.000000", 2),
            minute("2026-11-04T11:00:00Z", "250.123457"),
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
    expect(by["11:00"]).toBe(250.123457);
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

  it("keeps a real zero speed as zero while an absent minute stays empty", () => {
    const { rows, hasData } = buildSpeedRows(
      [total(data([minute("2026-11-04T10:58:00Z", "0")]))],
      "UTC",
    );
    expect(rows.find((r) => r.label === "10:58")?.total).toBe(0);
    expect(rows.find((r) => r.label === "10:59")?.total).toBeNull();
    expect(hasData).toBe(true);
  });

  it("does not recompute speed from request time: the point value is all it uses", () => {
    // 点里没有输出/耗时合计；总耗时再长也不会改变服务端给出的分钟均值。
    const { rows } = buildSpeedRows(
      [total(data([minute("2026-11-04T10:58:00Z", "30.000000", 2)]))],
      "UTC",
    );
    expect(rows.find((r) => r.label === "10:58")?.total).toBe(30);
  });

  it("returns nothing before any series has loaded", () => {
    expect(buildSpeedRows([total(undefined)], "UTC")).toMatchObject({
      rows: [],
      hasData: false,
    });
  });

  it("labels minutes in the requested time zone", () => {
    const { rows } = buildSpeedRows(
      [total(data([minute("2026-11-04T11:00:00Z", "60.000000")]))],
      "Asia/Shanghai",
    );
    expect(rows.at(-1)?.label).toBe("19:00");
  });
});
