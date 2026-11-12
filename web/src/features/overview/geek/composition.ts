import type { MetricStat } from "../overviewApi";

/** 子集占父项的比例；任一方缺失或父项为 0 时无意义，返回 null。 */
export function shareOf(part: MetricStat, parent: MetricStat): number | null {
  if (part.value === null || parent.value === null) return null;
  const total = Number(parent.value);
  if (!(total > 0)) return null;
  return Number(part.value) / total;
}
