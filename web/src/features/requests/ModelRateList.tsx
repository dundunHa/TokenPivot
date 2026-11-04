import type {
  OverviewFilters,
  ResolvedRange,
} from "@/features/overview/overviewFilters";
import type { QueryScope } from "@/features/overview/overviewQueries";
import { SERIES_PALETTE } from "@/features/overview/geek/chartScale";
import { useTopModels } from "./modelRates";
import { perSecond } from "./model";
import { useModelRates } from "./useRequestLogs";

/** 实时速率卡里的分模型明细：近 60 秒 tokens/s，高→低排序，带相对长度。 */
export function ModelRateList({
  scope,
  filters,
  range,
}: {
  scope: QueryScope;
  filters: OverviewFilters;
  range: ResolvedRange;
}) {
  const { models, isPending, isError } = useTopModels(scope, filters, range);
  const results = useModelRates(scope, filters, models);

  const rows = models
    .map((model, i) => {
      const data = results[i]?.data;
      const known = data && data.eventCount > 0 && data.value !== null;
      return {
        model,
        color: SERIES_PALETTE[i % SERIES_PALETTE.length] ?? "#e0a64b",
        value: known ? Number(data.value) / 60 : null,
        text: known ? perSecond(data.value ?? "0") : null,
        estimated: (data?.estimatedCount ?? 0) > 0,
        loading: results[i]?.isPending ?? true,
      };
    })
    .sort((a, b) => (b.value ?? -1) - (a.value ?? -1));
  const max = Math.max(0, ...rows.map((r) => r.value ?? 0));

  if (isPending) {
    return (
      <div
        role="status"
        aria-label="模型速率加载中"
        className="geek-skeleton geek-skeleton-md"
      />
    );
  }
  if (isError) {
    return (
      <p role="alert" className="geek-kpi-note geek-err">
        模型速率加载失败。
      </p>
    );
  }
  if (rows.length === 0) return null;
  return (
    <ul className="geek-model-rates" aria-label="按模型的实时速率">
      {rows.map((r) => (
        <li
          key={r.model}
          data-testid="model-rate"
          style={{
            ["--w" as string]: `${max > 0 && r.value !== null ? Math.max(3, (r.value / max) * 100) : 0}%`,
            ["--c" as string]: r.color,
          }}
        >
          <i aria-hidden />
          <span className="geek-model-name">{r.model}</span>
          <span className="geek-num">
            {r.loading ? "…" : (r.text ?? "—")}
            {r.estimated ? <em title="含估算"> ≈</em> : null}
          </span>
        </li>
      ))}
    </ul>
  );
}
