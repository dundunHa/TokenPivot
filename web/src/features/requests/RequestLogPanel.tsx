import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { describeError } from "@/features/overview/overviewApi";
import {
  filterIdentity,
  type OverviewFilters,
} from "@/features/overview/overviewFilters";
import type { QueryScope } from "@/features/overview/overviewQueries";
import { formatExact, formatInstant } from "@/features/overview/overviewView";
import type { RateData, RequestRange, RequestRow } from "./api";
import {
  describeGenerationMetrics,
  formatDuration,
  formatMilliseconds,
  formatSpeedTps,
  formatStatus,
  formatTokenField,
  parseRequestRange,
  trimDecimal,
  UNAVAILABLE,
} from "./model";
import { eventsQuery, useRate, useRequestEvents } from "./useRequestLogs";

/** 请求明细面板：4 卡汇总条 + 流水表 + 右侧滑出详情抽屉。筛选变化时以 key 重置游标。 */
export function RequestLogPanel({
  scope,
  filters,
}: {
  scope: QueryScope;
  filters: OverviewFilters;
}) {
  return (
    <PanelBody
      key={`${scope.workspaceId}|${filterIdentity(filters)}`}
      scope={scope}
      filters={filters}
    />
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
  const [selected, setSelected] = useState<RequestRow | null>(null);
  const cursor = cursors.at(-1) ?? null;
  const events = useRequestEvents(scope, filters, cursor, range);
  const rate = useRate(scope, filters);
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

  const countText = events.data ? `${events.data.items.length} 条` : null;

  return (
    <section aria-label="请求明细" className="geek-main">
      <RateKpiStrip rate={rate} range={range} timezone={tz} />

      <article className="geek-card">
        <div className="geek-card-head">
          <div className="flex flex-wrap items-center gap-2.5">
            <h2 className="geek-card-title">请求流水</h2>
            {events.data ? (
              <span
                className="tp-quality-pill"
                title={`范围 ${formatInstant(events.data.start, tz, true)} → ${formatInstant(events.data.end, tz, true)}`}
              >
                {cursor === null ? "● 最新页每 5 秒刷新" : "更早页，不自动刷新"}
                {Date.parse(events.data.start) <
                Date.parse(events.data.retentionCutoff)
                  ? " · 旧时段可能不完整"
                  : ""}
              </span>
            ) : null}
          </div>
          {countText ? (
            <span className="geek-num text-muted-foreground text-xs">
              {countText}
            </span>
          ) : null}
        </div>

        <details className="geek-ms-inline">
          <summary>历史范围查询</summary>
          <div
            className="flex flex-col gap-2 pt-3"
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
              {range ? "固定范围" : "默认最近 1 小时"}
            </p>
            {rangeError ? (
              <p role="alert" className="text-destructive text-xs">
                {rangeError}
              </p>
            ) : null}
          </div>
        </details>

        <div
          aria-label="明细列表"
          role="region"
          className="flex flex-col gap-3"
        >
          {events.isPending ? (
            <div
              role="status"
              aria-label="明细加载中"
              className="geek-skeleton geek-skeleton-md"
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
              {events.data.items.length === 0 ? (
                <p className="geek-empty text-muted-foreground">
                  当前保留明细中没有匹配的请求。
                </p>
              ) : (
                <RequestTable
                  rows={events.data.items}
                  timezone={tz}
                  selectedId={selected?.id ?? null}
                  onSelect={setSelected}
                />
              )}
              <div className="flex flex-wrap items-center gap-2">
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
      </article>

      {selected ? (
        <RequestDetailDrawer
          row={selected}
          timezone={tz}
          onClose={() => setSelected(null)}
        />
      ) : null}
    </section>
  );
}

function RateKpiStrip({
  rate,
  range,
  timezone,
}: {
  rate: ReturnType<typeof useRate>;
  range: RequestRange | null;
  timezone: string;
}) {
  if (rate.isPending) {
    return (
      <section
        className="geek-kpis geek-kpis-4"
        role="status"
        aria-label="速率加载中"
      >
        {[0, 1, 2, 3].map((i) => (
          <div key={i} className="geek-kpi">
            <div className="geek-skeleton geek-skeleton-sm" />
          </div>
        ))}
      </section>
    );
  }
  if (rate.data === undefined) {
    const { kind, message } = describeError(rate.error);
    return (
      <div className="geek-card">
        <Alert variant="destructive">
          <AlertDescription>速率：{message}</AlertDescription>
        </Alert>
        {kind === "forbidden" ? null : (
          <Button
            variant="outline"
            size="sm"
            className="self-start"
            onClick={() => void rate.refetch()}
          >
            重试速率
          </Button>
        )}
      </div>
    );
  }
  return (
    <RateKpiCards
      data={rate.data}
      stale={rate.isError}
      range={range}
      timezone={timezone}
    />
  );
}

function RateKpiCards({
  data,
  stale,
  range,
  timezone,
}: {
  data: RateData;
  stale: boolean;
  range: RequestRange | null;
  timezone: string;
}) {
  const view = describeGenerationMetrics(data);
  const speedNum =
    data.generationSpeed.value !== null && data.generationSpeed.sampleCount > 0
      ? trimDecimal(data.generationSpeed.value)
      : null;
  const ttftNum =
    data.ttft.value !== null && data.ttft.sampleCount > 0
      ? trimDecimal(data.ttft.value)
      : null;

  return (
    <div className="flex flex-col gap-2" data-testid="generation-rate">
      <section className="geek-kpis geek-kpis-4" aria-label="实时速率指标">
        <article className="geek-kpi">
          <div className="geek-kpi-top">
            <span className="geek-kpi-label">生成速度</span>
            <span
              className="geek-num text-[11px]"
              data-testid="rate-speed-samples"
            >
              {view.speed.samples}
            </span>
          </div>
          <div className="geek-kpi-row">
            <div className="geek-kpi-number geek-num" data-testid="rate-speed">
              {speedNum !== null ? (
                <>
                  {speedNum} <span className="geek-kpi-unit">tokens/s</span>
                </>
              ) : (
                UNAVAILABLE
              )}
            </div>
          </div>
        </article>

        <article className="geek-kpi">
          <div className="geek-kpi-top">
            <span className="geek-kpi-label">首 Token 延迟 TTFT</span>
            <span
              className="geek-num text-[11px]"
              data-testid="rate-ttft-samples"
            >
              {view.ttft.samples}
            </span>
          </div>
          <div className="geek-kpi-row">
            <div className="geek-kpi-number geek-num" data-testid="rate-ttft">
              {ttftNum !== null ? (
                <>
                  {ttftNum} <span className="geek-kpi-unit">ms</span>
                </>
              ) : (
                UNAVAILABLE
              )}
            </div>
          </div>
        </article>

        <article className="geek-kpi">
          <div className="geek-kpi-top">
            <span className="geek-kpi-label">窗口请求</span>
            <span className="geek-num text-[11px]">60s</span>
          </div>
          <div className="geek-kpi-row">
            <div className="geek-kpi-number geek-num" data-testid="rate-events">
              {data.eventCount === 0 ? (
                "0"
              ) : (
                <>
                  {formatExact(String(data.eventCount))}{" "}
                  <span className="geek-kpi-unit">条</span>
                </>
              )}
            </div>
          </div>
        </article>

        <article className="geek-kpi">
          <div className="geek-kpi-top">
            <span className="geek-kpi-label">明细窗口</span>
            <span className="geek-num text-[11px]">180d</span>
          </div>
          <div className="geek-kpi-row">
            <div
              className="geek-kpi-number geek-num"
              style={{ fontSize: 20, lineHeight: 1.45 }}
            >
              {range ? "固定范围" : "最近 1 小时"}
            </div>
          </div>
        </article>
      </section>

      {stale ? (
        <p role="alert" className="text-destructive text-xs">
          速率更新失败，展示截至 {formatInstant(data.asOf, timezone, true)}{" "}
          的旧结果
        </p>
      ) : null}
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
          <Button variant="outline" size="sm" onClick={onRetry}>
            重试
          </Button>
        )}
        {onLatest && kind === "filter" ? (
          <Button variant="outline" size="sm" onClick={onLatest}>
            返回最新页
          </Button>
        ) : null}
      </div>
    </div>
  );
}

function lagInfo(row: RequestRow): { isBackfill: boolean; label: string } {
  const diffMs = Date.parse(row.receivedAt) - Date.parse(row.occurredAt);
  if (!Number.isFinite(diffMs) || diffMs < 0) {
    return { isBackfill: false, label: "实时" };
  }
  const sec = Math.round(diffMs / 1000);
  if (sec > 60) {
    const mins = Math.floor(sec / 60);
    const rem = sec % 60;
    return {
      isBackfill: true,
      label: rem > 0 ? `补报 (+${mins}m ${rem}s)` : `补报 (+${mins}m)`,
    };
  }
  const frac = (diffMs / 1000).toFixed(diffMs < 10_000 ? 1 : 0);
  return { isBackfill: false, label: `实时 (+${frac}s)` };
}

function statusPillClass(status: string | null): string {
  if (status === null) return "tp-status-pill tp-st-na";
  const lower = status.toLowerCase();
  if (
    lower === "ok" ||
    lower === "success" ||
    lower === "succeeded" ||
    lower === "completed" ||
    lower === "200"
  ) {
    return "tp-status-pill tp-st-ok";
  }
  if (
    lower.includes("err") ||
    lower.includes("fail") ||
    lower === "timeout" ||
    lower === "cancelled" ||
    lower === "canceled" ||
    /^[45]\d\d$/.test(lower)
  ) {
    return "tp-status-pill tp-st-err";
  }
  return "tp-status-pill tp-st-na";
}

function RequestTable({
  rows,
  timezone,
  selectedId,
  onSelect,
}: {
  rows: RequestRow[];
  timezone: string;
  selectedId: string | null;
  onSelect: (row: RequestRow) => void;
}) {
  return (
    <div className="geek-table-wrap">
      <table className="geek-table">
        <thead>
          <tr>
            <th scope="col">发生时间</th>
            <th scope="col">模型 / 供应商</th>
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
            <th scope="col" className="geek-th-num">
              生成速度
            </th>
            <th scope="col" className="geek-th-num">
              TTFT
            </th>
            <th scope="col">状态</th>
            <th scope="col" className="geek-th-num">
              耗时
            </th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => {
            const lag = lagInfo(r);
            return (
              <tr
                key={r.id}
                data-testid="request-row"
                data-selected={selectedId === r.id ? "true" : undefined}
                onClick={() => onSelect(r)}
              >
                <td className="geek-num text-muted-foreground">
                  {formatInstant(r.occurredAt, timezone, true)}
                  {lag.isBackfill ? (
                    <>
                      {" "}
                      <span className="tp-status-pill tp-st-warn">补报</span>
                    </>
                  ) : null}
                </td>
                <td>
                  <strong className="geek-num font-semibold">{r.model}</strong>{" "}
                  <span className="tp-prov-tag">{r.provider}</span>
                </td>
                <td className={r.project ? undefined : "text-muted-foreground"}>
                  {r.project ? r.project.name : "未归属"}
                </td>
                <td className="geek-num text-muted-foreground text-xs">
                  {r.source.platform} · {r.source.instanceKey}
                </td>
                <td className="geek-td-num">{formatTokenField(r.input)}</td>
                <td
                  className="geek-td-num"
                  style={
                    r.output.quality !== "missing" && r.output.value !== null
                      ? { color: "var(--c-output)" }
                      : undefined
                  }
                >
                  {formatTokenField(r.output)}
                </td>
                <td className="geek-td-num font-semibold">
                  {formatTokenField(r.total)}
                </td>
                <td className="geek-td-num">
                  {formatSpeedTps(r.generationSpeedTps)}
                </td>
                <td className="geek-td-num text-muted-foreground">
                  {formatMilliseconds(r.ttftMs)}
                </td>
                <td>
                  <span className={statusPillClass(r.status)}>
                    {formatStatus(r.status)}
                  </span>
                </td>
                <td className="geek-td-num text-muted-foreground">
                  {formatDuration(r.durationMs)}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function qualitySummary(row: RequestRow): string {
  const qs = new Set([
    row.input.quality,
    row.output.quality,
    row.total.quality,
  ]);
  if (qs.size === 1) return row.total.quality;
  return `in:${row.input.quality} / out:${row.output.quality} / total:${row.total.quality}`;
}

function RequestDetailDrawer({
  row,
  timezone,
  onClose,
}: {
  row: RequestRow;
  timezone: string;
  onClose: () => void;
}) {
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  const lag = lagInfo(row);

  return (
    <div
      className="tp-drawer-mask"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onClose();
      }}
    >
      <aside className="tp-drawer" role="dialog" aria-label="请求详情">
        <div className="tp-drawer-head">
          <div>
            <div className="geek-num text-muted-foreground text-[11px]">
              REQUEST DETAIL
            </div>
            <h3 className="geek-num mt-1 text-base font-semibold">
              {row.model}
            </h3>
          </div>
          <Button variant="outline" size="sm" onClick={onClose}>
            关闭
          </Button>
        </div>

        <div className="flex flex-wrap gap-2">
          <span className="tp-prov-tag">{row.provider}</span>
          <span className={statusPillClass(row.status)}>
            {formatStatus(row.status)}
          </span>
          <span
            className={
              lag.isBackfill
                ? "tp-status-pill tp-st-warn"
                : "tp-status-pill tp-st-na"
            }
          >
            {lag.label}
          </span>
        </div>

        <div
          className="geek-card"
          style={{
            padding: "12px 14px",
            gap: 10,
            background: "var(--surface-subtle)",
          }}
        >
          <div className="text-xs font-semibold">时间与上报延迟</div>
          <div className="tp-kv-grid">
            <span className="tp-kv-k">发生时间</span>
            <span className="tp-kv-v geek-num">
              {formatInstant(row.occurredAt, timezone, true)}
            </span>
            <span className="tp-kv-k">服务端接收</span>
            <span className="tp-kv-v geek-num">
              {formatInstant(row.receivedAt, timezone, true)}
            </span>
            <span className="tp-kv-k">上报延迟</span>
            <span className="tp-kv-v geek-num">{lag.label}</span>
          </div>
        </div>

        <div
          className="geek-card"
          style={{
            padding: "12px 14px",
            gap: 10,
            background: "var(--surface-subtle)",
          }}
        >
          <div className="text-xs font-semibold">Token 与性能指标</div>
          <div className="tp-kv-grid">
            <span className="tp-kv-k">输入 / 输出</span>
            <span className="tp-kv-v geek-num">
              {formatTokenField(row.input)} / {formatTokenField(row.output)}
            </span>
            <span className="tp-kv-k">总计 Token</span>
            <span className="tp-kv-v geek-num font-semibold">
              {formatTokenField(row.total)}
            </span>
            <span className="tp-kv-k">口径质量</span>
            <span className="tp-kv-v geek-num">{qualitySummary(row)}</span>
            <span className="tp-kv-k">生成速度</span>
            <span className="tp-kv-v geek-num">
              {formatSpeedTps(row.generationSpeedTps)}
            </span>
            <span className="tp-kv-k">首字延迟 TTFT</span>
            <span className="tp-kv-v geek-num">
              {formatMilliseconds(row.ttftMs)}
            </span>
            <span className="tp-kv-k">总耗时</span>
            <span className="tp-kv-v geek-num">
              {formatDuration(row.durationMs)}
            </span>
          </div>
        </div>

        <div
          className="geek-card"
          style={{
            padding: "12px 14px",
            gap: 10,
            background: "var(--surface-subtle)",
          }}
        >
          <div className="text-xs font-semibold">归属与事件标识</div>
          <div className="tp-kv-grid">
            <span className="tp-kv-k">事件 ID</span>
            <span className="tp-kv-v geek-num">{row.id}</span>
            <span className="tp-kv-k">归属项目</span>
            <span className="tp-kv-v geek-num">
              {row.project ? row.project.name : "未归属 (null)"}
            </span>
            <span className="tp-kv-k">来源实例</span>
            <span className="tp-kv-v geek-num">
              {row.source.platform} · {row.source.instanceKey}
            </span>
          </div>
        </div>
      </aside>
    </div>
  );
}
