import { describe, expect, it } from "vitest";
import { ApiError } from "@/lib/api";
import { http, HttpResponse, server } from "@/test/server";
import {
  fetchRate,
  fetchRequests,
  type RateData,
  type RequestPage,
} from "./api";

const WS = "0198f2b0-0000-7000-8000-0000000000aa";
const BASE = `/api/v1/workspaces/${WS}`;
const RANGE = {
  start: "2026-10-28T11:00:00Z",
  end: "2026-10-28T12:00:00Z",
  as_of: "2026-10-28T12:00:00Z",
  retention_cutoff: "2026-04-01T12:00:00Z",
};

const tok = (value: number | null, quality = "known") => ({ value, quality });

/** 明细行：over 覆盖或删除新字段，用来模拟新旧后端。 */
const item = (over: Record<string, unknown> = {}) => ({
  id: "e1",
  occurred_at: "2026-10-28T11:59:00Z",
  received_at: "2026-10-28T11:59:01Z",
  provider: "anthropic",
  model: "m1",
  project: null,
  source: { id: "s1", platform: "dsh", instance_key: "laptop" },
  input_tokens: tok(100),
  output_tokens: tok(100),
  total_tokens: tok(200),
  status: "success",
  duration_ms: 1250,
  generation_speed_tps: "49.500000",
  ttft_ms: 8000,
  ...over,
});

const page = (items: unknown[]) => ({
  items,
  next_cursor: null,
  has_more: false,
  ...RANGE,
});

const metric = (
  unit: string,
  value: string | null,
  sampleCount: unknown,
): Record<string, unknown> => ({
  unit,
  value,
  sample_count: sampleCount,
});

const rateBody = (over: Record<string, unknown> = {}) => ({
  start: "2026-10-28T11:59:00Z",
  end: "2026-10-28T12:00:00Z",
  as_of: "2026-10-28T12:00:00Z",
  event_count: 5,
  window_last_received_at: "2026-10-28T11:59:59Z",
  generation_speed: metric("tokens/s", "49.500000", 2),
  ttft: metric("ms", "8000.000000", 3),
  ...over,
});

async function events(items: unknown[]): Promise<RequestPage> {
  server.use(
    http.get(`${BASE}/usage-events`, () => HttpResponse.json(page(items))),
  );
  return fetchRequests(WS, "limit=50", new AbortController().signal);
}

async function rate(body: Record<string, unknown>): Promise<RateData> {
  server.use(http.get(`${BASE}/usage-rate`, () => HttpResponse.json(body)));
  return fetchRate(WS, "", new AbortController().signal);
}

async function failure(pending: Promise<unknown>) {
  const error = await pending.catch((e: unknown) => e);
  expect(error).toBeInstanceOf(ApiError);
  expect((error as ApiError).code).toBe("BAD_RESPONSE");
}

describe("request detail generation metrics", () => {
  it("keeps the decimal string and the integer instead of recomputing", async () => {
    const body = await events([item()]);
    expect(body.items[0]?.generationSpeedTps).toBe("49.500000");
    expect(body.items[0]?.ttftMs).toBe(8000);
  });

  it("treats an explicit null as missing and a missing key as a bad response", async () => {
    const body = await events([
      item({ generation_speed_tps: null, ttft_ms: null }),
    ]);
    expect(body.items[0]?.generationSpeedTps).toBeNull();
    expect(body.items[0]?.ttftMs).toBeNull();
    for (const field of ["generation_speed_tps", "ttft_ms"]) {
      const incomplete = item({ id: "e2" });
      delete (incomplete as Record<string, unknown>)[field];
      await failure(events([incomplete]));
    }
  });

  it("accepts per metric zeros as real observations, not missing", async () => {
    const body = await events([
      item({ generation_speed_tps: "0", ttft_ms: 0 }),
    ]);
    expect(body.items[0]?.generationSpeedTps).toBe("0");
    expect(body.items[0]?.ttftMs).toBe(0);
  });

  it("rejects malformed decimals", async () => {
    for (const value of [
      "49.5.0",
      "49.5000000",
      "49.",
      ".5",
      "00.5",
      "01",
      "-1",
      "1e3",
      " 49.5",
      49.5,
      true,
    ]) {
      await failure(events([item({ generation_speed_tps: value })]));
    }
  });

  it("rejects negative, fractional and unsafe ttft values", async () => {
    for (const value of [-1, 1.5, 2 ** 53, "8000"]) {
      await failure(events([item({ ttft_ms: value })]));
    }
  });

  it("keeps the largest safe ttft", async () => {
    const body = await events([item({ ttft_ms: Number.MAX_SAFE_INTEGER })]);
    expect(body.items[0]?.ttftMs).toBe(Number.MAX_SAFE_INTEGER);
  });
});

describe("current usage rate generation metrics", () => {
  it("parses both averages with their own sample counts", async () => {
    const body = await rate(rateBody());
    expect(body.generationSpeed).toEqual({
      unit: "tokens/s",
      value: "49.500000",
      sampleCount: 2,
    });
    expect(body.ttft).toEqual({
      unit: "ms",
      value: "8000.000000",
      sampleCount: 3,
    });
    expect(body.eventCount).toBe(5);
  });

  it("rejects a body that omits either metric object or the event count", async () => {
    for (const over of [
      { generation_speed: undefined },
      { ttft: undefined },
      { event_count: undefined },
      { event_count: -1 },
    ]) {
      await failure(rate(rateBody(over)));
    }
  });

  it("keeps a null average distinct from a zero average", async () => {
    const body = await rate(
      rateBody({
        generation_speed: metric("tokens/s", null, 0),
        ttft: metric("ms", "0.000000", 1),
      }),
    );
    expect(body.generationSpeed.value).toBeNull();
    expect(body.generationSpeed.sampleCount).toBe(0);
    expect(body.ttft.value).toBe("0.000000");
    expect(body.ttft.sampleCount).toBe(1);
  });

  it("keeps the two sample counts independent", async () => {
    const body = await rate(
      rateBody({
        generation_speed: metric("tokens/s", null, 0),
        ttft: metric("ms", "8000.000000", 2),
      }),
    );
    expect(body.generationSpeed.sampleCount).toBe(0);
    expect(body.ttft.sampleCount).toBe(2);
  });

  it("rejects a wrong unit, a null object and a malformed value", async () => {
    await failure(
      rate(rateBody({ generation_speed: metric("ms", "49.5", 1) })),
    );
    await failure(rate(rateBody({ ttft: metric("tokens/s", "8000", 1) })));
    await failure(rate(rateBody({ generation_speed: null })));
    await failure(rate(rateBody({ ttft: metric("ms", "8000", -1) })));
    await failure(rate(rateBody({ ttft: metric("ms", "8000.0000000", 1) })));
    await failure(rate(rateBody({ ttft: metric("ms", "8e3", 1) })));
    await failure(
      rate(rateBody({ generation_speed: { unit: "tokens/s", value: "49.5" } })),
    );
  });

  it("rejects a non-safe sample count on both metrics", async () => {
    // 超出安全整数范围、小数、负数与错误类型都必须拒绝，null 不是合法的样本数。
    for (const bad of [2 ** 53, 1.5, -1, "2", null, true]) {
      await failure(
        rate(
          rateBody({ generation_speed: metric("tokens/s", "49.500000", bad) }),
        ),
      );
      await failure(rate(rateBody({ ttft: metric("ms", "8000.000000", bad) })));
    }
  });

  it("keeps a zero and the largest safe sample count", async () => {
    const body = await rate(
      rateBody({
        generation_speed: metric(
          "tokens/s",
          "49.500000",
          Number.MAX_SAFE_INTEGER,
        ),
        ttft: metric("ms", "8000.000000", 0),
      }),
    );
    expect(body.generationSpeed.sampleCount).toBe(Number.MAX_SAFE_INTEGER);
    expect(body.ttft.sampleCount).toBe(0);
  });
});
