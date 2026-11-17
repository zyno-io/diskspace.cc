import { defineConfig } from "vitest/config";
import preact from "@preact/preset-vite";
import { resolve } from "node:path";

// Multi-page build: landing (/), viewer (/s/<sid>), GUI launcher.
// The Go relay serves dist/ and maps /s/* → viewer.html.
// Dev only: mirror the relay's routing so /s/<sid> and /launcher load the
// right page (Vite's SPA fallback would otherwise serve index.html).
const devRoutes = {
  name: "diskspace-dev-routes",
  configureServer(server: { middlewares: { use: (fn: (req: { url?: string }, res: unknown, next: () => void) => void) => void } }) {
    server.middlewares.use((req, _res, next) => {
      const url = req.url ?? "";
      if (/^\/s\/[^/?#]+(\?.*)?$/.test(url)) req.url = "/viewer.html" + (url.includes("?") ? url.slice(url.indexOf("?")) : "");
      else if (/^\/demo(\?.*)?$/.test(url)) req.url = "/viewer.html" + (url.includes("?") ? url.slice(url.indexOf("?")) : "");
      else if (/^\/launcher(\?.*)?$/.test(url)) req.url = "/launcher.html" + (url.includes("?") ? url.slice(url.indexOf("?")) : "");
      next();
    });
  },
};

export default defineConfig({
  plugins: [preact(), devRoutes],
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
