import { expect, test, type Page } from "@playwright/test";

const dir = process.env.SCREENSHOT_DIR ?? "test-results/screenshots";

// The synthetic ladder in e2e/media-server.ts: segment URLs carry ?rung=.
function rungs(page: Page) {
  const seen: number[] = [];
  page.on("request", (r) => {
    const m = r.url().match(/\/abr\/.*\.seg\?rung=(\d+)/);
    if (m) seen.push(Number(m[1]));
  });
  return seen;
}

async function play(page: Page, before?: () => Promise<unknown>) {
  await page.goto(`/gallery.html?page=abr&media=${process.env.MEDIA_PORT ?? 4180}`);
  await before?.();
  const player = page.locator("[data-demo=abr] [data-ckui=video-player]");
  await player.getByRole("button", { name: "Play video" }).click();
  return player;
}

test("a fast connection starts at 1080p or better", async ({ page }) => {
  const seen = rungs(page);
  const player = await play(page);
  await expect(player).toHaveAttribute("data-status", "playing", { timeout: 15_000 });
  expect(seen[0]).toBeGreaterThanOrEqual(1080);
});

test("1.5 Mbps starts low and plays without stalling", async ({ page }) => {
  test.setTimeout(60_000);
  // The app and hls.js are already cached (dev-server modules are huge); only media crosses the slow link.
  await (await play(page)).locator("video").evaluate((v: HTMLVideoElement) => new Promise((r) => v.addEventListener("playing", r, { once: true })));
  await page.goto("about:blank");
  const cdp = await page.context().newCDPSession(page);
  await cdp.send("Network.enable");
  await cdp.send("Network.emulateNetworkConditions", { offline: false, latency: 60, downloadThroughput: 1_500_000 / 8, uploadThroughput: 750_000 / 8 });
  const seen = rungs(page);
  const player = await play(page);
  expect(await page.evaluate(() => (navigator as unknown as { connection?: { downlink: number } }).connection?.downlink)).toBeCloseTo(1.5, 0);
  await expect(player).toHaveAttribute("data-status", "playing", { timeout: 15_000 });
  expect(seen[0]).toBe(480);
  const video = player.locator("video");
  await video.evaluate((v: HTMLVideoElement) => {
    (window as unknown as { stalls: number }).stalls = 0;
    v.addEventListener("waiting", () => (window as unknown as { stalls: number }).stalls++);
  });
  const t0 = await video.evaluate((v: HTMLVideoElement) => v.currentTime);
  await page.waitForTimeout(12_000);
  const t1 = await video.evaluate((v: HTMLVideoElement) => v.currentTime);
  expect(t1 - t0).toBeGreaterThan(10);
  expect(await page.evaluate(() => (window as unknown as { stalls: number }).stalls)).toBe(0);
  await expect(player).toHaveAttribute("data-status", "playing");
  expect(Math.max(...seen)).toBeLessThanOrEqual(720);
});

test("fullscreen on a 4K display climbs to 2160p", async ({ browser }, info) => {
  test.skip(info.project.name !== "desktop");
  const context = await browser.newContext({ baseURL: info.project.use.baseURL, viewport: { width: 1920, height: 1080 }, deviceScaleFactor: 2 });
  const page = await context.newPage();
  const seen = rungs(page);
  const player = await play(page);
  await expect(player).toHaveAttribute("data-status", "playing", { timeout: 15_000 });
  // Inline the player is ~650 CSS px (1300 device px): capped at 1080p however fast the link.
  await page.waitForTimeout(4_000);
  expect(seen[0]).toBe(1080);
  expect(Math.max(...seen)).toBe(1080);
  await page.getByRole("button", { name: "Fullscreen" }).click();
  await expect.poll(() => Math.max(...seen), { timeout: 20_000 }).toBe(2160);
  await expect(player).toHaveAttribute("data-status", "playing");
  await context.close();
});

test("the quality menu locks a rung, remembers it, and returns to Auto", async ({ page }, info) => {
  test.skip(info.project.name !== "desktop");
  test.setTimeout(60_000);
  const seen = rungs(page);
  const player = await play(page);
  await expect(player).toHaveAttribute("data-status", "playing", { timeout: 15_000 });
  const menu = page.locator("[data-ckui=quality-menu]");
  const open = async () => {
    await player.getByRole("button", { name: "Quality" }).click();
    await expect(menu).toBeVisible();
  };
  await open();
  await expect(menu.getByRole("menuitemradio")).toHaveText(["2160p 4K", "1440p", "1080p HD", "720p", "480p", "Auto (1080p)"]);
  await expect(menu.getByRole("menuitemradio", { name: "Auto (1080p)" })).toHaveAttribute("aria-checked", "true");
  await page.screenshot({ path: `${dir}/player-quality-menu.png`, clip: (await player.boundingBox())! });
  // Keyboard: ArrowUp from Auto reaches 480p.
  await page.keyboard.press("End");
  await page.keyboard.press("ArrowUp");
  await expect(menu.getByRole("menuitemradio", { name: "480p" })).toBeFocused();
  const at = seen.length;
  await page.keyboard.press("Enter");
  await expect(menu).toBeHidden();
  await expect.poll(() => seen.slice(at).filter((r) => r === 480).length, { timeout: 15_000 }).toBeGreaterThanOrEqual(3);
  // Once switched, nothing else loads.
  await page.waitForTimeout(3_000);
  expect(new Set(seen.slice(seen.indexOf(480, at)))).toEqual(new Set([480]));

  // Remembered across loads.
  expect(await page.evaluate(() => localStorage.getItem("ckui.player.quality"))).toBe("480");
  seen.length = 0;
  await play(page);
  await expect(player).toHaveAttribute("data-status", "playing", { timeout: 15_000 });
  expect(seen[0]).toBe(480);
  await open();
  await expect(menu.getByRole("menuitemradio", { name: "480p" })).toHaveAttribute("aria-checked", "true");
  await menu.getByRole("menuitemradio", { name: "Auto" }).click();
  const back = seen.length;
  await expect.poll(() => Math.max(0, ...seen.slice(back)), { timeout: 20_000 }).toBeGreaterThanOrEqual(1080);
  expect(await page.evaluate(() => localStorage.getItem("ckui.player.quality"))).toBe("auto");
});
