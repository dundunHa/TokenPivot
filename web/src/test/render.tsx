import { render } from "@testing-library/react";
import { QueryClientProvider, type QueryClient } from "@tanstack/react-query";
import { MemoryRouter, useLocation, useNavigationType } from "react-router";
import { App } from "@/App";
import { createAppQueryClient } from "@/features/auth/queries";

// 测试专用探针组件与 renderApp 同文件，无需热更新。
// eslint-disable-next-line react-refresh/only-export-components
function LocationProbe() {
  const location = useLocation();
  const type = useNavigationType();
  return (
    <p data-testid="location">
      {location.pathname}|{type}
    </p>
  );
}

/** 每个用例使用独立 QueryClient 与 MemoryRouter。 */
export function renderApp(
  path: string,
  client: QueryClient = createAppQueryClient(),
) {
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <LocationProbe />
        <App />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return client;
}
