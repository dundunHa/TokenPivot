import { describe, expect, it } from "vitest";
import type { RateData } from "./api";
import { eventsQueryString } from "./api";
import { DEFAULT_OVERVIEW_FILTERS } from "@/features/overview/overviewFilters";
import {
  describeRate,
  eventsPollOptions,
  formatDuration,
  formatStatus,
  formatTokenField,
  parseRequestRange,
  POLL_MS,
} from "./model";

const rate = (over: Partial<RateData>): RateData => ({
  start: "s",
  end: "e",
  asOf: "e",
  value: null,
  eventCount: 0,
  knownCount: 0,
  estimatedCount: 0,
  missingCount: 0,
  windowLastReceivedAt: null,
  ...over,
});

describe("request model", () => {
  it("separates unavailable from a real zero", () => {
    expect(formatTokenField({ value: null, quality: "missing" })).toBe(
      "不可用",
    );
    expect(formatTokenField({ value: 0, quality: "known" })).toBe("0");
    expect(formatTokenField({ value: 1234, quality: "estimated" })).toBe(
      "1,234（估算）",
    );
    expect(formatDuration(null)).toBe("耗时不详");
    expect(formatDuration(0)).toBe("0 ms");
    expect(formatStatus(null)).toBe("状态不可用");
  });

  it("describes no events, unavailable, zero, estimated and partial coverage apart", () => {
    expect(describeRate(rate({})).headline).toBe("窗口内无事件");
    expect(
      describeRate(rate({ eventCount: 2, missingCount: 2 })).headline,
    ).toBe("用量不可用");
    expect(
      describeRate(rate({ value: "0", eventCount: 1, knownCount: 1 })),
    ).toEqual({ headline: "0", notes: [] });
    const mixed = describeRate(
      rate({
        value: "12000",
        eventCount: 5,
        knownCount: 3,
        estimatedCount: 1,
        missingCount: 1,
      }),
    );
    expect(mixed.headline).toBe("12,000");
    expect(mixed.notes).toEqual(["含估算 1 条", "部分覆盖：缺失 1 条"]);
  });

  it("polls events only on the latest page", () => {
    expect(eventsPollOptions(true).refetchInterval).toBe(POLL_MS);
    expect(eventsPollOptions(false).refetchInterval).toBe(false);
    expect(eventsPollOptions(false).refetchOnWindowFocus).toBe(false);
  });

  it("maps the shared unassigned choice to unattributed and sends no range on page one", () => {
    const q = new URLSearchParams(
      eventsQueryString(
        {
          ...DEFAULT_OVERVIEW_FILTERS,
          timezone: "UTC",
          providers: ["p"],
          projects: ["unassigned", "0198f2b0-0000-7000-8000-0000000000bb"],
        },
        null,
        null,
      ),
    );
    expect(q.getAll("project")).toEqual([
      "unattributed",
      "0198f2b0-0000-7000-8000-0000000000bb",
    ]);
    expect(q.get("limit")).toBe("50");
    expect(q.has("start")).toBe(false);
    expect(q.has("cursor")).toBe(false);
    const paged = new URLSearchParams(
      eventsQueryString(
        { ...DEFAULT_OVERVIEW_FILTERS, timezone: "UTC" },
        "c1",
        null,
      ),
    );
    expect(paged.get("cursor")).toBe("c1");
    expect(paged.has("start")).toBe(false);
  });

  it("sends the same fixed range on every page", () => {
    const range = {
      start: "2026-10-01T00:00:00Z",
      end: "2026-10-02T00:00:00Z",
    };
    const f = { ...DEFAULT_OVERVIEW_FILTERS, timezone: "UTC" };
    for (const cursor of [null, "c1"]) {
      const q = new URLSearchParams(eventsQueryString(f, cursor, range));
      expect(q.get("start")).toBe(range.start);
      expect(q.get("end")).toBe(range.end);
    }
  });

  it("compares the range at microsecond precision across offsets", () => {
    const ok = (a: string, b: string) => "range" in parseRequestRange(a, b);
    expect(
      ok("2026-10-01T00:00:00.000001Z", "2026-10-01T00:00:00.000002Z"),
    ).toBe(true);
    expect(
      ok("2026-10-01T00:00:00.000002Z", "2026-10-01T00:00:00.000001Z"),
    ).toBe(false);
    expect(
      ok("2026-10-01T00:00:00.000001Z", "2026-10-01T00:00:00.000001Z"),
    ).toBe(false);
    expect(ok("2026-10-01T00:00:00.5Z", "2026-10-01T00:00:00.500001Z")).toBe(
      true,
    );
    // 同一时刻的不同偏移写法相等；偏移之后的微秒先后仍成立。
    expect(ok("2026-10-01T08:00:00+08:00", "2026-10-01T00:00:00Z")).toBe(false);
    expect(
      ok("2026-10-01T08:00:00.000001+08:00", "2026-10-01T00:00:00.000002Z"),
    ).toBe(true);
    expect(
      ok("2026-10-01T08:00:00.000002+08:00", "2026-10-01T00:00:00.000001Z"),
    ).toBe(false);
    expect(ok("2026-09-30T23:59:59.999999-05:00", "2026-10-01T05:00:00Z")).toBe(
      true,
    );
  });

  it("validates the detail range like the server", () => {
    expect(
      parseRequestRange("2026-10-01T00:00:00.123456Z", "2026-10-02T00:00:00Z"),
    ).toHaveProperty("range");
    for (const [a, b] of [
      ["2026-10-01T00:00:00.1234567Z", "2026-10-02T00:00:00Z"],
      ["2026-10-01T00:00:00", "2026-10-02T00:00:00Z"],
      ["2026-10-02T00:00:00Z", "2026-10-02T00:00:00Z"],
    ] as const) {
      expect(parseRequestRange(a, b)).toHaveProperty("error");
    }
  });
});
