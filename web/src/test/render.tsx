import { render } from "@testing-library/react";
import { QueryClientProvider, type QueryClient } from "@tanstack/react-query";
import { MemoryRouter, useLocation, useNavigationType } from "react-router";
import { App } from "@/App";
import { createAppQueryClient } from "@/features/auth/queries";

/** 每个用例使用独立 QueryClient 与 MemoryRouter。 */
export function renderApp(
  path: string,
  client: QueryClient = createAppQueryClient(),
) {
  // 测试专用探针，定义在函数内，不作为模块导出组件。
  function LocationProbe() {
    const location = useLocation();
    const type = useNavigationType();
    return (
      <p data-testid="location">
        {location.pathname}|{type}
      </p>
    );
  }

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
