export interface AuthCredentials {
  email: string;
  password: string;
}

export interface AuthUser {
  id: string;
  email: string;
}

export interface AuthSession {
  user: AuthUser;
  expires_at: string;
}

export interface CredentialErrors {
  email?: string;
  password?: string;
}

export type CredentialsValidation =
  | { ok: true; credentials: AuthCredentials }
  | { ok: false; errors: CredentialErrors };

const PASSWORD_MIN_CODE_POINTS = 8;
const PASSWORD_MAX_CODE_POINTS = 64;
const PASSWORD_MAX_BYTES = 256;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** 过期时间的毫秒时间戳；无效日期为 NaN。 */
export function expiresAtMs(session: AuthSession): number {
  return Date.parse(session.expires_at);
}

/** 把未知响应体收窄为 AuthSession；结构或日期不合法时返回 undefined。 */
export function parseAuthSession(value: unknown): AuthSession | undefined {
  if (!isRecord(value)) return undefined;
  const { user, expires_at } = value;
  if (!isRecord(user) || typeof expires_at !== "string") return undefined;
  if (typeof user.id !== "string" || typeof user.email !== "string") {
    return undefined;
  }
  if (Number.isNaN(Date.parse(expires_at))) return undefined;
  return { user: { id: user.id, email: user.email }, expires_at };
}

/** 从未知错误体取出字符串 error.code；形状不对返回 undefined。 */
export function parseAuthErrorCode(value: unknown): string | undefined {
  if (!isRecord(value) || !isRecord(value.error)) return undefined;
  return typeof value.error.code === "string" ? value.error.code : undefined;
}

export class AuthApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly retryAfter: string | undefined;

  constructor(status: number, code: string, retryAfter?: string) {
    super(code);
    this.name = "AuthApiError";
    this.status = status;
    this.code = code;
    this.retryAfter = retryAfter;
  }
}

/**
 * 仅改善体验的前端校验，服务端仍是唯一权威。
 * email 原样通过（不 trim、不转小写、不检查格式），只拒绝完全空串；
 * password 按 Unicode 码点（8–64）和 UTF-8 字节（≤256）检查，不 trim。
 */
export function validateCredentials(
  email: string,
  password: string,
): CredentialsValidation {
  const errors: CredentialErrors = {};
  if (email === "") errors.email = "请输入邮箱";
  if (password === "") {
    errors.password = "请输入密码";
  } else {
    const codePoints = Array.from(password).length;
    const bytes = new TextEncoder().encode(password).length;
    if (
      codePoints < PASSWORD_MIN_CODE_POINTS ||
      codePoints > PASSWORD_MAX_CODE_POINTS ||
      bytes > PASSWORD_MAX_BYTES
    ) {
      errors.password = "密码需为 8–64 个字符，且不超过 256 字节";
    }
  }
  if (errors.email !== undefined || errors.password !== undefined) {
    return { ok: false, errors };
  }
  return { ok: true, credentials: { email, password } };
}

/** 用户可见错误文案；不回显响应原文、密码或令牌。 */
export function authErrorMessage(error: unknown): string {
  if (!(error instanceof AuthApiError)) {
    return "服务暂时不可用，请稍后重试。";
  }
  switch (error.code) {
    case "INVALID_CREDENTIALS":
      return "邮箱或密码错误。";
    case "EMAIL_UNAVAILABLE":
      return "该邮箱已被使用，请改用登录。";
    case "REGISTRATION_CLOSED":
      return "当前部署未开放注册。";
    case "RATE_LIMITED":
      return error.retryAfter !== undefined && /^\d+$/.test(error.retryAfter)
        ? `尝试过于频繁，请 ${error.retryAfter} 秒后再试。`
        : "尝试过于频繁，请稍后重试。";
    case "PERMISSION_DENIED":
      return "请求被服务器拒绝，请确认从正确的站点地址访问。";
    case "INVALID_ARGUMENT":
      return "请检查邮箱和密码是否符合要求。";
    case "NETWORK":
      return "无法连接服务，请检查网络后重试。";
    default:
      return "服务暂时不可用，请稍后重试。";
  }
}
