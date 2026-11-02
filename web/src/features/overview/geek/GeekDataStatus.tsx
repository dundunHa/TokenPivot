import { fmtAgo } from "./geek-utils";

interface GeekDataStatusProps {
  workspaceName: string;
  timezone: string;
  lastReceivedAt: string | null;
  sourceCount: number;
  rangeStart: string;
  rangeEnd: string;
  asOf: string;
  retentionCutoff: string;
  pageUpdatedAt: string;
}

/**
 * J 风格数据状态面板。
 * PRD §5.1 + §5.2(5)：时区 / 最后接收时间 / 覆盖来源。
 */
export function GeekDataStatus({
  workspaceName,
  timezone,
  lastReceivedAt,
  sourceCount,
  rangeStart,
  rangeEnd,
  asOf,
  retentionCutoff,
  pageUpdatedAt,
}: GeekDataStatusProps) {
  return (
    <div className="geek-data-status-panel">
      <div className="geek-ds-row">
        <span className="geek-ds-key geek-dim">workspace / tz</span>
        <span className="geek-ds-val">{workspaceName} · {timezone}</span>
      </div>
      <div className="geek-ds-row">
        <span className="geek-ds-key geek-dim">last received</span>
        <span className="geek-ds-val">
          {lastReceivedAt ? (
            <>
              <span className="geek-dot geek-ok" />
              {fmtAgo(Date.now() - new Date(lastReceivedAt).getTime())}
            </>
          ) : "—"}
        </span>
      </div>
      <div className="geek-ds-row">
        <span className="geek-ds-key geek-dim">coverage</span>
        <span className="geek-ds-val">{sourceCount} sources</span>
      </div>
      <div className="geek-ds-row">
        <span className="geek-ds-key geek-dim">range</span>
        <span className="geek-ds-val geek-num">{formatShort(rangeStart)} → {formatShort(rangeEnd)}</span>
      </div>
      <div className="geek-ds-row">
        <span className="geek-ds-key geek-dim">as_of</span>
        <span className="geek-ds-val geek-num">{formatShort(asOf)}</span>
      </div>
      <div className="geek-ds-row">
        <span className="geek-ds-key geek-dim">retention</span>
        <span className="geek-ds-val geek-num">{formatShort(retentionCutoff)}</span>
      </div>
      <div className="geek-ds-row">
        <span className="geek-ds-key geek-dim">updated</span>
        <span className="geek-ds-val geek-num">{formatShort(pageUpdatedAt)}</span>
      </div>
    </div>
  );
}

function formatShort(iso: string): string {
  const d = new Date(iso);
  return `${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")} ${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
}
