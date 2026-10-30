import { describe, expect, it } from "vitest";
import {
  parseFilters,
  resolveRange,
  toSearchParams,
  validateCustomRange,
} from "./overviewFilters";
import type { AggregateData, AggregatePoint, UsageStats } from "./overviewApi";
import { buildMetricCards, buildTrend } from "./overviewView";

const metric = (value: string | null, missing = "0", estimated = "0") => ({
  value,
  known: value === null ? "0" : "1",
  estimated,
  missing,
});
const stats = (
  i: string | null,
  o: string | null,
  t: string | null,
  req = "1",
): UsageStats => ({
  requestCount: req,
  input: metric(i),
  output: metric(o),
  total: metric(t),
});
const point = (
  s: string,
  e: string,
  st: UsageStats,
  partial = false,
): AggregatePoint => ({
  bucketStart: s,
  bucketEnd: e,
  partial,
  stats: st,
});
const agg = (points: AggregatePoint[]): AggregateData => ({
  range: {
    start: "",
    end: "",
    asOf: "",
    retentionCutoff: "",
    timezone: "UTC",
    granularity: "hour",
  },
  totals: stats("0", "0", "0", "0"),
  points,
});

describe("URL filters", () => {
  it("restores a shared link and round-trips with tz always written", () => {
    const url = new URLSearchParams(
      "provider=b&provider=a&range=7d&granularity=day&tz=Asia/Shanghai&x=1",
    );
    const r = parseFilters(url, "UTC");
    if (!r.ok) throw new Error(r.reason);
    expect(r.filters.providers).toEqual(["a", "b"]);
    expect(r.timezoneImplied).toBe(false);
    const out = toSearchParams(r.filters, url);
    expect(out.getAll("provider")).toEqual(["a", "b"]);
    expect(out.get("tz")).toBe("Asia/Shanghai");
    expect(out.get("x")).toBe("1");
  });

  it("defaults to today+hour and marks an implied timezone", () => {
    const r = parseFilters(new URLSearchParams(), "UTC");
    if (!r.ok) throw new Error(r.reason);
    expect(r.filters.range).toBe("today");
    expect(r.filters.granularity).toBe("hour");
    expect(r.timezoneImplied).toBe(true);
  });

  it.each([
    "range=bogus",
    "granularity=week",
    "tz=Not/AZone",
    "range=7d&range=1h",
    "project=not-a-uuid",
    "range=custom",
    "start=2026-10-28T00:00:00Z",
  ])("rejects invalid %s", (q) => {
    expect(parseFilters(new URLSearchParams(q), "UTC").ok).toBe(false);
  });
});

describe("time ranges", () => {
  it("resolves today and yesterday in the chosen timezone", () => {
    const now = Date.parse("2026-10-28T20:30:00Z"); // Shanghai 10-29 04:30
    const base = {
      providers: [],
      models: [],
      projects: [],
      granularity: "hour" as const,
      timezone: "Asia/Shanghai",
      start: null,
      end: null,
    };
    const today = resolveRange({ ...base, range: "today" }, now);
    expect(new Date(today.startMs).toISOString()).toBe(
      "2026-10-28T16:00:00.000Z",
    );
    expect(today.endMs).toBeNull();
    const y = resolveRange({ ...base, range: "yesterday" }, now);
    expect(new Date(y.startMs).toISOString()).toBe("2026-10-27T16:00:00.000Z");
    expect(new Date(y.endMs ?? 0).toISOString()).toBe(
      "2026-10-28T16:00:00.000Z",
    );
  });

  it("validates custom range offsets against the timezone (DST aware)", () => {
    const tz = "America/New_York";
    expect(
      validateCustomRange(
        "2026-11-01T01:30:00-04:00",
        "2026-11-01T01:30:00-05:00",
        tz,
      ),
    ).toBeNull();
    expect(
      validateCustomRange(
        "2026-03-08T02:30:00-05:00",
        "2026-03-08T04:00:00-04:00",
        tz,
      ),
    ).not.toBeNull();
    expect(
      validateCustomRange(
        "2026-10-28T10:00:00Z",
        "2026-10-28T09:00:00Z",
        "UTC",
      ),
    ).toContain("早于");
    expect(
      validateCustomRange(
        "2026-10-28T10:00:30Z",
        "2026-10-28T11:00:00Z",
        "UTC",
      ),
    ).toContain("分钟");
  });
});

describe("metrics and trend", () => {
  it("keeps zero, missing and request count distinct", () => {
    const cards = buildMetricCards(stats("0", null, null, "0"));
    const by = Object.fromEntries(cards.map((c) => [c.id, c]));
    expect(by.input?.value).toBe("0");
    expect(by.input?.note).toBeNull();
    expect(by.output?.value).toBeNull();
    expect(by.output?.note).toContain("缺失");
    expect(by.requests?.value).toBe("0");
  });

  it("inserts a null gap between non-adjacent buckets and keeps real zeros", () => {
    const t = buildTrend(
      agg([
        point(
          "2026-10-28T00:00:00Z",
          "2026-10-28T01:00:00Z",
          stats("0", "0", "0"),
        ),
        point(
          "2026-10-28T03:00:00Z",
          "2026-10-28T04:00:00Z",
          stats("5", "5", "10"),
        ),
      ]),
    );
    expect(t.points.map((p) => p.gap)).toEqual([false, true, false]);
    const gap = t.points[1];
    expect([gap?.input, gap?.output, gap?.total]).toEqual([null, null, null]);
    expect(t.points[0]?.total).toBe(0);
    expect(t.stackable).toBe(true);
  });

  it("does not stack when total differs from input+output or a part is missing", () => {
    const mismatch = buildTrend(
      agg([
        point(
          "2026-10-28T00:00:00Z",
          "2026-10-28T01:00:00Z",
          stats("1", "1", "5"),
        ),
      ]),
    );
    expect(mismatch.stackable).toBe(false);
    const missing = buildTrend(
      agg([
        point(
          "2026-10-28T00:00:00Z",
          "2026-10-28T01:00:00Z",
          stats("1", null, "1"),
        ),
      ]),
    );
    expect(missing.stackable).toBe(false);
    expect(missing.points[0]?.output).toBeNull();
  });

  it("flags completed buckets with no usable metrics as unavailable", () => {
    const t = buildTrend(
      agg([
        point(
          "2026-10-28T00:00:00Z",
          "2026-10-28T01:00:00Z",
          stats(null, null, null, "0"),
        ),
      ]),
    );
    expect(t.points[0]?.unavailable).toBe(true);
  });
});

describe("DST edge cases", () => {
  const base = {
    providers: [],
    models: [],
    projects: [],
    granularity: "hour" as const,
    start: null,
    end: null,
  };

  it("picks the earliest repeated midnight (Havana fall-back on 2020-11-01)", () => {
    const now = Date.parse("2020-11-01T15:00:00Z");
    const r = resolveRange(
      { ...base, range: "today", timezone: "America/Havana" },
      now,
    );
    expect(new Date(r.startMs).toISOString()).toBe("2020-11-01T04:00:00.000Z");
    const y = resolveRange(
      { ...base, range: "yesterday", timezone: "America/Havana" },
      Date.parse("2020-11-02T15:00:00Z"),
    );
    expect(new Date(y.startMs).toISOString()).toBe("2020-11-01T04:00:00.000Z");
    expect(new Date(y.endMs ?? 0).toISOString()).toBe(
      "2020-11-02T05:00:00.000Z",
    );
  });

  it("uses the transition instant when midnight does not exist (São Paulo 2018-11-04)", () => {
    const now = Date.parse("2018-11-04T15:00:00Z");
    const r = resolveRange(
      { ...base, range: "today", timezone: "America/Sao_Paulo" },
      now,
    );
    expect(new Date(r.startMs).toISOString()).toBe("2018-11-04T03:00:00.000Z");
  });

  it("keeps ordinary days unchanged", () => {
    const r = resolveRange(
      { ...base, range: "today", timezone: "Asia/Shanghai" },
      Date.parse("2026-10-28T20:30:00Z"),
    );
    expect(new Date(r.startMs).toISOString()).toBe("2026-10-28T16:00:00.000Z");
  });

  it("disambiguates repeated local hour labels with the UTC offset", () => {
    const t = buildTrend({
      ...agg([
        point(
          "2026-11-01T05:00:00Z",
          "2026-11-01T06:00:00Z",
          stats("1", "1", "2"),
        ),
        point(
          "2026-11-01T06:00:00Z",
          "2026-11-01T07:00:00Z",
          stats("1", "1", "2"),
        ),
        point(
          "2026-11-01T07:00:00Z",
          "2026-11-01T08:00:00Z",
          stats("1", "1", "2"),
        ),
      ]),
      range: {
        start: "",
        end: "",
        asOf: "",
        retentionCutoff: "",
        timezone: "America/New_York",
        granularity: "hour",
      },
    });
    const labels = t.points.map((p) => p.label);
    expect(new Set(labels).size).toBe(3);
    expect(labels[0]).toContain("-04:00");
    expect(labels[1]).toContain("-05:00");
    expect(labels[2]).not.toMatch(/[+-]\d\d:\d\d$/);
  });
});
