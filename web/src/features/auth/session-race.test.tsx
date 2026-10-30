import { useEffect } from "react";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import {
  notifyManager,
  QueryObserver,
  QueryClientProvider,
  type QueryClient,
} from "@tanstack/react-query";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "@/App";
import {
  createAppQueryClient,
  replaceSessionQuery,
  sessionKey,
  useCredentialsMutation,
  useLogoutMutation,
} from "./queries";
import type { AuthSession } from "./model";
import { fetchSession } from "./api";
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
const DAY = 86_400_000;
const realSetTimeout = setTimeout;

/** 假时间下不能用 RTL waitFor（它依赖被替换的 setTimeout）；用真实定时器轮询。 */
async function until(check: () => unknown) {
  for (let i = 0; i < 200; i += 1) {
    try {
      check();
      return;
    } catch {
      await act(async () => {
        // MSW 的响应交付依赖被替换的 setTimeout：假时钟下只等真实时间不会放行，
        // 须每轮推进 1ms 让其定时器执行；1ms 远小于各用例的时间断言粒度。
        if (vi.isFakeTimers()) await vi.advanceTimersByTimeAsync(1);
        await new Promise((r) => realSetTimeout(r, 5));
      });
    }
  }
  check();
}

interface Get {
  signal: AbortSignal;
  gate: ReturnType<typeof deferred<Response>>;
  /** MSW 处理器在放行后真正返回时 resolve（服务端完成信号，与客户端是否已中止无关）。 */
  done: Promise<void>;
}

/** 每个 GET session 请求都挂起，直到测试显式放行；记录其 AbortSignal。 */
function deferredSessionGets() {
  const gets: Get[] = [];
  server.use(
    http.get(SESSION, async ({ request }) => {
      const gate = deferred<Response>();
      const finished = deferred();
      gets.push({
        signal: request.signal,
        gate,
        done: finished.promise,
      });
      const response = await gate.promise;
      finished.resolve();
      return response;
    }),
  );
  return gets;
}

/**
 * 放行一个挂起的 GET，并等待服务端处理器确实返回（明确完成信号，不数轮询）。
 * 中止后的客户端 fetch 已拒绝，不可能再收到该响应；
 * “客户端 queryFn 忽略 AbortSignal”的迟到结果由 controlledSession 用例单独证明。
 */
async function release(get: Get | undefined, response: Response) {
  if (!get) throw new Error("missing GET");
  get.gate.resolve(response);
  await get.done;
}

const ok = (email: string, expiresAt = FUTURE) =>
  HttpResponse.json(sessionBody(email, expiresAt));
const anonymous = () => errorResponse(401, "UNAUTHENTICATED");

function seed(client: QueryClient, email: string, expiresAt = FUTURE) {
  // 无 observer 的种子查询需要 queryFn 才能被 refetchQueries 重新请求。
  client.setQueryDefaults(sessionKey, {
    queryFn: ({ signal }) => fetchSession(signal),
  });
  client.setQueryData<AuthSession>(sessionKey, {
    user: { id: "u", email },
    expires_at: expiresAt,
  });
}

interface Call {
  signal: AbortSignal;
  gate: ReturnType<typeof deferred<AuthSession | null>>;
  returned: Promise<AuthSession | null>;
}

/**
 * 不走 HTTP、且忽略 AbortSignal 的 queryFn（独立边界）。
 * release 在 queryFn 返回的 promise 上等待：React Query 的 retryer 在调用 queryFn
 * 时先挂了处理器，测试后挂，按挂接顺序保证旧结果已被 React Query 处理完才继续。
 */
function controlledSession(client: QueryClient) {
  const calls: Call[] = [];
  client.setQueryDefaults(sessionKey, {
    queryFn: ({ signal }) => {
      const gate = deferred<AuthSession | null>();
      const returned = (async () => gate.promise)();
      calls.push({ signal, gate, returned });
      return returned;
    },
  });
  return calls;
}

async function releaseCall(call: Call | undefined, value: AuthSession | null) {
  if (!call) throw new Error("missing call");
  call.gate.resolve(value);
  await call.returned;
}

const session = (email: string, expiresAt = FUTURE): AuthSession => ({
  user: { id: "u", email },
  expires_at: expiresAt,
});

/** 记录期间是否有任何 DOM 变更曾加入含该文本的节点（用于证明从未展示，而非仅最终隐藏）。 */
function watchIdentity(text: string) {
  let seen = false;
  const scan = (records: MutationRecord[]) => {
    for (const record of records) {
      record.addedNodes.forEach((node) => {
        if (node.textContent?.includes(text)) seen = true;
      });
      if (
        record.type === "characterData" &&
        record.target.textContent?.includes(text)
      )
        seen = true;
    }
  };
  const observer = new MutationObserver(scan);
  observer.observe(document.body, {
    childList: true,
    subtree: true,
    characterData: true,
  });
  return {
    seen: () => {
      scan(observer.takeRecords());
      return seen;
    },
    stop: () => observer.disconnect(),
  };
}

/** 直接驱动 mutation 的探针，使竞态用例能精确控制顺序。 */
function Driver({
  actionRef,
}: {
  actionRef: { current: ((kind: string) => void) | undefined };
}) {
  const login = useCredentialsMutation("login");
  const register = useCredentialsMutation("register");
  const logout = useLogoutMutation();
  useEffect(() => {
    actionRef.current = (kind) => {
      if (kind === "login")
        login.mutate({ email: "new@example.com", password: "password-1" });
      if (kind === "register")
        register.mutate({ email: "new@example.com", password: "password-1" });
      if (kind === "logout") logout.mutate();
    };
  });
  return null;
}

function renderDriver(client: QueryClient, path = "/") {
  const actionRef: { current: ((kind: string) => void) | undefined } = {
    current: undefined,
  };
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <Driver actionRef={actionRef} />
        <App />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return (kind: string) => act(() => actionRef.current?.(kind));
}

describe("first load without cache", () => {
  it("issues exactly one GET with an AbortSignal and shows no identity while pending", async () => {
    const gets = deferredSessionGets();
    renderApp("/");
    await waitFor(() => expect(gets).toHaveLength(1));
    expect(gets[0]?.signal).toBeInstanceOf(AbortSignal);
    expect(gets[0]?.signal.aborted).toBe(false);
    expect(screen.getByRole("status").textContent).toContain(
      "正在确认登录状态",
    );
    expect(screen.queryByText("Personal")).toBeNull();
    gets[0]?.gate.resolve(ok("a@example.com"));
    await screen.findByText("a@example.com");
    expect(gets).toHaveLength(1);
  });

  it("goes to the login page only on 401 UNAUTHENTICATED", async () => {
    const gets = deferredSessionGets();
    renderApp("/");
    await waitFor(() => expect(gets).toHaveLength(1));
    gets[0]?.gate.resolve(anonymous());
    await screen.findByRole("heading", { name: "登录" });
    expect(gets).toHaveLength(1);
  });

  it.each([
    ["500", () => errorResponse(500, "INTERNAL")],
    ["503", () => errorResponse(503, "UNAVAILABLE")],
    ["network", () => HttpResponse.error()],
    ["bad body", () => new HttpResponse("<html>", { status: 200 })],
    ["401 with another code", () => errorResponse(401, "SOMETHING")],
    ["401 with broken body", () => new HttpResponse("nope", { status: 401 })],
  ])(
    "treats %s as an error with a manual retry, not as anonymous",
    async (_n, respond) => {
      let gets = 0;
      server.use(
        http.get(SESSION, () => {
          gets += 1;
          return gets === 1 ? respond() : ok("a@example.com");
        }),
      );
      const user = userEvent.setup();
      renderApp("/");
      const alert = await screen.findByRole("alert");
      expect(alert.textContent).toContain("无法确认登录状态");
      expect(screen.queryByRole("heading", { name: "登录" })).toBeNull();
      expect(gets).toBe(1);
      await user.click(screen.getByRole("button", { name: "重试" }));
      await screen.findByText("a@example.com");
      expect(gets).toBe(2);
    },
  );
});

describe("replacing an existing session query", () => {
  it("cancels A before starting B; a late stale 200 cannot revive the identity", async () => {
    const gets = deferredSessionGets();
    const client = createAppQueryClient();
    seed(client, "old@example.com");
    const a = client.refetchQueries({ queryKey: sessionKey });
    await waitFor(() => expect(gets).toHaveLength(1));
    const b = replaceSessionQuery(client);
    await waitFor(() => expect(gets).toHaveLength(2));
    expect(gets[0]?.signal.aborted).toBe(true);
    expect(gets[1]?.signal.aborted).toBe(false);
    gets[1]?.gate.resolve(anonymous());
    await b;
    expect(client.getQueryData(sessionKey)).toBeNull();
    // A 忽略 abort，迟到返回旧 200。
    await release(gets[0], ok("old@example.com"));
    await a;
    expect(client.getQueryData(sessionKey)).toBeNull();
    expect(gets).toHaveLength(2);
  });

  it("does the same when A is the uncached first load", async () => {
    const gets = deferredSessionGets();
    const client = renderApp("/");
    await waitFor(() => expect(gets).toHaveLength(1));
    const b = replaceSessionQuery(client);
    await waitFor(() => expect(gets).toHaveLength(2));
    expect(gets[0]?.signal.aborted).toBe(true);
    gets[1]?.gate.resolve(anonymous());
    await b;
    await release(gets[0], ok("late@example.com"));
    await screen.findByRole("heading", { name: "登录" });
    expect(client.getQueryData(sessionKey)).toBeNull();
    expect(gets).toHaveLength(2);
  });
});

/** 不渲染 App（它的观察者会换成真实 queryFn），只挂 mutation 驱动。 */
function renderDriverOnly(client: QueryClient) {
  const actionRef: { current: ((kind: string) => void) | undefined } = {
    current: undefined,
  };
  render(
    <QueryClientProvider client={client}>
      <Driver actionRef={actionRef} />
    </QueryClientProvider>,
  );
  return (kind: string) => act(() => actionRef.current?.(kind));
}

describe.each([
  { name: "with a cached session", cached: true },
  { name: "without a cache", cached: false },
])("queryFn that ignores AbortSignal, $name", ({ cached }) => {
  const unsubscribe: Array<() => void> = [];
  afterEach(() => {
    unsubscribe.splice(0).forEach((fn) => fn());
  });

  /**
   * 发起首个会话查询，吞掉取消产生的拒绝。
   * 无缓存时必须有观察者：refetchQueries 不会重新请求没有数据也没有观察者的查询。
   */
  const start = (client: QueryClient) => {
    if (cached) {
      return client
        .refetchQueries({ queryKey: sessionKey })
        .catch(() => undefined);
    }
    const observer = new QueryObserver(client, { queryKey: sessionKey });
    unsubscribe.push(observer.subscribe(() => undefined));
    return Promise.resolve();
  };
  const again = (client: QueryClient) =>
    client.refetchQueries({ queryKey: sessionKey }).catch(() => undefined);

  function setup() {
    const client = createAppQueryClient();
    // 先装 queryFn 默认值，再写缓存，否则已创建的 query 不会带上它。
    const calls = controlledSession(client);
    if (cached) client.setQueryData(sessionKey, session("old@example.com"));
    return { client, calls };
  }

  it("replaceSessionQuery: an old result returning after the new one cannot overwrite it", async () => {
    const { client, calls } = setup();
    const a = start(client);
    await waitFor(() => expect(calls).toHaveLength(1));
    const b = replaceSessionQuery(client);
    await waitFor(() => expect(calls).toHaveLength(2));
    expect(calls[0]?.signal.aborted).toBe(true);
    await releaseCall(calls[1], null);
    await b;
    expect(client.getQueryData(sessionKey)).toBeNull();
    await releaseCall(calls[0], session("old@example.com"));
    await a;
    expect(client.getQueryData(sessionKey)).toBeNull();
    expect(calls).toHaveLength(2);
  });

  it.each([
    { kind: "login", url: LOGIN, status: 200 },
    { kind: "register", url: REGISTER, status: 201 },
    { kind: "logout", url: LOGOUT, status: 204 },
  ])(
    "$kind: old queries returning after the write cannot overwrite it",
    async ({ kind, url, status }) => {
      const post = deferred<Response>();
      server.use(http.post(url, () => post.promise));
      const { client, calls } = setup();
      const drive = renderDriverOnly(client);
      const a = start(client);
      await waitFor(() => expect(calls).toHaveLength(1));
      drive(kind);
      await waitFor(() => expect(calls[0]?.signal.aborted).toBe(true));
      const b = again(client);
      await waitFor(() => expect(calls).toHaveLength(2));
      post.resolve(
        kind === "logout"
          ? new HttpResponse(null, { status })
          : HttpResponse.json(sessionBody("new@example.com", FUTURE), {
              status,
            }),
      );
      const expected = kind === "logout" ? null : "new@example.com";
      await waitFor(() => {
        const data = client.getQueryData<AuthSession | null>(sessionKey);
        expect(data === null ? null : data?.user.email).toBe(expected);
      });
      expect(calls[1]?.signal.aborted).toBe(true);
      await releaseCall(calls[0], session("old@example.com"));
      await releaseCall(calls[1], session("older@example.com"));
      await Promise.all([a, b]);
      const data = client.getQueryData<AuthSession | null>(sessionKey);
      expect(data === null ? null : data?.user.email).toBe(expected);
      expect(calls).toHaveLength(2);
    },
  );
});

describe.each([
  { kind: "login", url: LOGIN, status: 200 },
  { kind: "register", url: REGISTER, status: 201 },
])("$kind races with an old session GET", ({ kind, url, status }) => {
  it("cancels before the POST and again before writing the new session", async () => {
    const gets = deferredSessionGets();
    const post = deferred<Response>();
    let posts = 0;
    server.use(
      http.post(url, () => {
        posts += 1;
        return post.promise;
      }),
    );
    const client = createAppQueryClient();
    seed(client, "old@example.com");
    const drive = renderDriver(client, "/login");
    void client.refetchQueries({ queryKey: sessionKey });
    await waitFor(() => expect(gets).toHaveLength(1));

    drive(kind);
    await waitFor(() => expect(posts).toBe(1));
    expect(gets[0]?.signal.aborted).toBe(true);

    // POST 在途时又发出一个新的 GET（B）；成功后写缓存前必须再次取消它。
    void client.refetchQueries({ queryKey: sessionKey });
    await waitFor(() => expect(gets).toHaveLength(2));
    expect(gets[1]?.signal.aborted).toBe(false);

    post.resolve(
      HttpResponse.json(sessionBody("new@example.com", FUTURE), { status }),
    );
    await waitFor(() =>
      expect(client.getQueryData<AuthSession>(sessionKey)?.user.email).toBe(
        "new@example.com",
      ),
    );
    expect(gets[1]?.signal.aborted).toBe(true);

    await release(gets[0], ok("old@example.com"));
    await release(gets[1], ok("older@example.com"));
    expect(client.getQueryData<AuthSession>(sessionKey)?.user.email).toBe(
      "new@example.com",
    );
    expect(posts).toBe(1);
    expect(gets).toHaveLength(2);
  });
});

describe("logout races with old session GETs", () => {
  it("aborts A in onMutate and B again before writing anonymous", async () => {
    const gets = deferredSessionGets();
    const post = deferred<Response>();
    let posts = 0;
    server.use(
      http.post(LOGOUT, () => {
        posts += 1;
        return post.promise;
      }),
    );
    const client = createAppQueryClient();
    seed(client, "old@example.com");
    const drive = renderDriver(client, "/");
    void client.refetchQueries({ queryKey: sessionKey });
    await waitFor(() => expect(gets).toHaveLength(1));

    drive("logout");
    await waitFor(() => expect(posts).toBe(1));
    expect(gets[0]?.signal.aborted).toBe(true);

    void client.refetchQueries({ queryKey: sessionKey });
    await waitFor(() => expect(gets).toHaveLength(2));

    post.resolve(new HttpResponse(null, { status: 204 }));
    await waitFor(() => expect(client.getQueryData(sessionKey)).toBeNull());
    expect(gets[1]?.signal.aborted).toBe(true);

    await release(gets[0], ok("old@example.com"));
    await release(gets[1], ok("old@example.com"));
    expect(client.getQueryData(sessionKey)).toBeNull();
    await screen.findByRole("heading", { name: "登录" });
    expect(gets).toHaveLength(2);
  });
});

describe("expires_at timer", () => {
  const START = Date.parse("2030-01-01T00:00:00Z");
  const iso = (offsetMs: number) => new Date(START + offsetMs).toISOString();
  const hidden = (value: "hidden" | "visible") =>
    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      get: () => value,
    });

  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "Date"] });
    vi.setSystemTime(START);
    notifyManager.setScheduler((cb) => queueMicrotask(cb));
  });
  afterEach(() => {
    vi.useRealTimers();
    notifyManager.setScheduler((cb) => setTimeout(cb, 0));
    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      get: () => "visible",
    });
  });

  const advance = (ms: number) => act(() => vi.advanceTimersByTimeAsync(ms));

  function mount(expiresInMs: number, email = "a@example.com") {
    const client = createAppQueryClient();
    seed(client, email, iso(expiresInMs));
    renderApp("/", client);
    return client;
  }

  it("keeps the identity until expiry, then hides it and refetches exactly once", async () => {
    const gets = deferredSessionGets();
    mount(60_000);
    expect(screen.getByText("a@example.com")).toBeTruthy();
    await advance(59_000);
    expect(gets).toHaveLength(0);
    expect(screen.getByText("a@example.com")).toBeTruthy();
    await advance(1_000);
    await until(() => expect(gets).toHaveLength(1));
    expect(screen.queryByText("a@example.com")).toBeNull();
    expect(screen.getByRole("status")).toBeTruthy();
    expect(gets[0]?.signal.aborted).toBe(false);
    gets[0]?.gate.resolve(ok("a@example.com", iso(DAY)));
    await until(() => screen.getByText("a@example.com"));
    await advance(60_000);
    expect(gets).toHaveLength(1);
  });

  it("goes anonymous on 401 after expiry", async () => {
    const gets = deferredSessionGets();
    mount(1_000);
    await advance(1_000);
    await until(() => expect(gets).toHaveLength(1));
    gets[0]?.gate.resolve(anonymous());
    await until(() => screen.getByRole("heading", { name: "登录" }));
  });

  it("shows an error after a service failure and never loops on time or visibility", async () => {
    let gets = 0;
    server.use(
      http.get(SESSION, () => ((gets += 1), errorResponse(503, "UNAVAILABLE"))),
    );
    mount(1_000);
    await advance(1_000);
    await until(() => screen.getByRole("alert"));
    expect(gets).toBe(1);
    await advance(10 * 60_000);
    document.dispatchEvent(new Event("visibilitychange"));
    document.dispatchEvent(new Event("visibilitychange"));
    await advance(1_000);
    expect(gets).toBe(1);
    expect(screen.queryByText("a@example.com")).toBeNull();
    server.use(
      http.get(SESSION, () => ((gets += 1), ok("a@example.com", iso(DAY)))),
    );
    act(() => fireEvent.click(screen.getByRole("button", { name: "重试" })));
    await until(() => screen.getByText("a@example.com"));
    expect(gets).toBe(2);
  });

  it("does not overflow setTimeout for a 30 day session", async () => {
    const gets = deferredSessionGets();
    mount(30 * DAY);
    await advance(2_147_483_647);
    expect(gets).toHaveLength(0);
    expect(screen.getByText("a@example.com")).toBeTruthy();
    await advance(30 * DAY - 2_147_483_647 - 1);
    expect(gets).toHaveLength(0);
    await advance(1);
    await until(() => expect(gets).toHaveLength(1));
  });

  it("hides an already expired cached identity on first render and refetches once", async () => {
    const gets = deferredSessionGets();
    mount(-5_000);
    expect(screen.queryByText("a@example.com")).toBeNull();
    await advance(0);
    await until(() => expect(gets).toHaveLength(1));
    expect(screen.queryByText("a@example.com")).toBeNull();
  });

  it("catches up on visibility when the timer was throttled, once per expires_at", async () => {
    const gets = deferredSessionGets();
    mount(60_000);
    hidden("hidden");
    vi.setSystemTime(START + 120_000);
    document.dispatchEvent(new Event("visibilitychange"));
    expect(gets).toHaveLength(0);
    hidden("visible");
    document.dispatchEvent(new Event("visibilitychange"));
    await until(() => expect(gets).toHaveLength(1));
    document.dispatchEvent(new Event("visibilitychange"));
    document.dispatchEvent(new Event("visibilitychange"));
    await advance(1_000);
    expect(gets).toHaveLength(1);
    expect(screen.queryByText("a@example.com")).toBeNull();
  });

  it("does not refetch while a write is pending and re-checks afterwards", async () => {
    const gets = deferredSessionGets();
    const post = deferred<Response>();
    server.use(http.post(LOGOUT, () => post.promise));
    mount(60_000);
    act(() => fireEvent.click(screen.getByRole("button", { name: "登出" })));
    await until(() =>
      expect(screen.getByRole("button", { name: "正在退出…" })).toBeTruthy(),
    );
    // POST 释放前：已经到期，身份必须隐藏，且仍然没有 GET。
    await advance(61_000);
    expect(screen.queryByText("a@example.com")).toBeNull();
    expect(gets).toHaveLength(0);
    post.resolve(errorResponse(500, "INTERNAL"));
    await until(() =>
      expect(screen.queryByRole("button", { name: "正在退出…" })).toBeNull(),
    );
    await advance(0);
    await until(() => expect(gets).toHaveLength(1));
    expect(screen.queryByText("a@example.com")).toBeNull();
    gets[0]?.gate.resolve(ok("a@example.com", iso(DAY)));
    await until(() => screen.getByText("a@example.com"));
    expect(gets).toHaveLength(1);
  });

  it("never shows an expired session returned by the first GET", async () => {
    const gets = deferredSessionGets();
    renderApp("/");
    const watch = watchIdentity("a@example.com");
    await until(() => expect(gets).toHaveLength(1));
    await release(gets[0], ok("a@example.com", iso(-5_000)));
    await advance(0);
    await until(() => expect(gets).toHaveLength(2));
    expect(watch.seen()).toBe(false);
    expect(screen.queryByText("a@example.com")).toBeNull();
    await release(gets[1], ok("a@example.com", iso(DAY)));
    await until(() => screen.getByText("a@example.com"));
    expect(gets).toHaveLength(2);
    watch.stop();
  });

  it("never shows an expired session written by setQueryData after mount", async () => {
    const gets = deferredSessionGets();
    const client = mount(DAY, "first@example.com");
    expect(screen.getByText("first@example.com")).toBeTruthy();
    const watch = watchIdentity("second@example.com");
    await act(async () => {
      client.setQueryData(sessionKey, session("second@example.com", iso(-1)));
    });
    expect(screen.queryByText("second@example.com")).toBeNull();
    expect(screen.queryByText("first@example.com")).toBeNull();
    await advance(0);
    await until(() => expect(gets).toHaveLength(1));
    expect(watch.seen()).toBe(false);
    expect(screen.queryByText("second@example.com")).toBeNull();
    watch.stop();
  });

  it("stops the old timer and visibility listener after the session changes", async () => {
    const gets = deferredSessionGets();
    const client = mount(60_000);
    act(() => {
      client.setQueryData(sessionKey, session("b@example.com", iso(DAY)));
    });
    await until(() => screen.getByText("b@example.com"));
    await advance(61_000);
    vi.setSystemTime(START + 61_000);
    document.dispatchEvent(new Event("visibilitychange"));
    await advance(0);
    expect(gets).toHaveLength(0);
    expect(screen.getByText("b@example.com")).toBeTruthy();
  });

  it("stops the timer and visibility listener after unmount", async () => {
    const gets = deferredSessionGets();
    const addListener = vi.spyOn(document, "addEventListener");
    const removeListener = vi.spyOn(document, "removeEventListener");
    const setTimer = vi.spyOn(globalThis, "setTimeout");
    const clearTimer = vi.spyOn(globalThis, "clearTimeout");
    mount(60_000);
    // 只取本组件注册的 visibilitychange 监听与到期（60s）定时器，不依赖 Query 自身的定时器。
    const listener = addListener.mock.calls.find(
      ([type]) => type === "visibilitychange",
    )?.[1];
    const expiryIndex = setTimer.mock.calls.findIndex(
      ([, delay]) => delay === 60_000,
    );
    expect(listener).toBeDefined();
    expect(expiryIndex).toBeGreaterThanOrEqual(0);
    const timerId = setTimer.mock.results[expiryIndex]?.value;
    expect(timerId).toBeDefined();
    cleanup();
    expect(removeListener).toHaveBeenCalledWith("visibilitychange", listener);
    expect(clearTimer).toHaveBeenCalledWith(timerId);
    vi.setSystemTime(START + 120_000);
    document.dispatchEvent(new Event("visibilitychange"));
    await advance(120_000);
    expect(gets).toHaveLength(0);
  });

  it("a stale response after a successful logout cannot revive the identity", async () => {
    const gets = deferredSessionGets();
    server.use(
      http.post(LOGOUT, () => new HttpResponse(null, { status: 204 })),
    );
    const client = mount(60_000);
    act(() => fireEvent.click(screen.getByRole("button", { name: "登出" })));
    await until(() => screen.getByRole("heading", { name: "登录" }));
    await advance(61_000);
    expect(gets).toHaveLength(0);
    expect(client.getQueryData(sessionKey)).toBeNull();
  });
});
