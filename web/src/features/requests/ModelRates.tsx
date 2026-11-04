import { useMemo } from "react";
import type {
  OverviewFilters,
  ResolvedRange,
} from "@/features/overview/overviewFilters";
import {
  useRankings,
  type QueryScope,
} from "@/features/overview/overviewQueries";
import { perSecond } from "./model";
import { useModelRates } from "./useRequestLogs";

const LIMIT = 5;

/** 实时速率（按模型）：取当前范围内用量最高的几个模型，各自显示近 60 秒 tokens/s。 */
export function ModelRates({
  scope,
  filters,
  range,
}: {
  scope: QueryScope;
  filters: OverviewFilters;
  range: ResolvedRange;
}) {
  const rankings = useRankings(scope, filters, range, "model", null);
  const models = useMemo(() => {
    const seen = new Set<string>();
    for (const g of rankings.data?.groups ?? []) {
      if (g.key.model !== null) seen.add(g.key.model);
      if (seen.size >= LIMIT) break;
    }
    return [...seen];
  }, [rankings.data]);
  const results = useModelRates(scope, filters, models);

  const rows = models
    .map((model, i) => {
      const data = results[i]?.data;
      const value =
        data && data.eventCount > 0 && data.value !== null
          ? Number(data.value) / 60
          : null;
      return {
        model,
        value,
        text: data?.value && data.eventCount > 0 ? perSecond(data.value) : null,
        estimated: (data?.estimatedCount ?? 0) > 0,
        loading: results[i]?.isPending ?? true,
      };
    })
    .sort((a, b) => (b.value ?? -1) - (a.value ?? -1));
  const max = Math.max(0, ...rows.map((r) => r.value ?? 0));

  return (
    <div className="geek-card" role="region" aria-label="按模型的实时速率">
      <div className="geek-card-head">
        <h2 className="geek-card-title">
          实时速率
          <span className="geek-card-sub">按模型 · tokens/s</span>
        </h2>
      </div>
      {rankings.isPending ? (
        <div
          role="status"
          aria-label="模型速率加载中"
          className="geek-skeleton geek-skeleton-md"
        />
      ) : rankings.isError ? (
        <p role="alert" className="geek-empty geek-err">
          模型列表加载失败，暂时无法显示按模型的速率。
        </p>
      ) : rows.length === 0 ? (
        <p className="geek-empty geek-dim">暂无数据</p>
      ) : (
        <ul className="geek-rank-list">
          {rows.map((r) => (
            <li
              key={r.model}
              className="geek-rank-row geek-rate-row"
              data-testid="model-rate"
            >
              <span className="geek-rank-name">{r.model}</span>
              <span className="geek-rank-value geek-num">
                {r.loading ? "…" : (r.text ?? "—")}
                {r.estimated ? <em title="含估算"> ≈</em> : null}
              </span>
              <div className="geek-rank-track">
                <span
                  style={{
                    width: `${max > 0 && r.value !== null ? Math.max(2, (r.value / max) * 100) : 0}%`,
                  }}
                />
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
