import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import type { RateData } from "@/features/requests/api";
import { trimDecimal } from "@/features/requests/model";
import type { AggregateData, UsageStats } from "../overviewApi";
import { formatCount, qualityNote } from "../overviewView";
import { shareOf } from "./composition";
import { fmt } from "./geek-utils";

function toNumber(value: string | null): number | null {
  return value === null ? null : Number(value);
}

function change(current: number | null, previous: number | null) {
  if (current === null || previous === null || previous === 0) return null;
  return (current - previous) / previous;
}

function Delta({ value }: { value: number | null }) {
  if (value === null) return null;
  const up = value >= 0;
  return (
    <span
      className={`geek-delta ${up ? "geek-delta-up" : "geek-delta-down"}`}
      title="较上一周期"
    >
      {up ? "+" : "-"}
      {Math.abs(value * 100).toFixed(1)}%
    </span>
  );
}

function Kpi({
  label,
  tag,
  children,
  className = "",
  testId,
}: {
  label: string;
  tag?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
  testId?: string;
}) {
  return (
    <article className={`geek-kpi ${className}`} data-testid={testId}>
      <div className="geek-kpi-top">
        <span className="geek-kpi-label">{label}</span>
        {tag ? <span className="geek-num text-[11px]">{tag}</span> : null}
      </div>
      {children}
    </article>
  );
}

export interface FastestModelInfo {
  model: string;
  speedText: string;
}

interface GeekKpisProps {
  data: AggregateData;
  previous: UsageStats | undefined;
  rate?: RateData;
  fastestModel?: FastestModelInfo | null;
}

/** 顶部 5 格 KPI 条：总 Token 消耗、请求总量、缓存命中率、生成速度、首 Token 延迟 TTFT。 */
export function GeekKpis({
  data,
  previous,
  rate,
  fastestModel,
}: GeekKpisProps) {
  const { totals } = data;
  const reduced = usePrefersReducedMotion();
  const total = toNumber(totals.total.value);
  const input = toNumber(totals.input.value);
  const output = toNumber(totals.output.value);
  const requests = Number(totals.requestCount);
  const average = total !== null && requests > 0 ? total / requests : null;

  const prevTotal = previous ? toNumber(previous.total.value) : null;
  const prevRequests = previous ? Number(previous.requestCount) : null;

  const inputShare = total && input !== null ? input / total : null;
  const outputShare = total && output !== null ? output / total : null;
  const cacheHit = shareOf(totals.cacheRead, totals.input);
  const prevCacheHit = previous
    ? shareOf(previous.cacheRead, previous.input)
    : null;

  const empty = totals.requestCount === "0" && total === null;
  const totalNote = empty ? "暂无数据" : qualityNote(totals.total);

  const speedVal =
    rate &&
    rate.generationSpeed.value !== null &&
    rate.generationSpeed.sampleCount > 0
      ? trimDecimal(rate.generationSpeed.value)
      : null;
  const ttftVal =
    rate && rate.ttft.value !== null && rate.ttft.sampleCount > 0
      ? trimDecimal(rate.ttft.value)
      : null;

  return (
    <section className="geek-kpis" aria-label="核心指标">
      <Kpi
        label="总 Token 消耗"
        tag={
          inputShare !== null && outputShare !== null
            ? `输入 ${(inputShare * 100).toFixed(0)}% · 输出 ${(outputShare * 100).toFixed(0)}%`
            : undefined
        }
        className="geek-kpi-hero"
      >
        <div className="geek-kpi-row">
          <div className="geek-kpi-number geek-num" data-testid="metric-total">
            {total !== null ? (
              <CountUp target={total} reduced={reduced} />
            ) : (
              "—"
            )}
            {total === null ? null : <span className="geek-kpi-unit">tok</span>}
          </div>
          <Delta value={change(total, prevTotal)} />
        </div>
        {totalNote ? <div className="geek-kpi-note">{totalNote}</div> : null}
        <div className="geek-split-bar" aria-hidden="true">
          <span
            className="geek-split-in"
            style={{ width: `${(inputShare ?? 0) * 100}%` }}
          />
          <span
            className="geek-split-out"
            style={{ width: `${(outputShare ?? 0) * 100}%` }}
          />
        </div>
        <div className="geek-split-legend">
          <span data-testid="metric-input">
            <i className="geek-dot-in" />
            输入 {formatCount(totals.input.value)}
          </span>
          <span data-testid="metric-output">
            <i className="geek-dot-out" />
            输出 {formatCount(totals.output.value)}
          </span>
        </div>
      </Kpi>

      <Kpi label="请求总量" testId="metric-requests">
        <div className="geek-kpi-row">
          <div className="geek-kpi-number geek-num">
            {formatCount(totals.requestCount)}
          </div>
          <Delta value={change(requests, prevRequests)} />
        </div>
        {average !== null ? (
          <div className="geek-kpi-note geek-num">
            均 {fmt(average)} tok/req
          </div>
        ) : null}
      </Kpi>

      <Kpi label="缓存命中率" tag="占输入" testId="metric-cache-hit">
        <div className="geek-kpi-row">
          <div className="geek-kpi-number geek-num">
            {cacheHit !== null ? (
              <>
                {(cacheHit * 100).toFixed(1)}
                <span className="geek-kpi-unit">%</span>
              </>
            ) : (
              "—"
            )}
          </div>
          <Delta value={change(cacheHit, prevCacheHit)} />
        </div>
        <div className="geek-split-bar" aria-hidden="true">
          <span
            className="geek-split-cache"
            style={{ width: `${Math.min(100, (cacheHit ?? 0) * 100)}%` }}
          />
        </div>
        <div className="geek-kpi-note geek-num">
          读 {formatCount(totals.cacheRead.value)} · 写{" "}
          {formatCount(totals.cacheWrite.value)}
        </div>
      </Kpi>

      <Kpi
        label="生成速度"
        tag={
          rate
            ? `${rate.generationSpeed.sampleCount}/${rate.eventCount}`
            : undefined
        }
        testId="metric-speed"
      >
        <div className="geek-kpi-row">
          <div className="geek-kpi-number geek-num">
            {speedVal ?? "—"}
            {speedVal === null ? null : (
              <span className="geek-kpi-unit">tok/s</span>
            )}
          </div>
        </div>
        {fastestModel ? (
          <div className="geek-kpi-note geek-num truncate">
            最快 {fastestModel.model} {fastestModel.speedText} tok/s
          </div>
        ) : null}
      </Kpi>

      <Kpi
        label="首 Token 延迟 TTFT"
        tag={rate ? `${rate.ttft.sampleCount}/${rate.eventCount}` : undefined}
        testId="metric-ttft"
      >
        <div className="geek-kpi-row">
          <div className="geek-kpi-number geek-num">
            {ttftVal ?? "—"}
            {ttftVal === null ? null : (
              <span className="geek-kpi-unit">ms</span>
            )}
          </div>
        </div>
      </Kpi>
    </section>
  );
}

function usePrefersReducedMotion(): boolean {
  return useSyncExternalStore(
    (callback) => {
      if (typeof window === "undefined" || !window.matchMedia) return () => {};
      const mq = window.matchMedia("(prefers-reduced-motion: reduce)");
      mq.addEventListener("change", callback);
      return () => mq.removeEventListener("change", callback);
    },
    () => {
      if (typeof window === "undefined" || !window.matchMedia) return false;
      return window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    },
    () => false,
  );
}

function CountUp({ target, reduced }: { target: number; reduced: boolean }) {
  const isTest =
    typeof process !== "undefined" && process.env.NODE_ENV === "test";
  const skip = reduced || isTest;
  const [current, setCurrent] = useState(skip ? target : 0);
  const shown = useRef(skip ? target : 0);
  const raf = useRef(0);

  useEffect(() => {
    if (skip) return;
    const from = shown.current;
    if (from === target) return;
    const start = performance.now();
    const duration = 600;
    const animate = (now: number) => {
      const progress = Math.min((now - start) / duration, 1);
      const value = Math.round(
        from + (target - from) * (1 - Math.pow(1 - progress, 3)),
      );
      shown.current = value;
      setCurrent(value);
      if (progress < 1) raf.current = requestAnimationFrame(animate);
    };
    raf.current = requestAnimationFrame(animate);
    return () => cancelAnimationFrame(raf.current);
  }, [target, skip]);

  return <>{fmt(skip ? target : current)}</>;
}
