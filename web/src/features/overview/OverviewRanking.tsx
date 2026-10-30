import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Section } from "./OverviewSection";
import type { RankingDimension } from "./overviewApi";
import type { OverviewFilters, ResolvedRange } from "./overviewFilters";
import { useRankings, type QueryScope } from "./overviewQueries";
import {
  formatCount,
  formatShare,
  glyphBar,
  MISSING,
  rankingRow,
  type RankingRow,
} from "./overviewView";

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

function Rows({
  rows,
  selected,
  onToggle,
}: {
  rows: RankingRow[];
  selected: string[];
  onToggle: (value: string) => void;
}) {
  return (
    <ul className="flex flex-col gap-1">
      {rows.map((row) => {
        const on = selected.includes(row.filterValue);
        return (
          <li key={row.id}>
            <button
              type="button"
              aria-pressed={on}
              aria-label={`${row.label}${row.sublabel ? `（${row.sublabel}）` : ""}`}
              onClick={() => onToggle(row.filterValue)}
              className="hover:bg-accent grid w-full grid-cols-[1fr_auto] gap-x-3 rounded-md px-2 py-1 text-left text-sm aria-pressed:border aria-pressed:border-primary"
            >
              <span className="truncate">
                {row.label}
                {row.sublabel ? (
                  <span className="text-subtle"> · {row.sublabel}</span>
                ) : null}
                {on ? <span className="text-primary"> [已筛选]</span> : null}
              </span>
              <span>
                {row.total === null ? MISSING : formatCount(row.total)} ·{" "}
                {formatShare(row.share)}
              </span>
              <span aria-hidden className="text-primary text-xs">
                {glyphBar(row.share) ?? ""}
              </span>
              <span className="text-muted-foreground text-xs">{row.note}</span>
            </button>
          </li>
        );
      })}
    </ul>
  );
}

interface Common {
  scope: QueryScope;
  filters: OverviewFilters;
  range: ResolvedRange;
  dimension: RankingDimension;
  projectNames: Map<string, string>;
  onToggle: (value: string) => void;
}

/** 长尾页：范围固定的游标分页，可继续展开下一页。 */
function Tail({ cursor, ...c }: Common & { cursor: string }) {
  const query = useRankings(c.scope, c.filters, c.range, c.dimension, cursor);
  const [more, setMore] = useState(false);
  return (
    <Section title="其他排名" query={query} timezone={c.filters.timezone}>
      {query.data ? (
        <>
          <Rows
            rows={query.data.groups.map((g) =>
              rankingRow(g, c.dimension, c.projectNames),
            )}
            selected={selectedList(c.filters, c.dimension)}
            onToggle={c.onToggle}
          />
          {query.data.nextCursor && !more ? (
            <Button variant="link" size="inline" onClick={() => setMore(true)}>
              继续展开
            </Button>
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
  onToggle,
}: Omit<Common, "dimension" | "onToggle"> & {
  /** 筛选标识；变化时收起旧长尾，但保留当前排名维度。 */
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
    <div className="flex flex-col gap-3">
      <div role="tablist" aria-label="排名维度" className="flex gap-2">
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
            className="aria-selected:text-primary aria-selected:border-primary border-b border-transparent px-2 py-1 text-sm"
          >
            {tab.label}
          </button>
        ))}
      </div>
      <Section title="排名" query={query} timezone={filters.timezone}>
        {data && data.groups.length === 0 && !data.other ? (
          <p className="text-muted-foreground text-sm">
            当前筛选下没有排名数据。
          </p>
        ) : null}
        {data ? (
          <>
            <Rows
              rows={data.groups.map((g) =>
                rankingRow(g, dimension, projectNames),
              )}
              selected={selectedList(filters, dimension)}
              onToggle={toggle}
            />
            {data.other && data.nextCursor ? (
              <Button
                variant="link"
                size="inline"
                aria-expanded={expanded}
                onClick={() => setExpanded((v) => !v)}
              >
                {expanded
                  ? "收起其他"
                  : `其他 ${formatShare(data.other.share)}`}
              </Button>
            ) : null}
            {expanded && data.nextCursor ? (
              <Tail
                scope={scope}
                filters={filters}
                range={range}
                dimension={dimension}
                projectNames={projectNames}
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
