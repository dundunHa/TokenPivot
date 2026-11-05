import { parseInstant } from "@/features/overview/overviewFilters";
import { formatExact } from "@/features/overview/overviewView";
import type { RequestRange, SpeedSums, TokenField } from "./api";

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

export interface SpeedView {
  headline: string;
  notes: string[];
}

/**
 * 生成速度（tokens/s）= 可测量请求的输出 token 之和 ÷ 请求耗时之和。
 * 只看模型在跑的时间：空闲时段不进分母，缓存读取与输入不进分子。无法测量返回 null。
 */
export function speedOf(sums: {
  outputTokens: string | null;
  durationMs: string | null;
}): number | null {
  if (sums.outputTokens === null || sums.durationMs === null) return null;
  const duration = Number(sums.durationMs);
  if (!(duration > 0)) return null;
  return (Number(sums.outputTokens) / duration) * 1000;
}

/** ≥100 取整并加千分位，其余保留一位小数（去掉多余的 .0）。 */
export function formatSpeed(speed: number): string {
  if (speed === 0) return "0";
  if (speed >= 100) return formatExact(String(Math.round(speed)));
  return speed.toFixed(1).replace(/\.0$/, "");
}

/** 速度文案（tokens/s）：无数据与无法测量显示“—”并说明原因；含估算 / 部分覆盖必须标出。 */
export function describeSpeed(current: SpeedSums): SpeedView {
  if (current.eventCount === 0) return { headline: "—", notes: ["暂无数据"] };
  const speed = speedOf(current);
  const unmeasured = current.eventCount - current.measuredCount;
  if (speed === null) {
    return {
      headline: "—",
      notes: [`${current.eventCount} 条请求无耗时，无法计算`],
    };
  }
  const notes: string[] = [];
  if (current.estimatedCount > 0) {
    notes.push(`含估算 ${current.estimatedCount} 条`);
  }
  if (unmeasured > 0) notes.push(`部分覆盖：${unmeasured} 条无耗时`);
  return { headline: formatSpeed(speed), notes };
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
