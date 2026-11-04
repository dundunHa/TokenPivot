import { describe, expect, it } from "vitest";
import type {
  AggregateData,
  MetricStat,
} from "@/features/overview/overviewApi";
import { buildRateRows } from "./rateSeries";

const metric = (value: string | null): MetricStat => ({
  value,
  known: value === null ? "0" : "1",
  estimated: "0",
  missing: "0",
});
const stats = (tokens: string | null) => ({
  requestCount: "1",
  input: metric(null),
  output: metric(null),
  total: metric(tokens),
  cacheRead: metric(null),
  cacheWrite: metric(null),
  reasoning: metric(null),
});
const data = (
  asOf: string,
  points: { at: string; tokens: string | null; partial?: boolean }[],
): AggregateData => ({
  range: {
    start: "2026-11-04T10:57:00Z",
    end: asOf,
    asOf,
    retentionCutoff: "",
    timezone: "UTC",
    granularity: "minute",
  },
  totals: stats("0"),
  points: points.map((p) => ({
    bucketStart: p.at,
    bucketEnd: p.at,
    partial: p.partial ?? false,
    stats: stats(p.tokens),
  })),
});

describe("buildRateRows", () => {
  it("turns minute totals into tokens/s and fills silent minutes with zero", () => {
    const total = data("2026-11-04T11:00:30Z", [
      { at: "2026-11-04T10:58:00Z", tokens: "6000" },
      { at: "2026-11-04T11:00:00Z", tokens: "3000", partial: true },
    ]);
    const { rows, hasData } = buildRateRows(
      [{ key: "total", name: "总量", color: "#fff", data: total }],
      "UTC",
    );
    const by = Object.fromEntries(rows.map((r) => [r.label, r.total]));
    expect(by["10:57"]).toBe(0);
    expect(by["10:58"]).toBe(100); // 6000 ÷ 60
    expect(by["10:59"]).toBe(0);
    expect(by["11:00"]).toBe(100); // 当前分钟：3000 ÷ 已过 30 秒
    expect(hasData).toBe(true);
  });

  it("leaves a too-short current minute and an all-missing bucket empty, not zero", () => {
    const total = data("2026-11-04T11:00:10Z", [
      { at: "2026-11-04T10:58:00Z", tokens: null },
      { at: "2026-11-04T11:00:00Z", tokens: "500", partial: true },
    ]);
    const { rows } = buildRateRows(
      [{ key: "total", name: "总量", color: "#fff", data: total }],
      "UTC",
    );
    const by = Object.fromEntries(rows.map((r) => [r.label, r.total]));
    expect(by["10:58"]).toBeNull();
    expect(by["11:00"]).toBeNull();
  });

  it("keeps a series that is still loading empty and reports no data when all zero", () => {
    const loaded = data("2026-11-04T11:00:30Z", []);
    const { rows, hasData } = buildRateRows(
      [
        { key: "total", name: "总量", color: "#fff", data: loaded },
        { key: "m:a", name: "a", color: "#000", data: undefined },
      ],
      "UTC",
    );
    expect(rows.every((r) => r["m:a"] === null)).toBe(true);
    expect(hasData).toBe(false);
  });

  it("returns nothing before any series has loaded", () => {
    expect(
      buildRateRows(
        [{ key: "total", name: "总量", color: "#fff", data: undefined }],
        "UTC",
      ),
    ).toMatchObject({ rows: [], hasData: false });
  });

  it("labels minutes in the requested time zone", () => {
    const total = data("2026-11-04T11:00:30Z", [
      { at: "2026-11-04T11:00:00Z", tokens: "60", partial: true },
    ]);
    const { rows } = buildRateRows(
      [{ key: "total", name: "总量", color: "#fff", data: total }],
      "Asia/Shanghai",
    );
    expect(rows.at(-1)?.label).toBe("19:00");
  });
});
