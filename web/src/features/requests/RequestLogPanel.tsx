import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { describeError } from "@/features/overview/overviewApi";
import {
  filterIdentity,
  type OverviewFilters,
} from "@/features/overview/overviewFilters";
import type { QueryScope } from "@/features/overview/overviewQueries";
import { formatInstant } from "@/features/overview/overviewView";
import type { RequestRange, RequestRow } from "./api";
import {
  formatDuration,
  formatStatus,
  formatTokenField,
  parseRequestRange,
} from "./model";
import { eventsQuery, useRequestEvents } from "./useRequestLogs";

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
      <div className="geek-card-head">
        <h2 className="geek-card-title">请求明细</h2>
      </div>
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
  const [range, setRange] = useState<RequestRange | null>(null);
  const [draftStart, setDraftStart] = useState("");
  const [draftEnd, setDraftEnd] = useState("");
  const [rangeError, setRangeError] = useState<string | null>(null);
  const cursor = cursors.at(-1) ?? null;
  const events = useRequestEvents(scope, filters, cursor, range);
  const tz = filters.timezone;

  const goLatest = () => {
    client.removeQueries({
      queryKey: eventsQuery(scope, filters, null, range).queryKey,
      exact: true,
    });
    setCursors([]);
  };

  // 范围变化必须同时丢弃旧游标，游标只对产生它的范围有效。
  const applyRange = () => {
    const parsed = parseRequestRange(draftStart, draftEnd);
    if ("error" in parsed) {
      setRangeError(parsed.error);
      return;
    }
    setRangeError(null);
    setCursors([]);
    setRange(parsed.range);
  };
  const resetRange = () => {
    setRangeError(null);
    setCursors([]);
    setRange(null);
  };

  return (
    <div className="flex flex-col gap-4">
      <details className="geek-ms-inline">
        <summary>历史范围查询</summary>
        <div
          className="flex flex-col gap-2 pt-2"
          aria-label="明细时间范围"
          role="group"
        >
          <div className="flex flex-wrap items-end gap-2">
            <label className="text-muted-foreground flex flex-col gap-1 text-xs">
              明细起点
              <Input
                className="h-8 w-64 text-xs"
                value={draftStart}
                placeholder="2026-10-28T09:00:00+08:00"
                onChange={(e) => setDraftStart(e.target.value)}
              />
            </label>
            <label className="text-muted-foreground flex flex-col gap-1 text-xs">
              明细终点
              <Input
                className="h-8 w-64 text-xs"
                value={draftEnd}
                placeholder="2026-10-28T18:00:00+08:00"
                onChange={(e) => setDraftEnd(e.target.value)}
              />
            </label>
            <Button
              variant="outline"
              size="sm"
              className="h-8"
              onClick={applyRange}
            >
              查询范围
            </Button>
            {range ? (
              <Button
                variant="outline"
                size="sm"
                className="h-8"
                onClick={resetRange}
              >
                最近 1 小时
              </Button>
            ) : null}
          </div>
          <p className="text-subtle text-xs">
            {range ? "固定范围（历史补报可在此查询）" : "默认最近 1 小时"}
          </p>
          {rangeError ? (
            <p role="alert" className="text-destructive text-xs">
              {rangeError}
            </p>
          ) : null}
        </div>
      </details>
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
            <p
              className="text-subtle text-xs"
              title={`范围 ${formatInstant(events.data.start, tz, true)} → ${formatInstant(events.data.end, tz, true)} · 截点 ${formatInstant(events.data.asOf, tz, true)} · 分页不是某一时刻的完整快照`}
            >
              {cursor === null ? "最新页每 5 秒刷新" : "更早页，不自动刷新"}
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
                size="sm"
                disabled={cursors.length === 0}
                onClick={() => setCursors((c) => c.slice(0, -1))}
              >
                上一页
              </Button>
              <Button
                variant="outline"
                size="sm"
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
                <Button variant="outline" size="sm" onClick={goLatest}>
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

function RequestTable({
  rows,
  timezone,
}: {
  rows: RequestRow[];
  timezone: string;
}) {
  return (
    <div className="geek-table-wrap">
      <table className="geek-table">
        <thead>
          <tr>
            <th scope="col">发生时间</th>
            <th scope="col">Provider / Model</th>
            <th scope="col">项目</th>
            <th scope="col">来源</th>
            <th scope="col" className="geek-th-num">
              输入
            </th>
            <th scope="col" className="geek-th-num">
              输出
            </th>
            <th scope="col" className="geek-th-num">
              总计
            </th>
            <th scope="col">状态</th>
            <th scope="col" className="geek-th-num">
              耗时
            </th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.id} data-testid="request-row">
              <td className="geek-num">
                {formatInstant(r.occurredAt, timezone, true)}
              </td>
              <td>
                {r.provider} / {r.model}
              </td>
              <td>{r.project ? r.project.name : "未归属"}</td>
              <td className="text-muted-foreground">
                {r.source.platform} · {r.source.instanceKey}
              </td>
              <td className="geek-td-num">{formatTokenField(r.input)}</td>
              <td className="geek-td-num">{formatTokenField(r.output)}</td>
              <td className="geek-td-num">{formatTokenField(r.total)}</td>
              <td>{formatStatus(r.status)}</td>
              <td className="geek-td-num">{formatDuration(r.durationMs)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
