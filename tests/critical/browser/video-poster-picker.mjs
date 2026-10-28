import assert from "node:assert/strict";
import { writeFile } from "node:fs/promises";
import path from "node:path";
import { chromium, expect } from "../../../sdk/ui/node_modules/@playwright/test/index.mjs";
const fixture = JSON.parse(process.env.CONTENTKIT_POSTER_FIXTURE);
const browser = await chromium.launch({ headless: true, ...(process.env.CONTENTKIT_CRITICAL_CHROMIUM ? { executablePath: process.env.CONTENTKIT_CRITICAL_CHROMIUM } : {}) });
for (const signal of ["SIGINT", "SIGTERM"]) process.once(signal, async () => { await browser.close(); process.exit(1); });
let page;
const near = (a, b) => Math.abs(a - b) < 0.04;
try {
  page = await browser.newPage({ viewport: { width: 1100, height: 900 } });
  page.setDefaultTimeout(30000);
  const errors = [];
  page.on("pageerror", (error) => errors.push(String(error)));
  // Hosts serve no frames: the picker never asks the API to decode one.
  const frameRequests = [];
  page.on("request", (request) => { if (/\/upload\/frame/.test(request.url())) frameRequests.push(request.url()); });
  await page.goto(`${fixture.url}/video-poster-picker.html`);
  await page.waitForFunction(() => !!window.posterPicker);
  const picker = page.locator('[data-ckui="poster-picker"]');
  const video = picker.locator('[data-ckui="frame-video"]');
  const shown = () => video.evaluate((v) => (v.readyState >= 2 && !v.seeking ? v.currentTime : -1));
  // The decoded frame is drawable (an untainted canvas) and not blank.
  const detail = () => video.evaluate((v) => {
    const canvas = document.createElement("canvas");
    canvas.width = v.videoWidth;
    canvas.height = v.videoHeight;
    const g = canvas.getContext("2d");
    g.drawImage(v, 0, 0);
    const px = g.getImageData(0, 0, canvas.width, canvas.height).data;
    let sum = 0, sq = 0;
    for (let i = 0; i < px.length; i += 4) { sum += px[i]; sq += px[i] * px[i]; }
    const n = px.length / 4;
    return { width: v.videoWidth, height: v.videoHeight, deviation: Math.sqrt(sq / n - (sum / n) ** 2), png: canvas.toDataURL("image/png") };
  });
  // The frame the browser shows, for the worker's grab to be compared with.
  const keep = async (name) => {
    const file = path.join(fixture.results, name);
    await writeFile(file, Buffer.from((await detail()).png.split(",")[1], "base64"));
    return file;
  };
  const tiles = picker.locator('[data-ckui="frame-strip"] image');
  const awaitStrip = async () => {
    await expect(tiles).toHaveCount(8);
    const href = await tiles.first().getAttribute("href");
    const width = await page.evaluate((src) => new Promise((resolve) => { const img = new Image(); img.onload = () => resolve(img.naturalWidth); img.onerror = () => resolve(0); img.src = src; }), href);
    assert.ok(width > 0, `seek sprite sheet ${href} did not load`);
  };
  const saved = async () => {
    await expect.poll(() => page.evaluate(() => window.picker.saved), { timeout: 90000 }).not.toBeNull();
    return page.evaluate(() => window.picker);
  };

  // The item's own video: strip, exact preview, then a cropped frame.
  await page.evaluate(([f]) => window.posterPicker.open(f, { item: f.own }), [fixture]);
  await awaitStrip();
  await expect.poll(shown).toBeCloseTo(0.5, 1);
  const first = await detail();
  assert.deepEqual([first.width, first.height], [256, 144]);
  assert.ok(first.deviation > 10, `blank preview frame: ${JSON.stringify(first)}`);
  await picker.locator('[data-ckui="frame-strip"] button').nth(6).click();
  await expect.poll(shown).toBeCloseTo(1.625, 2);
  assert.equal(await picker.locator('[data-ckui="time"]').innerText(), "0:01.6");
  await expect(picker.locator('[data-ckui="frame-stage"]')).not.toHaveAttribute("data-loading");
  await page.screenshot({ path: path.join(fixture.results, "picker-own.png") });
  const ownShown = await keep("shown-own.png");
  await picker.getByRole("button", { name: "Crop…" }).click();
  const crop = page.locator('[data-ckui="crop-dialog"]');
  await expect(crop).toBeVisible();
  const source = crop.locator('img[src^="blob:"]').first();
  await expect.poll(() => source.evaluate((img) => img.naturalWidth)).toBe(256);
  await crop.getByRole("button", { name: "Zoom in" }).click();
  await crop.getByRole("button", { name: "Zoom in" }).click();
  await crop.getByRole("button", { name: "Save" }).click();
  const own = await saved();
  assert.ok(near(own.saved.frame.t, 1.625) && !own.saved.frame.from, JSON.stringify(own.saved));
  assert.ok(own.saved.edit?.crop && own.saved.edit.crop.w < 256, `crop not in the source's pixels: ${JSON.stringify(own.saved.edit)}`);
  assert.ok(own.closed);

  // Another item's video: its strip and preview, saved as a frame op from it.
  await page.evaluate(([f]) => window.posterPicker.open(f, { item: f.other, frames: { item: f.own, path: "source" } }), [fixture]);
  await awaitStrip();
  await expect.poll(shown).toBeCloseTo(0.5, 1);
  assert.ok((await detail()).deviation > 10);
  const otherShown = await keep("shown-other.png");
  await expect(picker.getByRole("button", { name: "Automatic" })).toHaveCount(0);
  await picker.getByRole("button", { name: "Use this frame" }).click();
  const other = await saved();
  assert.deepEqual(other.saved.frame.from, { kind: fixture.own.kind, id: fixture.own.id, version: fixture.own.version, path: "source.mp4" });
  assert.ok(near(other.saved.frame.t, 0.5), JSON.stringify(other.saved.frame));
  assert.ok(other.closed);
  await page.screenshot({ path: path.join(fixture.results, "picker-other.png") });

  assert.deepEqual(frameRequests, []);
  assert.deepEqual([...errors, ...own.errors, ...other.errors], []);
  console.log(JSON.stringify({ status: "passed", ownShown, otherShown }));
} catch (error) {
  if (page) {
    await page.screenshot({ path: path.join(fixture.results, "failure.png"), fullPage: true }).catch(() => {});
    console.error(JSON.stringify(await page.evaluate(() => window.picker).catch(() => null)));
    console.error((await page.locator("body").innerText().catch(() => "")).slice(0, 4000));
  }
  throw error;
} finally { await browser.close(); }
