import { useState } from "react";
import { Section } from "./OverviewSection";
import type { RankingDimension, RankingGroup } from "./overviewApi";
import type { OverviewFilters, ResolvedRange } from "./overviewFilters";
import { useRankings, type QueryScope } from "./overviewQueries";
import { formatCount, formatShare, MISSING, rankingRow } from "./overviewView";
import { SERIES_PALETTE } from "./geek/chartScale";
import { shareOf } from "./geek/composition";
import { fmt } from "./geek/geek-utils";

const TABS: { id: RankingDimension; label: string }[] = [
  { id: "project", label: "项目" },
  { id: "model", label: "模型" },
  { id: "provider", label: "供应商" },
];

function selectedList(filters: OverviewFilters, dim: RankingDimension) {
  return dim === "project"
    ? filters.projects
    : dim === "provider"
      ? filters.providers
      : filters.models;
}

function MatrixTable({
  groups,
  dimension,
  projectNames,
  selected,
  modelSpeeds,
  onToggle,
}: {
  groups: RankingGroup[];
  dimension: RankingDimension;
  projectNames: Map<string, string>;
  selected: string[];
  modelSpeeds?: Map<string, string>;
  onToggle: (value: string) => void;
}) {
  const showSpeed = dimension === "model" && modelSpeeds !== undefined;
  return (
    <div className="tp-matrix-wrap">
      <table className="tp-matrix">
        <thead>
          <tr>
            <th scope="col">维度项</th>
            <th scope="col" className="geek-th-num">
              总 Token
            </th>
            <th scope="col" className="geek-th-num">
              占比
            </th>
            <th scope="col" className="geek-th-num">
              请求数
            </th>
            <th scope="col" className="geek-th-num">
              均值 / 请求
            </th>
            <th scope="col" className="geek-th-num">
              缓存命中率
            </th>
            {showSpeed ? (
              <th scope="col" className="geek-th-num">
                近 60s 速度
              </th>
            ) : null}
          </tr>
        </thead>
        <tbody>
          {groups.map((group, idx) => {
            const row = rankingRow(group, dimension, projectNames);
            const on = selected.includes(row.filterValue);
            const reqs = Number(group.stats.requestCount);
            const totalNum =
              group.stats.total.value === null
                ? null
                : Number(group.stats.total.value);
            const avg = totalNum !== null && reqs > 0 ? totalNum / reqs : null;
            const cacheHit = shareOf(group.stats.cacheRead, group.stats.input);
            const color =
              SERIES_PALETTE[idx % SERIES_PALETTE.length] ?? "var(--c-s1)";
            const speed =
              dimension === "model" && group.key.model
                ? modelSpeeds?.get(group.key.model)
                : undefined;

            return (
              <tr
                key={`${row.id}-${idx}`}
                data-selected={on ? "true" : undefined}
                data-testid="ranking-row"
              >
                <td>
                  <button
                    type="button"
                    aria-pressed={on}
                    aria-label={`${row.label}${row.sublabel ? `（${row.sublabel}）` : ""}`}
                    className="tp-row-trigger"
                    onClick={() => onToggle(row.filterValue)}
                  >
                    <span className="tp-row-trigger-top">
                      <span className="tp-row-name">{row.label}</span>
                      {row.sublabel ? (
                        <span className="tp-prov-tag">{row.sublabel}</span>
                      ) : null}
                      {on ? (
                        <span className="geek-amber text-[11px]">[已筛选]</span>
                      ) : null}
                    </span>
                    <span aria-hidden="true" className="tp-mini-bar">
                      <span
                        style={{
                          width: `${Math.round((row.share ?? 0) * 100)}%`,
                          background: color,
                        }}
                      />
                    </span>
                  </button>
                </td>
                <td className="geek-td-num font-semibold">
                  {row.total === null ? MISSING : formatCount(row.total)}
                </td>
                <td className="geek-td-num geek-muted">
                  {formatShare(row.share)}
                </td>
                <td className="geek-td-num">
                  {formatCount(group.stats.requestCount)}
                </td>
                <td className="geek-td-num geek-muted">
                  {avg === null ? MISSING : fmt(avg)}
                </td>
                <td className="geek-td-num" data-testid="ranking-cache-hit">
                  {cacheHit === null ? (
                    <span className="geek-faint">{MISSING}</span>
                  ) : (
                    <span className="tp-hit-badge">
                      {(cacheHit * 100).toFixed(cacheHit < 0.1 ? 1 : 0)}%
                    </span>
                  )}
                </td>
                {showSpeed ? (
                  <td className="geek-td-num" data-testid="ranking-speed">
                    {speed ? (
                      <>
                        {speed}{" "}
                        <span className="geek-faint text-[10.5px]">tok/s</span>
                      </>
                    ) : (
                      <span className="geek-faint">{MISSING}</span>
                    )}
                  </td>
                ) : null}
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

interface Common {
  scope: QueryScope;
  filters: OverviewFilters;
  range: ResolvedRange;
  dimension: RankingDimension;
  projectNames: Map<string, string>;
  modelSpeeds?: Map<string, string>;
  onToggle: (value: string) => void;
}

function Tail({ cursor, ...c }: Common & { cursor: string }) {
  const query = useRankings(c.scope, c.filters, c.range, c.dimension, cursor);
  const [more, setMore] = useState(false);
  return (
    <Section title="其他排名" query={query} timezone={c.filters.timezone}>
      {query.data ? (
        <>
          <MatrixTable
            groups={query.data.groups}
            dimension={c.dimension}
            projectNames={c.projectNames}
            selected={selectedList(c.filters, c.dimension)}
            modelSpeeds={c.modelSpeeds}
            onToggle={c.onToggle}
          />
          {query.data.nextCursor && !more ? (
            <button
              type="button"
              className="geek-link"
              onClick={() => setMore(true)}
            >
              继续展开
            </button>
          ) : null}
          {query.data.nextCursor && more ? (
            <Tail {...c} cursor={query.data.nextCursor} />
          ) : null}
        </>
      ) : null}
    </Section>
  );
}

export function OverviewRanking({
  identity,
  scope,
  filters,
  range,
  projectNames,
  modelSpeeds,
  onToggle,
}: Omit<Common, "dimension" | "onToggle"> & {
  identity: string;
  onToggle: (dimension: RankingDimension, value: string) => void;
}) {
  const [dimension, setDimension] = useState<RankingDimension>("project");
  const [expanded, setExpanded] = useState(false);
  const [seenIdentity, setSeenIdentity] = useState(identity);
  if (seenIdentity !== identity) {
    setSeenIdentity(identity);
    setExpanded(false);
  }
  const query = useRankings(scope, filters, range, dimension, null);
  const data = query.data;
  const toggle = (value: string) => onToggle(dimension, value);
  return (
    <div className="geek-card">
      <div className="geek-card-head">
        <h2 className="geek-card-title">多维消耗矩阵</h2>
        <div role="tablist" aria-label="排名维度" className="geek-segmented">
          {TABS.map((tab) => (
            <button
              key={tab.id}
              type="button"
              role="tab"
              aria-selected={dimension === tab.id}
              onClick={() => {
                setDimension(tab.id);
                setExpanded(false);
              }}
              className="geek-ranking-tab"
            >
              {tab.label}
            </button>
          ))}
        </div>
      </div>
      <Section title="排名" query={query} timezone={filters.timezone}>
        {data && data.groups.length === 0 && !data.other ? (
          <p className="geek-dim geek-empty">暂无数据</p>
        ) : null}
        {data ? (
          <>
            <MatrixTable
              groups={data.groups}
              dimension={dimension}
              projectNames={projectNames}
              selected={selectedList(filters, dimension)}
              modelSpeeds={modelSpeeds}
              onToggle={toggle}
            />
            {data.other && data.nextCursor ? (
              <button
                type="button"
                className="geek-link"
                aria-expanded={expanded}
                onClick={() => setExpanded((v) => !v)}
              >
                {expanded
                  ? "收起其他"
                  : `其他 ${formatShare(data.other.share)}`}
              </button>
            ) : null}
            {expanded && data.nextCursor ? (
              <Tail
                scope={scope}
                filters={filters}
                range={range}
                dimension={dimension}
                projectNames={projectNames}
                modelSpeeds={modelSpeeds}
                onToggle={toggle}
                cursor={data.nextCursor}
              />
            ) : null}
          </>
        ) : null}
      </Section>
    </div>
  );
}
