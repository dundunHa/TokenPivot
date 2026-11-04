import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import type { AggregateData, UsageStats } from "../overviewApi";
import { formatCount, qualityNote } from "../overviewView";
import { fmt } from "./geek-utils";

function toNumber(value: string | null): number | null {
  return value === null ? null : Number(value);
}

/** 当前值相对上一周期的变化；上一周期为 0 或缺失时没有环比。 */
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
      {up ? "▲" : "▼"} {Math.abs(value * 100).toFixed(1)}%
    </span>
  );
}

/** 极简趋势线：只表达走势，不带坐标轴。null 视为断点。 */
function Sparkline({ values }: { values: (number | null)[] }) {
  const nums = values.map((v) => v ?? 0);
  if (nums.length < 2) return <div className="geek-spark" />;
  const max = Math.max(...nums, 1);
  const x = (i: number) => (i / (nums.length - 1)) * 100;
  const y = (v: number) => 28 - (v / max) * 24;
  const line = nums
    .map(
      (v, i) => `${i === 0 ? "M" : "L"}${x(i).toFixed(2)},${y(v).toFixed(2)}`,
    )
    .join(" ");
  return (
    <svg
      className="geek-spark"
      viewBox="0 0 100 32"
      preserveAspectRatio="none"
      aria-hidden
    >
      <path
        d={line}
        fill="none"
        stroke="var(--geek-amber)"
        strokeWidth="1.5"
        vectorEffect="non-scaling-stroke"
      />
    </svg>
  );
}

function Kpi({
  label,
  children,
  className = "",
  testId,
}: {
  label: string;
  children: React.ReactNode;
  className?: string;
  testId?: string;
}) {
  return (
    <div className={`geek-kpi ${className}`} data-testid={testId}>
      <div className="geek-kpi-label">{label}</div>
      {children}
    </div>
  );
}

interface GeekKpisProps {
  data: AggregateData;
  /** 上一周期总量；未就绪或失败时为 undefined。 */
  previous: UsageStats | undefined;
  /** 第四张卡（实时速率）由请求模块提供。 */
  rate: React.ReactNode;
}

/** 顶部指标：总用量（含输入/输出占比）、请求数、每请求均值、实时速率。 */
export function GeekKpis({ data, previous, rate }: GeekKpisProps) {
  const { totals, points } = data;
  const reduced = usePrefersReducedMotion();
  const total = toNumber(totals.total.value);
  const input = toNumber(totals.input.value);
  const output = toNumber(totals.output.value);
  const requests = Number(totals.requestCount);
  const average = total !== null && requests > 0 ? total / requests : null;

  const prevTotal = previous ? toNumber(previous.total.value) : null;
  const prevRequests = previous ? Number(previous.requestCount) : null;
  const prevAverage =
    prevTotal !== null && prevRequests !== null && prevRequests > 0
      ? prevTotal / prevRequests
      : null;

  const totalSeries = points.map((p) => toNumber(p.stats.total.value));
  const requestSeries = points.map((p) => Number(p.stats.requestCount));
  const averageSeries = points.map((p) => {
    const t = toNumber(p.stats.total.value);
    const n = Number(p.stats.requestCount);
    return t !== null && n > 0 ? t / n : null;
  });

  const inputShare = total && input !== null ? input / total : null;
  const outputShare = total && output !== null ? output / total : null;
  const empty = totals.requestCount === "0" && total === null;
  const totalNote = empty ? "暂无数据" : qualityNote(totals.total);

  return (
    <div className="geek-kpis">
      <Kpi label="总用量" className="geek-kpi-hero">
        <div className="geek-kpi-row">
          <div className="geek-kpi-number geek-num" data-testid="metric-total">
            {total !== null ? (
              <CountUp target={total} reduced={reduced} />
            ) : (
              "—"
            )}
            {total === null ? null : (
              <span className="geek-kpi-unit">tokens</span>
            )}
          </div>
          <Delta value={change(total, prevTotal)} />
        </div>
        {totalNote ? <div className="geek-kpi-note">{totalNote}</div> : null}
        <Sparkline values={totalSeries} />
        <div className="geek-split-bar" aria-hidden>
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
            {inputShare !== null ? ` · ${(inputShare * 100).toFixed(0)}%` : ""}
          </span>
          <span data-testid="metric-output">
            <i className="geek-dot-out" />
            输出 {formatCount(totals.output.value)}
            {outputShare !== null
              ? ` · ${(outputShare * 100).toFixed(0)}%`
              : ""}
          </span>
        </div>
      </Kpi>

      <Kpi label="请求数" testId="metric-requests">
        <div className="geek-kpi-row">
          <div className="geek-kpi-number geek-num">
            {formatCount(totals.requestCount)}
          </div>
          <Delta value={change(requests, prevRequests)} />
        </div>
        <Sparkline values={requestSeries} />
      </Kpi>

      <Kpi label="平均每请求">
        <div className="geek-kpi-row">
          <div className="geek-kpi-number geek-num">
            {average === null ? "—" : fmt(average)}
            {average === null ? null : (
              <span className="geek-kpi-unit">tokens</span>
            )}
          </div>
          <Delta value={change(average, prevAverage)} />
        </div>
        <Sparkline values={averageSeries} />
      </Kpi>

      {rate}
    </div>
  );
}

/** 第四张卡的外壳，供速率模块复用同一视觉。 */
export function GeekKpiShell({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return <Kpi label={label}>{children}</Kpi>;
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

/**
 * 数字变化时从“当前正在显示的值”过渡到新值（增量），而不是每次从 0 重新计数；
 * 只有首次出现时从 0 起。
 */
function CountUp({ target, reduced }: { target: number; reduced: boolean }) {
  // 测试环境直接显示目标值，避免异步动画导致断言不稳定
  const isTest =
    typeof process !== "undefined" && process.env.NODE_ENV === "test";
  const skip = reduced || isTest;
  const [current, setCurrent] = useState(skip ? target : 0);
  /** 最近一次渲染出的值：下一段动画的起点。 */
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
