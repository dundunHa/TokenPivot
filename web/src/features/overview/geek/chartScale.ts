/** 折线图与柱状图的纵轴刻度：把最大值向上取整到“好读”的步长（1/2/2.5/5 × 10^n），从 0 起。 */
export function niceTicks(
  max: number,
  intervals = 4,
): { ticks: number[]; max: number } {
  if (!Number.isFinite(max) || max <= 0) {
    return { ticks: [0, 1], max: 1 };
  }
  const raw = max / intervals;
  const magnitude = 10 ** Math.floor(Math.log10(raw));
  const step =
    ([1, 2, 2.5, 5, 10].find((m) => m * magnitude >= raw) ?? 10) * magnitude;
  const count = Math.ceil(max / step);
  const ticks = Array.from({ length: count + 1 }, (_, i) =>
    Number((i * step).toPrecision(12)),
  );
  return { ticks, max: ticks[ticks.length - 1] ?? step };
}

/** 纵轴紧凑数字：去掉多余的 .0（340K 而不是 340.0K）。 */
export function fmtAxis(n: number): string {
  const trim = (v: number) => String(Number(v.toFixed(1)));
  if (n >= 1e9) return `${trim(n / 1e9)}B`;
  if (n >= 1e6) return `${trim(n / 1e6)}M`;
  if (n >= 1e3) return `${trim(n / 1e3)}K`;
  return trim(n);
}

/** 速率（tokens/s）：≥100 取整，<100 保留一位小数，并带千分位压缩。 */
export function fmtRate(n: number): string {
  if (n >= 1e3) return fmtAxis(n);
  if (n >= 100) return String(Math.round(n));
  return String(Number(n.toFixed(1)));
}

/** 主题感知分组调色：随 data-theme 自动适配暖白与石墨暗。 */
export const SERIES_PALETTE = [
  "var(--c-s1)",
  "var(--c-s2)",
  "var(--c-s3)",
  "var(--c-s4)",
  "var(--c-s5)",
] as const;
