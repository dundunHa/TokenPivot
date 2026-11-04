import { Button } from "@/components/ui/button";
import { describeError } from "@/features/overview/overviewApi";
import type { OverviewFilters } from "@/features/overview/overviewFilters";
import type { QueryScope } from "@/features/overview/overviewQueries";
import { formatInstant } from "@/features/overview/overviewView";
import { GeekKpiShell } from "@/features/overview/geek/GeekKpis";
import { describeRate } from "./model";
import { useRate } from "./useRequestLogs";

/** 实时速率指标卡：近 60 秒 tokens/min，与请求明细共用同一个速率查询。 */
export function RateKpi({
  scope,
  filters,
}: {
  scope: QueryScope;
  filters: OverviewFilters;
}) {
  const rate = useRate(scope, filters);
  const timezone = filters.timezone;
  return (
    <GeekKpiShell label="实时速率 · 每分钟">
      {rate.isPending ? (
        <div
          role="status"
          aria-label="速率加载中"
          className="geek-skeleton geek-skeleton-sm"
        />
      ) : rate.data === undefined ? (
        <div className="geek-kpi-error">
          <p role="alert" className="geek-err">
            速率：{describeError(rate.error).message}
          </p>
          {describeError(rate.error).kind === "forbidden" ? null : (
            <Button
              variant="outline"
              size="sm"
              onClick={() => void rate.refetch()}
            >
              重试速率
            </Button>
          )}
        </div>
      ) : (
        <RateView data={rate.data} stale={rate.isError} timezone={timezone} />
      )}
    </GeekKpiShell>
  );
}

function RateView({
  data,
  stale,
  timezone,
}: {
  data: NonNullable<ReturnType<typeof useRate>["data"]>;
  stale: boolean;
  timezone: string;
}) {
  const view = describeRate(data);
  const fresh = data.windowLastReceivedAt
    ? `窗口内最后接收 ${formatInstant(data.windowLastReceivedAt, timezone, true)}`
    : "窗口内无事件，接收新鲜度未知";
  return (
    <div
      data-testid="rate"
      title={`窗口截至 ${formatInstant(data.end, timezone, true)} · ${fresh}`}
    >
      <div className="geek-kpi-row">
        <div
          className="geek-kpi-number geek-num geek-live"
          data-testid="rate-value"
        >
          {view.headline}
        </div>
      </div>
      {view.notes.length > 0 ? (
        <p className="geek-kpi-note" data-testid="rate-notes">
          {view.notes.join(" · ")}
        </p>
      ) : null}
      <p className="sr-only" data-testid="rate-fresh">
        窗口截至 {formatInstant(data.end, timezone, true)} · {fresh}
      </p>
      {stale ? (
        <p role="alert" className="geek-err geek-kpi-note">
          速率更新失败，展示截至 {formatInstant(data.asOf, timezone, true)}{" "}
          的旧结果
        </p>
      ) : null}
    </div>
  );
}
