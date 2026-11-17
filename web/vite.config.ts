import { defineConfig } from "vitest/config";
import preact from "@preact/preset-vite";
import { resolve } from "node:path";

// Multi-page build: landing (/), viewer (/s/<sid>), GUI launcher.
// The Go relay serves dist/ and maps /s/* → viewer.html.
export default defineConfig({
  plugins: [preact()],
  build: {
    outDir: "dist",
    emptyOutDir: false,
    assetsDir: "assets",
    rollupOptions: {
      input: {
        index: resolve(__dirname, "index.html"),
        viewer: resolve(__dirname, "viewer.html"),
        launcher: resolve(__dirname, "launcher.html"),
      },
    },
  },
  worker: { format: "es" },
  server: {
    proxy: {
      "/v1": { target: "http://127.0.0.1:8080", ws: true },
      "/run": "http://127.0.0.1:8080",
      "/dl": "http://127.0.0.1:8080",
    },
  },
  test: { include: ["test/**/*.test.ts"], environment: "node" },
});
