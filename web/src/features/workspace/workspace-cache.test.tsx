import { describe, expect, it } from "vitest";
import { QueryClientProvider, type QueryClient } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import { ApiError } from "@/lib/api";
import {
  createAppQueryClient,
  sessionKey,
  useCredentialsMutation,
  useLogoutMutation,
} from "@/features/auth/queries";
import { login } from "@/features/auth/api";
import type { AuthSession } from "@/features/auth/model";
import {
  deferred,
  errorResponse,
  http,
  HttpResponse,
  server,
} from "@/test/server";
import {
  useCreateWorkspace,
  useUsageOptions,
  useWorkspaceList,
  usageOptionsQuery,
  workspaceDetailQuery,
  workspaceListQuery,
} from "./queries";

const SESSION_A: AuthSession = {
  user: { id: "user-a", email: "a@example.com" },
  expires_at: "2999-01-01T00:00:00Z",
};
const SESSION_B: AuthSession = {
  user: { id: "user-b", email: "b@example.com" },
  expires_at: "2999-01-01T00:00:00Z",
};
const WS = "0199a0b0-0000-7000-8000-00000000000a";
const WS_B = "0199a0b0-0000-7000-8000-00000000000b";

function seed(client: QueryClient, session: AuthSession | null) {
  client.setQueryData(sessionKey, session);
}

function Probe({ userId }: { userId: string | undefined }) {
  const list = useWorkspaceList(userId);
  const workspaceId = list.data?.[0]?.workspaceId;
  const options = useUsageOptions(userId, workspaceId);
  return (
    <p data-testid="out">
      {list.data?.map((w) => w.name).join(",") ?? "-"}|
      {options.data?.providers.join(",") ?? "-"}
    </p>
  );
}

function mount(client: QueryClient, userId: string | undefined) {
  return render(
    <QueryClientProvider client={client}>
      <Probe userId={userId} />
    </QueryClientProvider>,
  );
}

function mockWorkspace() {
  const hits = { list: 0, options: 0 };
  server.use(
    http.get("/api/v1/workspaces", () => {
      hits.list += 1;
      return HttpResponse.json([{ workspace_id: WS, name: "Alpha" }]);
    }),
    http.get(`/api/v1/workspaces/${WS}/usage/options`, () => {
      hits.options += 1;
      return HttpResponse.json({ providers: ["p1"], models: [] });
    }),
  );
  return hits;
}

describe("workspace queries", () => {
  it("do not request anything until the user is known, then use the server workspace id", async () => {
    const hits = mockWorkspace();
    const client = createAppQueryClient();
    const view = mount(client, undefined);
    await new Promise((r) => setTimeout(r, 30));
    expect(hits).toEqual({ list: 0, options: 0 });
    seed(client, SESSION_A);
    view.rerender(
      <QueryClientProvider client={client}>
        <Probe userId="user-a" />
      </QueryClientProvider>,
    );
    await waitFor(() =>
      expect(screen.getByTestId("out").textContent).toBe("Alpha|p1"),
    );
    expect(hits).toEqual({ list: 1, options: 1 });
  });

  it("a malformed workspace list body is a data error, not an empty workspace", async () => {
    server.use(
      http.get("/api/v1/workspaces", () => HttpResponse.json({ name: "x" })),
    );
    const client = createAppQueryClient();
    seed(client, SESSION_A);
    await client
      .fetchQuery(workspaceListQuery("user-a"))
      .catch((e: unknown) => e);
    const state = client.getQueryState(workspaceListQuery("user-a").queryKey);
    expect((state?.error as ApiError).code).toBe("BAD_RESPONSE");
    expect(state?.data).toBeUndefined();
  });

  it("keeps user, workspace and filter inputs in scoped keys and passes an AbortSignal", async () => {
    const signals: AbortSignal[] = [];
    server.use(
      http.get("/api/v1/workspaces", ({ request }) => {
        signals.push(request.signal);
        return HttpResponse.json([{ workspace_id: WS, name: "Alpha" }]);
      }),
      http.get(`/api/v1/workspaces/${WS}/usage/options`, ({ request }) => {
        signals.push(request.signal);
        return HttpResponse.json({ providers: ["p1"], models: [] });
      }),
    );
    const client = createAppQueryClient();
    seed(client, SESSION_A);
    await client.fetchQuery(workspaceListQuery("user-a"));
    await client.fetchQuery(usageOptionsQuery("user-a", WS));
    expect(workspaceListQuery("user-a").queryKey).toEqual([
      "workspace",
      "list",
      "user-a",
    ]);
    expect(usageOptionsQuery("user-a", WS).queryKey).toEqual([
      "workspace",
      "usage-options",
      "user-a",
      WS,
    ]);
    expect(usageOptionsQuery("user-a", WS).meta).toEqual({
      userId: "user-a",
      workspaceId: WS,
    });
    expect(signals).toHaveLength(2);
    expect(signals.every((s) => s instanceof AbortSignal)).toBe(true);
  });
});

describe("business cache cleanup", () => {
  async function loaded() {
    mockWorkspace();
    const client = createAppQueryClient();
    seed(client, SESSION_A);
    mount(client, "user-a");
    await waitFor(() =>
      expect(screen.getByTestId("out").textContent).toBe("Alpha|p1"),
    );
    return client;
  }
  const businessCount = (client: QueryClient) =>
    client
      .getQueryCache()
      .findAll({ predicate: (q) => q.queryKey[0] !== "auth" }).length;

  it("business 401 from the current identity clears business cache and goes anonymous", async () => {
    const client = await loaded();
    server.use(
      http.get(`/api/v1/workspaces/${WS}/usage/options`, () =>
        errorResponse(401, "UNAUTHENTICATED"),
      ),
    );
    await act(async () => {
      await client.refetchQueries({ queryKey: ["workspace", "usage-options"] });
    });
    await waitFor(() => expect(client.getQueryData(sessionKey)).toBeNull());
    await waitFor(() => expect(businessCount(client)).toBe(0));
  });

  it("business 403 drops other business data and keeps the session", async () => {
    const client = await loaded();
    server.use(
      http.get(`/api/v1/workspaces/${WS}/usage/options`, () =>
        errorResponse(403, "PERMISSION_DENIED"),
      ),
    );
    await act(async () => {
      await client.refetchQueries({ queryKey: ["workspace", "usage-options"] });
    });
    await waitFor(() =>
      expect(
        client.getQueryData(workspaceListQuery("user-a").queryKey),
      ).toBeUndefined(),
    );
    expect(client.getQueryData(sessionKey)).toEqual(SESSION_A);
  });

  it("403 on a background refetch hides the failing query's old data but keeps its error", async () => {
    const client = await loaded();
    const key = ["workspace", "usage-options", "user-a", WS];
    expect(client.getQueryData(key)).toBeDefined();
    server.use(
      http.get(`/api/v1/workspaces/${WS}/usage/options`, () =>
        errorResponse(403, "PERMISSION_DENIED"),
      ),
    );
    await act(async () => {
      await client.refetchQueries({ queryKey: key });
    });
    await waitFor(() => expect(client.getQueryData(key)).toBeUndefined());
    const state = client.getQueryState(key);
    expect(state?.status).toBe("error");
    expect((state?.error as ApiError).status).toBe(403);
    expect(client.getQueryData(sessionKey)).toEqual(SESSION_A);
    expect(screen.getByTestId("out").textContent).not.toContain("p1");
  });

  it("a detail 403 marked local keeps the old workspace data, list cache and URL", async () => {
    const client = await loaded();
    const listKey = workspaceListQuery("user-a").queryKey;
    const optionsKey = ["workspace", "usage-options", "user-a", WS];
    await client.fetchQuery(usageOptionsQuery("user-a", WS));
    expect(client.getQueryData(listKey)).toBeDefined();
    expect(client.getQueryData(optionsKey)).toBeDefined();

    server.use(
      http.get(`/api/v1/workspaces/${WS_B}`, () =>
        errorResponse(403, "PERMISSION_DENIED"),
      ),
    );
    const detailKey = workspaceDetailQuery("user-a", WS_B).queryKey;
    await act(async () => {
      await client
        .fetchQuery(workspaceDetailQuery("user-a", WS_B))
        .catch(() => undefined);
    });
    expect((client.getQueryState(detailKey)?.error as ApiError).status).toBe(
      403,
    );
    expect(client.getQueryData(listKey)).toBeDefined();
    expect(client.getQueryData(optionsKey)).toBeDefined();
    expect(client.getQueryData(sessionKey)).toEqual(SESSION_A);
  });

  it("revalidates a cached target detail and keeps local 403 from dropping current data", async () => {
    const client = await loaded();
    const listKey = workspaceListQuery("user-a").queryKey;
    const optionsKey = ["workspace", "usage-options", "user-a", WS];
    await client.fetchQuery(usageOptionsQuery("user-a", WS));

    let conflict = false;
    const hits: string[] = [];
    server.use(
      http.get(`/api/v1/workspaces/${WS_B}`, () => {
        hits.push("hit");
        return conflict
          ? errorResponse(403, "PERMISSION_DENIED")
          : HttpResponse.json({ workspace_id: WS_B, name: "Beta" });
      }),
    );
    await client.fetchQuery(workspaceDetailQuery("user-a", WS_B));
    expect(
      client.getQueryData(workspaceDetailQuery("user-a", WS_B).queryKey),
    ).toEqual({ workspaceId: WS_B, name: "Beta" });
    conflict = true;
    await act(async () => {
      await client
        .fetchQuery(workspaceDetailQuery("user-a", WS_B))
        .catch(() => undefined);
    });
    expect(hits).toEqual(["hit", "hit"]);
    expect(client.getQueryData(listKey)).toBeDefined();
    expect(client.getQueryData(optionsKey)).toBeDefined();
    expect(client.getQueryData(sessionKey)).toEqual(SESSION_A);
  });

  it("a late 401 from an old identity cannot sign out the new login", async () => {
    mockWorkspace();
    const gate = deferred();
    const client = createAppQueryClient();
    seed(client, SESSION_A);
    server.use(
      http.get(`/api/v1/workspaces/${WS}/usage/options`, async () => {
        await gate.promise;
        return errorResponse(401, "UNAUTHENTICATED");
      }),
    );
    mount(client, "user-a");
    await waitFor(() =>
      expect(
        client.getQueryState(["workspace", "usage-options", "user-a", WS])
          ?.fetchStatus,
      ).toBe("fetching"),
    );
    // 新账号登录成功，先清掉旧业务缓存再写入会话。
    server.use(
      http.post("/api/v1/auth/login", () =>
        HttpResponse.json({
          user: SESSION_B.user,
          expires_at: SESSION_B.expires_at,
        }),
      ),
    );
    const session = await login({
      email: "b@example.com",
      password: "synthetic-password",
    });
    seed(client, session);
    gate.resolve();
    await new Promise((r) => setTimeout(r, 50));
    expect(client.getQueryData(sessionKey)).toEqual(SESSION_B);
  });

  function LoginProbe() {
    const login = useCredentialsMutation("login");
    return (
      <button
        type="button"
        onClick={() =>
          login.mutate({
            email: "b@example.com",
            password: "synthetic-password",
          })
        }
      >
        in
      </button>
    );
  }

  it.each([401, 403])(
    "a delayed old-identity %i that lands after the real login mutation cannot touch the new session",
    async (status) => {
      mockWorkspace();
      const gate = deferred();
      const client = createAppQueryClient();
      seed(client, SESSION_A);
      server.use(
        http.get(`/api/v1/workspaces/${WS}/usage/options`, async () => {
          await gate.promise;
          return errorResponse(
            status,
            status === 401 ? "UNAUTHENTICATED" : "PERMISSION_DENIED",
          );
        }),
        http.post("/api/v1/auth/login", () =>
          HttpResponse.json({
            user: SESSION_B.user,
            expires_at: SESSION_B.expires_at,
          }),
        ),
      );
      mount(client, "user-a");
      const key = ["workspace", "usage-options", "user-a", WS];
      await waitFor(() =>
        expect(client.getQueryState(key)?.fetchStatus).toBe("fetching"),
      );
      render(
        <QueryClientProvider client={client}>
          <LoginProbe />
        </QueryClientProvider>,
      );
      await act(async () => {
        screen.getByRole("button", { name: "in" }).click();
      });
      await waitFor(() =>
        expect(client.getQueryData(sessionKey)).toEqual(SESSION_B),
      );
      // 登录 onSuccess 已取消并移除旧身份业务缓存；旧请求此时才返回。
      expect(client.getQueryState(key)).toBeUndefined();
      gate.resolve();
      await new Promise((r) => setTimeout(r, 50));
      expect(client.getQueryData(sessionKey)).toEqual(SESSION_B);
      expect(client.getQueryState(key)).toBeUndefined();
    },
  );

  it("session refetch resolving anonymous removes business cache", async () => {
    const client = await loaded();
    server.use(
      http.get("/api/v1/auth/session", () =>
        errorResponse(401, "UNAUTHENTICATED"),
      ),
    );
    await act(async () => {
      await client.fetchQuery({
        queryKey: sessionKey,
        queryFn: () => null,
        staleTime: 0,
      });
    });
    await waitFor(() => expect(businessCount(client)).toBe(0));
  });

  function LogoutProbe() {
    const logout = useLogoutMutation();
    return (
      <button type="button" onClick={() => logout.mutate()}>
        out
      </button>
    );
  }

  it("logout success removes business cache; failure keeps data and session", async () => {
    const client = await loaded();
    const view = render(
      <QueryClientProvider client={client}>
        <LogoutProbe />
      </QueryClientProvider>,
    );
    server.use(
      http.post("/api/v1/auth/logout", () => errorResponse(500, "INTERNAL")),
    );
    await act(async () => {
      screen.getByRole("button", { name: "out" }).click();
    });
    await new Promise((r) => setTimeout(r, 50));
    expect(client.getQueryData(sessionKey)).toEqual(SESSION_A);
    expect(businessCount(client)).toBeGreaterThan(0);

    server.use(
      http.post(
        "/api/v1/auth/logout",
        () => new HttpResponse(null, { status: 204 }),
      ),
    );
    await act(async () => {
      screen.getByRole("button", { name: "out" }).click();
    });
    await waitFor(() => expect(client.getQueryData(sessionKey)).toBeNull());
    expect(businessCount(client)).toBe(0);
    view.unmount();
  });
});

describe("workspace create mutation errors", () => {
  function CreateProbe({ userId }: { userId: string }) {
    const create = useCreateWorkspace(userId);
    return (
      <button type="button" onClick={() => create.mutate("New")}>
        create
      </button>
    );
  }

  const businessCount = (client: QueryClient) =>
    client
      .getQueryCache()
      .findAll({ predicate: (q) => q.queryKey[0] !== "auth" }).length;

  it("a create 401 from the current user clears the session and business cache", async () => {
    mockWorkspace();
    const gate = deferred<Response>();
    const client = createAppQueryClient();
    seed(client, SESSION_A);
    mount(client, "user-a");
    await waitFor(() =>
      expect(screen.getByTestId("out").textContent).toBe("Alpha|p1"),
    );
    server.use(http.post("/api/v1/workspaces", () => gate.promise));
    render(
      <QueryClientProvider client={client}>
        <CreateProbe userId="user-a" />
      </QueryClientProvider>,
    );
    await act(async () => {
      screen.getByRole("button", { name: "create" }).click();
    });
    await waitFor(() => expect(businessCount(client)).toBeGreaterThan(0));
    await act(async () => {
      gate.resolve(errorResponse(401, "UNAUTHENTICATED"));
      await Promise.resolve();
    });
    await waitFor(() => expect(client.getQueryData(sessionKey)).toBeNull());
    await waitFor(() => expect(businessCount(client)).toBe(0));
  });

  it("a create 401 issued by an old user cannot clear the new user's session or cache", async () => {
    const gate = deferred<Response>();
    const client = createAppQueryClient();
    seed(client, SESSION_A);
    server.use(http.post("/api/v1/workspaces", () => gate.promise));
    render(
      <QueryClientProvider client={client}>
        <CreateProbe userId="user-a" />
      </QueryClientProvider>,
    );
    await act(async () => {
      screen.getByRole("button", { name: "create" }).click();
    });

    // 新用户登录并已加载自己的业务缓存。
    const keyB = workspaceListQuery("user-b").queryKey;
    client.setQueryData(keyB, [{ workspaceId: WS_B, name: "Beta" }]);
    seed(client, SESSION_B);
    await act(async () => {
      gate.resolve(errorResponse(401, "UNAUTHENTICATED"));
      await Promise.resolve();
    });
    await new Promise((r) => setTimeout(r, 30));
    expect(client.getQueryData(sessionKey)).toEqual(SESSION_B);
    expect(client.getQueryData(keyB)).toEqual([
      { workspaceId: WS_B, name: "Beta" },
    ]);
  });

  it("a create 403 stays local and keeps the session and business data", async () => {
    mockWorkspace();
    const client = createAppQueryClient();
    seed(client, SESSION_A);
    mount(client, "user-a");
    await waitFor(() =>
      expect(screen.getByTestId("out").textContent).toBe("Alpha|p1"),
    );
    const before = businessCount(client);
    server.use(
      http.post("/api/v1/workspaces", () =>
        errorResponse(403, "PERMISSION_DENIED"),
      ),
    );
    render(
      <QueryClientProvider client={client}>
        <CreateProbe userId="user-a" />
      </QueryClientProvider>,
    );
    await act(async () => {
      screen.getByRole("button", { name: "create" }).click();
    });
    await new Promise((r) => setTimeout(r, 30));
    expect(client.getQueryData(sessionKey)).toEqual(SESSION_A);
    expect(businessCount(client)).toBe(before);
  });
});
