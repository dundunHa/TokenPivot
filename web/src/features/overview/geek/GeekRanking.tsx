import { fmt } from "./geek-utils";

export interface RankingItem {
  name: string;
  value: number;
  /** 可点击筛选 */
  clickable?: boolean;
  /** 筛选键 */
  filterKey?: string;
  /** 筛选值 */
  filterVal?: string;
}

interface GeekRankingProps {
  items: RankingItem[];
  activeTab: "project" | "model" | "provider";
  onTabChange: (tab: "project" | "model" | "provider") => void;
  onToggle: (filterKey: string, filterVal: string) => void;
  activeFilter?: { key: string; val: string } | null;
}

/**
 * J 风格字符条排名。
 * PRD §5.2(3)：长尾归「其他」。
 * PRD §5.2：点击排名项直接筛选，再点取消。
 */
export function GeekRanking({
  items,
  activeTab,
  onTabChange,
  onToggle,
  activeFilter,
}: GeekRankingProps) {
  const maxV = Math.max(...items.map((i) => i.value), 1);

  return (
    <div className="geek-ranking">
      <div className="geek-ranking-tabs">
        {(["project", "model", "provider"] as const).map((tab) => (
          <button
            key={tab}
            className={`geek-ranking-tab ${activeTab === tab ? "geek-tab-active" : ""}`}
            onClick={() => onTabChange(tab)}
          >
            {tab}
          </button>
        ))}
      </div>
      <div className="geek-ranking-list">
        {items.map((item, i) => {
          const BAR_W = 28;
          const filled = Math.round((item.value / maxV) * BAR_W);
          const bar = "█".repeat(filled) + "░".repeat(BAR_W - filled);
          const isActive =
            activeFilter &&
            activeFilter.key === item.filterKey &&
            activeFilter.val === item.filterVal?.toLowerCase();

          return (
            <div
              key={`${item.name}-${i}`}
              className={`geek-rank-item ${item.clickable ? "geek-rank-clickable" : "geek-rank-dim"} ${isActive ? "geek-rank-active" : ""}`}
              onClick={() => {
                if (item.clickable && item.filterKey && item.filterVal) {
                  onToggle(item.filterKey, item.filterVal.toLowerCase());
                }
              }}
              role={item.clickable ? "button" : undefined}
              tabIndex={item.clickable ? 0 : undefined}
              onKeyDown={(e) => {
                if (
                  e.key === "Enter" &&
                  item.clickable &&
                  item.filterKey &&
                  item.filterVal
                ) {
                  onToggle(item.filterKey, item.filterVal.toLowerCase());
                }
              }}
            >
              <span className="geek-rank-name">{item.name}</span>
              <span className="geek-rank-bar geek-amber">{bar}</span>
              <span className="geek-rank-value geek-num">
                {fmt(item.value)}
              </span>
            </div>
          );
        })}
      </div>
    </div>
  );
}
