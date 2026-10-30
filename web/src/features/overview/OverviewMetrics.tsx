import type { UsageStats } from "./overviewApi";
import { buildMetricCards, formatCount, MISSING } from "./overviewView";

export function OverviewMetrics({ totals }: { totals: UsageStats }) {
  return (
    <ul className="grid grid-cols-2 gap-3 lg:grid-cols-4">
      {buildMetricCards(totals).map((card) => (
        <li
          key={card.id}
          data-testid={`metric-${card.id}`}
          className="border-border bg-card flex flex-col gap-1 rounded-md border p-3"
        >
          <span className="text-subtle text-xs">{card.label}</span>
          <span
            className="text-xl font-semibold"
            title={card.value === null ? undefined : card.exact}
          >
            {card.value === null ? MISSING : formatCount(card.value)}
          </span>
          {card.note ? (
            <span className="text-muted-foreground text-xs">{card.note}</span>
          ) : null}
        </li>
      ))}
    </ul>
  );
}
