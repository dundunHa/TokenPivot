import { useMemo } from "react";
import type { AggregateData } from "../overviewApi";
import { formatCount } from "../overviewView";
import { buildHeatmap } from "./heatmap";

const HOURS = Array.from({ length: 24 }, (_, h) => h);

/** 最近 7 天的活跃时段：行为日期，列为本地小时，颜色深浅为 token 总量。 */
export function GeekHeatmap({
  data,
  timezone,
}: {
  data: AggregateData;
  timezone: string;
}) {
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
        <h2 className="geek-card-title">
          活跃时段
          <span className="geek-card-sub">最近 7 天</span>
        </h2>
        {peakText ? (
          <span className="geek-card-sub" data-testid="heat-peak">
            {peakText}
          </span>
        ) : null}
      </div>
      {empty ? (
        <p className="geek-empty geek-dim">暂无数据</p>
      ) : (
        <div className="geek-heat" data-testid="heatmap">
          <div className="geek-heat-axis" aria-hidden>
            <span />
            {HOURS.map((h) => (
              <span key={h}>
                {h % 3 === 0 ? String(h).padStart(2, "0") : ""}
              </span>
            ))}
          </div>
          {map.days.map((day) => (
            <div className="geek-heat-row" key={day.key}>
              <span className="geek-heat-day">{day.label}</span>
              {day.cells.map((cell, hour) => {
                const ratio =
                  cell.tokens === null || map.max === 0
                    ? null
                    : cell.tokens / map.max;
                return (
                  <i
                    key={hour}
                    className="geek-heat-cell"
                    style={
                      ratio === null
                        ? undefined
                        : {
                            background: `rgba(224, 166, 75, ${(0.07 + 0.93 * ratio).toFixed(3)})`,
                          }
                    }
                    title={`${day.label} ${String(hour).padStart(2, "0")}:00 · ${
                      cell.tokens === null
                        ? "无数据"
                        : `${formatCount(String(Math.round(cell.tokens)))} tokens · ${cell.requests} 请求`
                    }`}
                  />
                );
              })}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
