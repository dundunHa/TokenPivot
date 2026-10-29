import { describe, expect, it } from "vitest";
import {
  AuthApiError,
  authErrorMessage,
  parseAuthErrorCode,
  parseAuthSession,
  validateCredentials,
} from "./model";

const GOOD_PASSWORD = "correct-horse";

describe("validateCredentials email", () => {
  it("rejects only the completely empty email", () => {
    const result = validateCredentials("", GOOD_PASSWORD);
    expect(result).toEqual({ ok: false, errors: { email: "请输入邮箱" } });
  });

  it.each([
    "  Alice@Example.COM  ",
    "İstanbul@example.com",
    "i\u0307@example.com",
    "a\u0085b@example.com",
    "\uFEFFa@example.com",
    "ΑΣ@example.com",
    " ",
    "no-at-sign",
    "a@@b",
    "x".repeat(400),
  ])("passes %j through code point for code point", (email) => {
    const result = validateCredentials(email, GOOD_PASSWORD);
    expect(result.ok).toBe(true);
    if (result.ok) expect(result.credentials.email).toBe(email);
  });
});

describe("validateCredentials password", () => {
  const ok = (password: string) =>
    validateCredentials("a@example.com", password).ok;

  it("counts Unicode code points 7/8/64/65", () => {
    expect(ok("a".repeat(7))).toBe(false);
    expect(ok("a".repeat(8))).toBe(true);
    expect(ok("a".repeat(64))).toBe(true);
    expect(ok("a".repeat(65))).toBe(false);
  });

  it("does not use UTF-16 length and honours the 256 byte boundary", () => {
    const emoji64 = "😀".repeat(64);
    expect(emoji64.length).toBe(128);
    expect(new TextEncoder().encode(emoji64).length).toBe(256);
    expect(ok(emoji64)).toBe(true);
    expect(ok("😀".repeat(65))).toBe(false);
    expect(ok("😀".repeat(7))).toBe(false);
    expect(ok("😀".repeat(8))).toBe(true);
  });

  it("does not trim or normalise the password", () => {
    const padded = "  pass  ";
    const result = validateCredentials("a@example.com", padded);
    expect(result.ok).toBe(true);
    if (result.ok) expect(result.credentials.password).toBe(padded);
    expect(ok("       a")).toBe(true);
  });

  it("asks for an empty password", () => {
    expect(validateCredentials("a@example.com", "")).toEqual({
      ok: false,
      errors: { password: "请输入密码" },
    });
  });
});

describe("response narrowing", () => {
  const good = {
    user: { id: "u", email: "a@example.com" },
    expires_at: "2030-01-01T00:00:00Z",
  };

  it("accepts a valid session", () => {
    expect(parseAuthSession(good)).toEqual(good);
  });

  it.each([
    ["null", null],
    ["array", []],
    ["missing user", { expires_at: good.expires_at }],
    ["user not object", { ...good, user: "x" }],
    ["missing email", { ...good, user: { id: "u" } }],
    ["numeric id", { ...good, user: { id: 1, email: "a" } }],
    ["bad date", { ...good, expires_at: "tomorrow" }],
    ["numeric date", { ...good, expires_at: 1 }],
  ])("rejects %s", (_name, value) => {
    expect(parseAuthSession(value)).toBeUndefined();
  });

  it("reads error codes defensively", () => {
    expect(parseAuthErrorCode({ error: { code: "X" } })).toBe("X");
    expect(parseAuthErrorCode({ error: { code: 1 } })).toBeUndefined();
    expect(parseAuthErrorCode({ error: "x" })).toBeUndefined();
    expect(parseAuthErrorCode(undefined)).toBeUndefined();
  });

  it("falls back to a safe message for unknown codes and foreign errors", () => {
    const generic = "服务暂时不可用，请稍后重试。";
    expect(authErrorMessage(new AuthApiError(418, "TEAPOT"))).toBe(generic);
    expect(authErrorMessage(new Error("secret sql"))).toBe(generic);
    expect(
      authErrorMessage(new AuthApiError(429, "RATE_LIMITED", "12")),
    ).toContain("12");
  });
});
