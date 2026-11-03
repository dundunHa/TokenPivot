import { useNavigate, useSearchParams } from "react-router";
import { AuthForm } from "../auth-form";
import { AuthLayout } from "../auth-layout";
import { AuthApiError } from "../model";
import { useCredentialsMutation, useFeishuBindMutation } from "../queries";

/** 飞书回调带回的待绑定地址；参数不完整时退回普通邮箱登录。 */
function readBindEmail(params: URLSearchParams): string | undefined {
  if (params.get("feishu_bind") !== "1") return undefined;
  const email = params.get("email");
  return email === null || email === "" ? undefined : email;
}

export function LoginPage() {
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const login = useCredentialsMutation("login");
  const bind = useFeishuBindMutation();

  const bindEmail = readBindEmail(params);
  const redirectCode = params.get("error");
  // 回调失败时服务端只给查询参数，这里合成一个同形状错误交给统一文案。
  const error =
    login.error ??
    bind.error ??
    (redirectCode === null ? undefined : new AuthApiError(0, redirectCode));

  const notice =
    bindEmail === undefined ? undefined : (
      <div className="lc-banner notice" role="status">
        <p className="lc-banner-tag">LINK_ONCE</p>
        <p className="lc-banner-msg">
          {`检测到同名邮箱账号，请确认绑定 — 飞书账号 ${bindEmail} 与已有本地账号同名。请在右侧输入一次原密码完成绑定，后续即可飞书直登。`}
        </p>
      </div>
    );

  return (
    <AuthLayout
      mode="login"
      title="登录"
      description={
        bindEmail === undefined
          ? "使用邮箱密码，或左侧飞书企业账号进入 TokenPivot。"
          : "确认一次原账号密码，即可把飞书身份绑定到这个账号。"
      }
      notice={notice}
    >
      <AuthForm
        mode="login"
        pending={bindEmail === undefined ? login.isPending : bind.isPending}
        error={error}
        bindEmail={bindEmail}
        onSubmit={async (credentials) => {
          if (bindEmail === undefined) {
            await login.mutateAsync(credentials);
          } else {
            await bind.mutateAsync(credentials.password);
          }
          void navigate("/", { replace: true });
        }}
      />
    </AuthLayout>
  );
}
