import { keepPreviousData, useQueries } from "@tanstack/react-query";
import { useMemo } from "react";
import { GeekLineChart } from "@/features/overview/geek/GeekLineChart";
import { fmtRate, SERIES_PALETTE } from "@/features/overview/geek/chartScale";
import type {
  OverviewFilters,
  ResolvedRange,
} from "@/features/overview/overviewFilters";
import { resolveRange } from "@/features/overview/overviewFilters";
import {
  aggregateQuery,
  REFRESH_MS,
  type QueryScope,
} from "@/features/overview/overviewQueries";
import { useTopModels } from "./modelRates";
import { buildRateRows, type RateInput } from "./rateSeries";

const TOTAL_COLOR = "#f4f1ea";

/** 实时速率趋势：最近 60 分钟，按分钟的 tokens/s；总量 + 用量最高的几个模型各一条线。 */
export function RateTrend({
  scope,
  filters,
  range,
  nowMs,
}: {
  scope: QueryScope;
  filters: OverviewFilters;
  range: ResolvedRange;
  nowMs: number;
}) {
  const { models } = useTopModels(scope, filters, range);
  const hourFilters = useMemo<OverviewFilters>(
    () => ({
      ...filters,
      range: "1h",
      granularity: "minute",
      start: null,
      end: null,
    }),
    [filters],
  );
  const window = resolveRange(hourFilters, nowMs);
  const keys = [null, ...models];
  const results = useQueries({
    queries: keys.map((model) => ({
      ...aggregateQuery(
        scope,
        model === null ? hourFilters : { ...hourFilters, models: [model] },
        window,
      ),
      refetchInterval: REFRESH_MS,
      placeholderData: keepPreviousData,
      retry: false,
    })),
  });

  const inputs: RateInput[] = keys.map((model, i) => ({
    key: model === null ? "total" : `m:${model}`,
    name: model === null ? "总量" : model,
    color:
      model === null
        ? TOTAL_COLOR
        : (SERIES_PALETTE[(i - 1) % SERIES_PALETTE.length] ?? "#e0a64b"),
    dashed: model === null,
    data: results[i]?.data,
  }));
  // 数据真正更新（含模型清单变化）时才重算；inputs 每次渲染都是新数组，不能直接作依赖。
  const signature = `${results.map((r) => r.dataUpdatedAt).join(",")}|${models.join("|")}`;
  const timezone = filters.timezone;
  const built = useMemo(
    () => buildRateRows(inputs, timezone),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [signature, timezone],
  );
  const pending = results[0]?.isPending ?? true;
  const failed = results[0]?.isError ?? false;

  return (
    <div className="geek-card" role="region" aria-label="实时速率趋势">
      <div className="geek-card-head">
        <h2 className="geek-card-title">
          实时速率
          <span className="geek-card-sub">近 60 分钟 · tokens/s</span>
        </h2>
      </div>
      {pending ? (
        <div
          role="status"
          aria-label="速率趋势加载中"
          className="geek-skeleton"
        />
      ) : failed ? (
        <p role="alert" className="geek-empty geek-err">
          速率趋势加载失败，请稍后重试。
        </p>
      ) : !built.hasData ? (
        <p className="geek-empty geek-dim">暂无数据</p>
      ) : (
        <>
          <ul className="geek-legend" aria-label="图例">
            {built.series
              .filter((s) => !s.dashed)
              .map((s) => (
                <li key={s.key}>
                  <i style={{ background: s.color }} />
                  {s.name}
                </li>
              ))}
            <li>
              <i style={{ background: TOTAL_COLOR }} />
              总量（虚线）
            </li>
          </ul>
          <GeekLineChart
            rows={built.rows}
            series={built.series}
            timezone={filters.timezone}
            format={fmtRate}
            height={260}
          />
        </>
      )}
    </div>
  );
}
