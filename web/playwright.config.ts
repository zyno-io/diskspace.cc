// SPDX-License-Identifier: MIT
import { defineConfig } from "@playwright/test";

// Visual review harness: run `npx vite --port 5199` first, then `npx playwright test`.
export default defineConfig({
  testDir: "e2e",
  timeout: 180_000,
  expect: { timeout: 60_000 },
  workers: 1,
  reporter: "line",
  use: { baseURL: process.env.BASE_URL ?? "http://localhost:5199", browserName: (process.env.E2E_BROWSER as "chromium" | "webkit" | "firefox" | undefined) ?? "chromium" },
});
