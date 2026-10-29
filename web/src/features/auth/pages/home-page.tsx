import { Navigate } from "react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { authErrorMessage } from "../model";
import { useAuthSession, useLogoutMutation } from "../queries";

export function HomePage() {
  const auth = useAuthSession();
  const logout = useLogoutMutation();

  if (auth.status === "anonymous") return <Navigate to="/login" replace />;

  return (
    <main className="mx-auto flex min-h-screen w-full max-w-md flex-col justify-center gap-6 px-6 py-10">
      <header className="flex flex-col gap-2">
        <p className="text-primary text-sm">TokenPivot</p>
        <h1 className="text-xl font-semibold">当前会话</h1>
      </header>
      <div className="border-divider border-t" />
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
          <Button variant="outline" onClick={() => void auth.retry()}>
            重试
          </Button>
        </div>
      ) : null}
      {auth.status === "ready" ? (
        <div className="flex flex-col gap-4">
          {logout.isError ? (
            <Alert variant="destructive">
              <AlertDescription>
                退出失败：{authErrorMessage(logout.error)}
              </AlertDescription>
            </Alert>
          ) : null}
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
            <dt className="text-subtle">状态</dt>
            <dd className="text-success">已登录</dd>
            <dt className="text-subtle">用户</dt>
            <dd className="break-all">{auth.session.user.email}</dd>
            <dt className="text-subtle">空间</dt>
            <dd>Personal</dd>
          </dl>
          <Button
            variant="outline"
            disabled={logout.isPending}
            onClick={() => logout.mutate()}
          >
            {logout.isPending ? "正在退出…" : "登出"}
          </Button>
        </div>
      ) : null}
    </main>
  );
}
