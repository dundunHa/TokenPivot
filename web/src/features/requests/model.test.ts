import { describe, expect, it } from "vitest";
import type { RateData, SpeedSums } from "./api";
import { eventsQueryString } from "./api";
import { DEFAULT_OVERVIEW_FILTERS } from "@/features/overview/overviewFilters";
import {
  describeGenerationMetrics,
  describeSpeed,
  eventsPollOptions,
  formatDuration,
  formatMilliseconds,
  formatSpeed,
  formatSpeedTps,
  formatStatus,
  formatTokenField,
  parseRequestRange,
  POLL_MS,
  speedOf,
  trimDecimal,
  UNAVAILABLE,
} from "./model";

const sums = (over: Partial<SpeedSums>): SpeedSums => ({
  outputTokens: null,
  durationMs: null,
  eventCount: 0,
  measuredCount: 0,
  estimatedCount: 0,
  ...over,
});

const rate = (over: Partial<RateData>): RateData => ({
  start: "s",
  end: "e",
  asOf: "e",
  eventCount: 0,
  windowLastReceivedAt: null,
  generationSpeed: { unit: "tokens/s", value: null, sampleCount: 0 },
  ttft: { unit: "ms", value: null, sampleCount: 0 },
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

  it("trims only the backend decimals without changing the value", () => {
    expect(trimDecimal("49.500000")).toBe("49.5");
    expect(trimDecimal("0.000000")).toBe("0");
    expect(trimDecimal("100")).toBe("100");
    expect(trimDecimal("0.250000")).toBe("0.25");
    expect(trimDecimal("123456.123456")).toBe("123456.123456");
  });

  it("formats per request speed and TTFT, keeping missing apart from zero", () => {
    expect(formatSpeedTps("49.500000")).toBe("49.5 tokens/s");
    expect(formatSpeedTps("0")).toBe("0 tokens/s");
    expect(formatSpeedTps(null)).toBe(UNAVAILABLE);
    expect(formatMilliseconds(8000)).toBe("8000 ms");
    expect(formatMilliseconds(0)).toBe("0 ms");
    expect(formatMilliseconds("8000.000000")).toBe("8000 ms");
    expect(formatMilliseconds(null)).toBe(UNAVAILABLE);
  });

  it("describes the rolling averages with their own sample counts", () => {
    const view = describeGenerationMetrics(
      rate({
        eventCount: 3,
        generationSpeed: {
          unit: "tokens/s",
          value: "49.500000",
          sampleCount: 1,
        },
        ttft: { unit: "ms", value: "8000.000000", sampleCount: 2 },
      }),
    );
    expect(view.speed).toEqual({
      headline: "49.5 tokens/s",
      samples: "1 个有效样本",
    });
    expect(view.ttft).toEqual({
      headline: "8000 ms",
      samples: "2 个有效样本",
    });
    expect(view.eventCount).toBe("窗口内 3 条事件");
  });

  it("reports no events apart from a window without valid samples", () => {
    const empty = describeGenerationMetrics(rate({}));
    expect(empty.speed.headline).toBe(UNAVAILABLE);
    expect(empty.speed.samples).toBe("0 个有效样本");
    expect(empty.eventCount).toBe("窗口内无事件");

    const sampled = describeGenerationMetrics(
      rate({
        eventCount: 5,
        generationSpeed: {
          unit: "tokens/s",
          value: "0",
          sampleCount: 2,
        },
      }),
    );
    expect(sampled.speed.headline).toBe("0 tokens/s");
    expect(sampled.ttft.headline).toBe(UNAVAILABLE);
  });

  it("never turns zero samples or a null average into zero", () => {
    const view = describeGenerationMetrics(
      rate({
        eventCount: 4,
        generationSpeed: {
          unit: "tokens/s",
          value: "49.500000",
          sampleCount: 0,
        },
        ttft: { unit: "ms", value: null, sampleCount: 3 },
      }),
    );
    expect(view.speed.headline).toBe(UNAVAILABLE);
    expect(view.ttft.headline).toBe(UNAVAILABLE);
  });

  it("keeps the two sample counts independent when only one metric has samples", () => {
    const view = describeGenerationMetrics(
      rate({
        eventCount: 2,
        generationSpeed: {
          unit: "tokens/s",
          value: null,
          sampleCount: 0,
        },
        ttft: { unit: "ms", value: "8000.000000", sampleCount: 1 },
      }),
    );
    expect(view.speed.samples).toBe("0 个有效样本");
    expect(view.ttft.samples).toBe("1 个有效样本");
    expect(view.ttft.headline).toBe("8000 ms");
  });

  it("computes speed as output tokens over request time, not over wall-clock time", () => {
    // 3,000 输出 token 用了 10 s 请求耗时 → 300 tokens/s，与窗口里有多少空闲时间无关。
    expect(speedOf({ outputTokens: "3000", durationMs: "10000" })).toBe(300);
    expect(speedOf({ outputTokens: "0", durationMs: "500" })).toBe(0);
    expect(speedOf({ outputTokens: null, durationMs: null })).toBeNull();
    expect(speedOf({ outputTokens: "10", durationMs: "0" })).toBeNull();
    expect(formatSpeed(0)).toBe("0");
    expect(formatSpeed(7.04)).toBe("7");
    expect(formatSpeed(12.34)).toBe("12.3");
    expect(formatSpeed(1234.6)).toBe("1,235");
  });

  it("describes no events, unmeasurable, zero, estimated and partial coverage apart", () => {
    expect(describeSpeed(sums({}))).toEqual({
      headline: "—",
      notes: ["暂无数据"],
    });
    // 有请求但都没有耗时（旧数据）：不显示 0，说明原因。
    expect(describeSpeed(sums({ eventCount: 2 }))).toEqual({
      headline: "—",
      notes: ["2 条请求无耗时，无法计算"],
    });
    expect(
      describeSpeed(
        sums({
          outputTokens: "0",
          durationMs: "800",
          eventCount: 1,
          measuredCount: 1,
        }),
      ),
    ).toEqual({ headline: "0", notes: [] });
    const mixed = describeSpeed(
      sums({
        outputTokens: "20000",
        durationMs: "100000",
        eventCount: 5,
        measuredCount: 3,
        estimatedCount: 1,
      }),
    );
    expect(mixed.headline).toBe("200");
    expect(mixed.notes).toEqual(["含估算 1 条", "部分覆盖：2 条无耗时"]);
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
