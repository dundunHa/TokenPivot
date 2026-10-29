import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router";
import { App } from "@/App";
import { createAppQueryClient } from "@/features/auth/queries";
import "@/index.css";

const root = document.getElementById("root");
if (!root) throw new Error("Missing root element");
const queryClient = createAppQueryClient();
createRoot(root).render(
  <StrictMode>
    <BrowserRouter>
      <QueryClientProvider client={queryClient}>
        <App />
      </QueryClientProvider>
    </BrowserRouter>
  </StrictMode>,
);
