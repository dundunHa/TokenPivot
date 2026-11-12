import type { AggregateData } from "../overviewApi";
import type { OverviewFilters, ResolvedRange } from "../overviewFilters";
import { useRankings, type QueryScope } from "../overviewQueries";
import { formatCount } from "../overviewView";
import { shareOf } from "./composition";

const pct = (r: number) => `${(r * 100).toFixed(r < 0.1 ? 1 : 0)}%`;

/** 缓存与推理：缓存是输入的子集、推理是输出的子集，都不加到总量。 */
export function GeekComposition({
  data,
  scope,
  filters,
  range,
}: {
  data: AggregateData;
  scope: QueryScope;
  filters: OverviewFilters;
  range: ResolvedRange;
}) {
  const t = data.totals;
  // 缓存读取 / 写入只给总量；它们的命中率按模型分别展示，不再给总体平均值。
  const rows = [
    {
      key: "cache-read",
      label: "缓存读取",
      stat: t.cacheRead,
      ratio: null,
      of: "输入",
    },
    {
      key: "cache-write",
      label: "缓存写入",
      stat: t.cacheWrite,
      ratio: null,
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

  // 命中率不用总量平均，按模型分别给出读取 ÷ 输入、写入 ÷ 输入。
  const rankings = useRankings(scope, filters, range, "model", null);
  const models = (rankings.data?.groups ?? []).flatMap((g) =>
    g.key.model === null
      ? []
      : [
          {
            model: g.key.model,
            read: shareOf(g.stats.cacheRead, g.stats.input),
            write: shareOf(g.stats.cacheWrite, g.stats.input),
          },
        ],
  );
  const modelsEmpty = models.every((m) => m.read === null && m.write === null);

  return (
    <div className="geek-card" aria-label="缓存与推理" role="region">
      <div className="geek-card-head">
        <h2 className="geek-card-title">缓存与推理</h2>
      </div>
      {empty ? (
        <p className="geek-empty geek-dim">暂无数据</p>
      ) : (
        <>
          <div className="geek-comp-models">
            <div className="geek-comp-hero">
              <span className="geek-comp-label">缓存命中率</span>
              <span className="geek-comp-label">读取 · 写入</span>
            </div>
            {rankings.isPending ? (
              <div
                role="status"
                aria-label="按模型缓存命中率加载中"
                className="geek-skeleton geek-skeleton-sm"
              />
            ) : rankings.isError ? (
              <p role="alert" className="geek-comp-note geek-err">
                按模型命中率加载失败。
              </p>
            ) : modelsEmpty ? (
              <p className="geek-dim geek-comp-note">暂无按模型的缓存数据</p>
            ) : (
              <ul className="geek-comp-list" aria-label="按模型的缓存命中率">
                {models.map((m) => (
                  <li
                    key={m.model}
                    data-testid="cache-hit"
                    data-model={m.model}
                  >
                    <div className="geek-comp-row">
                      <span className="geek-model-name">{m.model}</span>
                      <span className="geek-num">
                        读取 <b>{m.read === null ? "—" : pct(m.read)}</b>
                        <em>写入 {m.write === null ? "—" : pct(m.write)}</em>
                      </span>
                    </div>
                    <div className="geek-rank-track">
                      <span
                        style={{
                          width: `${Math.min(100, (m.read ?? 0) * 100)}%`,
                        }}
                      />
                    </div>
                  </li>
                ))}
              </ul>
            )}
            {rankings.data?.other ? (
              <p className="geek-dim geek-comp-note">
                其余模型较多，未单独列出。
              </p>
            ) : null}
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
