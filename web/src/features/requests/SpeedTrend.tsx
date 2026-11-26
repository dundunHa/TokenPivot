import { useMemo, useState } from "react";
import { GeekLineChart } from "@/features/overview/geek/GeekLineChart";
import { fmtRate, SERIES_PALETTE } from "@/features/overview/geek/chartScale";
import type {
  OverviewFilters,
  ResolvedRange,
} from "@/features/overview/overviewFilters";
import type { QueryScope } from "@/features/overview/overviewQueries";
import { useTopModels } from "./modelRates";
import { buildSpeedRows, type SpeedInput } from "./speedSeries";
import { useModelSpeeds, useSpeed } from "./useRequestLogs";

const TOTAL_COLOR = "var(--c-total-line)";

/** 生成速度趋势：最近 60 分钟，纯净单调折线，支持点击图例聚焦单系列。 */
export function SpeedTrend({
  scope,
  filters,
  range,
  intervalMs,
}: {
  scope: QueryScope;
  filters: OverviewFilters;
  range: ResolvedRange;
  intervalMs?: number;
}) {
  const [isolated, setIsolated] = useState<string | null>(null);
  const { models } = useTopModels(scope, filters, range);
  const total = useSpeed(scope, filters, intervalMs);
  const perModel = useModelSpeeds(scope, filters, models, intervalMs);

  const inputs: SpeedInput[] = [
    {
      key: "total",
      name: "总体",
      color: TOTAL_COLOR,
      dashed: true,
      data: total.data,
    },
    ...models.map((model, i) => ({
      key: `m:${model}`,
      name: model,
      color: SERIES_PALETTE[i % SERIES_PALETTE.length] ?? "var(--c-s1)",
      data: perModel[i]?.data,
    })),
  ];
  const signature = `${total.dataUpdatedAt},${perModel.map((r) => r.dataUpdatedAt).join(",")}|${models.join("|")}`;
  const timezone = filters.timezone;
  const built = useMemo(
    () => buildSpeedRows(inputs, timezone),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [signature, timezone],
  );

  const visibleSeries =
    isolated && built.series.some((s) => s.key === isolated)
      ? built.series.filter((s) => s.key === isolated)
      : built.series;

  return (
    <div className="geek-card" role="region" aria-label="生成速度趋势">
      <div className="geek-card-head">
        <h2 className="geek-card-title">生成速度趋势</h2>
      </div>
      {total.isPending ? (
        <div
          role="status"
          aria-label="速度趋势加载中"
          className="geek-skeleton"
        />
      ) : total.data === undefined ? (
        <p role="alert" className="geek-empty geek-err">
          速度趋势加载失败，请稍后重试。
        </p>
      ) : !built.hasData ? (
        <p className="geek-empty geek-dim">暂无数据</p>
      ) : (
        <>
          <ul className="geek-legend" aria-label="图例">
            {built.series
              .filter((s) => !s.dashed)
              .map((s) => {
                const state = isolated
                  ? isolated === s.key
                    ? "focused"
                    : "dimmed"
                  : undefined;
                return (
                  <li key={s.key}>
                    <button
                      type="button"
                      className="tp-legend-btn"
                      data-state={state}
                      aria-pressed={isolated === s.key}
                      onClick={() =>
                        setIsolated((cur) => (cur === s.key ? null : s.key))
                      }
                    >
                      <span
                        className="tp-swatch"
                        style={{ background: s.color }}
                      />
                      <span>{s.name}</span>
                    </button>
                  </li>
                );
              })}
            <li>
              <button
                type="button"
                className="tp-legend-btn"
                data-state={
                  isolated
                    ? isolated === "total"
                      ? "focused"
                      : "dimmed"
                    : undefined
                }
                aria-pressed={isolated === "total"}
                onClick={() =>
                  setIsolated((cur) => (cur === "total" ? null : "total"))
                }
              >
                <span className="tp-swatch tp-swatch-dash" />
                <span>总体（虚线）</span>
              </button>
            </li>
          </ul>
          <GeekLineChart
            rows={built.rows}
            series={visibleSeries}
            timezone={timezone}
            format={fmtRate}
            height={222}
            connectNulls
          />
        </>
      )}
    </div>
  );
}
