import { type ReactNode } from "react";

interface GeekShellProps {
  children: ReactNode;
  workspaceName: string;
  filterSummary: string;
}

/**
 * J 极客重奏 F++ 整体壳。
 * 命令行顶栏 + 细分隔线 + 60/40 双栏布局。
 * PRD §8：不要横向滚动事件条。
 */
export function GeekShell({
  children,
  workspaceName,
  filterSummary,
}: GeekShellProps) {
  return (
    <div className="geek-shell">
      {/* 命令行顶栏 */}
      <div className="geek-topbar">
        <span className="geek-topbar-prompt">
          yg@tokenpivot<span className="geek-dim">:</span>~/{workspaceName}
          /overview
        </span>
        <span className="geek-topbar-badge geek-amber">J</span>
        <span className="geek-cursor" />
        <span className="geek-topbar-right geek-dim">ws:{workspaceName}</span>
      </div>
      <hr className="geek-divider" />

      {/* 主内容区 */}
      <div className="geek-main">{children}</div>

      {/* tmux 状态行 */}
      <GeekStatusBar workspace={workspaceName} filter={filterSummary} />
    </div>
  );
}

function GeekStatusBar({
  workspace,
  filter,
}: {
  workspace: string;
  filter: string;
}) {
  return (
    <div className="geek-statusbar">
      <div className="geek-statusbar-left">
        <span className="geek-statusbar-ws">[{workspace}] overview</span>
        <span className="geek-statusbar-sep">│</span>
        <span className="geek-statusbar-filter">filter:{filter || "*"}</span>
      </div>
      <div className="geek-statusbar-right">
        <kbd>j</kbd>
        <kbd>k</kbd> scroll
        <span className="geek-statusbar-sep">│</span>
        <kbd>1</kbd>
        <kbd>2</kbd>
        <kbd>3</kbd> window
        <span className="geek-statusbar-sep">│</span>
        <kbd>/</kbd> cmd
      </div>
    </div>
  );
}
