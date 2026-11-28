// SPDX-License-Identifier: MIT
//
// Rough frame-time probe: full demo scan (~24k dirs), then
// zoom in/out and hover while sampling requestAnimationFrame intervals.
// Headless numbers are indicative only; printed, loosely asserted.

import { expect, test } from "@playwright/test";

test.use({ viewport: { width: 1440, height: 900 } });

test("treemap frame times", async ({ page }) => {
  await page.goto("/viewer.html?demo");
  await page.getByRole("button", { name: /^Home/ }).click();

  // sample during streaming (tweens + relayouts)
  await page.waitForTimeout(2500);
  const streaming = await sample(page, 2000);
  await expect(page.getByRole("button", { name: "Scan again" })).toBeVisible({ timeout: 150_000 });
  await page.waitForTimeout(800);

  // zoom animations via list double-click / Escape
  const zoomP = sample(page, 2600);
  for (let i = 0; i < 3; i++) {
    await page.locator(".list .row.dir").first().dblclick();
    await page.waitForTimeout(380);
    await page.keyboard.press("Escape");
    await page.waitForTimeout(380);
  }
  const zoom = await zoomP;

  // hover sweep (overlay-only redraws)
  const hoverP = sample(page, 1500);
  for (let x = 20; x < 1000; x += 12) await page.mouse.move(x, 120 + (x % 600));
  const hover = await hoverP;
  console.log(JSON.stringify({ streaming, zoom, hover }));
  expect(hover.p50).toBeLessThan(40);
});

async function sample(page: import("@playwright/test").Page, ms: number) {
  return page.evaluate(
    (ms) =>
      new Promise<{ frames: number; fps: number; p50: number; p95: number; worst: number }>((res) => {
        const d: number[] = [];
        let last = performance.now();
        const t0 = last;
        const f = (now: number) => {
          d.push(now - last);
          last = now;
          if (now - t0 < ms) requestAnimationFrame(f);
          else {
            d.sort((a, b) => a - b);
            const q = (p: number) => Math.round(d[Math.min(d.length - 1, Math.floor(d.length * p))] * 10) / 10;
            res({ frames: d.length, fps: Math.round((d.length / (now - t0)) * 1000), p50: q(0.5), p95: q(0.95), worst: q(1) });
          }
        };
        requestAnimationFrame(f);
      }),
    ms,
  );
}
