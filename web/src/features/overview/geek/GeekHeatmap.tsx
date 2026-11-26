import { useMemo, useState } from "react";
import type { AggregateData } from "../overviewApi";
import { formatCount } from "../overviewView";
import { buildHeatmap } from "./heatmap";

const HOURS = Array.from({ length: 24 }, (_, h) => h);

function tierOf(tokens: number | null, max: number): number {
  if (tokens === null || tokens <= 0 || max <= 0) return 0;
  const ratio = Math.sqrt(tokens / max);
  if (ratio < 0.25) return 1;
  if (ratio < 0.5) return 2;
  if (ratio < 0.78) return 3;
  return 4;
}

interface HoverCell {
  dayKey: string;
  dayLabel: string;
  hour: number;
  tokens: number | null;
  requests: number;
  x: number;
  y: number;
}

/** 7×24 活跃时段热力图：5 档主题色阶、行列联动高亮与即时浮层提示。 */
export function GeekHeatmap({
  data,
  timezone,
}: {
  data: AggregateData;
  timezone: string;
}) {
  const [hover, setHover] = useState<HoverCell | null>(null);
  const map = useMemo(
    () => buildHeatmap(data.points, timezone),
    [data.points, timezone],
  );
  const empty = map.days.length === 0 || map.max === 0;
  const peakText = map.peak
    ? `峰值 ${map.peak.day} ${String(map.peak.hour).padStart(2, "0")}:00 · ${formatCount(String(Math.round(map.peak.tokens)))}`
    : null;

  return (
    <div className="geek-card" role="region" aria-label="活跃时段">
      <div className="geek-card-head">
        <div className="flex flex-wrap items-baseline gap-2">
          <h2 className="geek-card-title">7×24 活跃时段</h2>
          {peakText ? (
            <span className="geek-card-sub" data-testid="heat-peak">
              <strong className="geek-amber font-medium">{peakText}</strong>
            </span>
          ) : null}
        </div>
        <div className="tp-heat-legend" aria-hidden="true">
          <span>少</span>
          <i className="tp-heat-sw tp-h0" />
          <i className="tp-heat-sw tp-h1" />
          <i className="tp-heat-sw tp-h2" />
          <i className="tp-heat-sw tp-h3" />
          <i className="tp-heat-sw tp-h4" />
          <span>多</span>
        </div>
      </div>

      {empty ? (
        <p className="geek-empty geek-dim">暂无数据</p>
      ) : (
        <div
          className="geek-heat"
          data-testid="heatmap"
          onMouseLeave={() => setHover(null)}
        >
          <div className="geek-heat-axis" aria-hidden="true">
            <span />
            {HOURS.map((h) => (
              <span key={h} data-hi={hover?.hour === h ? "true" : undefined}>
                {h % 3 === 0 ? String(h).padStart(2, "0") : ""}
              </span>
            ))}
          </div>
          {map.days.map((day) => (
            <div
              className="geek-heat-row"
              key={day.key}
              data-hi={hover?.dayKey === day.key ? "true" : undefined}
            >
              <span className="geek-heat-day">{day.label}</span>
              {day.cells.map((cell, hour) => {
                const lv = tierOf(cell.tokens, map.max);
                const hh = String(hour).padStart(2, "0");
                return (
                  <i
                    key={hour}
                    className={`geek-heat-cell tp-h${lv}`}
                    title={`${day.label} ${hh}:00 · ${
                      cell.tokens === null
                        ? "无数据"
                        : `${formatCount(String(Math.round(cell.tokens)))} tokens · ${cell.requests} 请求`
                    }`}
                    onMouseMove={(e) =>
                      setHover({
                        dayKey: day.key,
                        dayLabel: day.label,
                        hour,
                        tokens: cell.tokens,
                        requests: cell.requests,
                        x: e.clientX,
                        y: e.clientY,
                      })
                    }
                  />
                );
              })}
            </div>
          ))}
        </div>
      )}

      {hover ? (
        <div
          className="tp-float-tooltip geek-tooltip"
          style={{
            left: Math.max(
              10,
              Math.min(
                hover.x + 14,
                (typeof window !== "undefined" ? window.innerWidth : 1200) -
                  220,
              ),
            ),
            top: Math.max(
              10,
              Math.min(
                hover.y + 14,
                (typeof window !== "undefined" ? window.innerHeight : 800) -
                  100,
              ),
            ),
          }}
        >
          <div className="geek-tooltip-time">
            {hover.dayLabel} · {String(hover.hour).padStart(2, "0")}:00–
            {String(hover.hour).padStart(2, "0")}:59
          </div>
          {hover.tokens === null ? (
            <div className="geek-tooltip-row">
              <span>暂无数据</span>
            </div>
          ) : (
            <>
              <div className="geek-tooltip-row">
                <i style={{ background: "var(--accent-brand)" }} />
                <span>消耗总量</span>
                <b className="geek-num">
                  {formatCount(String(Math.round(hover.tokens)))} tok
                </b>
              </div>
              <div className="geek-tooltip-row">
                <i style={{ background: "var(--c-input)" }} />
                <span>请求数</span>
                <b className="geek-num">{hover.requests} 次</b>
              </div>
            </>
          )}
        </div>
      ) : null}
    </div>
  );
}
