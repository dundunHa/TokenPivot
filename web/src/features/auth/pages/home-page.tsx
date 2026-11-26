import { Navigate } from "react-router";
import { AppTopbar } from "@/components/AppTopbar";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { WorkspaceHome } from "@/features/workspace/WorkspaceHome";
import { authErrorMessage } from "../model";
import { useAuthSession, useLogoutMutation } from "../queries";

export function HomePage({
  view = "overview",
}: {
  view?: "overview" | "requests";
}) {
  const auth = useAuthSession();
  const logout = useLogoutMutation();

  if (auth.status === "anonymous") return <Navigate to="/login" replace />;

  if (auth.status === "ready") {
    return (
      <WorkspaceHome
        userId={auth.session.user.id}
        view={view}
        header={{
          email: auth.session.user.email,
          logoutPending: logout.isPending,
          logoutError: logout.isError
            ? `退出失败：${authErrorMessage(logout.error)}`
            : null,
          onLogout: () => logout.mutate(),
        }}
      />
    );
  }

  return (
    <>
      <AppTopbar />
      <main className="tp-page-shell">
        {auth.status === "loading" || auth.status === "checking" ? (
          <p role="status" className="text-muted-foreground text-sm">
            正在确认登录状态…
          </p>
        ) : null}
        {auth.status === "error" ? (
          <div className="flex flex-col gap-4">
            <Alert variant="destructive">
              <AlertDescription>
                无法确认登录状态：{authErrorMessage(auth.error)}
              </AlertDescription>
            </Alert>
            <Button
              variant="outline"
              className="self-start"
              onClick={() => void auth.retry()}
            >
              重试
            </Button>
          </div>
        ) : null}
      </main>
    </>
  );
}
