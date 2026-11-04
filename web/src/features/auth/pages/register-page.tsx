import { useNavigate } from "react-router";
import { AuthForm } from "../auth-form";
import { AuthLayout } from "../auth-layout";
import { useCredentialsMutation } from "../queries";

export function RegisterPage() {
  const navigate = useNavigate();
  const register = useCredentialsMutation("register");
  return (
    <AuthLayout
      mode="register"
      title="注册"
      description="创建账号后立即登录；企业邮箱可由管理员关闭公开注册。"
    >
      <AuthForm
        mode="register"
        pending={register.isPending}
        error={register.error}
        onSubmit={async (credentials) => {
          await register.mutateAsync(credentials);
          void navigate("/", { replace: true });
        }}
      />
    </AuthLayout>
  );
}
