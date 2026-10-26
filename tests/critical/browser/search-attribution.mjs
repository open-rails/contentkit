import assert from "node:assert/strict";
import { chromium } from "../../../sdk/ui/node_modules/@playwright/test/index.mjs";

const fixture = JSON.parse(process.env.CONTENTKIT_BROWSER_FIXTURE);
const browser = await chromium.launch({ headless: true });
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
const diagnostics = [];
context.on("page", (page) => page.on("pageerror", (error) => diagnostics.push(String(error))));
const results = [];
try {
  const source = await context.newPage();
  const initialized = async (page) => {
    await page.waitForFunction(() => !!window.attribution);
  };
  const resultsReady = async (page) => {
    await page.waitForSelector('#results [data-ckui="card-title"][href*="ck_nav="]');
    await page.evaluate(() => window.attribution.flush());
    // Visible-stage collection uses the real IntersectionObserver debounce.
    await page.waitForTimeout(450);
    await page.evaluate(() => window.attribution.flush());
  };
  const hrefInfo = async (link) => {
    const href = await link.getAttribute("href");
    assert(href, "result has no ordinary href");
    const url = new URL(href, fixture.url);
    assert(!decodeURIComponent(href).toLowerCase().includes(fixture.query.toLowerCase()), "raw search text entered destination href");
    assert.deepEqual([...url.searchParams.keys()], ["ck_nav"], "destination carries extra search context");
    assert.match(url.searchParams.get("ck_nav"), /^[a-f0-9-]{36}$/);
    const [, , content, version] = url.pathname.split("/");
    const rank = await link.evaluate((a) => Number(a.closest('[data-exposure-position]')?.dataset.exposurePosition ?? 0));
    return { content, version, rank, href: url.href };
  };
  const observe = async (page, mode, before, milliseconds, expectedOrigin = true) => {
    await initialized(page);
    await page.waitForSelector("#measured-body");
    const prior = await page.evaluate(() => window.attribution.accepted.length);
    await page.bringToFront();
    await page.locator("#measured-body").click();
    await page.waitForTimeout(milliseconds);
    // The reader closes observation before awaiting the network. Slow API
    // acknowledgement must not turn the short observed visit into a long one.
    if (mode !== "same_tab_long") {
      await page.evaluate(() => window.attribution.stopReading());
      await page.waitForTimeout(50);
    }
    await page.evaluate(() => window.attribution.flush());
    await page.waitForFunction(({ content, prior }) => window.attribution.accepted.slice(prior).some((s) => s.content_id === content), { content: before.content, prior });
    const snapshot = await page.evaluate((content) => [...window.attribution.accepted].reverse().find((s) => s.content_id === content), before.content);
    assert(snapshot.active_ms > 0, "visit has no actual observed attention");
    assert.equal(Boolean(snapshot.origin_click_id), expectedOrigin, `${mode} borrowed or lost its exact navigation origin`);
    assert.equal(snapshot.content_id, before.content);
    assert.equal(snapshot.version_id, before.version);
    await page.waitForFunction(() => !new URL(location.href).searchParams.has("ck_nav"));
    assert(!decodeURIComponent(page.url()).includes(fixture.query), "raw query reached reader URL");
    const row = { mode, content: before.content, version: before.version, rank: before.rank,
      origin: snapshot.origin_click_id ?? "", visit: snapshot.visit_id, active: snapshot.active_ms };
    results.push(row);
    return row;
  };
  await source.goto(fixture.url + "/results");
  await initialized(source);
  await resultsReady(source);
  let link = source.locator('#results [data-ckui="card-title"]').first();
  let before = await hrefInfo(link);
  assert.equal(before.rank, 3, "page two reset absolute rank to one");
  await link.click();
  const long = await observe(source, "same_tab_long", before, 1800);
  const replay = await source.evaluate(() => window.attribution.replay());
  assert.equal(replay.origin_click_id, long.origin);
  assert.equal(replay.visit_id, long.visit);
  // Unmount the reader in this very same client, then directly revisit the
  // canonical URL. A last-click cache must not attach the previous visit.
  await source.evaluate(() => window.attribution.navigate("/results"));
  await resultsReady(source);
  await source.evaluate((url) => window.attribution.navigate(url), `/read/${before.content}/${before.version}`);
  const direct = await observe(source, "same_client_direct", before, 350, false);
  assert.notEqual(direct.visit, long.visit);

  await source.goto(fixture.url + "/results");
  await initialized(source); await resultsReady(source);
  link = source.locator('#results [data-ckui="card-title"]').last();
  before = await hrefInfo(link);
  assert.equal(before.rank, 4);
  const popupPromise = context.waitForEvent("page");
  await link.click({ modifiers: ["Control"] });
  const popup = await popupPromise;
  const short = await observe(popup, "modifier_tab_short", before, 350);
  assert.notEqual(short.origin, long.origin);
  await popup.close();

  await source.bringToFront();
  const middleLink = source.locator('#results [data-ckui="card-title"]').first();
  const middleBefore = await hrefInfo(middleLink);
  const middlePromise = context.waitForEvent("page");
  await middleLink.click({ button: "middle" });
  const middle = await middlePromise;
  // Opening the same rendered result twice must produce independent clicks,
  // while both exact origins remain readable in separate tabs.
  await source.bringToFront();
  const againPromise = context.waitForEvent("page");
  await middleLink.click({ modifiers: ["Control"] });
  const again = await againPromise;
  const middleVisit = await observe(middle, "middle_tab", middleBefore, 350);
  assert.notEqual(middleVisit.origin, short.origin);
  await middle.close();
  const againVisit = await observe(again, "repeated_result_tab", middleBefore, 350);
  await again.close();
  assert.notEqual(againVisit.origin, middleVisit.origin, "two result clicks reused one click identity");
  assert.notEqual(againVisit.visit, middleVisit.visit);

  await source.bringToFront();
  const input = source.getByPlaceholder("Find attributed content");
  await input.fill(fixture.query);
  await source.waitForSelector('[data-ckui="suggestion"][data-type="content"] a[href*="ck_nav="]');
  const suggestion = source.locator('[data-ckui="suggestion"][data-type="content"] a').first();
  await source.waitForTimeout(450);
  const suggested = await hrefInfo(suggestion);
  await source.evaluate(() => window.attribution.flush());
  await suggestion.click();
  await observe(source, "header_suggestion", suggested, 350);
  await source.goto(fixture.url + "/browse");
  await initialized(source); await resultsReady(source);
  const browseLink = source.locator('#results [data-ckui="card-title"]').first();
  const browse = await hrefInfo(browseLink);
  assert.equal(browse.rank, 1);
  await browseLink.click();
  await observe(source, "ordinary_grid", browse, 350);
  await Promise.all(context.pages().map(async (page) => {
    if (!page.isClosed()) await page.evaluate(() => window.attribution?.flush());
  }));
  assert.equal(diagnostics.length, 0, diagnostics.join("\n"));
  console.log(JSON.stringify({ status: "passed", results }));
} finally {
  await context.close();
  await browser.close();
}
