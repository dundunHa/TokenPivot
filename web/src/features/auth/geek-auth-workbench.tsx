import type { ReactNode } from "react";
import { AuthDashboardSnapshot } from "./auth-dashboard-snapshot";
import { AuthDualCabinCard } from "./auth-dual-cabin-card";
import "./auth.css";

/**
 * J 极客重奏（F++）登录工作台外壳。
 * 顶部 shell prompt、双栏主舞台、底部 tmux 状态栏三件套构成终端语境，
 * 语义结构仍是 header / main / footer，main 里只有一块 h1 与一个表单。
 */
export function GeekAuthWorkbench({
  mode,
  title,
  description,
  notice,
  children,
}: {
  mode: "login" | "register";
  title: string;
  description: string;
  notice?: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className="auth-workbench">
      <header className="shell-bar">
        <p className="shell-prompt">
          <span className="shell-path">tokenpivot:~/overview$</span> tp-watch
          --window=1h --granularity=1m
        </p>
        <div className="shell-meta">
          <span className="shell-badge">J · F++</span>
          <span>WORKSPACE: ws_prod_core (UTC+08:00)</span>
          <span>SOURCES: dsh-cli, pi-agent</span>
        </div>
      </header>

      <main className="main-stage">
        <div className="split-workbench">
          <AuthDashboardSnapshot />
          <div className="login-spotlight-wrap">
            <AuthDualCabinCard
              mode={mode}
              title={title}
              description={description}
              notice={notice}
            >
              {children}
            </AuthDualCabinCard>
          </div>
        </div>
      </main>

      <footer className="tmux-bar">
        <span className="tmux-session">[0:overview-preview]</span>
        <span className="tmux-session on">[1:auth-gate*]</span>
        <span className="tmux-status">
          MODE: {mode} · SESSION: anonymous · AUTH: feishu+password
        </span>
        <span className="tmux-hint">/ 邮箱 · F 飞书 · R 刷新 · Esc 失焦</span>
        <span className="tmux-version">TokenPivot v0.4</span>
      </footer>
    </div>
  );
}
