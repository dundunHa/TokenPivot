import { fileURLToPath, URL } from "node:url";
import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

function apiTarget(): string {
  const value = process.env.TOKENPIVOT_DEV_API_TARGET;
  if (value === undefined) return "http://127.0.0.1:8080";
  try {
    const url = new URL(value);
    if (
      !/^https?:\/\/[^/?#@\\\s]+\/?$/.test(value) ||
      (url.protocol !== "http:" && url.protocol !== "https:") ||
      url.username !== "" ||
      url.password !== "" ||
      url.pathname !== "/" ||
      url.search !== "" ||
      url.hash !== ""
    )
      throw new Error();
    return url.origin;
  } catch {
    throw new Error(
      "TOKENPIVOT_DEV_API_TARGET must be an HTTP(S) origin without credentials, path, query or fragment",
    );
  }
}

export default defineConfig({
  base: "/",
  plugins: [react(), tailwindcss()],
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  build: { sourcemap: false },
  test: {
    environment: "jsdom",
    include: ["src/**/*.test.{ts,tsx}"],
    setupFiles: ["./src/test/setup.ts"],
  },
  server: {
    host: "127.0.0.1",
    port: 5173,
    strictPort: true,
    proxy: {
      "^/api(?:/|[?]|$)": {
        target: apiTarget(),
        changeOrigin: false,
        ws: false,
      },
    },
  },
});
