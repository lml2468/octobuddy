// Refresh the README hero screenshots from the running vite dev server in
// preview mode (?preview=1). Two shots match the current README:
//
//   docs/screenshot-chat.png    — full app view, dark theme, default chat
//   docs/screenshot-skills.png  — per-bot Skills panel, dark theme
//
// Run:
//   (term 1) cd desktop/frontend && npm run dev
//   (term 2) cd desktop/frontend && node scripts/screenshots.mjs

import { chromium } from "playwright-core";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const __dir = dirname(fileURLToPath(import.meta.url));
const docs = resolve(__dir, "../../../docs");

const BASE = process.env.XCLAW_TEST_URL ?? "http://127.0.0.1:9245";
const VIEW = { width: 1280, height: 800 };

const browser = await chromium.launch({ channel: "chrome", headless: true });
const ctx = await browser.newContext({
  viewport: VIEW,
  deviceScaleFactor: 2,
  colorScheme: "dark",
});

async function shot(query, out, settle = 350) {
  const page = await ctx.newPage();
  await page.goto(`${BASE}/?preview=1&theme=dark${query ? `&${query}` : ""}`);
  // Let fonts + the spaces-theme gradient settle; the chat seeds a turn with
  // status-strip steps that animate in.
  await page.waitForTimeout(settle);
  await page.screenshot({ path: out, fullPage: false });
  await page.close();
  console.log("▸", out);
}

await shot("", `${docs}/screenshot-chat.png`);
await shot("skills", `${docs}/screenshot-skills.png`);

await browser.close();
console.log("done");
