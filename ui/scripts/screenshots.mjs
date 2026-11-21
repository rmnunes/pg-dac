// Captures the README screenshots (docs/images/*.png) from the browser harness:
// the frontend with the in-browser mock engine, so no database is involved and
// every run shows the same schema. Run `pnpm screenshots` after a UI change.
//
// Uses an installed Chromium browser through playwright-core (no browser
// download): Edge by default on Windows, Chrome elsewhere. Set
// PGDAC_SCREENSHOT_BROWSER to a Playwright channel ("chrome", "msedge", ...) to pick one.

import { mkdir } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { createServer } from "vite";
import { chromium } from "playwright-core";

const ui = fileURLToPath(new URL("..", import.meta.url));
const out = fileURLToPath(new URL("../../docs/images/", import.meta.url));
const channel = process.env.PGDAC_SCREENSHOT_BROWSER ?? (process.platform === "win32" ? "msedge" : "chrome");

const server = await createServer({ root: ui, logLevel: "error", server: { port: 14210, strictPort: false } });
await server.listen();
const url = server.resolvedUrls.local[0];
const browser = await chromium.launch({ channel });

try {
  await mkdir(out, { recursive: true });
  const page = await browser.newPage({ viewport: { width: 1440, height: 860 }, deviceScaleFactor: 2 });
  await page.goto(url);

  const shot = async (name) => {
    await page.waitForTimeout(300); // let Monaco finish laying out
    await page.screenshot({ path: `${out}${name}.png` });
    console.log(`docs/images/${name}.png`);
  };
  const pick = async (side, source) => {
    await page.getByRole("button", { name: new RegExp(`^${side}`) }).click();
    await page.getByRole("menuitem", { name: source }).click();
  };

  // A project as the desired state, a database as the target.
  await pick("Source", /^Demo project/);
  await pick("Target", /^\[ACC\] Demo/);
  await page.getByRole("button", { name: /^Compare/ }).click();
  await page.getByRole("tab", { name: "Definition" }).waitFor();
  await page.getByTitle("Hide the notes").click(); // the risks stay visible in the list
  await page.getByRole("treeitem", { name: /sales\.reviewers/ }).click();
  await page.locator(".monaco-diff-editor").first().waitFor();
  await shot("compare");

  await page.getByRole("tab", { name: /^Risks/ }).click();
  await shot("risks");

  await page.getByRole("button", { name: /^Publish to Target/ }).click();
  await page.getByRole("dialog").waitFor();
  await shot("publish");
  await page.keyboard.press("Escape");
  await page.getByRole("dialog").waitFor({ state: "hidden" });

  await page.getByRole("button", { name: /^Generate Script/ }).click();
  await page.getByRole("dialog").locator(".monaco-editor").first().waitFor();
  await shot("script");
} finally {
  await browser.close();
  await server.close();
}
