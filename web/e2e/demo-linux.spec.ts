// SPDX-License-Identifier: MIT
//
// The default demo plays a Linux server (web-01, run via sudo), not a Mac:
// the picker shows the host and the ext4 root volume, and a scan of / renders
// the usual top-level dirs. The Mac profile stays reachable through ?os=mac.

import { expect, test } from "@playwright/test";

test.use({ viewport: { width: 1440, height: 900 } });

test("default demo is a Linux server and a scan of / shows var", async ({ page }) => {
  await page.goto("/viewer.html?demo");
  await expect(page.getByRole("heading", { name: "Choose what to scan" })).toBeVisible();
  await expect(page.getByText("web-01").first()).toBeVisible();
  await page.getByRole("button", { name: /^\/, / }).click();
  await expect(page.locator(".row:not([hidden])").filter({ has: page.locator(".label", { hasText: /^var$/ }) }).first()).toBeVisible({ timeout: 60_000 });
});

test("?os=mac keeps the MacBook", async ({ page }) => {
  await page.goto("/viewer.html?demo&os=mac");
  await expect(page.getByText("Alexs-MacBook-Pro").first()).toBeVisible();
  await expect(page.getByRole("button", { name: /^Macintosh HD/ })).toBeVisible();
});
