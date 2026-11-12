import {
  useEffect,
  useRef,
  useState,
  type FormEvent,
  type KeyboardEvent as ReactKeyboardEvent,
} from "react";
import {
  AuthApiError,
  authErrorMessage,
  passwordMetrics,
  rateLimitSeconds,
  validateCredentials,
  type AuthCredentials,
  type CredentialErrors,
} from "./model";

const PASSWORD_MAX_CODE_POINTS = 64;
const METER_BLOCKS = 10;

/**
 * 429 的秒级退避倒计时。
 * 秒数用 useState 的初始值接住（父组件用 key 让新错误重新挂载），
 * 之后只由定时器回调递减，避免在 effect 体内同步 setState。
 */
function RateLimitCountdown({
  seconds,
  onDone,
}: {
  seconds: number;
  onDone: () => void;
}) {
  const [left, setLeft] = useState(seconds);

  useEffect(() => {
    const timer = setInterval(
      () => setLeft((value) => (value > 0 ? value - 1 : 0)),
      1000,
    );
    return () => clearInterval(timer);
  }, []);

  useEffect(() => {
    if (left === 0) onDone();
  }, [left, onDone]);

  return <p className="lc-banner-hint">退避倒计时 {left}s</p>;
}

/**
 * 右舱：邮箱与密码。
 * 视觉是终端输入符，语义仍是标准表单：label 关联、aria-invalid、
 * aria-describedby 指到说明与错误节点，屏幕阅读器与键盘路径不受样式影响。
 */
export function AuthForm({
  mode,
  pending,
  error,
  onSubmit,
  bindEmail,
}: {
  mode: "login" | "register";
  pending: boolean;
  error: unknown;
  onSubmit: (credentials: AuthCredentials) => Promise<void>;
  /** 同邮箱绑定确认态：邮箱固定为飞书回调带回的地址。 */
  bindEmail?: string;
}) {
  const [email, setEmail] = useState(bindEmail ?? "");
  const [password, setPassword] = useState("");
  const [reveal, setReveal] = useState(false);
  const [focused, setFocused] = useState<"email" | "password" | null>(null);
  const [capsLock, setCapsLock] = useState(false);
  const [fieldErrors, setFieldErrors] = useState<CredentialErrors>({});
  /** 已经走完倒计时的那个 Retry-After；与当前错误不同即仍在退避中。 */
  const [retiredLimit, setRetiredLimit] = useState<number | null>(null);
  const submitting = useRef(false);
  const emailRef = useRef<HTMLInputElement>(null);
  const passwordRef = useRef<HTMLInputElement>(null);

  const isLogin = mode === "login";
  const binding = isLogin && bindEmail !== undefined;
  const metrics = passwordMetrics(password);
  const showMeter = !isLogin || focused === "password" || password !== "";
  const filledBlocks = Math.min(
    METER_BLOCKS,
    Math.floor((metrics.codePoints / PASSWORD_MAX_CODE_POINTS) * METER_BLOCKS),
  );
  const retryLimit = rateLimitSeconds(error);
  const closedRegistration =
    error instanceof AuthApiError && error.code === "REGISTRATION_CLOSED";
  // 当前错误对应的倒计时还没走完时禁止提交；新错误会带来新的秒数。
  const rateLimited = retryLimit !== undefined && retiredLimit !== retryLimit;

  function trackCapsLock(event: ReactKeyboardEvent<HTMLInputElement>) {
    if (typeof event.getModifierState === "function") {
      setCapsLock(event.getModifierState("CapsLock"));
    }
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (submitting.current) return;
    const result = validateCredentials(email, password);
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

  const submitLabel = binding
    ? pending
      ? "正在绑定…"
      : "确认密码并绑定飞书"
    : isLogin
      ? pending
        ? "正在登录…"
        : "登录"
      : pending
        ? "正在注册…"
        : "注册";

  const passwordDescribedBy = [
    `${mode}-password-hint`,
    showMeter ? `${mode}-password-meter` : undefined,
    fieldErrors.password !== undefined ? `${mode}-password-error` : undefined,
  ]
    .filter(Boolean)
    .join(" ");

  return (
    <div className="cf">
      {error ? (
        <div className="lc-banner err" role="alert">
          <p className="lc-banner-tag">// AUTH_ERROR</p>
          <p className="lc-banner-msg">{authErrorMessage(error)}</p>
          {closedRegistration ? (
            <p className="lc-banner-hint">
              管理员已关闭邮箱公开注册，请直接使用左侧飞书企业账号扫码或一键登录。
            </p>
          ) : null}
          {retryLimit !== undefined ? (
            <RateLimitCountdown
              key={retryLimit}
              seconds={retryLimit}
              onDone={() => setRetiredLimit(retryLimit)}
            />
          ) : null}
        </div>
      ) : null}

      <form
        noValidate
        aria-busy={pending}
        onSubmit={(event) => void handleSubmit(event)}
        className="cf-form"
      >
        <div
          className="cf-field"
          data-invalid={fieldErrors.email !== undefined}
        >
          <label className="cf-label" htmlFor={`${mode}-email`}>
            邮箱
          </label>
          <div className="cf-input-row">
            <span className="cf-prefix" aria-hidden="true">
              &gt;
            </span>
            <input
              ref={emailRef}
              id={`${mode}-email`}
              name="email"
              className="cf-input"
              type="text"
              inputMode="email"
              autoComplete="email"
              value={email}
              readOnly={binding}
              onChange={(event) => setEmail(event.target.value)}
              onFocus={() => setFocused("email")}
              onBlur={() => setFocused(null)}
              aria-invalid={fieldErrors.email !== undefined}
              aria-describedby={
                fieldErrors.email !== undefined
                  ? `${mode}-email-error`
                  : undefined
              }
            />
          </div>
          {fieldErrors.email !== undefined ? (
            <p className="cf-error" id={`${mode}-email-error`}>
              {fieldErrors.email}
            </p>
          ) : null}
        </div>

        <div
          className="cf-field"
          data-invalid={fieldErrors.password !== undefined}
        >
          <label className="cf-label" htmlFor={`${mode}-password`}>
            密码
          </label>
          <div className="cf-input-row">
            <span className="cf-prefix" aria-hidden="true">
              #
            </span>
            <input
              ref={passwordRef}
              id={`${mode}-password`}
              name="password"
              className="cf-input"
              type={reveal ? "text" : "password"}
              autoComplete={isLogin ? "current-password" : "new-password"}
              value={password}
              onChange={(event) => setPassword(event.target.value)}
              onFocus={() => setFocused("password")}
              onBlur={() => {
                setFocused(null);
                setCapsLock(false);
              }}
              onKeyDown={trackCapsLock}
              onKeyUp={trackCapsLock}
              aria-invalid={fieldErrors.password !== undefined}
              aria-describedby={passwordDescribedBy}
            />
            <button
              type="button"
              className="cf-reveal"
              aria-pressed={reveal}
              onClick={() => setReveal((value) => !value)}
            >
              {reveal ? "隐藏" : "显示"}
            </button>
          </div>
          <p className="cf-hint" id={`${mode}-password-hint`}>
            8–64 个字符，不超过 256 字节，区分大小写。
          </p>
          {showMeter ? (
            <div className="pwd-progressive-hint" id={`${mode}-password-meter`}>
              <span className="pwd-blocks" aria-hidden="true">
                {Array.from({ length: METER_BLOCKS }, (_, index) => (
                  <span
                    key={index}
                    className={
                      index < filledBlocks ? "pwd-block on" : "pwd-block"
                    }
                  >
                    {index < filledBlocks ? "■" : "□"}
                  </span>
                ))}
              </span>
              <span className="pwd-metrics">
                {`${metrics.codePoints}/8–64 字符 · ${metrics.bytes}/256B`}
              </span>
            </div>
          ) : null}
          {capsLock ? <p className="cf-caps">⇪ 大写锁定</p> : null}
          {fieldErrors.password !== undefined ? (
            <p className="cf-error" id={`${mode}-password-error`}>
              {fieldErrors.password}
            </p>
          ) : null}
        </div>

        <div className="cf-actions">
          <button
            type="submit"
            className="cf-submit"
            disabled={pending || rateLimited}
          >
            {submitLabel}
          </button>
        </div>
      </form>
    </div>
  );
}
