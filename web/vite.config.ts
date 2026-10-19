/// <reference types="vitest/config" />
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// Dev server proxies match cmd/webserve: /api → scorer gateway, /auth → dev-issuer, /summarizer → summarizer.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    port: 8500,
    proxy: {
      "/api": { target: "http://127.0.0.1:8511", rewrite: (p) => p.replace(/^\/api/, "") },
      "/auth": { target: "http://127.0.0.1:8515", rewrite: (p) => p.replace(/^\/auth/, "") },
      "/summarizer": { target: "http://127.0.0.1:8513", rewrite: (p) => p.replace(/^\/summarizer/, "") },
    },
  },
  build: {
    sourcemap: true,
    outDir: "dist",
    // The CSP is font-src 'self' (cmd/webserve): fonts must stay files, never data: URIs.
    assetsInlineLimit: (file) => (/\.(woff2?|ttf|otf)$/.test(file) ? false : undefined),
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    include: ["src/**/*.test.{ts,tsx}"],
    coverage: { provider: "v8", include: ["src/**"], exclude: ["src/api/schema.d.ts", "src/test/**"] },
  },
});
