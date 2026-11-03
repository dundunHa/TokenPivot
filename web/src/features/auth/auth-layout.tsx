import type { ReactNode } from "react";
import { GeekAuthWorkbench } from "./geek-auth-workbench";

/**
 * 认证页外壳：保留既有导出名，内部换成 J 极客重奏双栏工作台。
 */
export function AuthLayout({
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
    <GeekAuthWorkbench
      mode={mode}
      title={title}
      description={description}
      notice={notice}
    >
      {children}
    </GeekAuthWorkbench>
  );
}
