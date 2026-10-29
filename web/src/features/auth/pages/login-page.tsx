import { Link, useNavigate } from "react-router";
import { Button } from "@/components/ui/button";
import { AuthForm } from "../auth-form";
import { AuthLayout } from "../auth-layout";
import { useCredentialsMutation } from "../queries";

export function LoginPage() {
  const navigate = useNavigate();
  const login = useCredentialsMutation("login");
  return (
    <AuthLayout title="登录" description="使用邮箱和密码进入 TokenPivot。">
      <AuthForm
        mode="login"
        pending={login.isPending}
        error={login.error}
        onSubmit={async (credentials) => {
          await login.mutateAsync(credentials);
          void navigate("/", { replace: true });
        }}
      />
      <p className="text-muted-foreground text-sm">
        还没有账号？{" "}
        <Button asChild variant="link" size="inline">
          <Link to="/register">注册</Link>
        </Button>
      </p>
    </AuthLayout>
  );
}
