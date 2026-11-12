import type { ReactNode } from "react";
import { Link } from "react-router";
import { FeishuCabin } from "./feishu-qr-matrix";

/**
 * 右栏方案 III：全场唯一的聚光登录主卡。
 * 左舱走飞书企业账号，右舱走邮箱密码；琥珀亮顶 + 径向背光承担视觉焦点。
 */
export function AuthDualCabinCard({
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
  const isLogin = mode === "login";

  return (
    <section className="login-card" aria-labelledby="auth-card-title">
      <div className="lc-head">
        <p className="lc-eyebrow">TOKENPIVOT</p>
        <h1 className="lc-title" id="auth-card-title">
          {title}
        </h1>
        <p className="lc-desc">{description}</p>
        <div className="lc-mode-pills" role="group" aria-label="登录与注册切换">
          {isLogin ? (
            <>
              <span className="mode-pill on" aria-current="page">
                登录
              </span>
              <Link className="mode-pill" to="/register">
                注册
              </Link>
            </>
          ) : (
            <>
              <Link className="mode-pill" to="/login">
                登录
              </Link>
              <span className="mode-pill on" aria-current="page">
                注册
              </span>
            </>
          )}
        </div>
      </div>
      {notice}
      <div className="lc-cabins">
        <FeishuCabin />
        <div className="lc-cabin lc-cabin-cred">
          <div className="lc-cabin-head">
            <h3 className="lc-cabin-title" id="cred-cabin-title">
              EMAIL &amp; PASSWORD
            </h3>
          </div>
          {children}
        </div>
      </div>
    </section>
  );
}
