import { type ReactNode } from "react";
import { QUALITY_SYMBOLS, type QualityLevel } from "./geek-utils";

interface GeekRequestLogProps {
  children: ReactNode;
  /** 质量图例 */
  showLegend?: boolean;
}

/**
 * J 风格终端日志包装器。
 * 包装现有 RequestLogPanel，添加质量图例。
 * PRD §6 + 数据模型 §5：known/estimated/missing 标记。
 */
export function GeekRequestLog({ children, showLegend = true }: GeekRequestLogProps) {
  return (
    <div className="geek-request-log">
      <div className="geek-request-log-header">
        <span className="geek-amber">[08]</span> TAIL -F REQUESTS.LOG
        <span className="geek-dim">3s tick</span>
      </div>
      {showLegend && (
        <div className="geek-feed-legend">
          <span><i className="geek-q-known">{QUALITY_SYMBOLS.known}</i> known</span>
          <span><i className="geek-q-est">{QUALITY_SYMBOLS.estimated}</i> estimated</span>
          <span><i className="geek-q-miss">{QUALITY_SYMBOLS.missing}</i> missing</span>
          <span className="geek-dim" style={{ marginLeft: "auto" }}>§6 用量质量</span>
        </div>
      )}
      <div className="geek-request-log-body">
        {children}
      </div>
    </div>
  );
}

/** 质量标记单元格 */
export function QualityCell({ level }: { level: QualityLevel }) {
  const cls = level === "known" ? "geek-q-known" : level === "estimated" ? "geek-q-est" : "geek-q-miss";
  return <span className={`geek-q-cell ${cls}`} title={`用量质量：${level}`}>{QUALITY_SYMBOLS[level]}</span>;
}
