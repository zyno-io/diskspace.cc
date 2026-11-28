// SPDX-License-Identifier: MIT
//
// Deletion queue against the demo (mock agent; nothing on disk is touched):
// context menu, queue marks, drawer, and the request dialog through awaiting
// approval, running, done and a partial result. Screenshots go to $SHOTS_DIR
// (default e2e/__shots__).

import { expect, test, type Page } from "@playwright/test";
import { join } from "node:path";

const SHOTS = process.env.SHOTS_DIR ?? join(import.meta.dirname, "__shots__");
const shot = (page: Page, name: string) => page.screenshot({ path: join(SHOTS, `${name}.png`) });
const row = (page: Page, label: string) => page.locator(".row:not([hidden])").filter({ has: page.locator(".label", { hasText: new RegExp(`^${label}$`) }) }).first();

async function finishedScan(page: Page, approve = 3500, scan = "/home/deploy") {
  await page.goto(`/viewer.html?demo&scan=${scan}&approve=${approve}`);
  await expect(page.getByRole("button", { name: "Scan again" })).toBeVisible({ timeout: 120_000 });
  await page.waitForTimeout(800);
}

test.describe("deletion desktop", () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test("queue while scanning: apply waits for the scan", async ({ page }) => {
    await page.goto("/viewer.html?demo&scan=/home/deploy");
    await expect(row(page, "backups")).toBeVisible({ timeout: 30_000 });
    await row(page, "backups").click({ button: "right" });
    await page.getByRole("menuitem", { name: "Add to deletion queue" }).click();
    await page.getByRole("button", { name: /^Deletion queue/ }).click();
    await expect(page.locator(".dq-hint")).toHaveText("Finish or stop the scan first");
    await expect(page.getByRole("button", { name: "Move to Trash" })).toBeDisabled();
    await shot(page, "desktop-drawer-scanning");
  });

  test("menu, marks, drawer, approve, run, done", async ({ page }) => {
    await finishedScan(page);
    // protected folder: its reason shows under the disabled item
    await row(page, "\\.config").click({ button: "right" });
    await expect(page.locator(".ctx-reason").first()).toContainText("Protected folder");
    await shot(page, "desktop-menu-protected");
    await page.keyboard.press("Escape");
    await row(page, "backups").click({ button: "right" });
    const menu = page.getByRole("menu");
    await expect(menu).toBeVisible();
    await shot(page, "desktop-menu-list");
    await page.getByRole("menuitem", { name: "Add to deletion queue" }).click();
    // keyboard: select .cache, Delete toggles it into the queue
    await row(page, "\\.cache").click();
    await page.keyboard.press("Delete");
    // treemap parcel menu
    const map = page.locator(".tm-overlay");
    const box = (await map.boundingBox())!;
    await page.mouse.click(box.x + box.width * 0.8, box.y + box.height * 0.85, { button: "right" });
    await expect(menu).toBeVisible();
    await shot(page, "desktop-menu-map");
    await page.keyboard.press("Escape");
    await expect(menu).toHaveCount(0);
    await page.waitForTimeout(300);
    await shot(page, "desktop-queued-marks");
    await page.getByRole("button", { name: /^Deletion queue/ }).click();
    await expect(page.locator(".dq")).toBeVisible();
    await shot(page, "desktop-drawer");
    await page.getByRole("button", { name: "Move to Trash" }).click();
    const dlg = page.locator("dialog.dq-dialog");
    await expect(dlg.getByRole("heading", { name: /^Approve on / })).toBeVisible();
    await shot(page, "desktop-dialog-awaiting");
    await expect(dlg.getByRole("heading", { name: "Moving to the Trash" })).toBeVisible({ timeout: 10_000 });
    await page.waitForTimeout(300);
    await shot(page, "desktop-dialog-running");
    await expect(dlg.getByRole("heading", { name: "Moved to the Trash" })).toBeVisible({ timeout: 15_000 });
    await shot(page, "desktop-dialog-done");
    await dlg.getByRole("button", { name: "Done" }).click();
    await expect(row(page, "backups")).toHaveCount(0);
    await expect(page.getByRole("button", { name: /^Deletion queue/ })).toHaveCount(0);
    await shot(page, "desktop-after-delete");
  });

  test("partial result keeps failed items queued", async ({ page }) => {
    // /opt/acme-agent holds an immutable queue.db: a permanent delete removes the rest and reports it partial
    await finishedScan(page, 600, "/opt");
    await row(page, "acme-agent").click({ button: "right" });
    await page.getByRole("menuitem", { name: "Add to deletion queue" }).click();
    await row(page, "containerd").click({ button: "right" });
    await page.getByRole("menuitem", { name: "Add to deletion queue" }).click();
    await page.getByRole("button", { name: /^Deletion queue/ }).click();
    await page.getByRole("button", { name: "Delete permanently…" }).click();
    await shot(page, "desktop-drawer-confirm-permanent");
    await page.getByRole("button", { name: "Delete permanently", exact: true }).click();
    const dlg = page.locator("dialog.dq-dialog");
    await expect(dlg.getByRole("heading", { name: "Some items weren't deleted" })).toBeVisible({ timeout: 15_000 });
    await expect(dlg.locator(".dqd-why").first()).toHaveText(/^Partly deleted/);
    await shot(page, "desktop-dialog-partial");
    await dlg.getByRole("button", { name: "Close" }).click();
    await expect(page.locator(".dq-name")).toHaveText(["acme-agent"]);
    await shot(page, "desktop-drawer-after-partial");
  });

  test("declined on the host", async ({ page }) => {
    await page.goto("/viewer.html?demo&scan=/home/deploy&approve=no");
    await expect(page.getByRole("button", { name: "Scan again" })).toBeVisible({ timeout: 120_000 });
    await row(page, "backups").click({ button: "right" });
    await page.getByRole("menuitem", { name: "Move to Trash now…" }).click();
    const dlg = page.locator("dialog.dq-dialog");
    await expect(dlg.getByRole("heading", { name: "Not deleted" })).toBeVisible({ timeout: 10_000 });
    await shot(page, "desktop-dialog-declined");
  });
});

test.describe("deletion phone", () => {
  test.use({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2 });

  test("menu and bottom sheet", async ({ page }) => {
    await finishedScan(page);
    await row(page, "backups").scrollIntoViewIfNeeded();
    await row(page, "backups").click({ button: "right" });
    await expect(page.getByRole("menu")).toBeVisible();
    await shot(page, "phone-menu");
    await page.getByRole("menuitem", { name: "Add to deletion queue" }).click();
    await row(page, "\\.cache").click({ button: "right" });
    await page.getByRole("menuitem", { name: "Add to deletion queue" }).click();
    await page.evaluate(() => window.scrollTo(0, 0));
    await page.getByRole("button", { name: /^Deletion queue/ }).click();
    await expect(page.locator(".dq")).toBeVisible();
    await page.waitForTimeout(200);
    await shot(page, "phone-drawer");
    await page.getByRole("button", { name: "Move to Trash" }).click();
    await expect(page.locator("dialog.dq-dialog").getByRole("heading", { name: /^Approve on / })).toBeVisible();
    await shot(page, "phone-dialog-awaiting");
  });
});
