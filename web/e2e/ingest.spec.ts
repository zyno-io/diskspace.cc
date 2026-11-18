// SPDX-License-Identifier: MIT
//
// Main-thread responsiveness while a scan streams in: a 50 ms setTimeout drift
// probe runs from page load until the scan finishes; the event loop must stay
// free (p95 drift < 50 ms, max < 200 ms) and the scan must complete.
//
//  - "demo" runs against the mock session (no agent, no filesystem access).
//  - "real agent" scans a synthetic sparse tree built with scripts/mktree
//    through internal/testharness. It walks a filesystem, so it only runs when
//    DS_E2E_AGENT=1 (run it on a test machine, never on a workstation):
//
//      DS_E2E_AGENT=1 [DS_E2E_DIRS=200000 DS_E2E_FILES=1000000] \
//        [E2E_BROWSER=webkit] npx playwright test e2e/ingest.spec.ts -g "real agent"
//
//    Needs a built web/dist (npm run build) because the harness embeds it,
//    and the Go toolchain on PATH. Runs from the web/ directory.

import { expect, test, type Page } from "@playwright/test";
import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { createInterface } from "node:readline";

test.use({ viewport: { width: 1440, height: 900 } });

const P95_MAX_MS = 50;
const MAX_MS = 200;

async function installProbe(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const w = window as unknown as { __lag: number[]; __lagStop: () => void };
    w.__lag = [];
    let last = performance.now();
    const id = setInterval(() => {
      const now = performance.now();
      w.__lag.push(Math.max(0, now - last - 50));
      last = now;
    }, 50);
    w.__lagStop = () => clearInterval(id);
  });
}

async function readLag(page: Page) {
  return page.evaluate(() => {
    const w = window as unknown as { __lag: number[]; __lagStop: () => void };
    w.__lagStop();
    const d = [...w.__lag].sort((a, b) => a - b);
    const q = (p: number) => Math.round(d[Math.min(d.length - 1, Math.floor(d.length * p))] * 10) / 10;
    return { samples: d.length, p50: q(0.5), p95: q(0.95), p99: q(0.99), max: Math.round(d[d.length - 1] * 10) / 10 };
  });
}

test("demo scan keeps the event loop free", async ({ page }) => {
  await installProbe(page);
  await page.goto("/viewer.html?demo");
  await page.getByRole("button", { name: /^Home/ }).click();
  await expect(page.getByRole("button", { name: "Scan again" })).toBeVisible({ timeout: 150_000 });
  const lag = await readLag(page);
  console.log("demo lag", JSON.stringify(lag));
  expect(lag.samples).toBeGreaterThan(20);
  expect(lag.p95).toBeLessThan(P95_MAX_MS);
  expect(lag.max).toBeLessThan(MAX_MS);
});

test.describe("real agent", () => {
  test.skip(process.env.DS_E2E_AGENT !== "1", "set DS_E2E_AGENT=1 to scan a synthetic tree (test machines only)");
  test.setTimeout(600_000);

  let tree = "";
  let harness: ChildProcess | null = null;

  test.afterAll(() => {
    harness?.kill("SIGTERM");
    if (tree) rmSync(tree, { recursive: true, force: true });
  });

  test("scan of a large synthetic tree keeps the event loop free", async ({ page }) => {
    const repo = resolve(__dirname, "../..");
    tree = mkdtempSync(join(tmpdir(), "ds-e2e-tree-"));
    const dirs = process.env.DS_E2E_DIRS ?? "200000";
    const files = process.env.DS_E2E_FILES ?? "1000000";
    const mk = spawnSync("go", ["run", "./scripts/mktree", "-sparse", "-dirs", dirs, "-files", files, "-seed", "7", tree], { cwd: repo, stdio: "inherit" });
    expect(mk.status).toBe(0);

    harness = spawn("go", ["run", "./internal/testharness", "-tree", tree, "-listen", "127.0.0.1:0"], { cwd: repo, stdio: ["ignore", "pipe", "inherit"] });
    const url = await new Promise<string>((res, rej) => {
      const rl = createInterface({ input: harness!.stdout! });
      rl.once("line", res);
      harness!.once("exit", (c) => rej(new Error(`testharness exited early (${c})`)));
      setTimeout(() => rej(new Error("testharness did not print a link")), 120_000);
    });
    expect(url).toMatch(/^http:\/\/127\.0\.0\.1:\d+\/s\//);

    await installProbe(page);
    await page.goto(url);
    await expect(page.getByRole("button", { name: "Scan again" })).toBeVisible({ timeout: 500_000 });
    const lag = await readLag(page);
    console.log(`real agent lag (${dirs} dirs, ${files} files)`, JSON.stringify(lag));
    expect(lag.p95).toBeLessThan(P95_MAX_MS);
    expect(lag.max).toBeLessThan(MAX_MS);
  });
});
