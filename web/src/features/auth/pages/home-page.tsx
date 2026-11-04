import { Navigate } from "react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { WorkspaceHome } from "@/features/workspace/WorkspaceHome";
import { authErrorMessage } from "../model";
import { useAuthSession, useLogoutMutation } from "../queries";

export function HomePage() {
  const auth = useAuthSession();
  const logout = useLogoutMutation();

  if (auth.status === "anonymous") return <Navigate to="/login" replace />;

  return (
    <main className="mx-auto flex min-h-screen w-full max-w-[1400px] flex-col gap-5 px-6 py-6">
      <header className="border-divider flex flex-wrap items-center gap-x-4 gap-y-2 border-b pb-4">
        <h1 className="text-primary flex items-center gap-2 text-base font-semibold tracking-wide">
          <span
            aria-hidden
            className="border-primary inline-flex size-5 items-center justify-center rounded-sm border text-[11px]"
          >
            T
          </span>
          TokenPivot
        </h1>
        {auth.status === "ready" ? (
          <div className="ml-auto flex items-center gap-3">
            <span className="text-muted-foreground flex items-center gap-2 text-xs">
              <span
                aria-hidden
                className="bg-success inline-block size-1.5 rounded-full"
              />
              <span className="sr-only">已登录，用户</span>
              <span className="break-all">{auth.session.user.email}</span>
            </span>
            <Button
              variant="outline"
              size="sm"
              disabled={logout.isPending}
              onClick={() => logout.mutate()}
            >
              {logout.isPending ? "正在退出…" : "登出"}
            </Button>
          </div>
        ) : null}
      </header>
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
      {auth.status === "ready" ? (
        <>
          {logout.isError ? (
            <Alert variant="destructive">
              <AlertDescription>
                退出失败：{authErrorMessage(logout.error)}
              </AlertDescription>
            </Alert>
          ) : null}
          <WorkspaceHome userId={auth.session.user.id} />
        </>
      ) : null}
    </main>
  );
}
