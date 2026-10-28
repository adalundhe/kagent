// Screenshot the UI pages for one evidence run.
// Usage (in the Playwright image): node shots.mjs <ui-url> <evidence-dir> <label>
import { readFileSync, writeFileSync } from "node:fs";
import { chromium } from "playwright";

const [ui, dir, label] = process.argv.slice(2);
const report = JSON.parse(readFileSync(`${dir}/${label}.json`, "utf8"));
const pages = [
  { name: `${label}-chat.png`, url: `${ui}/agents/${report.session_id}/chat` },
  { name: `${label}-schedule.png`, url: `${ui}/schedules/${report.schedule_id}` },
];

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2 });
const page = await context.newPage();
for (const { name, url } of pages) {
  // The chat page keeps a task subscription open, so the network never idles.
  await page.goto(url, { waitUntil: "domcontentloaded", timeout: 60_000 });
  await page.waitForTimeout(10_000);
  await page.screenshot({ path: `${dir}/${name}` });
  const text = (await page.innerText("body")).replace(/\n{2,}/g, "\n");
  writeFileSync(`${dir}/${name.replace(/\.png$/, ".txt")}`, text);
  console.log(`--- ${name} text:\n${text.slice(0, 1800)}`);
  console.log(`${name} <- ${url} (landed on ${page.url()}, title ${JSON.stringify(await page.title())})`);
}
await browser.close();
