import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { fileURLToPath } from "node:url";

// The build output is emitted straight into the Go embed directory so the
// binary ships the UI (PLAN §1.3). The dev server proxies the API to the Go
// backend on :28734.
export default defineConfig({
  plugins: [react()],
  // Absolute base: the app is served at the domain root, and the SPA has
  // multi-segment routes (e.g. /servers/:id/containers/:cid). A relative base
  // ("./") breaks asset loading on a hard-load/refresh of those deep routes.
  base: "/",
  build: {
    outDir: fileURLToPath(new URL("../internal/web/dist", import.meta.url)),
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    proxy: {
      "/api": "http://127.0.0.1:28734",
      "/healthz": "http://127.0.0.1:28734",
    },
  },
  test: { environment: "jsdom", include: ["src/**/*.test.ts", "src/**/*.test.tsx"] },
});
