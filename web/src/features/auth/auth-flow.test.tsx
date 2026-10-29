import { describe, expect, it } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/test/render";
import {
  deferred,
  errorResponse,
  http,
  HttpResponse,
  server,
  sessionBody,
} from "@/test/server";

const SESSION = "/api/v1/auth/session";
const LOGIN = "/api/v1/auth/login";
const REGISTER = "/api/v1/auth/register";
const LOGOUT = "/api/v1/auth/logout";
const FUTURE = "2999-01-01T00:00:00Z";

function countSessionGets() {
  const counter = { count: 0 };
  server.use(
    http.get(SESSION, () => {
      counter.count += 1;
      return errorResponse(401, "UNAUTHENTICATED");
    }),
  );
  return counter;
}

async function fill(
  user: ReturnType<typeof userEvent.setup>,
  email: string,
  password: string,
) {
  await user.click(screen.getByLabelText("邮箱"));
  await user.paste(email);
  await user.click(screen.getByLabelText("密码"));
  await user.paste(password);
}

const location = () => screen.getByTestId("location").textContent;

describe("routes", () => {
  it("serves public pages without any session request", async () => {
    const gets = countSessionGets();
    renderApp("/login");
    expect(screen.getByRole("heading", { level: 1 }).textContent).toBe("登录");
    expect(screen.getByRole("main")).toBeTruthy();
    expect(
      screen.getByRole("link", { name: "注册" }).getAttribute("href"),
    ).toBe("/register");
    expect(gets.count).toBe(0);
    expect(location()).toBe("/login|POP");
  });

  it("does not request a session or redirect on /register", async () => {
    const gets = countSessionGets();
    renderApp("/register");
    expect(screen.getByRole("heading", { level: 1 }).textContent).toBe("注册");
    expect(
      screen.getByRole("link", { name: "登录" }).getAttribute("href"),
    ).toBe("/login");
    expect(screen.queryByText(/验证|重置|忘记/)).toBeNull();
    await new Promise((r) => setTimeout(r, 20));
    expect(gets.count).toBe(0);
    expect(location()).toBe("/register|POP");
  });

  it("sends an unknown path to / and then to login when anonymous", async () => {
    const gets = countSessionGets();
    renderApp("/nope/deeper");
    await screen.findByRole("heading", { name: "登录" });
    expect(gets.count).toBe(1);
    expect(location()).toBe("/login|REPLACE");
  });

  it("does not redirect public pages even with a live session", async () => {
    server.use(
      http.get(SESSION, () =>
        HttpResponse.json(sessionBody("a@example.com", FUTURE)),
      ),
    );
    renderApp("/login");
    await new Promise((r) => setTimeout(r, 20));
    expect(location()).toBe("/login|POP");
  });
});

describe.each([
  { kind: "login", url: LOGIN, status: 200, path: "/login", button: "登录" },
  {
    kind: "register",
    url: REGISTER,
    status: 201,
    path: "/register",
    button: "注册",
  },
])("$kind success", ({ url, status, path, button }) => {
  it.each([
    "  Alice@Example.COM  ",
    "İ@example.com",
    "i\u0307@example.com",
    "a\u0085b@example.com",
    "\uFEFFa@example.com",
    "ΑΣ@example.com",
  ])("submits %j verbatim and shows the normalised user", async (email) => {
    const password = "  Pässwörd😀!  ";
    const bodies: unknown[] = [];
    let sessionGets = 0;
    server.use(
      http.post(url, async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json(
          sessionBody("normalised@example.com", FUTURE),
          { status },
        );
      }),
      http.get(SESSION, () => {
        sessionGets += 1;
        return errorResponse(401, "UNAUTHENTICATED");
      }),
    );
    const user = userEvent.setup();
    renderApp(path);
    await fill(user, email, password);
    await user.click(screen.getByRole("button", { name: button }));
    await screen.findByText("normalised@example.com");
    expect(bodies).toEqual([{ email, password }]);
    expect(screen.getByText("Personal")).toBeTruthy();
    expect(screen.getByRole("button", { name: "登出" })).toBeTruthy();
    expect(location()).toBe("/|REPLACE");
    expect(sessionGets).toBe(0);
    expect(screen.queryByText(/验证|重置|忘记/)).toBeNull();
  });
});

describe("submit state", () => {
  it("disables the form, shows progress and sends one POST for repeated submits", async () => {
    const gate = deferred();
    let posts = 0;
    server.use(
      http.post(LOGIN, async () => {
        posts += 1;
        await gate.promise;
        return HttpResponse.json(sessionBody("a@example.com", FUTURE));
      }),
    );
    const user = userEvent.setup();
    renderApp("/login");
    await fill(user, "a@example.com", "password-1");
    const button = screen.getByRole("button", { name: "登录" });
    await user.dblClick(button);
    await user.type(screen.getByLabelText("密码"), "{Enter}{Enter}");
    const busy = await screen.findByRole("button", { name: "正在登录…" });
    expect(busy.hasAttribute("disabled")).toBe(true);
    expect(document.querySelector("form")?.getAttribute("aria-busy")).toBe(
      "true",
    );
    gate.resolve();
    await screen.findByText("a@example.com");
    expect(posts).toBe(1);
  });

  it("keeps the typed values after a failure", async () => {
    server.use(
      http.post(LOGIN, () => errorResponse(401, "INVALID_CREDENTIALS")),
    );
    const user = userEvent.setup();
    renderApp("/login");
    await fill(user, "a@example.com", "password-1");
    await user.click(screen.getByRole("button", { name: "登录" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("邮箱或密码错误");
    expect((screen.getByLabelText("邮箱") as HTMLInputElement).value).toBe(
      "a@example.com",
    );
    expect((screen.getByLabelText("密码") as HTMLInputElement).value).toBe(
      "password-1",
    );
    expect(
      screen.getByRole("button", { name: "登录" }).hasAttribute("disabled"),
    ).toBe(false);
  });
});

describe("business errors", () => {
  it.each([
    ["login", LOGIN, 401, "INVALID_CREDENTIALS", undefined, "邮箱或密码错误"],
    ["register", REGISTER, 409, "EMAIL_UNAVAILABLE", undefined, "已被使用"],
    ["register", REGISTER, 403, "REGISTRATION_CLOSED", undefined, "未开放注册"],
    ["login", LOGIN, 429, "RATE_LIMITED", { "Retry-After": "42" }, "42 秒"],
    ["login", LOGIN, 403, "PERMISSION_DENIED", undefined, "被服务器拒绝"],
    ["login", LOGIN, 400, "INVALID_ARGUMENT", undefined, "请检查邮箱和密码"],
    ["login", LOGIN, 500, "INTERNAL", undefined, "暂时不可用"],
    ["login", LOGIN, 503, "UNAVAILABLE", undefined, "暂时不可用"],
    ["login", LOGIN, 418, "SOMETHING_NEW", undefined, "暂时不可用"],
  ] as const)(
    "%s %i %s shows its own message",
    async (kind, url, status, code, headers, text) => {
      server.use(http.post(url, () => errorResponse(status, code, headers)));
      const user = userEvent.setup();
      renderApp(`/${kind}`);
      await fill(user, "a@example.com", "password-1");
      await user.click(
        screen.getByRole("button", {
          name: kind === "login" ? "登录" : "注册",
        }),
      );
      const alert = await screen.findByRole("alert");
      expect(alert.textContent).toContain(text);
      expect(alert.textContent).not.toContain("password-1");
      expect(alert.textContent).not.toContain("ignored");
      expect(screen.getByRole("heading", { level: 1 })).toBeTruthy();
    },
  );

  it("shows a connectivity message for network failure", async () => {
    server.use(http.post(LOGIN, () => HttpResponse.error()));
    const user = userEvent.setup();
    renderApp("/login");
    await fill(user, "a@example.com", "password-1");
    await user.click(screen.getByRole("button", { name: "登录" }));
    expect((await screen.findByRole("alert")).textContent).toContain(
      "无法连接服务",
    );
  });
});

describe("response boundaries", () => {
  it.each([
    ["broken JSON", () => new HttpResponse("{oops", { status: 200 })],
    ["missing user", () => HttpResponse.json({ expires_at: FUTURE })],
    [
      "bad expires_at",
      () => HttpResponse.json(sessionBody("a@example.com", "tomorrow")),
    ],
    [
      "non-JSON 500",
      () => new HttpResponse("<html>boom</html>", { status: 500 }),
    ],
  ])(
    "treats %s as a service error and stays on the form",
    async (_name, respond) => {
      server.use(http.post(LOGIN, respond));
      const user = userEvent.setup();
      renderApp("/login");
      await fill(user, "a@example.com", "password-1");
      await user.click(screen.getByRole("button", { name: "登录" }));
      const alert = await screen.findByRole("alert");
      expect(alert.textContent).toContain("暂时不可用");
      expect(alert.textContent).not.toContain("boom");
      expect(location()).toBe("/login|POP");
    },
  );
});

describe("client validation and accessibility", () => {
  it("rejects an empty email, focuses it and links the error", async () => {
    let posts = 0;
    server.use(
      http.post(LOGIN, () => ((posts += 1), errorResponse(500, "INTERNAL"))),
    );
    const user = userEvent.setup();
    renderApp("/login");
    await user.click(screen.getByRole("button", { name: "登录" }));
    const email = screen.getByLabelText("邮箱");
    expect(document.activeElement).toBe(email);
    expect(email.getAttribute("aria-invalid")).toBe("true");
    const describedBy = email.getAttribute("aria-describedby") ?? "";
    expect(document.getElementById(describedBy)?.textContent).toBe(
      "请输入邮箱",
    );
    expect(posts).toBe(0);
  });

  it("focuses the password when only the password is invalid and submits by keyboard", async () => {
    let posts = 0;
    server.use(
      http.post(LOGIN, () => ((posts += 1), errorResponse(500, "INTERNAL"))),
    );
    const user = userEvent.setup();
    renderApp("/login");
    await user.click(screen.getByLabelText("邮箱"));
    await user.paste("a@example.com");
    await user.keyboard("{Tab}short{Enter}");
    const password = screen.getByLabelText("密码");
    expect(document.activeElement).toBe(password);
    expect(password.getAttribute("aria-invalid")).toBe("true");
    const ids = (password.getAttribute("aria-describedby") ?? "").split(" ");
    expect(
      ids.map((id) => document.getElementById(id)?.textContent).join(" "),
    ).toContain("8–64");
    expect(screen.getByLabelText("邮箱").getAttribute("aria-invalid")).toBe(
      "false",
    );
    expect(posts).toBe(0);
  });

  it("has labelled inputs with the right autocomplete and visible focus styling", () => {
    renderApp("/register");
    const email = screen.getByLabelText("邮箱") as HTMLInputElement;
    const password = screen.getByLabelText("密码") as HTMLInputElement;
    expect(email.type).toBe("text");
    expect(email.getAttribute("inputmode")).toBe("email");
    expect(email.autocomplete).toBe("email");
    expect(password.autocomplete).toBe("new-password");
    expect(document.querySelector("form")?.noValidate).toBe(true);
    expect(email.getAttribute("placeholder")).toBeNull();
  });
});

describe("logout", () => {
  it("goes to the login page after a real 204 and keeps no browser storage", async () => {
    server.use(
      http.get(SESSION, () =>
        HttpResponse.json(sessionBody("a@example.com", FUTURE)),
      ),
      http.post(LOGOUT, () => new HttpResponse(null, { status: 204 })),
    );
    const user = userEvent.setup();
    renderApp("/");
    await screen.findByText("a@example.com");
    await user.click(screen.getByRole("button", { name: "登出" }));
    await screen.findByRole("heading", { name: "登录" });
    expect(localStorage.length).toBe(0);
    expect(sessionStorage.length).toBe(0);
  });

  it("keeps the current user and shows an error when logout fails", async () => {
    server.use(
      http.get(SESSION, () =>
        HttpResponse.json(sessionBody("a@example.com", FUTURE)),
      ),
      http.post(LOGOUT, () => errorResponse(500, "INTERNAL")),
    );
    const user = userEvent.setup();
    renderApp("/");
    await screen.findByText("a@example.com");
    await user.click(screen.getByRole("button", { name: "登出" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("退出失败");
    expect(screen.getByText("a@example.com")).toBeTruthy();
    await waitFor(() => expect(location()).toBe("/|POP"));
  });
});
