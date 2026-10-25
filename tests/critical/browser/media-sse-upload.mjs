import assert from "node:assert/strict";
import path from "node:path";
import { chromium, expect } from "../../../sdk/ui/node_modules/@playwright/test/index.mjs";
const fixture = JSON.parse(process.env.CONTENTKIT_MEDIA_SSE_FIXTURE);
const browser = await chromium.launch({ headless: true, ...(process.env.CONTENTKIT_CRITICAL_CHROMIUM ? { executablePath: process.env.CONTENTKIT_CRITICAL_CHROMIUM } : {}) });
for (const signal of ["SIGINT", "SIGTERM"]) process.once(signal, async () => { await browser.close(); process.exit(1); });
let page, ref;
const mediaResponses = [];
try {
  page = await browser.newPage({ viewport: { width: 1200, height: 1000 } });
  const errors = [];
  page.on("requestfailed", request => { if (request.resourceType() === "image") mediaResponses.push({path: new URL(request.url()).pathname, failed: request.failure()?.errorText}); });
  page.on("pageerror", error => errors.push(String(error)));
  let streams = 0, longStreams = 0, shortStreams = 0, shortToken;
  page.on("response", async response => {
    if (response.request().resourceType() === "image") mediaResponses.push({path: new URL(response.url()).pathname, status: response.status(), headers: response.headers()});
    if (response.headers()["content-type"]?.startsWith("text/event-stream")) {
      streams++;
      const credential = response.request().headers().authorization;
      if (credential === `Bearer ${fixture.token}`) longStreams++;
      if (shortToken && credential === `Bearer ${shortToken}`) shortStreams++;
    }
    if (response.request().method() !== "POST" || new URL(response.url()).pathname !== "/content/v1/contents") return;
    const content = await response.json();
    assert.equal(response.status(), 201);
    ref = { kind: "gallery", id: content.id, version: content.versions[0].id };
    const saved = await page.request.post(`${fixture.url}/fixture/ref`, { data: ref });
    assert.equal(saved.status(), 204);
  });
  await page.goto(`${fixture.url}/media-sse-upload.html`);
  await page.waitForFunction(() => !!window.mediaAcceptance);
  await page.evaluate(input => window.mediaAcceptance.initialize(input), fixture);
  const root = page.locator('[data-ckui="content-upload"]');
  await expect(root.getByRole("button", { name: "Add files" })).toBeVisible({ timeout: 30000 });
  const cdp = await page.context().newCDPSession(page);
  await cdp.send("Network.enable");
  await cdp.send("Network.emulateNetworkConditions", { offline: false, latency: 2, downloadThroughput: -1, uploadThroughput: 256 * 1024 });
  await page.evaluate(() => { const input = document.createElement("input"); input.id = "actual-zip"; input.type = "file"; input.hidden = true; document.body.append(input); });
  await page.locator("#actual-zip").setInputFiles(fixture.archive);
  await page.evaluate(() => { const input = document.querySelector("#actual-zip"); const transfer = new DataTransfer(); transfer.items.add(input.files[0]); document.querySelector('[data-ckui="upload-drop"]').dispatchEvent(new DragEvent("drop", { bubbles: true, dataTransfer: transfer })); input.remove(); });
  const transfer = root.locator('[data-ckui="upload-transfer"][data-phase="uploading"]');
  await expect(transfer).toBeVisible({ timeout: 30000 });
  await expect.poll(async () => Number(await transfer.getAttribute("data-loaded")), { timeout: 30000 }).toBeGreaterThan(0);
  assert.ok(Number(await transfer.getAttribute("data-loaded")) < fixture.bytes);
  assert.equal(Number(await transfer.getAttribute("data-total")), fixture.bytes);
  await page.screenshot({ path: path.join(fixture.results, "zip-transfer.png"), fullPage: true });
  await cdp.send("Network.emulateNetworkConditions", { offline: false, latency: 0, downloadThroughput: -1, uploadThroughput: -1 });
  await expect(root.locator('[data-stage="archive-pages"]')).toContainText("8/25", { timeout: 60000 });
  const previews = root.locator('[data-ckui="media-progress-preview"][data-state="ready"] img');
  await expect(previews.first()).toBeVisible({ timeout: 60000 });
  await previews.first().scrollIntoViewIfNeeded();
  await expect.poll(() => previews.evaluateAll(images => images.filter(img => img.naturalWidth > 0).length)).toBeGreaterThan(0);
  assert.equal(await page.evaluate(id => window.mediaClient.contents.get(id).then(item => item.state), ref.id), "draft");
  await page.screenshot({ path: path.join(fixture.results, "zip-partial-thumbnails.png"), fullPage: true });
  await root.getByRole("button", { name: "Open", exact: true }).click();
  await page.reload();
  await page.waitForFunction(() => !!window.mediaAcceptance);
  const minted = await page.request.post(`${fixture.url}/fixture/short-token`);
  assert.equal(minted.status(), 200);
  shortToken = (await minted.json()).token;
  const priorLongStreams = longStreams;
  await page.evaluate(input => window.mediaAcceptance.initialize(input), { ...fixture, token: shortToken });
  const editor = page.locator('[data-ckui="reader-content-editor"]');
  await expect(editor.locator('[data-stage="archive-pages"]')).toContainText("8/25", { timeout: 30000 });
  await expect(editor.getByRole("list", { name: "Images", exact: true }).locator("img").first()).toBeVisible({ timeout: 30000 });
  await expect.poll(() => shortStreams, { timeout: 10000 }).toBeGreaterThan(0);
  // Rotate the host's token getter without changing viewer/read scope. The
  // already-open stream still owns the short token and must reconnect once
  // it expires; an HTTP fetch can then use the fresh credential.
  await page.evaluate(token => window.mediaAcceptance.rotateCredential(token), fixture.token);
  await expect.poll(() => longStreams, { timeout: 15000 }).toBeGreaterThan(priorLongStreams);
  await expect(editor.locator('[data-state="unavailable"]')).toHaveCount(0);
  await expect(editor.locator('[data-stage="archive-pages"]')).toContainText("8/25");
  assert.equal((await page.request.post(`${fixture.url}/fixture/release`)).status(), 204);
  await expect(editor.getByRole("list", { name: "Images", exact: true }).locator("img")).toHaveCount(25, { timeout: 60000 });
  await expect.poll(() => page.evaluate(ref => window.mediaClient.media.read(ref, { status: true }).then(read => read.status?.state), ref), { timeout: 60000 }).toBe("ready");
  const status = await page.evaluate(ref => window.mediaClient.media.read(ref, { status: true }).then(read => read.status), ref);
  assert.equal(status.archives.pages.completed, 25); assert.equal(status.archives.pages.total, 25);
  assert.equal(status.presets.find(p => p.name === "thumb").completed, 25);
  assert.equal(await page.evaluate(id => window.mediaClient.contents.get(id).then(item => item.state), ref.id), "draft");
  await page.screenshot({ path: path.join(fixture.results, "zip-draft-ready.png"), fullPage: true });
  assert.ok(streams >= 2, `expected authenticated SSE before and after editor reopen; observed ${streams}`);
  assert.deepEqual(errors, []);
  console.log(JSON.stringify({ status: "passed", pages: 25, uploadBytes: fixture.bytes }));
} catch (error) {
  console.error(JSON.stringify(mediaResponses));
  if (page && ref) console.error(JSON.stringify(await page.evaluate(async ref => {
    const read = await window.mediaClient.media.read(ref, { editor: true });
    return read.files.map(file => ({path: file.path, from: file.from, upload: file.upload, type: file.type, url: !!file.url}));
  }, ref).catch(() => [])));
  if (page) { await page.screenshot({ path: path.join(fixture.results, "failure.png"), fullPage: true }).catch(() => {}); console.error((await page.locator("body").innerText().catch(() => "")).slice(0, 6000)); }
  throw error;
} finally { await browser.close(); }
