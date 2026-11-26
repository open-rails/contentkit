import assert from "node:assert/strict";
import path from "node:path";
import { chromium, expect } from "../../../sdk/ui/node_modules/@playwright/test/index.mjs";
const fixture = JSON.parse(process.env.CONTENTKIT_PLAYER_FIXTURE);
const browser = await chromium.launch({ headless: true, args: ["--autoplay-policy=no-user-gesture-required"],
  ...(process.env.CONTENTKIT_CRITICAL_CHROMIUM ? { executablePath: process.env.CONTENTKIT_CRITICAL_CHROMIUM } : {}) });
for (const signal of ["SIGINT", "SIGTERM"]) process.once(signal, async () => { await browser.close(); process.exit(1); });

// One screen: an SDR one (headless Chromium's own), or an HDR one whose
// decoder MediaCapabilities answers confirms HDR or refuses it.
async function watch(screen) {
  const context = await browser.newContext({ viewport: { width: 640, height: 360 } });
  await context.addInitScript((screen) => {
    window.canvases = 0;
    const getContext = HTMLCanvasElement.prototype.getContext;
    HTMLCanvasElement.prototype.getContext = function (...args) { window.canvases++; return getContext.apply(this, args); };
    window.asked = [];
    if (screen === "sdr") return;
    const matchMedia = window.matchMedia.bind(window);
    window.matchMedia = (query) => query === "(dynamic-range: high)" ? { matches: true, media: query, addEventListener() {}, removeEventListener() {} } : matchMedia(query);
    const caps = navigator.mediaCapabilities, decodingInfo = caps.decodingInfo.bind(caps);
    caps.decodingInfo = async (config) => {
      if (!config.video?.transferFunction) return decodingInfo(config);
      window.asked.push(config.video.transferFunction);
      const ok = screen === "hdr";
      return { supported: ok, smooth: ok, powerEfficient: ok };
    };
  }, screen);
  const page = await context.newPage();
  page.setDefaultTimeout(30000);
  const errors = [], playlists = [];
  page.on("pageerror", (error) => errors.push(String(error)));
  page.on("request", (request) => { const m = /streams\/[^/?]+\.m3u8/.exec(request.url()); if (m) playlists.push(m[0]); });
  try {
    await page.goto(`${fixture.url}/video-player.html`);
    await page.waitForFunction(() => !!window.hdrPlayer);
    await page.evaluate((f) => window.hdrPlayer.open(f), fixture);
    await page.waitForFunction(() => !!window.player);
    await page.evaluate(() => window.player.play());
    await expect.poll(() => page.evaluate(() => [window.player.status, window.player.error?.code ?? ""].join(" ")), { timeout: 30000 }).toBe("playing ");
    await expect.poll(() => page.evaluate(() => document.querySelector("video").currentTime), { timeout: 30000 }).toBeGreaterThan(0.3);
    const state = await page.evaluate(() => ({ ranges: window.player.levels.map((l) => l.range), asked: window.asked, canvases: window.canvases }));
    await page.screenshot({ path: path.join(fixture.results, `player-${screen}.png`) });
    assert.deepEqual(errors, []);
    return { screen, ...state, playlists: [...new Set(playlists)] };
  } catch (error) {
    await page.screenshot({ path: path.join(fixture.results, `player-${screen}-failure.png`) }).catch(() => {});
    console.error(JSON.stringify({ screen, playlists, errors, player: await page.evaluate(() => window.player && { ...window.player, play: undefined }).catch(() => null) }));
    throw error;
  } finally { await context.close(); }
}

try {
  const results = [];
  for (const screen of ["sdr", "hdr", "hdr-no-decode"]) results.push(await watch(screen));
  const [sdr, hdr, refused] = results;
  // SDR screens play the tone-mapped H.264 ladder without asking the decoder about HDR.
  assert.deepEqual([sdr.ranges, sdr.asked], [["SDR"], []], JSON.stringify(sdr));
  assert.ok(sdr.playlists.includes("streams/144-h264.mp4.m3u8") && !sdr.playlists.includes("streams/144-av1.mp4.m3u8"), JSON.stringify(sdr));
  // An HDR screen whose decoder confirms PQ plays the 10-bit AV1 ladder.
  assert.deepEqual(hdr.ranges, ["PQ"], JSON.stringify(hdr));
  assert.ok(hdr.asked.includes("pq") && hdr.playlists.includes("streams/144-av1.mp4.m3u8") && !hdr.playlists.includes("streams/144-h264.mp4.m3u8"), JSON.stringify(hdr));
  // One that refuses it plays SDR.
  assert.deepEqual(refused.ranges, ["SDR"], JSON.stringify(refused));
  assert.ok(refused.asked.includes("pq") && !refused.playlists.includes("streams/144-av1.mp4.m3u8"), JSON.stringify(refused));
  // The browser renders the video: nothing draws it through a canvas.
  assert.deepEqual(results.map((r) => r.canvases), [0, 0, 0]);
  console.log(JSON.stringify({ status: "passed", results }));
} finally { await browser.close(); }
