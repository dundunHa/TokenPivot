import { parseInstant } from "@/features/overview/overviewFilters";
import { formatExact } from "@/features/overview/overviewView";
import type {
  RateData,
  RateSample,
  RequestRange,
  SpeedSums,
  TokenField,
} from "./api";

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

/**
 * 去掉十进制字符串的尾随零（后端固定输出 6 位小数）：
 * 49.500000 → 49.5，0.000000 → 0，100 保持 100。只裁剪后端已给出的值，
 * 不改变数值精度，也不重新计算。
 */
export function trimDecimal(value: string): string {
  if (!value.includes(".")) return value;
  const trimmed = value.replace(/0+$/, "").replace(/\.$/, "");
  return trimmed === "" ? "0" : trimmed;
}

/** 每请求生成速度：缺失是「不可用」，不是 0 tokens/s。 */
export function formatSpeedTps(value: string | null): string {
  return value === null ? UNAVAILABLE : `${trimDecimal(value)} tokens/s`;
}

/**
 * 毫秒展示：明细用整数、窗口平均用十进制字符串。0 是真实观测，照常显示。
 */
export function formatMilliseconds(value: string | number | null): string {
  if (value === null) return UNAVAILABLE;
  const text = typeof value === "number" ? String(value) : trimDecimal(value);
  return `${text} ms`;
}

export interface SampleView {
  headline: string;
  samples: string;
}

/**
 * 一项窗口平均的展示：样本数为 0（旧后端省略或窗口无有效样本）一律按未知，
 * 绝不把「没有样本」画成 0 tokens/s 或 0 ms。
 */
export function describeSample(
  sample: Pick<RateSample<string>, "value" | "sampleCount">,
  format: (value: string) => string,
): SampleView {
  const samples = `${formatExact(String(sample.sampleCount))} 个有效样本`;
  if (sample.value === null || sample.sampleCount === 0) {
    return { headline: UNAVAILABLE, samples };
  }
  return { headline: format(sample.value), samples };
}

/** 顶部两项的平均值；模型只透传后端口径，不在这里加权或重算。 */
export function describeGenerationMetrics(rate: RateData): {
  speed: SampleView;
  ttft: SampleView;
  eventCount: string;
} {
  return {
    speed: describeSample(rate.generationSpeed, formatSpeedTps),
    ttft: describeSample(rate.ttft, formatMilliseconds),
    eventCount:
      rate.eventCount === 0
        ? "窗口内无事件"
        : `窗口内 ${formatExact(String(rate.eventCount))} 条事件`,
  };
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
