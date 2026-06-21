// Modal affordance + visual regression baseline for the four settings panels.
// Drives the running vite dev server (`npm run dev`) via system Chrome and
// asserts (1) shared header chrome, (2) destructive actions route through the
// shared <Confirm> with Esc/focus/Tab affordances, (3) the chat reset confirm.
// Screenshots land in tests/baseline/ — commit them; future runs compare.
//
// Run:
//   (term 1) cd desktop/frontend && npm run dev
//   (term 2) cd desktop/frontend && node tests/modals.spec.mjs
//
// Exit 0 = all assertions pass. Screenshots are always (re)written.

import { chromium } from "playwright-core";
import { mkdir } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const __dir = dirname(fileURLToPath(import.meta.url));
const BASELINE = join(__dir, "baseline");
await mkdir(BASELINE, { recursive: true });

const BASE = process.env.XCLAW_TEST_URL ?? "http://127.0.0.1:9245";
const MODALS = [
  { key: "editor", title: "编辑 Bot", aria: "编辑 Bot" },
  { key: "skills", title: "技能", aria: "技能" },
  { key: "workflows", title: "工作流", aria: "工作流" },
  { key: "usage", title: "用量", aria: "Token 用量" },
];

let failed = 0;
const issues = [];
function check(cond, name) {
  if (cond) {
    console.log("  ✓", name);
  } else {
    failed++;
    issues.push(name);
    console.log("  ✗", name);
  }
}

const browser = await chromium.launch({ channel: "chrome", headless: true });
const ctx = await browser.newContext({
  viewport: { width: 1280, height: 800 },
  deviceScaleFactor: 2,
  colorScheme: "dark",
});

async function activeText(page) {
  return page.evaluate(() => (document.activeElement?.textContent ?? "").trim());
}

for (const m of MODALS) {
  console.log(`\n--- ${m.key} ---`);
  const page = await ctx.newPage();
  await page.goto(`${BASE}/?preview=1&theme=dark&${m.key}`);
  await page.waitForSelector(`div[role="dialog"][aria-label="${m.aria}"]`, { timeout: 4000 });

  // (1) Shared chrome — SettingsHeader title + active tab.
  const headerH2 = (await page.locator("header h2").first().textContent())?.trim();
  check(headerH2 === "设置", `${m.key}: shared header title "设置"`);
  const activeTab = (await page.locator('header .seg [role="tab"][aria-selected="true"]').textContent())?.trim();
  check(activeTab === m.title, `${m.key}: active segmented tab "${m.title}" (got "${activeTab}")`);
  const closeBtn = page.locator('header button.x[aria-label="关闭"]');
  check((await closeBtn.count()) === 1, `${m.key}: header ✕ close button present`);

  // Screenshot the modal in its default state.
  await page.screenshot({ path: join(BASELINE, `${m.key}.png`), fullPage: false });
  console.log(`  ▸ saved → tests/baseline/${m.key}.png`);

  // (2) Destructive-action affordances per modal.
  if (m.key === "editor") {
    await page.click("button.remove");
    const confirm = page.locator('div[role="alertdialog"]').first();
    await confirm.waitFor({ state: "visible", timeout: 1000 });
    check(await confirm.isVisible(), "editor: 删除 Bot opens shared <Confirm>");
    const msg = (await confirm.locator("p").textContent())?.trim() ?? "";
    check(msg.startsWith("删除 Bot"), `editor: confirm message starts with "删除 Bot" (got "${msg.slice(0, 40)}…")`);
    check((await activeText(page)) === "删除", "editor: primary 删除 button focused on open");
    await page.keyboard.press("Tab");
    check((await activeText(page)) === "取消", "editor: Tab cycles to 取消");
    await page.keyboard.press("Tab");
    check((await activeText(page)) === "删除", "editor: Tab wraps back to 删除");
    await page.keyboard.press("Escape");
    await page.waitForTimeout(50);
    check(!(await confirm.isVisible()), "editor: Esc dismisses confirm");
    check(await page.locator(`div[role="dialog"][aria-label="${m.aria}"]`).isVisible(), "editor: modal stays open after confirm Esc");
    // Screenshot the confirm dialog state.
    await page.click("button.remove");
    await confirm.waitFor({ state: "visible" });
    await page.screenshot({ path: join(BASELINE, `${m.key}-confirm.png`) });
    console.log(`  ▸ saved → tests/baseline/${m.key}-confirm.png`);
  }

  if (m.key === "skills" || m.key === "workflows") {
    const delBtn = page.locator(".list .row .del").first();
    if ((await delBtn.count()) > 0) {
      await delBtn.click();
      const confirm = page.locator('div[role="alertdialog"]').first();
      await confirm.waitFor({ state: "visible", timeout: 1000 });
      check(await confirm.isVisible(), `${m.key}: destructive row action opens <Confirm>`);
      const focused = await activeText(page);
      check(focused === "删除" || focused === "卸载", `${m.key}: primary focused (${focused})`);
      await page.keyboard.press("Tab");
      check((await activeText(page)) === "取消", `${m.key}: Tab cycles to 取消`);
      await page.keyboard.press("Escape");
      await page.waitForTimeout(50);
      check(!(await confirm.isVisible()), `${m.key}: Esc dismisses confirm`);
      check(await page.locator(`div[role="dialog"][aria-label="${m.aria}"]`).isVisible(), `${m.key}: modal stays open after confirm Esc`);
      // Screenshot confirm state for visual diff.
      await delBtn.click();
      await confirm.waitFor({ state: "visible" });
      await page.screenshot({ path: join(BASELINE, `${m.key}-confirm.png`) });
      console.log(`  ▸ saved → tests/baseline/${m.key}-confirm.png`);
    } else {
      console.log(`  ! ${m.key}: no destructive row in preview mock, skipping confirm assertions`);
    }
  }

  if (m.key === "usage") {
    const rangeTabs = await page.locator("header .range button").count();
    check(rangeTabs === 5, `usage: 5 range tabs in header (got ${rangeTabs})`);
  }

  // (3) Header ✕ closes the modal cleanly. First dismiss any open confirm so
  // the scrim doesn't intercept the click — but don't blanket-Esc, because on
  // modals without a confirm open Esc would itself close the modal.
  if ((await page.locator('div[role="alertdialog"]').count()) > 0) {
    await page.keyboard.press("Escape");
    await page.waitForTimeout(80);
  }
  await page.locator('header button.x[aria-label="关闭"]').click();
  await page.waitForTimeout(80);
  // After close, the modal's role=dialog is gone; the chat shell stays.
  const stillOpen = await page.locator(`div[role="dialog"][aria-label="${m.aria}"]`).count();
  check(stillOpen === 0, `${m.key}: ✕ closes modal`);

  await page.close();
}

await browser.close();
console.log("");
if (failed === 0) {
  console.log("ALL PASS");
  process.exit(0);
} else {
  console.log(`${failed} FAILED:`);
  for (const i of issues) console.log("  ·", i);
  process.exit(1);
}
