// SPDX-License-Identifier: MIT
//
// Pause / resume and the update banner against the demo (mock agent, no real
// scanner). Screenshots go to $SHOTS_DIR (default e2e/__shots__).

import { expect, test, type Page } from "@playwright/test";
import { join } from "node:path";

const SHOTS = process.env.SHOTS_DIR ?? join(import.meta.dirname, "__shots__");
const shot = (page: Page, name: string) => page.screenshot({ path: join(SHOTS, `${name}.png`) });

const VIEWPORTS = [
  { tag: "desktop", width: 1440, height: 900 },
  { tag: "mobile", width: 390, height: 844 },
] as const;

for (const vp of VIEWPORTS) {
  test.describe(`pause ${vp.tag}`, () => {
    test.use({ viewport: { width: vp.width, height: vp.height }, deviceScaleFactor: vp.tag === "mobile" ? 2 : 1 });

    test("pause, paused state, resume", async ({ page }) => {
      await page.goto("/viewer.html?demo&scan=/home/deploy");
      await expect(page.getByRole("button", { name: "Pause" })).toBeVisible();
      await page.waitForTimeout(2500);
      await shot(page, `${vp.tag}-pause-running`);
      await page.getByRole("button", { name: "Pause" }).click();
      await expect(page.getByRole("button", { name: "Resume" })).toBeVisible();
      await expect(page.getByRole("button", { name: "Stop", exact: true })).toBeVisible();
      const meter = page.locator(".meter");
      await expect(meter).toContainText("Paused");
      await expect(meter).toContainText("Resume to keep scanning. Results so far are shown.");
      await page.waitForTimeout(600);
      const before = await meter.innerText();
      await page.waitForTimeout(1500);
      expect(await meter.innerText()).toBe(before); // nothing advances while paused
      await shot(page, `${vp.tag}-paused`);
      await page.getByRole("button", { name: "Resume" }).click();
      await expect(page.getByRole("button", { name: "Pause" })).toBeVisible();
      await expect(meter).not.toContainText("Paused");
      await expect(page.getByRole("button", { name: "Scan again" })).toBeVisible({ timeout: 120_000 });
    });

    test("stop while paused", async ({ page }) => {
      await page.goto("/viewer.html?demo&scan=/home/deploy");
      await page.getByRole("button", { name: "Pause" }).click();
      await page.getByRole("button", { name: "Stop", exact: true }).click();
      await expect(page.locator(".meter")).toContainText("Scan stopped");
      await expect(page.getByRole("button", { name: "Scan again" })).toBeVisible();
    });

    test("update banner", async ({ page }) => {
      await page.goto("/viewer.html?demo&latest=0.1.2");
      const banner = page.getByRole("region", { name: "Update available" });
      await expect(banner).toContainText("A newer diskspace (v0.1.2) is available.");
      await expect(banner.getByRole("link", { name: "How to update" })).toHaveAttribute("href", "https://diskspace.cc/#install");
      await page.waitForTimeout(900);
      await shot(page, `${vp.tag}-update-banner-picker`);
      await page.getByRole("button", { name: /^Home/ }).click();
      await page.waitForTimeout(2500);
      await shot(page, `${vp.tag}-update-banner-scan`);
      await banner.getByRole("button", { name: "Dismiss" }).click();
      await expect(banner).toHaveCount(0);
    });
  });
}
