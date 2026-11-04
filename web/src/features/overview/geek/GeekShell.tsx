import { type ReactNode } from "react";

interface GeekShellProps {
  children: ReactNode;
  workspaceName: string;
  /** 可读的筛选摘要（命令行写法）；空串表示未筛选。 */
  filterSummary: string;
  /** 时间范围与粒度摘要，例如 `today · hour`。 */
  rangeSummary: string;
}

/**
 * 终端风格的外框：路径栏 + 内容区 + 底部状态行。
 * 键盘提示只列出真实可用的快捷键（`/` 聚焦命令条，Esc 清空）。
 */
export function GeekShell({
  children,
  workspaceName,
  filterSummary,
  rangeSummary,
}: GeekShellProps) {
  return (
    <div className="geek-shell">
      <div className="geek-topbar">
        <span className="geek-topbar-prompt">
          <span className="geek-amber">tokenpivot</span>
          <span className="geek-dim">:</span>~/{workspaceName}/overview
        </span>
        <span className="geek-cursor" aria-hidden />
      </div>

      <div className="geek-main">{children}</div>

      <div className="geek-statusbar">
        <div className="geek-statusbar-left">
          <span className="geek-statusbar-ws">[{workspaceName}]</span>
          <span className="geek-statusbar-sep">│</span>
          <span>filter: {filterSummary || "*"}</span>
          <span className="geek-statusbar-sep">│</span>
          <span>range: {rangeSummary}</span>
        </div>
        <div className="geek-statusbar-right">
          <kbd>/</kbd> 命令
          <span className="geek-statusbar-sep">│</span>
          <kbd>Esc</kbd> 清空筛选
        </div>
      </div>
    </div>
  );
}
