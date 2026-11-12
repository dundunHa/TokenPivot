import { afterEach, describe, expect, it } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/test/render";
import { http, HttpResponse, server, sessionBody } from "@/test/server";
import { DEFAULT_REFRESH_MS, setRefreshMs } from "./refreshInterval";

const WS = "0198f2b0-0000-7000-8000-0000000000aa";
const OLD = "01a1afc1-fbe4-789f-8fce-633b6f6ca311";
const NEW = "01a1afd5-df40-72ba-944d-08aeb3ea50cd";
const BASE = `/api/v1/workspaces/${WS}`;
const FIXED =
  "/?tz=UTC&range=custom&start=2026-10-28T00:00:00Z&end=2026-10-28T02:00:00Z";

const metric = (v: string) => ({
  value: v,
  known_count: "1",
  estimated_count: "0",
  missing_count: "0",
});
const stats = (t: string) => ({
  request_count: "1",
  input_tokens: metric(t),
  output_tokens: metric("0"),
  total_tokens: metric(t),
});

afterEach(() => {
  setRefreshMs(DEFAULT_REFRESH_MS);
  window.localStorage.clear();
});

describe("project names in the ranking", () => {
  it("replaces a raw project id with the name once a project created after page load appears", async () => {
    // 插件上报会自动创建项目：页面打开之后才出现的项目，名称必须在不刷新页面的情况下补上。
    let projects = [{ project_id: OLD, name: "howToSay", archived_at: null }];
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
      http.get(`${BASE}/projects`, () => HttpResponse.json(projects)),
      http.get(`${BASE}/usage/aggregate`, () =>
        HttpResponse.json({
          range: {
            start: "2026-10-28T00:00:00Z",
            end: "2026-10-28T02:00:00Z",
            as_of: "2026-10-28T02:00:00Z",
            retention_cutoff: "2025-10-28T00:00:00Z",
            timezone: "UTC",
            granularity: "hour",
          },
          totals: stats("100"),
          points: [],
        }),
      ),
      http.get(`${BASE}/usage/rankings`, () =>
        HttpResponse.json({
          group_by: "project",
          overall: stats("100"),
          groups: [OLD, NEW].map((id) => ({
            key: { project_id: id },
            share: 0.5,
            stats: stats("50"),
          })),
          other: null,
          next_cursor: null,
        }),
      ),
    );
    renderApp(FIXED);
    await screen.findAllByText("howToSay");
    expect(screen.getAllByText(NEW).length).toBeGreaterThan(0);

    projects = [
      ...projects,
      { project_id: NEW, name: "agent-demo", archived_at: null },
    ];
    await userEvent.click(await screen.findByRole("button", { name: "1 秒" }));
    await waitFor(() => expect(screen.queryByText(NEW)).toBeNull(), {
      timeout: 5000,
    });
    expect(screen.getAllByText("agent-demo").length).toBeGreaterThan(0);
  });
});
