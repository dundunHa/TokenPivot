import { NavLink, useLocation } from "react-router";
import { Button } from "@/components/ui/button";
import { setTheme, useTheme } from "@/lib/theme";

export function ThemeToggle() {
  const theme = useTheme();
  return (
    <div className="tp-theme-toggle" role="group" aria-label="主题切换">
      <button
        type="button"
        className="tp-theme-opt"
        aria-pressed={theme === "editorial-light"}
        onClick={() => setTheme("editorial-light")}
      >
        ☀ 暖白
      </button>
      <button
        type="button"
        className="tp-theme-opt"
        aria-pressed={theme === "graphite-dark"}
        onClick={() => setTheme("graphite-dark")}
      >
        ☾ 石墨暗
      </button>
    </div>
  );
}

function initialsOf(email: string): string {
  const local = email.split("@")[0] ?? email;
  return local.slice(0, 2).toUpperCase() || "TP";
}

export function AppTopbar({
  workspaceSlot,
  email,
  logoutPending,
  onLogout,
  showNav = false,
}: {
  workspaceSlot?: React.ReactNode;
  email?: string;
  logoutPending?: boolean;
  onLogout?: () => void;
  showNav?: boolean;
}) {
  const { search } = useLocation();
  return (
    <div className="tp-topbar-outer">
      <header className="tp-topbar">
        <h1 className="tp-brand">
          <span aria-hidden="true" className="tp-brand-logo">
            T
          </span>
          <span>TokenPivot</span>
        </h1>
        {workspaceSlot ? (
          <>
            <span className="tp-slash" aria-hidden="true">
              /
            </span>
            {workspaceSlot}
          </>
        ) : null}
        {showNav ? (
          <>
            <span className="tp-vdiv" aria-hidden="true" />
            <nav aria-label="主导航" className="tp-topnav">
              <NavLink to={{ pathname: "/", search }} end>
                总览
              </NavLink>
              <NavLink to={{ pathname: "/requests", search }}>请求明细</NavLink>
            </nav>
          </>
        ) : null}
        <div className="tp-topbar-right">
          <ThemeToggle />
          {email ? (
            <>
              <span className="tp-user-pill">
                <span className="tp-avatar" aria-hidden="true">
                  {initialsOf(email)}
                </span>
                <span className="sr-only">已登录，用户</span>
                <span className="tp-user-email break-all">{email}</span>
              </span>
              <Button
                variant="outline"
                size="sm"
                disabled={logoutPending}
                onClick={onLogout}
              >
                {logoutPending ? "正在退出…" : "登出"}
              </Button>
            </>
          ) : null}
        </div>
      </header>
    </div>
  );
}
