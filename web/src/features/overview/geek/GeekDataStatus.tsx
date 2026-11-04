import { fmtAgo } from "./geek-utils";
import { formatInstant } from "../overviewView";
import { Fragment, useSyncExternalStore, type ReactNode } from "react";

interface GeekDataStatusProps {
  workspaceName: string;
  timezone: string;
  lastReceivedAt: string | null;
  /** 覆盖来源数；服务端尚未提供时为 null。 */
  sourceCount: number | null;
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
  const rows: [string, ReactNode][] = [
    ["workspace", workspaceName],
    ["timezone", timezone],
    [
      "last received",
      agoText ? (
        <>
          <span className="geek-dot geek-ok" />
          {agoText}
        </>
      ) : (
        "—"
      ),
    ],
    ["coverage", sourceCount === null ? "—" : `${sourceCount} sources`],
    [
      "range",
      `${formatInstant(rangeStart, timezone)} → ${formatInstant(rangeEnd, timezone)}`,
    ],
    ["as_of", formatInstant(asOf, timezone)],
    ["retention", formatInstant(retentionCutoff, timezone)],
    ["updated", formatInstant(pageUpdatedAt, timezone)],
  ];
  return (
    <section className="geek-card" aria-label="数据状态">
      <div className="geek-card-head">
        <h2 className="geek-card-title">数据状态</h2>
      </div>
      <dl className="geek-ds-list">
        {rows.map(([key, value]) => (
          <Fragment key={key}>
            <dt>{key}</dt>
            <dd className="geek-num">{value}</dd>
          </Fragment>
        ))}
      </dl>
    </section>
  );
}
