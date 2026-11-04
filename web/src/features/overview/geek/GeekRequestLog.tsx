import { type ReactNode } from "react";
import { QUALITY_SYMBOLS, type QualityLevel } from "./geek-utils";

interface GeekRequestLogProps {
  children: ReactNode;
}

/** 请求明细的卡片外框；内容（标题、速率、表格）由 RequestLogPanel 提供。 */
export function GeekRequestLog({ children }: GeekRequestLogProps) {
  return <div className="geek-card">{children}</div>;
}

/** 质量标记单元格 */
export function QualityCell({ level }: { level: QualityLevel }) {
  const cls =
    level === "known"
      ? "geek-q-known"
      : level === "estimated"
        ? "geek-q-est"
        : "geek-q-miss";
  return (
    <span className={`geek-q-cell ${cls}`} title={`用量质量：${level}`}>
      {QUALITY_SYMBOLS[level]}
    </span>
  );
}
