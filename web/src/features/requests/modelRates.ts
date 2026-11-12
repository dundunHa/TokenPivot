import { useMemo } from "react";
import type {
  OverviewFilters,
  ResolvedRange,
} from "@/features/overview/overviewFilters";
import {
  useRankings,
  type QueryScope,
} from "@/features/overview/overviewQueries";

export const TOP_MODELS = 5;

/** 当前范围内用量最高的模型（按模型名去重，保持排名顺序），速率卡与速率趋势共用。 */
export function useTopModels(
  scope: QueryScope,
  filters: OverviewFilters,
  range: ResolvedRange,
) {
  const rankings = useRankings(scope, filters, range, "model", null);
  const models = useMemo(() => {
    const seen = new Set<string>();
    for (const g of rankings.data?.groups ?? []) {
      if (g.key.model !== null) seen.add(g.key.model);
      if (seen.size >= TOP_MODELS) break;
    }
    return [...seen];
  }, [rankings.data]);
  return {
    models,
    isPending: rankings.isPending,
    isError: rankings.isError,
  };
}
