import { describe, expect, it, vi } from "vitest";
import { type ReactNode } from "react";
import { QueryClientProvider, type QueryClient } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation, useNavigate } from "react-router";
import {
  createAppQueryClient,
  sessionKey,
  useCredentialsMutation,
  useLogoutMutation,
} from "@/features/auth/queries";
import type { AuthSession } from "@/features/auth/model";
import {
  deferred,
  errorResponse,
  http,
  HttpResponse,
  server,
} from "@/test/server";
import { trimWorkspaceName, workspaceNameProblem, type Workspace } from "./api";
import { workspaceListQuery } from "./queries";
import { WorkspaceHome } from "./WorkspaceHome";

const WS_A = "0198f2b0-0000-7000-8000-0000000000aa";
const WS_B = "0198f2b0-0000-7000-8000-0000000000bb";
const WS_C = "0198f2b0-0000-7000-8000-0000000000cc";
const WS_D = "0198f2b0-0000-7000-8000-0000000000dd";
const USER = "user-a";
const SESSION: AuthSession = {
  user: { id: USER, email: "a@example.com" },
  expires_at: "2999-01-01T00:00:00Z",
};
const SESSION_B: AuthSession = {
  user: { id: "user-b", email: "b@example.com" },
  expires_at: "2999-01-01T00:00:00Z",
};

const workspace = (workspaceId: string, name: string): Workspace => ({
  workspaceId,
  name,
});

const body = (w: Workspace) => ({ workspace_id: w.workspaceId, name: w.name });

const metric = (value: string) => ({
  value,
  known_count: "1",
  estimated_count: "0",
  missing_count: "0",
});
const stats = {
  request_count: "1",
  input_tokens: metric("1"),
  output_tokens: metric("1"),
  total_tokens: metric("2"),
};

function LocationProbe() {
  const location = useLocation();
  return (
    <p data-testid="location">
      {location.pathname}
      {location.search}
    </p>
  );
}

/** 模拟浏览器前进/后退或其他入口改 URL。 */
function NavigateTo({
  to,
  label = "外部导航",
}: {
  to: string;
  label?: string;
}) {
  const navigate = useNavigate();
  return (
    <button type="button" onClick={() => navigate(to)}>
      {label}
    </button>
  );
}

/** 模拟浏览器后退到历史里的上一个位置。 */
function NavigateBack({ label = "浏览器后退" }: { label?: string }) {
  const navigate = useNavigate();
  return (
    <button type="button" onClick={() => navigate(-1)}>
      {label}
    </button>
  );
}

/** 真实认证 mutation：退出后换另一个账号登录。 */
function SessionActions() {
  const logout = useLogoutMutation();
  const login = useCredentialsMutation("login");
  return (
    <>
      <button type="button" onClick={() => logout.mutate()}>
        退出
      </button>
      <button
        type="button"
        onClick={() =>
          login.mutate({
            email: "b@example.com",
            password: "synthetic-password",
          })
        }
      >
        登录 B
      </button>
    </>
  );
}

const aggregateBody = (total: string) => ({
  range: {
    start: "2026-10-28T00:00:00Z",
    end: "2026-10-28T01:00:00Z",
    as_of: "2026-10-28T01:00:00Z",
    retention_cutoff: "2025-10-28T00:00:00Z",
    timezone: "UTC",
    granularity: "hour",
  },
  totals: { ...stats, total_tokens: metric(total) },
  points: [],
});

/** 每个空间返回可区分的总量，用来判断当前可见数据属于哪个空间。 */
function aggregateByWorkspace(totals: Record<string, string>) {
  return http.get("/api/v1/workspaces/:id/usage/aggregate", ({ params }) =>
    HttpResponse.json(aggregateBody(totals[String(params.id)] ?? "0")),
  );
}

/**
 * 让“取消旧空间业务请求”真实发出后再停住：每次 predicate 取消各占一个 gate，
 * 测试可逐个放行，观察等待窗口里迟到回调的行为。
 */
function holdBusinessCancels(client: QueryClient) {
  const gates: Array<() => void> = [];
  const cancelQueries = client.cancelQueries.bind(client);
  vi.spyOn(client, "cancelQueries").mockImplementation((filters, options) => {
    const done = cancelQueries(filters, options);
    if (filters === undefined || !("predicate" in filters)) return done;
    const gate = deferred();
    gates.push(() => gate.resolve());
    return done.then(() => gate.promise);
  });
  return gates;
}

function homeTree(
  client: QueryClient,
  path: string,
  home: ReactNode,
  extra?: ReactNode,
) {
  return (
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <LocationProbe />
        {extra}
        {home}
      </MemoryRouter>
    </QueryClientProvider>
  );
}

function renderHome(client: QueryClient, path: string, extra?: ReactNode) {
  client.setQueryData(sessionKey, SESSION);
  render(homeTree(client, path, <WorkspaceHome userId={USER} />, extra));
  return client;
}

function search() {
  const text = screen.getByTestId("location").textContent ?? "";
  return new URLSearchParams(text.includes("?") ? text.split("?")[1] : "");
}

interface Backend {
  state: { workspaces: Workspace[] };
  calls: {
    detail: string[];
    options: string[];
    aggregate: string[];
    business: number;
    posts: unknown[];
  };
}

function useBackend(initial: Workspace[]): Backend {
  const state = { workspaces: [...initial] };
  const calls = {
    detail: [] as string[],
    options: [] as string[],
    aggregate: [] as string[],
    business: 0,
    posts: [] as unknown[],
  };
  const base = "/api/v1/workspaces";
  server.use(
    http.get(base, () => HttpResponse.json(state.workspaces.map(body))),
    http.get(`${base}/:id`, ({ params }) => {
      const id = String(params.id);
      calls.business += 1;
      calls.detail.push(id);
      const found = state.workspaces.find((w) => w.workspaceId === id);
      return found
        ? HttpResponse.json(body(found))
        : errorResponse(403, "PERMISSION_DENIED");
    }),
    http.get(`${base}/:id/usage/options`, ({ params }) => {
      calls.business += 1;
      calls.options.push(String(params.id));
      return HttpResponse.json({ providers: ["p1"], models: [] });
    }),
    http.get(`${base}/:id/projects`, () => {
      calls.business += 1;
      return HttpResponse.json([]);
    }),
    http.get(`${base}/:id/usage/aggregate`, ({ params }) => {
      calls.business += 1;
      calls.aggregate.push(String(params.id));
      return HttpResponse.json({
        range: {
          start: "2026-10-28T00:00:00Z",
          end: "2026-10-28T01:00:00Z",
          as_of: "2026-10-28T01:00:00Z",
          retention_cutoff: "2025-10-28T00:00:00Z",
          timezone: "UTC",
          granularity: "hour",
        },
        totals: stats,
        points: [],
      });
    }),
    http.get(`${base}/:id/usage/rankings`, ({ request }) =>
      HttpResponse.json({
        group_by: new URL(request.url).searchParams.get("group_by"),
        overall: stats,
        groups: [],
        other: null,
        next_cursor: null,
      }),
    ),
    http.get(`${base}/:id/usage-events`, () =>
      HttpResponse.json({
        items: [],
        next_cursor: null,
        has_more: false,
        start: "2026-10-28T01:00:00Z",
        end: "2026-10-28T02:00:00Z",
        as_of: "2026-10-28T02:00:00Z",
        retention_cutoff: "2026-04-01T00:00:00Z",
      }),
    ),
    http.get(`${base}/:id/usage-rate`, () =>
      HttpResponse.json({
        start: "2026-10-28T01:59:00Z",
        end: "2026-10-28T02:00:00Z",
        as_of: "2026-10-28T02:00:00Z",
        window_seconds: 60,
        unit: "tokens/min",
        total_tokens: {
          value: null,
          event_count: 0,
          known_count: 0,
          estimated_count: 0,
          missing_count: 0,
        },
        window_last_received_at: null,
      }),
    ),
  );
  return { state, calls };
}

describe("workspace name rules", () => {
  it("trims the same Unicode whitespace as Go strings.TrimSpace", () => {
    expect(trimWorkspaceName("  Alpha  ")).toBe("Alpha");
    expect(trimWorkspaceName("\t\nAlpha\r\v\f")).toBe("Alpha");
    expect(trimWorkspaceName("\u0085\u00A0Alpha\u1680\u3000")).toBe("Alpha");
    expect(trimWorkspaceName("\u2000Alpha\u202F")).toBe("Alpha");
    // U+180E 在 Go 的 IsSpace 中不是空白，不应被修剪。
    expect(trimWorkspaceName("\u180EAlpha\u180E")).toBe("\u180EAlpha\u180E");
  });

  it("accepts 80 code points and 256 UTF-8 bytes, and rejects one more", () => {
    expect(workspaceNameProblem("a".repeat(80))).toBeNull();
    expect(workspaceNameProblem("a".repeat(81))).not.toBeNull();
    expect(workspaceNameProblem("😀".repeat(64))).toBeNull();
    expect(workspaceNameProblem("😀".repeat(65))).not.toBeNull();
  });

  it("rejects empty, control characters and a middle tab", () => {
    expect(workspaceNameProblem("   ")).not.toBeNull();
    expect(workspaceNameProblem("a\u0000b")).not.toBeNull();
    expect(workspaceNameProblem("a\tb")).not.toBeNull();
    expect(workspaceNameProblem("a b")).toBeNull();
  });
});

describe("workspace home", () => {
  it("waits for the list before mounting any business query", async () => {
    const gate = deferred<Response>();
    const client = createAppQueryClient();
    server.use(http.get("/api/v1/workspaces", () => gate.promise));
    renderHome(client, "/?tz=UTC");
    expect(screen.getByRole("status").textContent).toContain("正在加载工作区");
    expect(
      client.getQueryCache().findAll({ queryKey: ["overview"] }),
    ).toHaveLength(0);
    await waitFor(() => expect(screen.getByRole("status")).toBeTruthy());
    gate.resolve(HttpResponse.json([]));
    await screen.findByRole("dialog");
  });

  it("shows a retryable list error instead of treating it as an empty workspace", async () => {
    useBackend([workspace(WS_A, "Alpha")]);
    let attempts = 0;
    server.use(
      http.get("/api/v1/workspaces", () => {
        attempts += 1;
        return attempts === 1
          ? errorResponse(500, "INTERNAL")
          : HttpResponse.json([body(workspace(WS_A, "Alpha"))]);
      }),
    );
    const client = createAppQueryClient();
    renderHome(client, "/?tz=UTC");
    await screen.findByText("工作区列表加载失败，请稍后重试。");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(
      client.getQueryCache().findAll({ queryKey: ["overview"] }),
    ).toHaveLength(0);

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "重试" }));
    await screen.findByTestId("metric-total");
    await waitFor(() => expect(search().get("workspace")).toBe(WS_A));
  });

  it("falls back to the server's first workspace and preserves filters in the URL", async () => {
    useBackend([workspace(WS_A, "Alpha"), workspace(WS_B, "Beta")]);
    const client = createAppQueryClient();
    renderHome(client, "/?tz=UTC&provider=anthropic");
    await screen.findByTestId("metric-total");
    await waitFor(() => expect(search().get("workspace")).toBe(WS_A));
    expect(search().get("tz")).toBe("UTC");
    expect(search().getAll("provider")).toEqual(["anthropic"]);
    const aggregate = client
      .getQueryCache()
      .findAll({ queryKey: ["overview", "aggregate"] })
      .at(0);
    expect(aggregate?.queryKey.slice(0, 5)).toEqual([
      "overview",
      "aggregate",
      USER,
      WS_A,
      "UTC",
    ]);
    expect(String(aggregate?.queryKey[5])).toContain("provider=anthropic");
    expect(aggregate?.meta).toEqual({ userId: USER, workspaceId: WS_A });
  });

  it("canonicalizes a workspace id that is not a member workspace", async () => {
    const backend = useBackend([
      workspace(WS_A, "Alpha"),
      workspace(WS_B, "Beta"),
    ]);
    renderHome(createAppQueryClient(), `/?workspace=${WS_C}&tz=UTC`);
    await screen.findByTestId("metric-total");
    await waitFor(() => expect(search().get("workspace")).toBe(WS_A));
    expect(backend.calls.detail).toEqual([]);
    expect(search().get("tz")).toBe("UTC");
  });

  it("forces a non-dismissible create dialog when the list is empty and mounts no business page", async () => {
    const backend = useBackend([]);
    renderHome(createAppQueryClient(), "/");
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("创建工作区")).toBeTruthy();
    expect(within(dialog).queryByRole("button", { name: "取消" })).toBeNull();
    const input = within(dialog).getByLabelText("名称");
    await userEvent.type(input, "Alpha");
    await userEvent.keyboard("{Escape}");
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(backend.calls.business).toBe(0);
    expect(backend.calls.posts).toHaveLength(0);
  });

  it("validates before submitting and keeps the entered name after a server error", async () => {
    const backend = useBackend([]);
    server.use(
      http.post("/api/v1/workspaces", async ({ request }) => {
        backend.calls.posts.push(await request.json());
        return errorResponse(409, "WORKSPACE_NAME_CONFLICT");
      }),
    );
    const user = userEvent.setup();
    renderHome(createAppQueryClient(), "/");
    const dialog = await screen.findByRole("dialog");
    const input = within(dialog).getByLabelText("名称");
    const submit = within(dialog).getByRole("button", { name: "创建" });
    await user.click(submit);
    await within(dialog).findByText("请输入工作区名称");
    expect(backend.calls.posts).toHaveLength(0);

    await user.type(input, "  Alpha  ");
    await user.click(submit);
    await within(dialog).findByText("名称已存在，请换一个。");
    expect((input as HTMLInputElement).value).toBe("  Alpha  ");
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(backend.calls.posts).toEqual([{ name: "Alpha" }]);
  });

  it.each([
    [409, "WORKSPACE_LIMIT_REACHED", "已达到 2 个工作区上限。"],
    [500, "INTERNAL", "服务暂不可用，请稍后重试。"],
    [403, "PERMISSION_DENIED", "没有权限创建工作区。"],
    [400, "INVALID_ARGUMENT", "名称不符合规则，请检查后重试。"],
  ])("keeps the dialog and input after a %i %s", async (status, code, text) => {
    useBackend([]);
    server.use(
      http.post("/api/v1/workspaces", () => errorResponse(status, code)),
    );
    const user = userEvent.setup();
    renderHome(createAppQueryClient(), "/");
    const dialog = await screen.findByRole("dialog");
    const input = within(dialog).getByLabelText("名称");
    await user.type(input, "Alpha");
    await user.click(within(dialog).getByRole("button", { name: "创建" }));
    await within(dialog).findByText(text);
    expect((input as HTMLInputElement).value).toBe("Alpha");
    expect(screen.getByRole("dialog")).toBeTruthy();
  });

  it("creates the first workspace once, selects it and only then loads its business data", async () => {
    const created = workspace(WS_B, "Beta");
    const backend = useBackend([]);
    const gate = deferred<Response>();
    server.use(
      http.post("/api/v1/workspaces", async ({ request }) => {
        backend.calls.posts.push(await request.json());
        return gate.promise;
      }),
    );
    const user = userEvent.setup();
    const client = renderHome(createAppQueryClient(), "/");
    const dialog = await screen.findByRole("dialog");
    const input = within(dialog).getByLabelText("名称");
    await user.type(input, "  Beta  ");
    const submit = within(dialog).getByRole("button", { name: "创建" });
    await user.dblClick(submit);
    await user.keyboard("{Enter}");
    await within(dialog).findByRole("button", { name: "正在创建…" });
    expect(backend.calls.posts).toHaveLength(1);

    backend.state.workspaces = [created];
    gate.resolve(HttpResponse.json(body(created), { status: 201 }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(search().get("workspace")).toBe(WS_B));
    await screen.findByTestId("metric-total");
    expect(backend.calls.posts).toEqual([{ name: "Beta" }]);
    expect(backend.calls.aggregate).toContain(WS_B);
    expect(backend.calls.business).toBeGreaterThan(0);
    expect(client.getQueryData(["workspace", "list", USER])).toEqual([
      { workspaceId: WS_B, name: "Beta" },
    ]);
  });

  it("opens a cancellable create dialog for an existing workspace and does not treat list length as quota", async () => {
    useBackend([workspace(WS_A, "Alpha"), workspace(WS_B, "Beta")]);
    const user = userEvent.setup();
    renderHome(createAppQueryClient(), `/?workspace=${WS_A}&tz=UTC`);
    await screen.findByTestId("metric-total");
    await user.click(screen.getByRole("button", { name: "创建工作区" }));
    expect(screen.getByRole("dialog")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "取消" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    await user.click(screen.getByRole("button", { name: "创建工作区" }));
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("switches only after the detail request succeeds and revalidates over HTTP", async () => {
    const backend = useBackend([
      workspace(WS_A, "Alpha"),
      workspace(WS_B, "Beta"),
    ]);
    const gate = deferred<Response>();
    server.use(
      http.get(`/api/v1/workspaces/${WS_B}`, ({ request }) => {
        backend.calls.detail.push(WS_B);
        expect(request.signal).toBeInstanceOf(AbortSignal);
        return gate.promise;
      }),
    );
    const user = userEvent.setup();
    renderHome(createAppQueryClient(), `/?workspace=${WS_A}&tz=UTC`);
    await screen.findByTestId("metric-total");
    await user.click(screen.getByRole("button", { name: "Beta" }));
    expect(search().get("workspace")).toBe(WS_A);
    expect(
      screen.getByRole("button", { name: /Beta/ }).hasAttribute("disabled"),
    ).toBe(true);
    gate.resolve(HttpResponse.json(body(workspace(WS_B, "Beta"))));
    await waitFor(() => expect(search().get("workspace")).toBe(WS_B));
    await waitFor(() => expect(backend.calls.aggregate).toContain(WS_B));
    expect(backend.calls.detail).toEqual([WS_B]);
  });

  it("keeps the old workspace, URL and list cache when the target detail returns 403", async () => {
    useBackend([workspace(WS_A, "Alpha"), workspace(WS_B, "Beta")]);
    server.use(
      http.get(`/api/v1/workspaces/${WS_B}`, () =>
        errorResponse(403, "PERMISSION_DENIED"),
      ),
    );
    const user = userEvent.setup();
    const client = renderHome(
      createAppQueryClient(),
      `/?workspace=${WS_A}&tz=UTC`,
    );
    await screen.findByTestId("metric-total");
    const listBefore = client.getQueryData(["workspace", "list", USER]);
    await user.click(screen.getByRole("button", { name: "Beta" }));
    await screen.findByText(/无法切换到「Beta」/);
    expect(search().get("workspace")).toBe(WS_A);
    expect(screen.getByTestId("metric-total")).toBeTruthy();
    expect(client.getQueryData(["workspace", "list", USER])).toEqual(
      listBefore,
    );
    expect(
      client.getQueryData(["workspace", "detail", USER, WS_B]),
    ).toBeUndefined();
  });

  it("revalidates a cached target and still keeps the old data on 403", async () => {
    useBackend([workspace(WS_A, "Alpha"), workspace(WS_B, "Beta")]);
    const status = { conflict: false };
    server.use(
      http.get(`/api/v1/workspaces/${WS_B}`, () =>
        status.conflict
          ? errorResponse(403, "PERMISSION_DENIED")
          : HttpResponse.json(body(workspace(WS_B, "Beta"))),
      ),
    );
    const user = userEvent.setup();
    const client = renderHome(
      createAppQueryClient(),
      `/?workspace=${WS_A}&tz=UTC`,
    );
    await screen.findByTestId("metric-total");

    // 第一次成功切换，让 B 的详情进入缓存。
    await user.click(screen.getByRole("button", { name: "Beta" }));
    await waitFor(() => expect(search().get("workspace")).toBe(WS_B));
    expect(client.getQueryData(["workspace", "detail", USER, WS_B])).toEqual(
      workspace(WS_B, "Beta"),
    );
    await user.click(screen.getByRole("button", { name: "Alpha" }));
    await waitFor(() => expect(search().get("workspace")).toBe(WS_A));
    await screen.findByTestId("metric-total");

    status.conflict = true;
    await user.click(screen.getByRole("button", { name: "Beta" }));
    await screen.findByText(/无法切换到「Beta」/);
    expect(search().get("workspace")).toBe(WS_A);
    expect(screen.getByTestId("metric-total")).toBeTruthy();
    expect(client.getQueryData(["workspace", "list", USER])).toBeDefined();
  });

  it("A→B→C cancels B's detail request and only the last target updates the URL", async () => {
    const backend = useBackend([
      workspace(WS_A, "Alpha"),
      workspace(WS_B, "Beta"),
      workspace(WS_C, "Gamma"),
    ]);
    const gates = new Map<string, ReturnType<typeof deferred<Response>>>();
    const signals = new Map<string, AbortSignal>();
    for (const id of [WS_B, WS_C]) {
      gates.set(id, deferred<Response>());
    }
    server.use(
      http.get(`/api/v1/workspaces/:id`, ({ params, request }) => {
        const id = String(params.id);
        backend.calls.detail.push(id);
        signals.set(id, request.signal);
        return gates.get(id)?.promise ?? HttpResponse.json({});
      }),
    );
    const user = userEvent.setup();
    renderHome(createAppQueryClient(), `/?workspace=${WS_A}&tz=UTC`);
    await screen.findByTestId("metric-total");
    await user.click(screen.getByRole("button", { name: "Beta" }));
    await waitFor(() => expect(backend.calls.detail).toContain(WS_B));
    await user.click(screen.getByRole("button", { name: "Gamma" }));
    await waitFor(() => expect(backend.calls.detail).toContain(WS_C));
    expect(signals.get(WS_B)?.aborted).toBe(true);
    gates.get(WS_C)?.resolve(HttpResponse.json(body(workspace(WS_C, "Gamma"))));
    await waitFor(() => expect(search().get("workspace")).toBe(WS_C));
    gates.get(WS_B)?.resolve(HttpResponse.json(body(workspace(WS_B, "Beta"))));
    await new Promise((r) => setTimeout(r, 30));
    expect(search().get("workspace")).toBe(WS_C);
  });

  it("selecting the current workspace again cancels the pending switch and keeps its URL", async () => {
    const backend = useBackend([
      workspace(WS_A, "Alpha"),
      workspace(WS_B, "Beta"),
    ]);
    const gate = deferred<Response>();
    let signalB: AbortSignal | undefined;
    server.use(
      http.get(`/api/v1/workspaces/${WS_B}`, ({ request }) => {
        backend.calls.detail.push(WS_B);
        signalB = request.signal;
        return gate.promise;
      }),
    );
    const user = userEvent.setup();
    renderHome(createAppQueryClient(), `/?workspace=${WS_A}&tz=UTC`);
    await screen.findByTestId("metric-total");
    await user.click(screen.getByRole("button", { name: "Beta" }));
    await waitFor(() => expect(signalB).toBeInstanceOf(AbortSignal));

    // 当前空间按钮可点：选它就是最新选择，先前的 B 随之作废。
    await user.click(screen.getByRole("button", { name: "Alpha" }));
    await waitFor(() => expect(signalB?.aborted).toBe(true));
    expect(screen.getByRole("button", { name: "Beta" })).toBeTruthy();
    expect(search().get("workspace")).toBe(WS_A);

    gate.resolve(HttpResponse.json(body(workspace(WS_B, "Beta"))));
    await new Promise((r) => setTimeout(r, 50));
    expect(search().get("workspace")).toBe(WS_A);
    expect(backend.calls.aggregate).not.toContain(WS_B);
  });

  it("an external URL change wins over a pending switch's late success", async () => {
    const backend = useBackend([
      workspace(WS_A, "Alpha"),
      workspace(WS_B, "Beta"),
      workspace(WS_C, "Gamma"),
    ]);
    const gate = deferred<Response>();
    server.use(
      http.get(`/api/v1/workspaces/${WS_B}`, () => {
        backend.calls.detail.push(WS_B);
        return gate.promise;
      }),
    );
    const user = userEvent.setup();
    renderHome(
      createAppQueryClient(),
      `/?workspace=${WS_A}&tz=UTC`,
      <NavigateTo to={`/?workspace=${WS_C}&tz=UTC`} />,
    );
    await screen.findByTestId("metric-total");
    await user.click(screen.getByRole("button", { name: "Beta" }));
    await waitFor(() => expect(backend.calls.detail).toContain(WS_B));

    // 浏览器/路由在切换途中把 URL 改到 C。
    await user.click(screen.getByRole("button", { name: "外部导航" }));
    await waitFor(() => expect(search().get("workspace")).toBe(WS_C));

    gate.resolve(HttpResponse.json(body(workspace(WS_B, "Beta"))));
    await new Promise((r) => setTimeout(r, 50));
    expect(search().get("workspace")).toBe(WS_C);
    expect(screen.getByRole("button", { name: "Beta" })).toBeTruthy();
  });

  it("a late switch success cannot navigate after the container unmounts", async () => {
    const backend = useBackend([
      workspace(WS_A, "Alpha"),
      workspace(WS_B, "Beta"),
    ]);
    const gate = deferred<Response>();
    let signalB: AbortSignal | undefined;
    server.use(
      http.get(`/api/v1/workspaces/${WS_B}`, ({ request }) => {
        backend.calls.detail.push(WS_B);
        signalB = request.signal;
        return gate.promise;
      }),
    );
    const client = createAppQueryClient();
    client.setQueryData(sessionKey, SESSION);
    const view = render(
      homeTree(
        client,
        `/?workspace=${WS_A}&tz=UTC`,
        <WorkspaceHome userId={USER} />,
      ),
    );
    await screen.findByTestId("metric-total");
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Beta" }));
    await waitFor(() => expect(backend.calls.detail).toContain(WS_B));

    view.rerender(homeTree(client, `/?workspace=${WS_A}&tz=UTC`, null));
    expect(screen.queryByRole("button", { name: "Beta" })).toBeNull();
    // 卸载即取消本容器发起的详情验证：请求被 abort，晚到的成功不写详情缓存。
    await waitFor(() => expect(signalB?.aborted).toBe(true));

    gate.resolve(HttpResponse.json(body(workspace(WS_B, "Beta"))));
    await new Promise((r) => setTimeout(r, 50));
    expect(search().get("workspace")).toBe(WS_A);
    expect(
      client.getQueryData(["workspace", "detail", USER, WS_B]),
    ).toBeUndefined();
  });

  it("a late switch success from the previous identity cannot change the URL", async () => {
    const backend = useBackend([
      workspace(WS_A, "Alpha"),
      workspace(WS_B, "Beta"),
    ]);
    const gate = deferred<Response>();
    let signalB: AbortSignal | undefined;
    server.use(
      http.get(`/api/v1/workspaces/${WS_B}`, ({ request }) => {
        backend.calls.detail.push(WS_B);
        signalB = request.signal;
        return gate.promise;
      }),
    );
    const client = createAppQueryClient();
    client.setQueryData(sessionKey, SESSION);
    const view = render(
      homeTree(
        client,
        `/?workspace=${WS_A}&tz=UTC`,
        <WorkspaceHome userId={USER} />,
      ),
    );
    await screen.findByTestId("metric-total");
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Beta" }));
    await waitFor(() => expect(backend.calls.detail).toContain(WS_B));

    client.setQueryData(sessionKey, SESSION_B);
    view.rerender(
      homeTree(
        client,
        `/?workspace=${WS_A}&tz=UTC`,
        <WorkspaceHome userId={SESSION_B.user.id} />,
      ),
    );
    // 换号后旧身份的详情验证被取消，pending 也不再保留。
    await waitFor(() => expect(signalB?.aborted).toBe(true));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Beta" })).toBeTruthy(),
    );

    gate.resolve(HttpResponse.json(body(workspace(WS_B, "Beta"))));
    await new Promise((r) => setTimeout(r, 50));
    expect(search().get("workspace")).toBe(WS_A);
  });

  it("lets the new workspace's metrics win when the old workspace's response arrives later", async () => {
    const backend = useBackend([
      workspace(WS_A, "Alpha"),
      workspace(WS_B, "Beta"),
    ]);
    const gates = new Map<string, ReturnType<typeof deferred<Response>>>();
    const signals = new Map<string, AbortSignal>();
    for (const id of [WS_A, WS_B]) gates.set(id, deferred<Response>());
    server.use(
      http.get(
        `/api/v1/workspaces/:id/usage/aggregate`,
        ({ params, request }) => {
          const id = String(params.id);
          backend.calls.aggregate.push(id);
          signals.set(id, request.signal);
          return (
            gates.get(id)?.promise ?? HttpResponse.json(aggregateBody("0"))
          );
        },
      ),
    );
    const user = userEvent.setup();
    renderHome(createAppQueryClient(), `/?workspace=${WS_A}&tz=UTC`);
    await waitFor(() => expect(backend.calls.aggregate).toContain(WS_A));
    expect(signals.get(WS_A)).toBeInstanceOf(AbortSignal);

    // A 的业务响应仍未返回就切到 B。
    await user.click(screen.getByRole("button", { name: "Beta" }));
    await waitFor(() => expect(search().get("workspace")).toBe(WS_B));
    await waitFor(() => expect(backend.calls.aggregate).toContain(WS_B));

    // 先完成 B：B 的指标可见，A 的请求已被取消。
    gates.get(WS_B)?.resolve(HttpResponse.json(aggregateBody("2222")));
    await waitFor(() =>
      expect(screen.getByTestId("metric-total").textContent).toContain("2.2K"),
    );
    expect(signals.get(WS_A)?.aborted).toBe(true);

    // A 的响应最后才到：不得覆盖 B 的指标，也不得改回 URL。
    gates.get(WS_A)?.resolve(HttpResponse.json(aggregateBody("1111")));
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.getByTestId("metric-total").textContent).toContain("2.2K");
    expect(screen.getByTestId("metric-total").textContent).not.toContain(
      "1.1K",
    );
    expect(search().get("workspace")).toBe(WS_B);
  });

  it("does not use the old workspace data as a placeholder for the new one", async () => {
    const backend = useBackend([
      workspace(WS_A, "Alpha"),
      workspace(WS_B, "Beta"),
    ]);
    const gate = deferred<Response>();
    server.use(
      http.get(`/api/v1/workspaces/${WS_B}/usage/aggregate`, ({ params }) => {
        backend.calls.aggregate.push(String(params.id));
        return gate.promise;
      }),
    );
    const user = userEvent.setup();
    renderHome(createAppQueryClient(), `/?workspace=${WS_A}&tz=UTC`);
    await screen.findByTestId("metric-total");
    await user.click(screen.getByRole("button", { name: "Beta" }));
    await waitFor(() => expect(search().get("workspace")).toBe(WS_B));
    // 旧空间的数据不能作为新空间占位；新聚合未返回前没有指标。
    expect(screen.queryByTestId("metric-total")).toBeNull();
    gate.resolve(
      HttpResponse.json({
        range: {
          start: "2026-10-28T00:00:00Z",
          end: "2026-10-28T01:00:00Z",
          as_of: "2026-10-28T01:00:00Z",
          retention_cutoff: "2025-10-28T00:00:00Z",
          timezone: "UTC",
          granularity: "hour",
        },
        totals: stats,
        points: [],
      }),
    );
    await screen.findByTestId("metric-total");
  });

  it("a create success that lands after logout and a new login cannot navigate the old container", async () => {
    const created = workspace(WS_C, "Gamma");
    const backend = useBackend([
      workspace(WS_A, "Alpha"),
      workspace(WS_B, "Beta"),
    ]);
    const gate = deferred<Response>();
    server.use(
      http.post("/api/v1/workspaces", async ({ request }) => {
        backend.calls.posts.push(await request.json());
        return gate.promise;
      }),
      http.post(
        "/api/v1/auth/logout",
        () => new HttpResponse(null, { status: 204 }),
      ),
      http.post("/api/v1/auth/login", () =>
        HttpResponse.json({
          user: SESSION_B.user,
          expires_at: SESSION_B.expires_at,
        }),
      ),
    );
    const client = renderHome(
      createAppQueryClient(),
      `/?workspace=${WS_A}&tz=UTC`,
      <SessionActions />,
    );
    await screen.findByTestId("metric-total");
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "创建工作区" }));
    const dialog = await screen.findByRole("dialog");
    await user.type(within(dialog).getByLabelText("名称"), "Gamma");
    await user.click(within(dialog).getByRole("button", { name: "创建" }));
    await within(dialog).findByRole("button", { name: "正在创建…" });
    expect(backend.calls.posts).toEqual([{ name: "Gamma" }]);

    // 创建还没返回：退出并换成 B 登录。
    await user.click(screen.getByRole("button", { name: "退出" }));
    await waitFor(() => expect(client.getQueryData(sessionKey)).toBeNull());
    await user.click(screen.getByRole("button", { name: "登录 B" }));
    await waitFor(() =>
      expect(client.getQueryData(sessionKey)).toEqual(SESSION_B),
    );
    const keyB = workspaceListQuery(SESSION_B.user.id).queryKey;
    client.setQueryData(keyB, [workspace(WS_B, "Beta")]);
    // 服务端列表此后包含新空间，迟到的导航会真实落到 URL 上，不会被规范化掩盖。
    backend.state.workspaces = [created];

    gate.resolve(HttpResponse.json(body(created), { status: 201 }));
    await new Promise((r) => setTimeout(r, 50));

    // 旧身份的迟到成功不让旧容器导航，也不清掉新会话与 B 的业务数据。
    expect(search().get("workspace")).toBe(WS_A);
    await waitFor(() =>
      expect(screen.getByTestId("metric-total")).toBeTruthy(),
    );
    expect(client.getQueryData(sessionKey)).toEqual(SESSION_B);
    expect(client.getQueryData(keyB)).toEqual([workspace(WS_B, "Beta")]);
  });

  it("a switch superseded during the old-workspace cancel cannot clear the newer selection", async () => {
    const backend = useBackend([
      workspace(WS_A, "Alpha"),
      workspace(WS_B, "Beta"),
      workspace(WS_C, "Gamma"),
    ]);
    const gateB = deferred<Response>();
    const gateC = deferred<Response>();
    const gateCancel = deferred();
    server.use(
      http.get(`/api/v1/workspaces/${WS_B}`, () => {
        backend.calls.detail.push(WS_B);
        return gateB.promise;
      }),
      http.get(`/api/v1/workspaces/${WS_C}`, () => {
        backend.calls.detail.push(WS_C);
        return gateC.promise;
      }),
    );
    const client = createAppQueryClient();
    // 给“取消旧空间业务请求”一个可控等待窗口：取消已真实发出，恢复由测试放行。
    const cancelQueries = client.cancelQueries.bind(client);
    const held: string[] = [];
    vi.spyOn(client, "cancelQueries").mockImplementation((filters, options) => {
      const done = cancelQueries(filters, options);
      if (filters !== undefined && "predicate" in filters) {
        held.push("business");
        return done.then(() => gateCancel.promise);
      }
      return done;
    });
    const user = userEvent.setup();
    renderHome(client, `/?workspace=${WS_A}&tz=UTC`);
    await screen.findByTestId("metric-total");

    await user.click(screen.getByRole("button", { name: "Beta" }));
    await waitFor(() => expect(backend.calls.detail).toContain(WS_B));
    gateB.resolve(HttpResponse.json(body(workspace(WS_B, "Beta"))));
    // B 的详情已成功，正卡在取消 A 业务请求的等待窗口里。
    await waitFor(() => expect(held).toHaveLength(1));

    // 窗口里用户选择 C：C 成为最新的 pending 选择。
    await user.click(screen.getByRole("button", { name: "Gamma" }));
    await waitFor(() => expect(backend.calls.detail).toContain(WS_C));
    expect(
      screen.getByRole("button", { name: /Gamma/ }).hasAttribute("disabled"),
    ).toBe(true);

    // B 恢复：它已不是最新选择，只能返回；C 的 pending 与 ticket 保持有效。
    gateCancel.resolve();
    await new Promise((r) => setTimeout(r, 30));
    expect(
      screen.getByRole("button", { name: /Gamma/ }).hasAttribute("disabled"),
    ).toBe(true);
    expect(search().get("workspace")).toBe(WS_A);

    gateC.resolve(HttpResponse.json(body(workspace(WS_C, "Gamma"))));
    await waitFor(() => expect(search().get("workspace")).toBe(WS_C));
    expect(search().get("tz")).toBe("UTC");
  });

  it("a create whose list refetch lands after an external navigation keeps the newer workspace", async () => {
    const backend = useBackend([
      workspace(WS_A, "Alpha"),
      workspace(WS_B, "Beta"),
      workspace(WS_C, "Gamma"),
    ]);
    const created = workspace(WS_D, "Delta");
    const listGate = deferred<Response>();
    let listCalls = 0;
    server.use(
      http.get("/api/v1/workspaces", () => {
        listCalls += 1;
        return listCalls === 1
          ? HttpResponse.json(backend.state.workspaces.map(body))
          : listGate.promise;
      }),
      http.post("/api/v1/workspaces", async ({ request }) => {
        backend.calls.posts.push(await request.json());
        return HttpResponse.json(body(created), { status: 201 });
      }),
      http.get("/api/v1/workspaces/:id/usage/aggregate", ({ params }) =>
        HttpResponse.json(aggregateBody(params.id === WS_C ? "3333" : "4444")),
      ),
    );
    const user = userEvent.setup();
    renderHome(
      createAppQueryClient(),
      `/?workspace=${WS_A}&tz=UTC`,
      <NavigateTo to={`/?workspace=${WS_C}&tz=UTC`} />,
    );
    await screen.findByTestId("metric-total");

    await user.click(screen.getByRole("button", { name: "创建工作区" }));
    const dialog = await screen.findByRole("dialog");
    await user.type(within(dialog).getByLabelText("名称"), "Delta");
    await user.click(within(dialog).getByRole("button", { name: "创建" }));
    // 201 已返回，列表重查仍被挂起。
    await waitFor(() => expect(listCalls).toBe(2));

    // 重查窗口里路由把 URL 改到 C。
    await user.click(screen.getByRole("button", { name: "外部导航" }));
    await waitFor(() => expect(search().get("workspace")).toBe(WS_C));
    await waitFor(() =>
      expect(screen.getByTestId("metric-total").textContent).toContain("3.3K"),
    );

    backend.state.workspaces = [
      workspace(WS_A, "Alpha"),
      workspace(WS_B, "Beta"),
      workspace(WS_C, "Gamma"),
      created,
    ];
    listGate.resolve(HttpResponse.json(backend.state.workspaces.map(body)));
    await new Promise((r) => setTimeout(r, 50));

    // 创建本身成功，但不得覆盖更晚的 URL 选择与可见数据。
    expect(search().get("workspace")).toBe(WS_C);
    expect(screen.getByTestId("metric-total").textContent).toContain("3.3K");
    expect(screen.getByTestId("metric-total").textContent).not.toContain(
      "4.4K",
    );
  });

  it("a selection made while a create list refetch is pending wins over the created workspace", async () => {
    const backend = useBackend([
      workspace(WS_A, "Alpha"),
      workspace(WS_B, "Beta"),
      workspace(WS_C, "Gamma"),
    ]);
    const created = workspace(WS_D, "Delta");
    const listGate = deferred<Response>();
    let listCalls = 0;
    server.use(
      http.get("/api/v1/workspaces", () => {
        listCalls += 1;
        return listCalls === 1
          ? HttpResponse.json(backend.state.workspaces.map(body))
          : listGate.promise;
      }),
      http.post("/api/v1/workspaces", async ({ request }) => {
        backend.calls.posts.push(await request.json());
        return HttpResponse.json(body(created), { status: 201 });
      }),
      http.get("/api/v1/workspaces/:id/usage/aggregate", ({ params }) =>
        HttpResponse.json(aggregateBody(params.id === WS_C ? "3333" : "4444")),
      ),
    );
    const user = userEvent.setup();
    renderHome(createAppQueryClient(), `/?workspace=${WS_A}&tz=UTC`);
    await screen.findByTestId("metric-total");

    await user.click(screen.getByRole("button", { name: "创建工作区" }));
    const dialog = await screen.findByRole("dialog");
    await user.type(within(dialog).getByLabelText("名称"), "Delta");
    await user.click(within(dialog).getByRole("button", { name: "创建" }));
    await waitFor(() => expect(listCalls).toBe(2));

    // 同一个重查窗口里用户选择 C：最新选择必须赢。
    await user.click(screen.getByRole("button", { name: "Gamma" }));
    await waitFor(() => expect(search().get("workspace")).toBe(WS_C));

    backend.state.workspaces = [
      workspace(WS_A, "Alpha"),
      workspace(WS_B, "Beta"),
      workspace(WS_C, "Gamma"),
      created,
    ];
    listGate.resolve(HttpResponse.json(backend.state.workspaces.map(body)));
    await new Promise((r) => setTimeout(r, 50));

    expect(search().get("workspace")).toBe(WS_C);
    expect(screen.getByTestId("metric-total").textContent).toContain("3.3K");
    expect(screen.getByTestId("metric-total").textContent).not.toContain(
      "4.4K",
    );
  });

  it("a create whose list refetch spans an external A→C→A navigation keeps the newest selection", async () => {
    const backend = useBackend([
      workspace(WS_A, "Alpha"),
      workspace(WS_B, "Beta"),
      workspace(WS_C, "Gamma"),
    ]);
    const created = workspace(WS_D, "Delta");
    const listGate = deferred<Response>();
    let listCalls = 0;
    server.use(
      http.get("/api/v1/workspaces", () => {
        listCalls += 1;
        return listCalls === 1
          ? HttpResponse.json(backend.state.workspaces.map(body))
          : listGate.promise;
      }),
      http.post("/api/v1/workspaces", async ({ request }) => {
        backend.calls.posts.push(await request.json());
        return HttpResponse.json(body(created), { status: 201 });
      }),
      aggregateByWorkspace({ [WS_A]: "4444", [WS_C]: "3333", [WS_D]: "9999" }),
    );
    const user = userEvent.setup();
    const client = renderHome(
      createAppQueryClient(),
      `/?workspace=${WS_A}&tz=UTC`,
      <>
        <NavigateTo to={`/?workspace=${WS_C}&tz=UTC`} label="去 C" />
        <NavigateTo to={`/?workspace=${WS_A}&tz=UTC`} label="回 A" />
      </>,
    );
    await waitFor(() =>
      expect(screen.getByTestId("metric-total").textContent).toContain("4.4K"),
    );

    await user.click(screen.getByRole("button", { name: "创建工作区" }));
    const dialog = await screen.findByRole("dialog");
    await user.type(within(dialog).getByLabelText("名称"), "Delta");
    await user.click(within(dialog).getByRole("button", { name: "创建" }));
    // 201 已返回，列表重查仍挂起。
    await waitFor(() => expect(listCalls).toBe(2));

    // 重查窗口里先外部去 C、再外部回 A：两次导航都要真实落到 URL 与可见数据上。
    await user.click(screen.getByRole("button", { name: "去 C" }));
    await waitFor(() => expect(search().get("workspace")).toBe(WS_C));
    await waitFor(() =>
      expect(screen.getByTestId("metric-total").textContent).toContain("3.3K"),
    );
    await user.click(screen.getByRole("button", { name: "回 A" }));
    await waitFor(() => expect(search().get("workspace")).toBe(WS_A));
    await waitFor(() =>
      expect(screen.getByTestId("metric-total").textContent).toContain("4.4K"),
    );

    backend.state.workspaces = [
      workspace(WS_A, "Alpha"),
      workspace(WS_B, "Beta"),
      workspace(WS_C, "Gamma"),
      created,
    ];
    listGate.resolve(HttpResponse.json(backend.state.workspaces.map(body)));
    await new Promise((r) => setTimeout(r, 50));

    // 创建本身成功，但期间用户已把选择定回 A：不得改 URL 到 D，也不得上 D 的数据。
    expect(search().get("workspace")).toBe(WS_A);
    expect(screen.getByTestId("metric-total").textContent).toContain("4.4K");
    expect(screen.getByTestId("metric-total").textContent).not.toContain(
      "10.0K",
    );
    expect(
      client.getQueryData<Workspace[]>(["workspace", "list", USER]),
    ).toContainEqual(created);
  });

  it("a create in the zero-workspace window selects the new workspace before the list refetch returns", async () => {
    const created = workspace(WS_B, "Beta");
    useBackend([]);
    const listGate = deferred<Response>();
    let listCalls = 0;
    server.use(
      http.get("/api/v1/workspaces", () => {
        listCalls += 1;
        return listCalls === 1 ? HttpResponse.json([]) : listGate.promise;
      }),
      http.post("/api/v1/workspaces", () =>
        HttpResponse.json(body(created), { status: 201 }),
      ),
      aggregateByWorkspace({ [WS_B]: "2222" }),
    );
    const user = userEvent.setup();
    const client = renderHome(createAppQueryClient(), "/");
    const dialog = await screen.findByRole("dialog");
    await user.type(within(dialog).getByLabelText("名称"), "Beta");
    await user.click(within(dialog).getByRole("button", { name: "创建" }));
    await waitFor(() => expect(listCalls).toBe(2));

    // 列表重查还挂着：新空间已写入缓存，Home 规范化 URL、旧 modal 卸载、业务数据跟上。
    await waitFor(() => expect(search().get("workspace")).toBe(WS_B));
    expect(screen.queryByRole("dialog")).toBeNull();
    await waitFor(() =>
      expect(screen.getByTestId("metric-total").textContent).toContain("2.2K"),
    );

    listGate.resolve(HttpResponse.json([body(created)]));
    await new Promise((r) => setTimeout(r, 30));
    expect(search().get("workspace")).toBe(WS_B);
    expect(screen.getByTestId("metric-total").textContent).toContain("2.2K");
    expect(client.getQueryData(["workspace", "list", USER])).toEqual([created]);
  });

  it.each(["external A→C→A", "new selection C"])(
    "a create blocked while cancelling the old workspace data cannot override a newer selection (%s)",
    async (variant) => {
      const backend = useBackend([
        workspace(WS_A, "Alpha"),
        workspace(WS_B, "Beta"),
        workspace(WS_C, "Gamma"),
      ]);
      const created = workspace(WS_D, "Delta");
      server.use(
        http.post("/api/v1/workspaces", async ({ request }) => {
          backend.calls.posts.push(await request.json());
          backend.state.workspaces = [...backend.state.workspaces, created];
          return HttpResponse.json(body(created), { status: 201 });
        }),
        aggregateByWorkspace({
          [WS_A]: "4444",
          [WS_C]: "3333",
          [WS_D]: "9999",
        }),
      );
      const client = createAppQueryClient();
      const gates = holdBusinessCancels(client);
      const user = userEvent.setup();
      const external = variant.startsWith("external");
      renderHome(
        client,
        `/?workspace=${WS_A}&tz=UTC`,
        external ? (
          <>
            <NavigateTo to={`/?workspace=${WS_C}&tz=UTC`} label="去 C" />
            <NavigateTo to={`/?workspace=${WS_A}&tz=UTC`} label="回 A" />
          </>
        ) : undefined,
      );
      await waitFor(() =>
        expect(screen.getByTestId("metric-total").textContent).toContain(
          "4.4K",
        ),
      );

      await user.click(screen.getByRole("button", { name: "创建工作区" }));
      const dialog = await screen.findByRole("dialog");
      await user.type(within(dialog).getByLabelText("名称"), "Delta");
      await user.click(within(dialog).getByRole("button", { name: "创建" }));
      // 创建已成功、列表已重查，正卡在取消 A 业务请求的等待窗口里。
      await waitFor(() => expect(gates).toHaveLength(1));

      if (external) {
        await user.click(screen.getByRole("button", { name: "去 C" }));
        await waitFor(() => expect(search().get("workspace")).toBe(WS_C));
        await waitFor(() =>
          expect(screen.getByTestId("metric-total").textContent).toContain(
            "3.3K",
          ),
        );
        await user.click(screen.getByRole("button", { name: "回 A" }));
        await waitFor(() => expect(search().get("workspace")).toBe(WS_A));
        await waitFor(() =>
          expect(screen.getByTestId("metric-total").textContent).toContain(
            "4.4K",
          ),
        );

        gates[0]?.();
        await new Promise((r) => setTimeout(r, 30));

        // 旧创建晚到：不得导航到 D，不得关掉 modal，也不得清掉更晚的选择结果。
        expect(search().get("workspace")).toBe(WS_A);
        expect(screen.getByTestId("metric-total").textContent).toContain(
          "4.4K",
        );
        expect(screen.getByTestId("metric-total").textContent).not.toContain(
          "10.0K",
        );
        expect(screen.getByRole("dialog")).toBeTruthy();
        expect(screen.queryByText(/切换失败/)).toBeNull();
        expect(
          client.getQueryData<Workspace[]>(["workspace", "list", USER]),
        ).toContainEqual(created);
        return;
      }

      // 窗口里用户选择 C：C 的详情成功，也在等它自己的旧业务取消窗口。
      await user.click(screen.getByRole("button", { name: "Gamma" }));
      await waitFor(() => expect(gates).toHaveLength(2));
      expect(
        screen.getByRole("button", { name: /Gamma/ }).hasAttribute("disabled"),
      ).toBe(true);

      // 先放行旧创建：它只能返回，不能清掉 C 的 pending、错误或 modal，也不能改 URL。
      gates[0]?.();
      await new Promise((r) => setTimeout(r, 30));
      expect(search().get("workspace")).toBe(WS_A);
      expect(
        screen.getByRole("button", { name: /Gamma/ }).hasAttribute("disabled"),
      ).toBe(true);
      expect(screen.getByRole("dialog")).toBeTruthy();
      expect(screen.queryByText(/切换失败/)).toBeNull();
      expect(screen.getByTestId("metric-total").textContent).toContain("4.4K");

      gates[1]?.();
      await waitFor(() => expect(search().get("workspace")).toBe(WS_C));
      await waitFor(() =>
        expect(screen.getByTestId("metric-total").textContent).toContain(
          "3.3K",
        ),
      );
      expect(screen.getByTestId("metric-total").textContent).not.toContain(
        "10.0K",
      );
      expect(
        client.getQueryData<Workspace[]>(["workspace", "list", USER]),
      ).toContainEqual(created);
    },
  );

  it("a create pending when the browser goes back to a normalized location keeps that location", async () => {
    const backend = useBackend([
      workspace(WS_A, "Alpha"),
      workspace(WS_B, "Beta"),
      workspace(WS_C, "Gamma"),
    ]);
    const created = workspace(WS_D, "Delta");
    const listGate = deferred<Response>();
    let listCalls = 0;
    server.use(
      http.get("/api/v1/workspaces", () => {
        listCalls += 1;
        return listCalls === 1
          ? HttpResponse.json(backend.state.workspaces.map(body))
          : listGate.promise;
      }),
      http.post("/api/v1/workspaces", async ({ request }) => {
        backend.calls.posts.push(await request.json());
        return HttpResponse.json(body(created), { status: 201 });
      }),
      aggregateByWorkspace({ [WS_A]: "4444", [WS_C]: "3333", [WS_D]: "9999" }),
    );
    const user = userEvent.setup();
    const client = renderHome(
      createAppQueryClient(),
      "/?tz=UTC",
      <>
        <NavigateTo to={`/?workspace=${WS_C}&tz=UTC`} label="去 C" />
        <NavigateBack />
      </>,
    );
    // 首次规范化写出的 A 位置带系统标记；后退会回到这个位置本身。
    await waitFor(() => expect(search().get("workspace")).toBe(WS_A));
    await waitFor(() =>
      expect(screen.getByTestId("metric-total").textContent).toContain("4.4K"),
    );

    await user.click(screen.getByRole("button", { name: "创建工作区" }));
    const dialog = await screen.findByRole("dialog");
    await user.type(within(dialog).getByLabelText("名称"), "Delta");
    await user.click(within(dialog).getByRole("button", { name: "创建" }));
    await waitFor(() => expect(listCalls).toBe(2));

    await user.click(screen.getByRole("button", { name: "去 C" }));
    await waitFor(() => expect(search().get("workspace")).toBe(WS_C));
    await waitFor(() =>
      expect(screen.getByTestId("metric-total").textContent).toContain("3.3K"),
    );

    // 后退回到同一个 A 位置：这是外部路由事件，不能当成系统自己的写入。
    await user.click(screen.getByRole("button", { name: "浏览器后退" }));
    await waitFor(() => expect(search().get("workspace")).toBe(WS_A));
    await waitFor(() =>
      expect(screen.getByTestId("metric-total").textContent).toContain("4.4K"),
    );

    backend.state.workspaces = [
      workspace(WS_A, "Alpha"),
      workspace(WS_B, "Beta"),
      workspace(WS_C, "Gamma"),
      created,
    ];
    listGate.resolve(HttpResponse.json(backend.state.workspaces.map(body)));
    await new Promise((r) => setTimeout(r, 50));

    expect(search().get("workspace")).toBe(WS_A);
    expect(screen.getByTestId("metric-total").textContent).toContain("4.4K");
    expect(screen.getByTestId("metric-total").textContent).not.toContain(
      "10.0K",
    );
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(
      client.getQueryData<Workspace[]>(["workspace", "list", USER]),
    ).toContainEqual(created);
  });
});
