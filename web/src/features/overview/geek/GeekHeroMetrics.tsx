import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import { fmt } from "./geek-utils";
import { metricQuality, qualityNote, formatCount } from "../overviewView";
import type { UsageStats, MetricStat } from "../overviewApi";

interface GeekHeroMetricsProps {
  totals: UsageStats;
  /** 最近 60 秒速率（tokens/min），null = 不可用 */
  rate60: number | null;
}

/**
 * J 风格大数 hero + 4 格指标 rail。
 * PRD §5.2：输入 Token / 输出 Token / 请求数 / 最近 60 秒速率。
 * PRD §7：total 取自服务端，cache 不额外加到 total。
 */
export function GeekHeroMetrics({ totals, rate60 }: GeekHeroMetricsProps) {
  const reduced = usePrefersReducedMotion();
  const totalStat = totals.total;
  const totalVal = totalStat.value !== null ? Number(totalStat.value) : null;

  return (
    <div className="geek-hero">
      <div className="geek-hero-label">TOTAL · 所选时段总用量</div>
      {/* 大数 hero — 总量取自服务端 */}
      <div className="geek-hero-number" data-testid="metric-total">
        {totalVal !== null ? (
          <CountUpNumber target={totalVal} reduced={reduced} />
        ) : (
          <span className="geek-num">—</span>
        )}
        <span className="geek-hero-unit">tokens</span>
      </div>
      <div className="geek-hero-axiom geek-dim">
        {qualityNote(totalStat) ??
          "口径：total = input + output · cache 为 input 子集，不加到 total · 完成时刻计数"}
      </div>

      {/* 4 格指标 rail */}
      <div className="geek-hero-metrics">
        <MetricCell
          label="INPUT"
          value={formatCount(totals.input.value)}
          sub={qualityLabel(totals.input)}
          testId="metric-input"
        />
        <MetricCell
          label="OUTPUT"
          value={formatCount(totals.output.value)}
          sub={qualityLabel(totals.output)}
          testId="metric-output"
        />
        <MetricCell
          label="REQUESTS"
          value={formatCount(totals.requestCount)}
          sub="所选时段完成"
          testId="metric-requests"
        />
        <MetricCell
          label="RATE 60S"
          value={rate60 !== null ? fmt(rate60) : "—"}
          sub={rate60 !== null ? "rolling 60s" : "不可用"}
          accent
        />
      </div>

      {/* breakdown 字符条 */}
      <div className="geek-hero-breakdown">
        <BreakdownRow label="input" stat={totals.input} total={totalVal} />
        <BreakdownRow label="output" stat={totals.output} total={totalVal} />
      </div>
    </div>
  );
}

/* ---------- 子组件 ---------- */

function MetricCell({
  label,
  value,
  sub,
  accent,
  testId,
}: {
  label: string;
  value: string;
  sub: string;
  accent?: boolean;
  testId?: string;
}) {
  return (
    <div
      className={`geek-hm-cell ${accent ? "geek-hm-accent" : ""}`}
      data-testid={testId}
    >
      <div className="geek-hm-label">{label}</div>
      <div className="geek-hm-value geek-num">{value}</div>
      <div className="geek-hm-sub geek-dim">{sub}</div>
    </div>
  );
}

function BreakdownRow({
  label,
  stat,
  total,
}: {
  label: string;
  stat: MetricStat;
  total: number | null;
}) {
  const val = stat.value !== null ? Number(stat.value) : null;
  const pct = total !== null && total > 0 && val !== null ? val / total : 0;
  const q = metricQuality(stat);

  return (
    <div className="geek-bd-row">
      <span className="geek-dim">{label}</span>
      <span className="geek-bd-bar" aria-hidden>
        <span style={{ width: `${Math.round(pct * 1000) / 10}%` }} />
      </span>
      <span className="geek-bd-value geek-num">{formatCount(stat.value)}</span>
      <span className="geek-bd-pct geek-dim">
        {q !== "known"
          ? qualityNote(stat)
          : total !== null && total > 0
            ? `${(pct * 100).toFixed(1)}%`
            : "—"}
      </span>
    </div>
  );
}

/* ---------- 工具 ---------- */

function qualityLabel(stat: MetricStat): string {
  return qualityNote(stat) ?? "已知 · known";
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

function CountUpNumber({
  target,
  reduced,
}: {
  target: number;
  reduced: boolean;
}) {
  // 测试环境直接显示目标值，避免异步动画导致断言不稳定
  const isTest =
    typeof process !== "undefined" && process.env.NODE_ENV === "test";
  const skipAnimation = reduced || isTest;
  const [current, setCurrent] = useState(skipAnimation ? target : 0);
  const rafRef = useRef<number>(0);

  useEffect(() => {
    if (skipAnimation) return;
    const start = performance.now();
    const duration = 1200;
    const from = 0;
    const animate = (now: number) => {
      const elapsed = now - start;
      const progress = Math.min(elapsed / duration, 1);
      const eased = 1 - Math.pow(1 - progress, 3);
      setCurrent(Math.round(from + (target - from) * eased));
      if (progress < 1) {
        rafRef.current = requestAnimationFrame(animate);
      }
    };
    rafRef.current = requestAnimationFrame(animate);
    return () => cancelAnimationFrame(rafRef.current);
  }, [target, skipAnimation]);

  // skipAnimation 时直接显示目标值
  const display = skipAnimation ? target : current;
  return <span className="geek-num">{fmt(display)}</span>;
}
