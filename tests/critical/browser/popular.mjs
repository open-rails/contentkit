import assert from "node:assert/strict";
import { chromium } from "../../../sdk/ui/node_modules/@playwright/test/index.mjs";
const fixture = JSON.parse(process.env.CONTENTKIT_BROWSER_FIXTURE);
const browser = await chromium.launch({ headless: true, ...(process.env.CONTENTKIT_CRITICAL_CHROMIUM ? { executablePath: process.env.CONTENTKIT_CRITICAL_CHROMIUM } : {}) });
try {
  const page = await browser.newPage();
  const errors = [];
  const queries = [];
  page.on("pageerror", error => errors.push(String(error)));
  page.on("request", request => { if (new URL(request.url()).pathname.endsWith("/contents")) queries.push(new URL(request.url()).searchParams); });
  await page.goto(fixture.url);
  await page.waitForFunction(() => !!window.popular);
  await page.evaluate(() => window.popular.mount());
  await page.getByRole("tab", { name: "Reader week", exact: true }).waitFor();
  assert.deepEqual(await page.getByRole("tab").allTextContents(), ["Reader week", "Reader month"]);
  await page.waitForFunction(first => document.querySelector("[data-item]")?.getAttribute("data-item") === first, fixture.first);
  assert(queries.some(q => q.get("sort") === "popular:readers-week"), "first declared bucket was not the default");
  await page.getByRole("tab", { name: "Reader month", exact: true }).click();
  await page.waitForFunction(second => document.querySelector("[data-item]")?.getAttribute("data-item") === second, fixture.second);
  assert(queries.some(q => q.get("sort") === "popular:readers-month"), "declared tab did not select its bucket");
  await page.evaluate(({ from, until }) => window.popular.range(from, until), fixture);
  await page.waitForFunction(first => document.querySelector("[data-item]")?.getAttribute("data-item") === first, fixture.first);
  const custom = queries.find(q => q.get("activity_from") === fixture.from && q.get("activity_until") === fixture.until);
  assert(custom, "controlled whole-day range never reached the real API");
  assert.equal(custom.get("sort"), "popular");
  const text = await page.locator("[data-ckui=popular-grid]").innerText();
  assert(text.includes(fixture.from) && text.includes(fixture.last), "display did not show inclusive activity dates");
  assert(!text.includes(fixture.until), "display exposed the exclusive next day as included history");
  assert(text.includes("Recent activity is still updating"), "recent activity was not marked provisional");
  const cutoff = await page.evaluate(({ before, until }) => window.popular.cutoff(before, until), fixture);
  assert.equal(cutoff.status, 400);
  assert.equal(cutoff.details?.available_from, fixture.from);
  assert.deepEqual(errors, []);
  console.log(JSON.stringify({ scenario: "CK-J07-S20", status: "passed" }));
} finally {
  await browser.close();
}
