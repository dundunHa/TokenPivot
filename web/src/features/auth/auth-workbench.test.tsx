import { describe, expect, it } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { sessionKey } from "./queries";
import { renderApp } from "@/test/render";
import {
  deferred,
  errorResponse,
  http,
  HttpResponse,
  server,
  sessionBody,
} from "@/test/server";

const AUTHORIZE = "/api/v1/auth/feishu/authorize";
const BIND = "/api/v1/auth/feishu/bind";
const LOGIN = "/api/v1/auth/login";
const REGISTER = "/api/v1/auth/register";
const FUTURE = "2999-01-01T00:00:00Z";

function authorizeEnabled(state = "abc123") {
  server.use(
    http.get(AUTHORIZE, () =>
      HttpResponse.json({
        enabled: true,
        authorize_url: `https://open.feishu.cn/open-apis/authen/v1/authorize?app_id=cli_x&redirect_uri=https%3A%2F%2Ftokenpivot.test&state=${state}`,
        expires_in_seconds: 180,
      }),
    ),
  );
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

describe("dashboard snapshot", () => {
  it("renders the hard-coded telemetry without creating a session query", async () => {
    authorizeEnabled();
    const client = renderApp("/login");

    expect(screen.getByText("67.25")).toBeTruthy();
    for (const label of ["输入 Token", "输出 Token", "请求数", "当前速率"]) {
      expect(screen.getByText(label)).toBeTruthy();
    }
    expect(screen.getByText("52.4M")).toBeTruthy();
    expect(screen.getByText("14.8M")).toBeTruthy();
    expect(screen.getByText("4,812")).toBeTruthy();
    expect(
      screen.getByRole("img", { name: "按模型 Token 速率趋势" }),
    ).toBeTruthy();
    expect(screen.getByTestId("snapshot-readout").textContent).toBe(
      "最新: 234K tok/min",
    );
    expect(document.querySelector(".shell-bar, .tmux-bar")).toBeNull();
    expect(screen.queryByText(/快捷键/)).toBeNull();
    expect(screen.queryByText("[↵]")).toBeNull();

    // 公开页不创建会话查询，也不写任何 ["auth", ...] 缓存。
    expect(
      client.getQueryCache().find({ queryKey: sessionKey }),
    ).toBeUndefined();
  });

  it("places telemetry before the login card in desktop reading order", () => {
    authorizeEnabled();
    renderApp("/login");

    const workbench = document.querySelector(".split-workbench");
    expect(
      workbench?.firstElementChild?.classList.contains("dash-snapshot"),
    ).toBe(true);
    expect(
      workbench?.lastElementChild?.classList.contains("login-spotlight-wrap"),
    ).toBe(true);
  });

  it("switches trend lines and legends between models and providers", async () => {
    authorizeEnabled();
    renderApp("/login");
    const user = userEvent.setup();

    expect(screen.getByText("Claude Sonnet 4.5")).toBeTruthy();
    expect(screen.getByText("DeepSeek v3.2")).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "按提供商" }));
    expect(screen.getByText("Anthropic")).toBeTruthy();
    expect(screen.getByText("OpenAI")).toBeTruthy();
    expect(screen.queryByText("Claude Sonnet 4.5")).toBeNull();
    expect(
      screen.getByRole("img", { name: "按提供商 Token 速率趋势" }),
    ).toBeTruthy();
    expect(
      screen
        .getByRole("button", { name: "按提供商" })
        .getAttribute("aria-pressed"),
    ).toBe("true");
  });
});

describe("feishu cabin", () => {
  it("renders an amber dot matrix with the countdown and no scanline", async () => {
    authorizeEnabled();
    renderApp("/login");

    const qr = screen.getByTestId("feishu-qr");
    expect(qr.querySelectorAll(".qr-d")).toHaveLength(225);
    expect(qr.querySelectorAll(".qr-d.on").length).toBeGreaterThan(0);
    expect(qr.querySelector(".qr-center-badge svg")).toBeTruthy();

    // 定稿要求静态点阵：整页不存在任何扫描线节点或样式钩子。
    expect(document.querySelector('[class*="scan"]')).toBeNull();
    expect(document.querySelector("[class*='scanline']")).toBeNull();

    await waitFor(() => {
      expect(screen.getByText(/^有效期 \d+s$/).textContent).toMatch(
        /有效期 (180|179)s/,
      );
    });
    expect(
      screen
        .getByRole("button", { name: "使用飞书账号继续" })
        .hasAttribute("disabled"),
    ).toBe(false);
  });

  it("ignores global launch and refresh shortcuts", async () => {
    let calls = 0;
    server.use(
      http.get(AUTHORIZE, () => {
        calls += 1;
        return HttpResponse.json({
          enabled: true,
          authorize_url:
            "https://open.feishu.cn/open-apis/authen/v1/authorize?state=abc",
          expires_in_seconds: 180,
        });
      }),
    );
    const user = userEvent.setup();
    renderApp("/login");

    await waitFor(() => {
      expect(
        screen
          .getByRole("button", { name: "使用飞书账号继续" })
          .hasAttribute("disabled"),
      ).toBe(false);
    });
    await user.keyboard("f");
    await user.keyboard("r");

    expect(calls).toBe(1);
    expect(location()).toBe("/login|POP");
  });

  it("disables the feishu entry when the deployment has no credentials", async () => {
    server.use(
      http.get(AUTHORIZE, () =>
        HttpResponse.json({
          enabled: false,
          authorize_url: null,
          expires_in_seconds: 0,
        }),
      ),
    );
    renderApp("/login");

    await screen.findByText("当前实例未配置飞书应用凭证");
    expect(
      screen
        .getByRole("button", { name: "使用飞书账号继续" })
        .hasAttribute("disabled"),
    ).toBe(true);
    expect(screen.getByText("未启用")).toBeTruthy();
  });

  it("keeps the feishu entry disabled while the code is expired and restores it after refresh", async () => {
    let calls = 0;
    server.use(
      http.get(AUTHORIZE, () => {
        calls += 1;
        return HttpResponse.json({
          enabled: true,
          authorize_url: `https://open.feishu.cn/open-apis/authen/v1/authorize?app_id=cli_x&state=s${calls}`,
          expires_in_seconds: calls === 1 ? 0 : 180,
        });
      }),
    );
    const user = userEvent.setup();
    renderApp("/login");

    await screen.findByText("二维码已过期");
    const launch = screen.getByRole("button", {
      name: "使用飞书账号继续",
    });
    expect(launch.hasAttribute("disabled")).toBe(true);

    await user.click(screen.getByRole("button", { name: "刷新二维码" }));
    await waitFor(() => expect(screen.queryByText("二维码已过期")).toBeNull());
    await waitFor(() => expect(calls).toBe(2));
    await waitFor(() => expect(launch.hasAttribute("disabled")).toBe(false));
  });

  it("keeps the launch entry disabled while a refresh is in flight", async () => {
    const pending = deferred();
    let calls = 0;
    server.use(
      http.get(AUTHORIZE, async () => {
        calls += 1;
        if (calls > 1) await pending.promise;
        return HttpResponse.json({
          enabled: true,
          authorize_url: `https://open.feishu.cn/open-apis/authen/v1/authorize?app_id=cli_x&state=s${calls}`,
          expires_in_seconds: calls === 1 ? 0 : 180,
        });
      }),
    );
    const user = userEvent.setup();
    renderApp("/login");

    await screen.findByText("二维码已过期");
    const launch = screen.getByRole("button", {
      name: "使用飞书账号继续",
    });
    expect(launch.hasAttribute("disabled")).toBe(true);

    await user.click(screen.getByRole("button", { name: "刷新二维码" }));
    await waitFor(() => expect(calls).toBe(2));

    // 新授权响应还没回来：旧地址已清空，按钮不得再跳转。
    expect(launch.hasAttribute("disabled")).toBe(true);
    expect(location()).toBe("/login|POP");

    pending.resolve();
    await waitFor(() => expect(launch.hasAttribute("disabled")).toBe(false));
    expect(screen.queryByText("二维码已过期")).toBeNull();
  });

  it("shows the expired mask and keeps the entry disabled when the endpoint fails", async () => {
    server.use(http.get(AUTHORIZE, () => errorResponse(500, "INTERNAL")));
    renderApp("/login");

    await screen.findByText("二维码已过期");
    expect(
      screen
        .getByRole("button", { name: "使用飞书账号继续" })
        .hasAttribute("disabled"),
    ).toBe(true);
    // 不暴露任何无效跳转入口：点阵仍在，但没有授权地址可去。
    expect(screen.getByTestId("feishu-qr")).toBeTruthy();
    expect(screen.getByText("有效期 0s")).toBeTruthy();
  });
});

describe("credential cabin", () => {
  it("reveals the password meter with code points and UTF-8 bytes", async () => {
    authorizeEnabled();
    renderApp("/register");
    const user = userEvent.setup();

    // 注册态默认展示标尺，让用户在建号时就看见长度口径。
    expect(document.querySelector(".pwd-progressive-hint")).toBeTruthy();
    await user.click(screen.getByLabelText("密码"));
    await user.paste("password-1");
    expect(screen.getByText("10/8–64 字符 · 10/256B")).toBeTruthy();
    expect(document.querySelectorAll(".pwd-block.on")).toHaveLength(1);
  });

  it("counts UTF-8 bytes for multi-byte passwords", async () => {
    authorizeEnabled();
    renderApp("/register");
    const user = userEvent.setup();

    await user.click(screen.getByLabelText("密码"));
    await user.paste("😀😀😀😀");
    expect(screen.getByText("4/8–64 字符 · 16/256B")).toBeTruthy();
  });

  it("guides the user to feishu when public registration is closed", async () => {
    authorizeEnabled();
    server.use(
      http.post(REGISTER, () => errorResponse(403, "REGISTRATION_CLOSED")),
    );
    const user = userEvent.setup();
    renderApp("/register");
    await fill(user, "a@example.com", "password-1");
    await user.click(screen.getByRole("button", { name: "注册" }));

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("未开放注册");
    expect(alert.textContent).toContain("飞书企业账号");
  });

  it("counts down the 429 retry window and disables submit", async () => {
    authorizeEnabled();
    server.use(
      http.post(LOGIN, () =>
        errorResponse(429, "RATE_LIMITED", { "Retry-After": "42" }),
      ),
    );
    const user = userEvent.setup();
    renderApp("/login");
    await fill(user, "a@example.com", "password-1");
    await user.click(screen.getByRole("button", { name: "登录" }));

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("42 秒");
    expect(alert.textContent).toContain("退避倒计时 42s");
    expect(
      screen.getByRole("button", { name: "登录" }).hasAttribute("disabled"),
    ).toBe(true);
  });

  it("explains callback failures carried in the query string", () => {
    authorizeEnabled();
    renderApp("/login?error=FEISHU_STATE_INVALID");
    expect(screen.getByRole("alert").textContent).toContain("飞书授权已失效");
  });
});

describe("same-email feishu binding", () => {
  it("asks for the original password once and then enters the workspace", async () => {
    authorizeEnabled();
    const bodies: unknown[] = [];
    server.use(
      http.post(BIND, async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json(sessionBody("yg@tokenpivot.dev", FUTURE));
      }),
      http.get("/api/v1/workspaces", () => HttpResponse.json([])),
    );
    const user = userEvent.setup();
    renderApp("/login?feishu_bind=1&email=yg%40tokenpivot.dev");

    const status = screen.getByRole("status");
    expect(status.textContent).toContain("LINK_ONCE");
    expect(status.textContent).toContain("yg@tokenpivot.dev");
    expect(status.textContent).toContain(
      "同名。请在右侧输入一次原密码完成绑定，后续即可飞书直登。",
    );

    const email = screen.getByLabelText("邮箱") as HTMLInputElement;
    expect(email.value).toBe("yg@tokenpivot.dev");
    expect(email.readOnly).toBe(true);

    await user.click(screen.getByLabelText("密码"));
    await user.paste("password-1");
    await user.click(
      screen.getByRole("button", { name: "确认密码并绑定飞书" }),
    );

    await waitFor(() => expect(bodies).toEqual([{ password: "password-1" }]));
    await waitFor(() => expect(location()).toBe("/|REPLACE"));
    expect(screen.queryByText(/验证|重置|忘记/)).toBeNull();
  });

  it("keeps the binding banner and shows the auth error for a wrong password", async () => {
    authorizeEnabled();
    server.use(
      http.post(BIND, () => errorResponse(401, "INVALID_CREDENTIALS")),
    );
    const user = userEvent.setup();
    renderApp("/login?feishu_bind=1&email=yg%40tokenpivot.dev");
    await user.click(screen.getByLabelText("密码"));
    await user.paste("wrong-password");
    await user.click(
      screen.getByRole("button", { name: "确认密码并绑定飞书" }),
    );

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("邮箱或密码错误");
    expect(screen.getByRole("status").textContent).toContain("LINK_ONCE");
    expect(location()).toBe("/login|POP");
  });
});
