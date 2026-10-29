import { useRef, useState, type FormEvent } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Field,
  FieldDescription,
  FieldError,
  FieldGroup,
  FieldLabel,
} from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  authErrorMessage,
  validateCredentials,
  type AuthCredentials,
  type CredentialErrors,
} from "./model";

export function AuthForm({
  mode,
  pending,
  error,
  onSubmit,
}: {
  mode: "login" | "register";
  pending: boolean;
  error: unknown;
  onSubmit: (credentials: AuthCredentials) => Promise<void>;
}) {
  const [fieldErrors, setFieldErrors] = useState<CredentialErrors>({});
  const submitting = useRef(false);
  const emailRef = useRef<HTMLInputElement>(null);
  const passwordRef = useRef<HTMLInputElement>(null);
  const isLogin = mode === "login";

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (submitting.current) return;
    const data = new FormData(event.currentTarget);
    const email = data.get("email");
    const password = data.get("password");
    const result = validateCredentials(
      typeof email === "string" ? email : "",
      typeof password === "string" ? password : "",
    );
    if (!result.ok) {
      setFieldErrors(result.errors);
      (result.errors.email !== undefined
        ? emailRef
        : passwordRef
      ).current?.focus();
      return;
    }
    setFieldErrors({});
    submitting.current = true;
    try {
      await onSubmit(result.credentials);
    } catch {
      // 失败由 error 属性呈现，输入保留。
    } finally {
      submitting.current = false;
    }
  }

  return (
    <form
      noValidate
      aria-busy={pending}
      onSubmit={(event) => void handleSubmit(event)}
      className="flex flex-col gap-5"
    >
      {error ? (
        <Alert variant="destructive">
          <AlertDescription>{authErrorMessage(error)}</AlertDescription>
        </Alert>
      ) : null}
      <FieldGroup>
        <Field data-invalid={fieldErrors.email !== undefined}>
          <FieldLabel htmlFor={`${mode}-email`}>邮箱</FieldLabel>
          <Input
            ref={emailRef}
            id={`${mode}-email`}
            name="email"
            type="text"
            inputMode="email"
            autoComplete="email"
            aria-invalid={fieldErrors.email !== undefined}
            aria-describedby={
              fieldErrors.email !== undefined
                ? `${mode}-email-error`
                : undefined
            }
          />
          {fieldErrors.email !== undefined ? (
            <FieldError id={`${mode}-email-error`}>
              {fieldErrors.email}
            </FieldError>
          ) : null}
        </Field>
        <Field data-invalid={fieldErrors.password !== undefined}>
          <FieldLabel htmlFor={`${mode}-password`}>密码</FieldLabel>
          <Input
            ref={passwordRef}
            id={`${mode}-password`}
            name="password"
            type="password"
            autoComplete={isLogin ? "current-password" : "new-password"}
            aria-invalid={fieldErrors.password !== undefined}
            aria-describedby={[
              `${mode}-password-hint`,
              fieldErrors.password !== undefined
                ? `${mode}-password-error`
                : undefined,
            ]
              .filter(Boolean)
              .join(" ")}
          />
          <FieldDescription id={`${mode}-password-hint`}>
            8–64 个字符，不超过 256 字节，区分大小写。
          </FieldDescription>
          {fieldErrors.password !== undefined ? (
            <FieldError id={`${mode}-password-error`}>
              {fieldErrors.password}
            </FieldError>
          ) : null}
        </Field>
      </FieldGroup>
      <Button type="submit" disabled={pending}>
        {pending
          ? isLogin
            ? "正在登录…"
            : "正在注册…"
          : isLogin
            ? "登录"
            : "注册"}
      </Button>
    </form>
  );
}
