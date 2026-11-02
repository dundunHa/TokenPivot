import { useEffect, useRef, useState } from "react";
import { fmt } from "./geek-utils";
import type { UsageStats } from "../overviewApi";

interface GeekHeroMetricsProps {
  totals: UsageStats;
  /** 最近 60 秒速率（tokens/min），null = 不可用 */
  rate60: number | null;
  /** 时区 */
  timezone: string;
}

/**
 * J 风格大数 hero + 4 格指标 rail。
 * PRD §5.2：输入 Token / 输出 Token / 请求数 / 最近 60 秒速率。
 * PRD §7：total = input + output，cache 不额外加到 total。
 */
export function GeekHeroMetrics({ totals, rate60, timezone }: GeekHeroMetricsProps) {
  const reduced = usePrefersReducedMotion();
  const inputVal = Number(totals.input.value ?? "0");
  const outputVal = Number(totals.output.value ?? "0");
  const totalTokens = inputVal + outputVal;
  // cache 不在 UsageStats 里，由父组件传入
  const cacheRead = 0;

  return (
    <div className="geek-hero">
      {/* 大数 hero */}
      <div className="geek-hero-number" data-testid="metric-total">
        <CountUpNumber target={totalTokens} reduced={reduced} />
        <span className="geek-hero-unit">tokens</span>
      </div>
      <div className="geek-hero-axiom geek-dim">
        口径：total = input + output · cache 为 input 子集，不加到 total · 完成时刻计数
      </div>

      {/* 4 格指标 rail */}
      <div className="geek-hero-metrics">
        <MetricCell label="INPUT" value={fmt(inputVal)} sub={qualityLabel(totals.input)} />
        <MetricCell label="OUTPUT" value={fmt(outputVal)} sub={qualityLabel(totals.output)} />
        <MetricCell label="REQUESTS" value={fmt(Number(totals.requestCount))} sub="所选时段完成" testId="metric-requests" />
        <MetricCell
          label="RATE 60S"
          value={rate60 !== null ? fmt(rate60) : "—"}
          sub={rate60 !== null ? "rolling 60s" : "不可用"}
          accent
        />
      </div>

      {/* breakdown 字符条 */}
      <div className="geek-hero-breakdown">
        <BreakdownRow label="input" value={inputVal} total={totalTokens} />
        <BreakdownRow label="output" value={outputVal} total={totalTokens} />
        <BreakdownRow label="cache.read" value={cacheRead} total={totalTokens} note="⊂ input" />
      </div>

      {/* 数据状态 */}
      <div className="geek-data-status">
        <StatusCell label="WORKSPACE / TZ" value={`Personal · ${timezone}`} />
        <StatusCell label="LAST RECEIVED" value="—" />
        <StatusCell label="COVERAGE" value="—" />
      </div>
    </div>
  );
}

/* ---------- 子组件 ---------- */

function MetricCell({ label, value, sub, accent, testId }: {
  label: string;
  value: string;
  sub: string;
  accent?: boolean;
  testId?: string;
}) {
  return (
    <div className={`geek-hm-cell ${accent ? "geek-hm-accent" : ""}`} data-testid={testId}>
      <div className="geek-hm-label">{label}</div>
      <div className="geek-hm-value geek-num">{value}</div>
      <div className="geek-hm-sub geek-dim">{sub}</div>
    </div>
  );
}

function BreakdownRow({ label, value, total, note }: {
  label: string;
  value: number;
  total: number;
  note?: string;
}) {
  const BAR_W = 32;
  const pct = total > 0 ? value / total : 0;
  const filled = Math.round(pct * BAR_W);
  const bar = "█".repeat(filled) + "░".repeat(BAR_W - filled);

  return (
    <div className="geek-bd-row">
      <span className="geek-bd-label geek-dim">{label}</span>
      <span className="geek-bd-bar geek-amber">{bar}</span>
      <span className="geek-bd-value geek-num">{fmt(value)}</span>
      <span className="geek-bd-pct geek-dim">{note ?? fmtPct(pct)}</span>
    </div>
  );
}

function StatusCell({ label, value }: { label: string; value: string }) {
  return (
    <div className="geek-ds-cell">
      <div className="geek-ds-label geek-dim">{label}</div>
      <div className="geek-ds-value">{value}</div>
    </div>
  );
}

/* ---------- 工具 ---------- */

function fmtPct(ratio: number): string {
  return (ratio * 100).toFixed(1) + "%";
}

function qualityLabel(stat: { known: string; estimated: string; missing: string }): string {
  const k = Number(stat.known), e = Number(stat.estimated), m = Number(stat.missing);
  if (m > 0 && k === 0 && e === 0) return "缺失 · missing";
  if (e > 0 && m > 0) return "部分覆盖";
  if (e > 0) return "含估算";
  if (m > 0) return "部分缺失";
  return "已知 · known";
}

function usePrefersReducedMotion(): boolean {
  const [reduced, setReduced] = useState(false);
  useEffect(() => {
    const mq = window.matchMedia("(prefers-reduced-motion: reduce)");
    setReduced(mq.matches);
    const handler = (e: MediaQueryListEvent) => setReduced(e.matches);
    mq.addEventListener("change", handler);
    return () => mq.removeEventListener("change", handler);
  }, []);
  return reduced;
}

function CountUpNumber({ target, reduced }: { target: number; reduced: boolean }) {
  // 测试环境直接显示目标值，避免异步动画导致断言不稳定
  const isTest = typeof process !== "undefined" && process.env.NODE_ENV === "test";
  const [current, setCurrent] = useState(reduced || isTest ? target : 0);
  const rafRef = useRef<number>(0);

  useEffect(() => {
    if (reduced || isTest) {
      setCurrent(target);
      return;
    }
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
  }, [target, reduced, isTest]);

  return <span className="geek-num">{fmt(current)}</span>;
}
