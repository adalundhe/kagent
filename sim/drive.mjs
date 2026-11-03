// Scratch evidence driver for kagent-dev/kagent#3398. Not part of any PR.
// Usage (in the Playwright image): node drive.mjs <ui-url> <out-dir> <label> idle|recover
//
// idle:    a turn completes, then the workers are killed with nothing streaming.
//          Records whether and when the crash banner appears without a reload.
// recover: a short turn, a saved snapshot, then a long streaming turn killed
//          mid-stream. Records the banner, the Session read rate while the crashed
//          page waits, then reverts and records the discarded-turn marking.
import { execFileSync } from "node:child_process";
import { writeFileSync } from "node:fs";
import { chromium } from "playwright";

const [ui, out, label, scenario] = process.argv.slice(2);
const report = { label, scenario, events: [] };
const t0 = Date.now();
const note = (event, extra = {}) => {
  const entry = { t: ((Date.now() - t0) / 1000).toFixed(1), event, ...extra };
  report.events.push(entry);
  console.log(JSON.stringify(entry));
};
const save = () => writeFileSync(`${out}/${label}-${scenario}.json`, JSON.stringify(report, null, 2));

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, deviceScaleFactor: 2 });
const page = await context.newPage();
const sessionReads = [];
page.on("request", (request) => {
  if (request.url().includes("SessionService/GetSession")) sessionReads.push(Date.now());
});
const readsBetween = (from, to) => sessionReads.filter((at) => at >= from && at < to).length;
const shot = async (name) => {
  await page.screenshot({ path: `${out}/${label}-${name}.png`, fullPage: false });
  writeFileSync(`${out}/${label}-${name}.txt`, (await page.innerText("body")).replace(/\n{2,}/g, "\n"));
  note("screenshot", { file: `${label}-${name}.png` });
};
const killWorkers = () => {
  const output = execFileSync("kubectl", ["delete", "pod", "-n", "kagent", "-l", "ate.dev/worker-pool",
    "--grace-period=0", "--force", "--wait=false"], { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] });
  note("workers killed", { output: output.trim() });
};
const send = async (text) => {
  const box = page.getByTestId("chat-input").first();
  await box.waitFor({ state: "visible" });
  await page.waitForFunction(() => !document.querySelector('[data-testid="chat-input"]')?.disabled, null, { timeout: 120_000 });
  await box.fill(text);
  await box.press("Enter");
  note("sent", { text });
};
const agentText = () => page.locator('[data-testid="chat-message"][data-role="agent"]').allInnerTexts();

try {
  await page.goto(`${ui}/agents/kagent/story/new`, { waitUntil: "domcontentloaded", timeout: 60_000 });
  await send("short hello");
  await page.waitForURL(/\/agents\/[^/]+\/chat$/, { timeout: 120_000 });
  report.session = page.url().split("/").at(-2);
  note("conversation", { id: report.session });
  await page.getByText("Short answer.").first().waitFor({ timeout: 180_000 });
  await page.waitForFunction(() => !document.querySelector('[data-testid="chat-input"]')?.disabled, null, { timeout: 60_000 });
  note("first turn complete");

  if (scenario === "idle") {
    killWorkers();
    const killed = Date.now();
    const banner = page.getByText("The agent runtime crashed");
    try {
      await banner.waitFor({ timeout: 90_000 });
      report.bannerAfterSeconds = (Date.now() - killed) / 1000;
      note("banner shown without reload", { seconds: report.bannerAfterSeconds });
    } catch {
      report.bannerAfterSeconds = null;
      note("no banner within 90s of the crash");
    }
    report.composerDisabled = await page.getByTestId("chat-input").first().isDisabled();
    note("composer", { disabled: report.composerDisabled });
    report.sessionReadsAfterKill = readsBetween(killed, Date.now());
    await shot("idle");
  } else {
    // A READY snapshot, so the banner's fork hint and the divider's Fork have something to name.
    await page.getByTestId("chat-checkpoint").click();
    await page.locator('[data-testid^="chat-checkpoint-mark-"]').first().waitFor({ timeout: 120_000 });
    note("snapshot saved");
    await send("Tell me a long story");
    await page.waitForFunction(() => [...document.querySelectorAll('[data-testid="chat-message"][data-role="agent"]')]
      .some((node) => (node.textContent ?? "").split("lighthouse").length > 6), null, { timeout: 180_000 });
    note("long reply streaming");
    killWorkers();
    await page.getByText("The agent runtime crashed").waitFor({ timeout: 180_000 });
    note("banner shown");
    // Let the in-flight turn's own polling wind down before counting.
    await page.waitForTimeout(3_000);
    const from = Date.now();
    await page.waitForTimeout(20_000);
    report.sessionReadsIn20sWhileCrashed = readsBetween(from, Date.now());
    note("Session reads in 20s while crashed", { reads: report.sessionReadsIn20sWhileCrashed });
    report.banner = {
      buttons: await page.locator(".ant-alert").filter({ hasText: "The agent runtime crashed" }).getByRole("button").allInnerTexts(),
      pickers: await page.locator(".ant-alert").filter({ hasText: "The agent runtime crashed" }).getByRole("combobox").count(),
      text: await page.locator(".ant-alert").filter({ hasText: "The agent runtime crashed" }).innerText(),
      dividerForks: await page.locator('[data-testid^="chat-checkpoint-fork-"]').count(),
    };
    note("banner", report.banner);
    await shot("crashed");

    await page.getByRole("button", { name: "Revert to last snapshot" }).click();
    note("revert clicked");
    await page.waitForFunction(() => !document.querySelector('[data-testid="chat-input"]')?.disabled, null, { timeout: 240_000 });
    note("composer enabled after revert");
    await page.waitForTimeout(5_000);
    report.transcript = await page.locator('[data-testid="chat-message"]').evaluateAll((nodes) => nodes.map((node) => ({
      role: node.getAttribute("data-role"),
      discarded: node.getAttribute("data-discarded"),
      liveRegions: node.querySelectorAll('[role="status"],[aria-live]').length,
      text: (node.textContent ?? "").replace(/\s+/g, " ").slice(0, 160),
    })));
    note("transcript", { messages: report.transcript });
    // The turn of interest is the last one; bring it into view.
    await page.locator('[data-testid="chat-message"][data-role="user"]').last().scrollIntoViewIfNeeded();
    await shot("reverted");
  }
} catch (error) {
  note("error", { message: String(error) });
  await shot("error").catch(() => {});
  process.exitCode = 1;
} finally {
  report.sessionReadTimes = sessionReads.map((at) => ((at - t0) / 1000).toFixed(1));
  save();
  await browser.close();
}
