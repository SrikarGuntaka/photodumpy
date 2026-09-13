import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

// In development the UI runs on Vite's server and proxies /api to the Go API,
// so the browser still sees ONE origin -- the same arrangement as production,
// where the API serves the built files itself. Nothing in the app needs to
// know which of the two it is running under.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": { target: "http://localhost:8080", changeOrigin: false },
    },
  },
  build: {
    outDir: "dist",
    // Content-hashed filenames under assets/ are what let the Go server cache
    // them as immutable. See internal/api/web.go.
    assetsDir: "assets",
    sourcemap: false,
  },
  test: {
    environment: "jsdom",
    globals: true,
    include: ["src/**/*.test.{ts,tsx}"],
  },
});
