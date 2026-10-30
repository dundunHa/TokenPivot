import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { describeError } from "@/features/overview/overviewApi";
import {
  filterIdentity,
  type OverviewFilters,
} from "@/features/overview/overviewFilters";
import type { QueryScope } from "@/features/overview/overviewQueries";
import { formatInstant } from "@/features/overview/overviewView";
import type { RateData, RequestRow } from "./api";
import {
  describeRate,
  formatDuration,
  formatStatus,
  formatTokenField,
} from "./model";
import { eventsQuery, useRate, useRequestEvents } from "./useRequestLogs";

/** 请求明细面板：筛选来自总览（单一来源）；筛选变化时以 key 重置游标。 */
export function RequestLogPanel({
  scope,
  filters,
}: {
  scope: QueryScope;
  filters: OverviewFilters;
}) {
  return (
    <section aria-label="请求明细" className="flex flex-col gap-4">
      <h2 className="text-sm font-medium">请求明细</h2>
      <PanelBody
        key={`${scope.workspaceId}|${filterIdentity(filters)}`}
        scope={scope}
        filters={filters}
      />
    </section>
  );
}

function PanelBody({
  scope,
  filters,
}: {
  scope: QueryScope;
  filters: OverviewFilters;
}) {
  const client = useQueryClient();
  const [cursors, setCursors] = useState<string[]>([]);
  const cursor = cursors.at(-1) ?? null;
  const events = useRequestEvents(scope, filters, cursor);
  const rate = useRate(scope, filters);
  const tz = filters.timezone;

  const goLatest = () => {
    client.removeQueries({
      queryKey: eventsQuery(scope, filters, null).queryKey,
      exact: true,
    });
    setCursors([]);
  };

  return (
    <div className="flex flex-col gap-4">
      <RateBlock rate={rate} timezone={tz} />
      <div aria-label="明细列表" role="region" className="flex flex-col gap-2">
        {events.isPending ? (
          <div
            role="status"
            aria-label="明细加载中"
            className="bg-accent h-24 animate-pulse rounded-md"
          />
        ) : events.data === undefined && events.isError ? (
          <ErrorBlock
            error={events.error}
            onRetry={() => void events.refetch()}
            onLatest={cursor !== null ? goLatest : null}
          />
        ) : events.data ? (
          <>
            {events.isError ? (
              <p role="alert" className="text-destructive text-xs">
                更新失败，展示截至 {formatInstant(events.data.asOf, tz, true)}{" "}
                的旧结果
              </p>
            ) : null}
            <p className="text-subtle text-xs">
              范围 {formatInstant(events.data.start, tz, true)} →{" "}
              {formatInstant(events.data.end, tz, true)} · 截点{" "}
              {formatInstant(events.data.asOf, tz, true)}
              {cursor === null
                ? " · 最新页每 5 秒刷新"
                : " · 更早页，不自动刷新"}
              {" · "}分页不是某一时刻的完整快照
              {Date.parse(events.data.start) <
              Date.parse(events.data.retentionCutoff)
                ? " · 旧时段可能不完整"
                : ""}
            </p>
            {events.data.items.length === 0 ? (
              <p className="text-muted-foreground text-sm">
                当前保留明细中没有匹配的请求。
              </p>
            ) : (
              <RequestTable rows={events.data.items} timezone={tz} />
            )}
            <div className="flex gap-2">
              <Button
                variant="outline"
                disabled={cursors.length === 0}
                onClick={() => setCursors((c) => c.slice(0, -1))}
              >
                上一页
              </Button>
              <Button
                variant="outline"
                disabled={
                  !events.data.hasMore || events.data.nextCursor === null
                }
                onClick={() => {
                  const next = events.data?.nextCursor;
                  if (next) setCursors((c) => [...c, next]);
                }}
              >
                下一页
              </Button>
              {cursor !== null ? (
                <Button variant="outline" onClick={goLatest}>
                  返回最新页
                </Button>
              ) : null}
            </div>
          </>
        ) : null}
      </div>
    </div>
  );
}

function ErrorBlock({
  error,
  onRetry,
  onLatest,
}: {
  error: unknown;
  onRetry: () => void;
  onLatest: (() => void) | null;
}) {
  const { kind, message } = describeError(error);
  return (
    <div className="flex flex-col gap-2">
      <Alert variant="destructive">
        <AlertDescription>{message}</AlertDescription>
      </Alert>
      <div className="flex gap-2">
        {kind === "forbidden" ? null : (
          <Button variant="outline" onClick={onRetry}>
            重试
          </Button>
        )}
        {onLatest && kind === "filter" ? (
          <Button variant="outline" onClick={onLatest}>
            返回最新页
          </Button>
        ) : null}
      </div>
    </div>
  );
}

function RateBlock({
  rate,
  timezone,
}: {
  rate: ReturnType<typeof useRate>;
  timezone: string;
}) {
  if (rate.isPending) {
    return (
      <div
        role="status"
        aria-label="速率加载中"
        className="bg-accent h-16 animate-pulse rounded-md"
      />
    );
  }
  if (rate.data === undefined) {
    const { kind, message } = describeError(rate.error);
    return (
      <div className="flex flex-col gap-2">
        <Alert variant="destructive">
          <AlertDescription>速率：{message}</AlertDescription>
        </Alert>
        {kind === "forbidden" ? null : (
          <Button
            variant="outline"
            className="self-start"
            onClick={() => void rate.refetch()}
          >
            重试速率
          </Button>
        )}
      </div>
    );
  }
  return <RateView data={rate.data} stale={rate.isError} timezone={timezone} />;
}

function RateView({
  data,
  stale,
  timezone,
}: {
  data: RateData;
  stale: boolean;
  timezone: string;
}) {
  const view = describeRate(data);
  return (
    <div className="flex flex-col gap-1" data-testid="rate">
      <p className="text-subtle text-xs">近 60 秒速率（tokens/min）</p>
      <p className="text-2xl font-semibold" data-testid="rate-value">
        {view.headline}
      </p>
      {view.notes.length > 0 ? (
        <p className="text-muted-foreground text-xs" data-testid="rate-notes">
          {view.notes.join(" · ")}
        </p>
      ) : null}
      <p className="text-subtle text-xs" data-testid="rate-fresh">
        窗口截至 {formatInstant(data.end, timezone, true)} ·{" "}
        {data.windowLastReceivedAt
          ? `窗口内最后接收 ${formatInstant(data.windowLastReceivedAt, timezone, true)}`
          : "窗口内无事件，接收新鲜度未知"}
      </p>
      {stale ? (
        <p role="alert" className="text-destructive text-xs">
          速率更新失败，展示截至 {formatInstant(data.asOf, timezone, true)}{" "}
          的旧结果
        </p>
      ) : null}
    </div>
  );
}

function RequestTable({
  rows,
  timezone,
}: {
  rows: RequestRow[];
  timezone: string;
}) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-xs">
        <thead>
          <tr className="text-muted-foreground">
            <th scope="col">发生时间</th>
            <th scope="col">Provider / Model</th>
            <th scope="col">项目</th>
            <th scope="col">来源</th>
            <th scope="col">输入</th>
            <th scope="col">输出</th>
            <th scope="col">总计</th>
            <th scope="col">状态</th>
            <th scope="col">耗时</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.id} data-testid="request-row">
              <td>{formatInstant(r.occurredAt, timezone, true)}</td>
              <td>
                {r.provider} / {r.model}
              </td>
              <td>{r.project ? r.project.name : "未归属"}</td>
              <td>
                {r.source.platform} · {r.source.instanceKey}
              </td>
              <td>{formatTokenField(r.input)}</td>
              <td>{formatTokenField(r.output)}</td>
              <td>{formatTokenField(r.total)}</td>
              <td>{formatStatus(r.status)}</td>
              <td>{formatDuration(r.durationMs)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
