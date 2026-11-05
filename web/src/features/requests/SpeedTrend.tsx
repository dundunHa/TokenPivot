import { useMemo } from "react";
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

const TOTAL_COLOR = "#f4f1ea";

/** 生成速度趋势：最近 60 分钟，每分钟的“输出 token ÷ 请求耗时”（tokens/s）；总体 + 用量最高的几个模型各一条线。 */
export function SpeedTrend({
  scope,
  filters,
  range,
}: {
  scope: QueryScope;
  filters: OverviewFilters;
  range: ResolvedRange;
}) {
  const { models } = useTopModels(scope, filters, range);
  const total = useSpeed(scope, filters);
  const perModel = useModelSpeeds(scope, filters, models);

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
      color: SERIES_PALETTE[i % SERIES_PALETTE.length] ?? "#e0a64b",
      data: perModel[i]?.data,
    })),
  ];
  // 数据真正更新（含模型清单变化）时才重算；inputs 每次渲染都是新数组，不能直接作依赖。
  const signature = `${total.dataUpdatedAt},${perModel.map((r) => r.dataUpdatedAt).join(",")}|${models.join("|")}`;
  const timezone = filters.timezone;
  const built = useMemo(
    () => buildSpeedRows(inputs, timezone),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [signature, timezone],
  );

  return (
    <div className="geek-card" role="region" aria-label="生成速度趋势">
      <div className="geek-card-head">
        <h2 className="geek-card-title">
          生成速度
          <span className="geek-card-sub">
            近 60 分钟 · tokens/s · 输出 ÷ 请求耗时
          </span>
        </h2>
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
        <p className="geek-empty geek-dim">
          暂无可测量的请求（需要上报了请求耗时的新数据）
        </p>
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
              总体（虚线）
            </li>
          </ul>
          <GeekLineChart
            rows={built.rows}
            series={built.series}
            timezone={timezone}
            format={fmtRate}
            height={260}
            connectNulls
          />
        </>
      )}
    </div>
  );
}
