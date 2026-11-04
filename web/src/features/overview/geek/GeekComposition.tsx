import type { AggregateData } from "../overviewApi";
import { formatCount } from "../overviewView";
import { shareOf } from "./composition";

const pct = (r: number) => `${(r * 100).toFixed(r < 0.1 ? 1 : 0)}%`;

/** 缓存与推理：缓存是输入的子集、推理是输出的子集，都不加到总量。 */
export function GeekComposition({ data }: { data: AggregateData }) {
  const t = data.totals;
  const hit = shareOf(t.cacheRead, t.input);
  const rows = [
    {
      key: "cache-read",
      label: "缓存读取",
      stat: t.cacheRead,
      ratio: shareOf(t.cacheRead, t.input),
      of: "输入",
    },
    {
      key: "cache-write",
      label: "缓存写入",
      stat: t.cacheWrite,
      ratio: shareOf(t.cacheWrite, t.input),
      of: "输入",
    },
    {
      key: "reasoning",
      label: "推理",
      stat: t.reasoning,
      ratio: shareOf(t.reasoning, t.output),
      of: "输出",
    },
  ];
  const empty = rows.every((r) => r.stat.value === null);
  return (
    <div className="geek-card" aria-label="缓存与推理" role="region">
      <div className="geek-card-head">
        <h2 className="geek-card-title">缓存与推理</h2>
      </div>
      {empty ? (
        <p className="geek-empty geek-dim">暂无数据</p>
      ) : (
        <>
          <div className="geek-comp-hero">
            <span className="geek-comp-label">缓存命中率</span>
            <span className="geek-comp-number geek-num" data-testid="cache-hit">
              {hit === null ? "—" : pct(hit)}
            </span>
          </div>
          <ul className="geek-comp-list">
            {rows.map((r) => (
              <li key={r.key} data-testid={`comp-${r.key}`}>
                <div className="geek-comp-row">
                  <span>{r.label}</span>
                  <span className="geek-num">
                    {formatCount(r.stat.value)}
                    {r.ratio === null ? null : (
                      <em>
                        占{r.of} {pct(r.ratio)}
                      </em>
                    )}
                  </span>
                </div>
                <div className="geek-rank-track">
                  <span
                    style={{
                      width: `${Math.min(100, (r.ratio ?? 0) * 100)}%`,
                    }}
                  />
                </div>
              </li>
            ))}
          </ul>
        </>
      )}
    </div>
  );
}
