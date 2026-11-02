/**
 * geek-utils.ts — 格式化工具
 */

/** 紧凑数字格式化：1234567 → "1.23M" */
export function fmt(n: number): string {
  if (n >= 1e9) return (n / 1e9).toFixed(2) + "B";
  if (n >= 1e6) return (n / 1e6).toFixed(2) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(1) + "K";
  return String(Math.round(n));
}

/** 毫秒格式化：1234 → "1.2s"，567 → "567ms" */
export function fmtMs(ms: number): string {
  if (ms >= 1000) return (ms / 1000).toFixed(1) + "s";
  return Math.round(ms) + "ms";
}

/** 百分比格式化：0.1234 → "12.3%" */
export function fmtPct(ratio: number): string {
  return (ratio * 100).toFixed(1) + "%";
}

/** 时间格式化：HH:MM:SS */
export function fmtTime(d: Date): string {
  return [d.getHours(), d.getMinutes(), d.getSeconds()]
    .map((n) => String(n).padStart(2, "0"))
    .join(":");
}

/** 相对时间：4200ms → "4.2s ago" */
export function fmtAgo(ms: number): string {
  if (ms < 1000) return "just now";
  if (ms < 60000) return (ms / 1000).toFixed(1) + "s ago";
  if (ms < 3600000) return Math.round(ms / 60000) + "m ago";
  return Math.round(ms / 3600000) + "h ago";
}

/** 用量质量标记符号 */
export const QUALITY_SYMBOLS = {
  known: "✓",
  estimated: "≈",
  missing: "—",
} as const;

export type QualityLevel = keyof typeof QUALITY_SYMBOLS;
