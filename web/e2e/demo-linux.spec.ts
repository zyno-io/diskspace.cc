// SPDX-License-Identifier: MIT
//
// The default demo plays a Linux server (web-01, run via sudo), not a Mac:
// the picker shows the host and the ext4 root volume, and a scan of / renders
// the usual top-level dirs. The Mac profile stays reachable through ?os=mac.

import { expect, test, type Page } from "@playwright/test";

const row = (page: Page, label: string | RegExp) => page.locator(".row:not([hidden])").filter({ has: page.locator(".label", { hasText: label instanceof RegExp ? label : new RegExp(`^${label}$`) }) }).first();

test.use({ viewport: { width: 1440, height: 900 } });

test("default demo is a Linux server and a scan of / shows var", async ({ page }) => {
  await page.goto("/viewer.html?demo");
  await expect(page.getByRole("heading", { name: "Choose what to scan" })).toBeVisible();
  await expect(page.getByText("web-01").first()).toBeVisible();
  await page.getByRole("button", { name: /^\/, / }).click();
  await expect(page.locator(".row:not([hidden])").filter({ has: page.locator(".label", { hasText: /^var$/ }) }).first()).toBeVisible({ timeout: 60_000 });
});

test("protected Linux paths can't be deleted, a container json log can", async ({ page }) => {
  await page.goto("/viewer.html?demo&os=linux&scan=/&approve=600");
  await expect(page.getByRole("button", { name: "Scan again" })).toBeVisible({ timeout: 120_000 });
  await page.waitForTimeout(800);
  // system roots and every folder above a mount are refused
  await row(page, "boot").click({ button: "right" });
  await expect(page.locator(".ctx-reason").first()).toContainText("Protected folder");
  await page.keyboard.press("Escape");
  // merged /usr: /usr/lib is protected like /lib
  await row(page, "usr").locator(".tw").click();
  await row(page, "lib").first().click({ button: "right" });
  await expect(page.locator(".ctx-reason").first()).toContainText("Protected folder");
  await page.keyboard.press("Escape");
  await row(page, "usr").locator(".tw").click();
  // a container's json log goes to the Trash, and the dialog says where
  await row(page, "var").locator(".tw").click();
  await row(page, "lib").locator(".tw").click();
  await row(page, "docker").locator(".tw").click();
  await row(page, "containers").locator(".tw").click();
  await row(page, /^[0-9a-f]{64}$/).locator(".tw").click();
  const log = row(page, /-json\.log$/);
  await expect(log).toBeVisible();
  await log.click({ button: "right" });
  await page.getByRole("menuitem", { name: "Add to deletion queue" }).click();
  await page.getByRole("button", { name: /^Deletion queue/ }).click();
  await page.getByRole("button", { name: "Move to Trash" }).click();
  const dlg = page.locator("dialog.dq-dialog");
  await expect(dlg.getByText("/home/deploy/.local/share/Trash/files")).toBeVisible({ timeout: 20_000 });
  await expect(dlg.getByText("Delete them there")).toBeVisible();
});

test("?os=mac keeps the MacBook", async ({ page }) => {
  await page.goto("/viewer.html?demo&os=mac");
  await expect(page.getByText("Alexs-MacBook-Pro").first()).toBeVisible();
  await expect(page.getByRole("button", { name: /^Macintosh HD/ })).toBeVisible();
});
