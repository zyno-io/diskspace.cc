// SPDX-License-Identifier: MIT
//
// Screenshot pass for design review (not pixel assertions). Writes PNGs to
// e2e/__shots__/ for desktop (1440×900) and mobile (390×844).

import { expect, test, type Page } from "@playwright/test";
import { join } from "node:path";

const SHOTS = join(import.meta.dirname, "__shots__");
const shot = (page: Page, name: string, fullPage = false) => page.screenshot({ path: join(SHOTS, `${name}.png`), fullPage });

const VIEWPORTS = [
  { tag: "desktop", width: 1440, height: 900 },
  { tag: "mobile", width: 390, height: 844 },
] as const;

for (const vp of VIEWPORTS) {
  test.describe(vp.tag, () => {
    test.use({ viewport: { width: vp.width, height: vp.height }, deviceScaleFactor: vp.tag === "mobile" ? 2 : 1 });

    test("landing", async ({ page }) => {
      await page.goto("/");
      await expect(page.getByRole("tab", { name: "Windows" })).toBeVisible();
      await page.waitForTimeout(6000);
      await shot(page, `${vp.tag}-landing-hero`);
      await shot(page, `${vp.tag}-landing-full`, true);
    });

    test("viewer flow", async ({ page }) => {
      await page.goto("/viewer.html?demo&os=mac");
      await expect(page.getByRole("heading", { name: "Choose what to scan" })).toBeVisible();
      await page.waitForTimeout(900);
      await shot(page, `${vp.tag}-picker`);
      await page.getByRole("button", { name: /^Home/ }).click();
      await page.waitForTimeout(3000);
      await shot(page, `${vp.tag}-midscan`);
      await expect(page.getByRole("button", { name: "Scan again" })).toBeVisible({ timeout: 120_000 });
      await page.waitForTimeout(1200);
      await shot(page, `${vp.tag}-done`);
      if (vp.tag === "desktop") {
        // hover tooltip over the biggest parcel area
        await page.mouse.move(300, 300);
        await page.waitForTimeout(400);
        await shot(page, `${vp.tag}-hover`);
      }
      // zoom into the biggest folder via the list
      const firstDir = page.locator(".list .row.dir").first();
      await firstDir.dblclick();
      await page.waitForTimeout(900);
      await shot(page, `${vp.tag}-zoomed`);
      // expand the first two folders in the list
      await page.locator(".list .row.dir .tw").first().click();
      await page.waitForTimeout(400);
      await page.locator(".list .row.dir .tw").nth(1).click();
      await page.waitForTimeout(600);
      if (vp.tag === "mobile") await page.locator(".rail").scrollIntoViewIfNeeded();
      await shot(page, `${vp.tag}-list-expanded`);
      if (vp.tag === "mobile") await shot(page, `${vp.tag}-results-full`, true);
    });
  });
}

test.describe("states", () => {
  test.use({ viewport: { width: 1440, height: 900 } });
  for (const s of ["superseded", "key-mismatch", "no-session"]) {
    test(s, async ({ page }) => {
      await page.goto(`/viewer.html?demo&state=${s}`);
      await page.waitForTimeout(2600);
      await shot(page, `state-${s}`);
    });
  }
  test("missing key", async ({ page }) => {
    await page.goto("/viewer.html");
    await page.waitForTimeout(800);
    await shot(page, "state-missing-key");
  });
  test("volume scan", async ({ page }) => {
    await page.goto("/viewer.html?demo&os=mac");
    await page.getByRole("button", { name: /^Macintosh HD/ }).click();
    await expect(page.getByRole("button", { name: "Scan again" })).toBeVisible({ timeout: 150_000 });
    await page.waitForTimeout(1200);
    await shot(page, "desktop-volume-done");
  });
});
