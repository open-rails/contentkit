import { expect, test, type Page } from "@playwright/test";
import { shared } from "../support/fixtures.ts";
import { mediaClient } from "../support/seed.ts";
import { harness, page as at, screenshots as dir } from "./pages.ts";

// The worker's 2160–480 ladder of a 20 s 4K source (e2e/support/fixtures.ts).
const f = shared();
const h = harness();
const ref = { kind: f.abr.kind, id: f.abr.id };
let ladder: { blobs: Map<string, number>; average: Map<number, number> };

test.beforeAll(async () => {
  const c = mediaClient(h, f.viewer);
  const read = await c.read(ref);
  // Each rung is one blob, its segments byte ranges of it at media-gateway.
  const blobs = new Map(read.files.flatMap((x) => {
    const m = x.path.match(/^hls\/(\d+)-\w+\.mp4$/);
    return m && x.url ? [[new URL(x.url).pathname, Number(m[1])] as const] : [];
  }));
  const master = await (await fetch(`${c.hlsBase(ref, read.hls![0]!)}master.m3u8`, { headers: { Authorization: `Bearer ${f.viewer.access_token}` } })).text();
  const average = new Map([...master.matchAll(/AVERAGE-BANDWIDTH=(\d+),RESOLUTION=\d+x(\d+)/g)].map((m) => [Number(m[2]), Number(m[1])] as const));
  ladder = { blobs, average };
});

// The rung of every segment request, in order.
function rungs(page: Page) {
  const seen: number[] = [];
  page.on("request", (r) => {
    const rung = ladder.blobs.get(new URL(r.url()).pathname);
    if (rung) seen.push(rung);
  });
  return seen;
}

// Rungs count from the click: on touch an inline preview may already be playing the lowest.
async function play(page: Page, seen?: number[]) {
  await page.goto(at("gallery.html", f.viewer, { page: "abr", abr: f.abr.id }));
  const player = page.locator("[data-demo=abr] [data-ckui=video-player]");
  if (seen) seen.length = 0;
  await player.getByRole("button", { name: "Play video" }).click();
  return player;
}

test("a fast connection starts at 1080p or better", async ({ page }) => {
  const seen = rungs(page);
  const player = await play(page, seen);
  await expect(player).toHaveAttribute("data-status", "playing", { timeout: 15_000 });
  expect(seen[0]).toBeGreaterThanOrEqual(1080);
});

test("a slow link starts at 480p and plays without stalling", async ({ page }) => {
  test.setTimeout(90_000);
  // 1.6× the 480p rung's average: it fits with headroom, 720p does not.
  const link = 1.6 * ladder.average.get(480)!;
  expect(link * 0.75).toBeLessThan(ladder.average.get(720)!);
  // The app and hls.js are already cached; only media crosses the slow link.
  await expect(await play(page)).toHaveAttribute("data-status", "playing", { timeout: 15_000 });
  await page.goto("about:blank");
  const cdp = await page.context().newCDPSession(page);
  await cdp.send("Network.enable");
  await cdp.send("Network.emulateNetworkConditions", { offline: false, latency: 60, downloadThroughput: link / 8, uploadThroughput: link / 16 });
  const seen = rungs(page);
  const player = await play(page, seen);
  expect(await page.evaluate(() => (navigator as unknown as { connection?: { downlink: number } }).connection?.downlink)).toBeCloseTo(link / 1e6, 0);
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
  await player.hover();
  await player.getByRole("button", { name: "Fullscreen" }).click();
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
    await player.hover();
    await player.getByRole("button", { name: "Settings" }).click();
    await page.locator("[data-ckui=settings-quality]").click();
    await expect(menu).toBeVisible();
  };
  await open();
  await expect(menu.getByRole("menuitemradio")).toHaveText(["2160p 4K", "1440p", "1080p HD", "720p", "480p", "Auto (1080p)"]);
  await expect(menu.getByRole("menuitemradio", { name: "Auto (1080p)" })).toHaveAttribute("aria-checked", "true");
  await expect(page.locator("[data-ckui=settings-quality]")).toContainText("Auto (1080p)");
  await page.screenshot({ path: `${dir}/player-quality-menu.png`, clip: (await player.boundingBox())! });
  await page.keyboard.press("Escape");
  await page.keyboard.press("Escape");
  await expect(menu).toBeHidden();
  // Keyboard: Settings, into Quality, ArrowUp from Auto reaches 480p.
  await player.getByRole("button", { name: "Settings" }).focus();
  await page.keyboard.press("Enter");
  await expect(page.locator("[data-ckui=settings-quality]")).toBeFocused();
  await page.keyboard.press("ArrowRight");
  await expect(menu).toBeVisible();
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
  await play(page, seen);
  await expect(player).toHaveAttribute("data-status", "playing", { timeout: 15_000 });
  expect(seen[0]).toBe(480);
  await open();
  await expect(menu.getByRole("menuitemradio", { name: "480p" })).toHaveAttribute("aria-checked", "true");
  await menu.getByRole("menuitemradio", { name: "Auto" }).click();
  const back = seen.length;
  await expect.poll(() => Math.max(0, ...seen.slice(back)), { timeout: 20_000 }).toBeGreaterThanOrEqual(1080);
  expect(await page.evaluate(() => localStorage.getItem("ckui.player.quality"))).toBe("auto");
});
