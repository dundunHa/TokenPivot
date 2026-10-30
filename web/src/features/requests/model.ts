import type { RateData, TokenField } from "./api";

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
  const n = String(field.value).replace(/\B(?=(\d{3})+(?!\d))/g, ",");
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
  const headline = rate.value.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  if (rate.estimatedCount > 0) notes.push(`含估算 ${rate.estimatedCount} 条`);
  if (rate.missingCount > 0) {
    notes.push(`部分覆盖：缺失 ${rate.missingCount} 条`);
  }
  return { headline, notes };
}
