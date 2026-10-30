import { parseInstant } from "@/features/overview/overviewFilters";
import { formatExact } from "@/features/overview/overviewView";
import type { RateData, RequestRange, TokenField } from "./api";

/** 纯展示与轮询决策；不重新计算后端口径。 */

export const POLL_MS = 5_000;
export const UNAVAILABLE = "不可用";

/** 只有可见的最新页自动刷新明细；翻到更早页后明细停止自动重取。 */
export function eventsPollOptions(onLatestPage: boolean) {
  return onLatestPage
    ? ({ refetchInterval: POLL_MS, refetchOnWindowFocus: "always" } as const)
    : ({ refetchInterval: false, refetchOnWindowFocus: false } as const);
}

export function formatTokenField(field: TokenField): string {
  if (field.quality === "missing" || field.value === null) return UNAVAILABLE;
  const n = formatExact(String(field.value));
  return field.quality === "estimated" ? `${n}（估算）` : n;
}

export function formatDuration(ms: number | null): string {
  return ms === null ? "耗时不详" : `${ms} ms`;
}

export function formatStatus(status: string | null): string {
  return status === null ? "状态不可用" : status;
}

export interface RateView {
  headline: string;
  notes: string[];
}

/** 速率文案：无事件、用量不可用、真实 0 互相区分；含估算 / 部分覆盖必须标出。 */
export function describeRate(rate: RateData): RateView {
  const notes: string[] = [];
  if (rate.eventCount === 0) {
    return { headline: "窗口内无事件", notes };
  }
  if (rate.value === null) {
    notes.push(`缺失 ${rate.missingCount} 条`);
    return { headline: "用量不可用", notes };
  }
  const headline = formatExact(rate.value);
  if (rate.estimatedCount > 0) notes.push(`含估算 ${rate.estimatedCount} 条`);
  if (rate.missingCount > 0) {
    notes.push(`部分覆盖：缺失 ${rate.missingCount} 条`);
  }
  return { headline, notes };
}

const FRACTION = /\.(\d+)(?=Z|[+-]\d{2}:\d{2}$)/;

function tooPrecise(text: string): boolean {
  return (FRACTION.exec(text)?.[1]?.length ?? 0) > 6;
}

/** 微秒精度的时刻：parseInstant 的 ms 忽略小数秒，这里补回（已确认至多 6 位）。 */
function micros(text: string, ms: number): bigint {
  const frac = FRACTION.exec(text)?.[1] ?? "";
  return BigInt(ms) * 1000n + BigInt(frac.padEnd(6, "0") || "0");
}

/** 明细范围校验：带偏移的 RFC 3339、最多 6 位小数秒、起点早于终点；不要求分钟对齐。 */
export function parseRequestRange(
  start: string,
  end: string,
): { range: RequestRange } | { error: string } {
  const a = parseInstant(start.trim());
  const b = parseInstant(end.trim());
  if (!a || !b || tooPrecise(start) || tooPrecise(end)) {
    return {
      error:
        "时间格式无效，需要带偏移、最多 6 位小数秒的 RFC 3339，如 2026-10-28T09:00:00+08:00",
    };
  }
  if (micros(start.trim(), a.ms) >= micros(end.trim(), b.ms))
    return { error: "起点必须早于终点" };
  return { range: { start: start.trim(), end: end.trim() } };
}
