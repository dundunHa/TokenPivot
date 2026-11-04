import type { ReactNode } from "react";
import { AuthDashboardSnapshot } from "./auth-dashboard-snapshot";
import { AuthDualCabinCard } from "./auth-dual-cabin-card";
import "./auth.css";

/** 左侧用量仪表盘、右侧登录卡的简洁工作台。 */
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
    </div>
  );
}
