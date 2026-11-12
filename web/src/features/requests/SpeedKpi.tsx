import type { ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { describeError } from "@/features/overview/overviewApi";
import type { OverviewFilters } from "@/features/overview/overviewFilters";
import type { QueryScope } from "@/features/overview/overviewQueries";
import { formatInstant } from "@/features/overview/overviewView";
import { GeekKpiShell } from "@/features/overview/geek/GeekKpis";
import { describeSpeed } from "./model";
import { useSpeed } from "./useRequestLogs";

/** 生成速度指标卡：近 60 秒内模型实际输出的 tokens/s（输出 ÷ 请求耗时），不含空闲时间。 */
export function SpeedKpi({
  scope,
  filters,
  children,
  intervalMs,
}: {
  scope: QueryScope;
  filters: OverviewFilters;
  /** 轮询间隔（毫秒）；缺省沿用请求页的 5 秒。 */
  intervalMs?: number;
  /** 总体速度下方的补充内容（总览页放各模型速度）。 */
  children?: ReactNode;
}) {
  const speed = useSpeed(scope, filters, intervalMs);
  const timezone = filters.timezone;
  return (
    <GeekKpiShell label="生成速度">
      {speed.isPending ? (
        <div
          role="status"
          aria-label="速度加载中"
          className="geek-skeleton geek-skeleton-sm"
        />
      ) : speed.data === undefined ? (
        <div className="geek-kpi-error">
          <p role="alert" className="geek-err">
            速度：{describeError(speed.error).message}
          </p>
          {describeError(speed.error).kind === "forbidden" ? null : (
            <Button
              variant="outline"
              size="sm"
              onClick={() => void speed.refetch()}
            >
              重试速度
            </Button>
          )}
        </div>
      ) : (
        <SpeedView
          data={speed.data}
          stale={speed.isError}
          timezone={timezone}
        />
      )}
      {children}
    </GeekKpiShell>
  );
}

function SpeedView({
  data,
  stale,
  timezone,
}: {
  data: NonNullable<ReturnType<typeof useSpeed>["data"]>;
  stale: boolean;
  timezone: string;
}) {
  const view = describeSpeed(data.current);
  const asOf = formatInstant(data.asOf, timezone, true);
  return (
    <div
      data-testid="rate"
      title={`近 60 秒，截至 ${asOf} · 输出 token ÷ 请求耗时，不含空闲时间`}
    >
      <div className="geek-kpi-row">
        <div className="geek-kpi-number geek-num geek-live">
          <span data-testid="rate-value">{view.headline}</span>
          {view.headline === "—" ? null : (
            <span className="geek-kpi-unit">tokens/s</span>
          )}
        </div>
      </div>
      {view.notes.length > 0 ? (
        <p className="geek-kpi-note" data-testid="rate-notes">
          {view.notes.join(" · ")}
        </p>
      ) : null}
      <p className="sr-only" data-testid="rate-fresh">
        近 60 秒，截至 {asOf}
      </p>
      {stale ? (
        <p role="alert" className="geek-err geek-kpi-note">
          速度更新失败，展示截至 {asOf} 的旧结果
        </p>
      ) : null}
    </div>
  );
}
