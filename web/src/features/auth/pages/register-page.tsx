import { Link, useNavigate } from "react-router";
import { Button } from "@/components/ui/button";
import { AuthForm } from "../auth-form";
import { AuthLayout } from "../auth-layout";
import { useCredentialsMutation } from "../queries";

export function RegisterPage() {
  const navigate = useNavigate();
  const register = useCredentialsMutation("register");
  return (
    <AuthLayout title="注册" description="创建账号后将直接登录。">
      <AuthForm
        mode="register"
        pending={register.isPending}
        error={register.error}
        onSubmit={async (credentials) => {
          await register.mutateAsync(credentials);
          void navigate("/", { replace: true });
        }}
      />
      <p className="text-muted-foreground text-sm">
        已有账号？{" "}
        <Button asChild variant="link" size="inline">
          <Link to="/login">登录</Link>
        </Button>
      </p>
    </AuthLayout>
  );
}
