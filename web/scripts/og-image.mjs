// SPDX-License-Identifier: MIT
//
// Renders web/public/og.png (1200x630 link-preview image) from the live
// landing hero: the demo treemap with the wordmark and pitch over it.
//
//   npx vite --port 5199 &        (in web/)
//   node scripts/og-image.mjs [http://localhost:5199]

import { chromium } from "@playwright/test";
import { fileURLToPath } from "node:url";

const base = process.argv[2] ?? "http://localhost:5199";
const out = fileURLToPath(new URL("../public/og.png", import.meta.url));

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1200, height: 630 }, deviceScaleFactor: 1, colorScheme: "dark" });
await page.goto(base + "/");
// keep only the map, the wordmark and the pitch
await page.addStyleTag({
  content: `.install, .demo, .sub, .hero-by, .below, .foot { display: none !important; }
    .hero { min-height: 630px !important; height: 630px !important; }
    .hero-copy { position: absolute !important; left: 0 !important; right: 0 !important; bottom: 0 !important; padding: 0 56px 48px !important; }`,
});
// let the wordmark refit to the new box
await page.evaluate(() => window.dispatchEvent(new Event("resize")));
// the hero map loops: wait for the first scan to finish (~17 s) and settle
await page.waitForTimeout(19_000);
await page.screenshot({ path: out, clip: { x: 0, y: 0, width: 1200, height: 630 } });
await browser.close();
console.log("wrote", out);
