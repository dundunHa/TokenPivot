import { fmtAgo } from "./geek-utils";
import { formatInstant } from "../overviewView";
import { useSyncExternalStore } from "react";

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
  // 通过 useSyncExternalStore 获取当前时间，避免 render 中调用 impure Date.now()
  const now = useSyncExternalStore(
    (cb) => {
      const t = setInterval(cb, 5000);
      return () => clearInterval(t);
    },
    () => Date.now(),
    () => 0,
  );
  const agoText =
    lastReceivedAt && now > 0
      ? fmtAgo(now - new Date(lastReceivedAt).getTime())
      : null;
  return (
    <div className="geek-data-status-panel">
      <div className="geek-ds-row">
        <span className="geek-ds-key geek-dim">workspace / tz</span>
        <span className="geek-ds-val">
          {workspaceName} · {timezone}
        </span>
      </div>
      <div className="geek-ds-row">
        <span className="geek-ds-key geek-dim">last received</span>
        <span className="geek-ds-val">
          {agoText ? (
            <>
              <span className="geek-dot geek-ok" />
              {agoText}
            </>
          ) : (
            "—"
          )}
        </span>
      </div>
      <div className="geek-ds-row">
        <span className="geek-ds-key geek-dim">coverage</span>
        <span className="geek-ds-val">{sourceCount} sources</span>
      </div>
      <div className="geek-ds-row">
        <span className="geek-ds-key geek-dim">range</span>
        <span className="geek-ds-val geek-num">
          {formatInstant(rangeStart, timezone)} →{" "}
          {formatInstant(rangeEnd, timezone)}
        </span>
      </div>
      <div className="geek-ds-row">
        <span className="geek-ds-key geek-dim">as_of</span>
        <span className="geek-ds-val geek-num">
          {formatInstant(asOf, timezone)}
        </span>
      </div>
      <div className="geek-ds-row">
        <span className="geek-ds-key geek-dim">retention</span>
        <span className="geek-ds-val geek-num">
          {formatInstant(retentionCutoff, timezone)}
        </span>
      </div>
      <div className="geek-ds-row">
        <span className="geek-ds-key geek-dim">updated</span>
        <span className="geek-ds-val geek-num">
          {formatInstant(pageUpdatedAt, timezone)}
        </span>
      </div>
    </div>
  );
}
