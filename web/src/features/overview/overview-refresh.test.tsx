import { afterEach, describe, expect, it } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/test/render";
import { http, HttpResponse, server, sessionBody } from "@/test/server";
import { DEFAULT_REFRESH_MS, setRefreshMs } from "./refreshInterval";

const WS = "0198f2b0-0000-7000-8000-0000000000aa";
const BASE = `/api/v1/workspaces/${WS}`;
const FIXED =
  "/?tz=UTC&range=custom&start=2026-10-28T00:00:00Z&end=2026-10-28T02:00:00Z";

function useBackend() {
  const calls: URL[] = [];
  const empty = { request_count: "0" };
  server.use(
    http.get("/api/v1/auth/session", () =>
      HttpResponse.json(sessionBody("a@example.com", "2999-01-01T00:00:00Z")),
    ),
    http.get("/api/v1/workspaces", () =>
      HttpResponse.json([{ workspace_id: WS, name: "Alpha" }]),
    ),
    http.get(`${BASE}/usage/options`, () =>
      HttpResponse.json({ providers: [], models: [] }),
    ),
    http.get(`${BASE}/projects`, () => HttpResponse.json([])),
    http.get(`${BASE}/usage/aggregate`, ({ request }) => {
      calls.push(new URL(request.url));
      return HttpResponse.json({ range: {}, points: [], totals: empty });
    }),
  );
  return calls;
}

afterEach(() => {
  setRefreshMs(DEFAULT_REFRESH_MS);
  window.localStorage.clear();
});

describe("overview refresh interval selector", () => {
  it("offers 1 / 5 / 10 seconds, defaults to 5 and remembers the choice", async () => {
    useBackend();
    renderApp(FIXED);
    const group = await screen.findByRole("group", { name: "刷新间隔" });
    const buttons = within(group).getAllByRole("button");
    expect(buttons.map((b) => b.textContent)).toEqual([
      "1 秒",
      "5 秒",
      "10 秒",
    ]);
    expect(buttons.map((b) => b.getAttribute("aria-pressed"))).toEqual([
      "false",
      "true",
      "false",
    ]);

    await userEvent.click(within(group).getByRole("button", { name: "10 秒" }));
    expect(
      within(group)
        .getByRole("button", { name: "10 秒" })
        .getAttribute("aria-pressed"),
    ).toBe("true");
    expect(window.localStorage.getItem("tokenpivot.overview.refreshMs")).toBe(
      "10000",
    );
  });

  it("polls faster after switching to 1 second", async () => {
    const calls = useBackend();
    renderApp(FIXED);
    const group = await screen.findByRole("group", { name: "刷新间隔" });
    await waitFor(() => expect(calls.length).toBeGreaterThan(0));
    await userEvent.click(within(group).getByRole("button", { name: "1 秒" }));
    const before = calls.length;
    // 默认 5 秒不会在 2.5 秒内再次请求，因此新增的请求只能来自 1 秒轮询。
    await waitFor(() => expect(calls.length).toBeGreaterThan(before), {
      timeout: 2500,
    });
  });
});
