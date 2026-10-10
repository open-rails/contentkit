import { expect, test, type Locator, type Page } from "@playwright/test";
import { shared } from "../support/fixtures.ts";
import { mediaClient } from "../support/seed.ts";
import { harness, page as at, screenshots as dir } from "./pages.ts";

// The watch demo (demo/gallery.tsx WatchDemo) over the worker's ladder of
// a 24 s source with English and Japanese audio, English and Spanish
// subtitle uploads and a seek sprite, through media-gateway in byte ranges.
const f = shared();
const h = harness();
const ref = { kind: f.watch.kind, id: f.watch.id };

type Logged = { name: string; reason?: string; type?: string; time: number; played?: number; duration?: number; [k: string]: unknown };
type Cue = { start: number; end: number; url: string; x: number; y: number };

// What hls.js asks for, from the master playlist the read API serves: each
// audio language's media playlist and blob, and the sprite's cues.
let ladder: { audio: Record<string, { playlist: string; blob: string }>; sprite: Cue[]; download: string };

test.beforeAll(async () => {
  const c = mediaClient(h, f.viewer);
  const read = await c.read(ref, { download: true });
  const base = c.hlsBase(ref, read.hls![0]!);
  const get = async (url: string) => (await fetch(url, { headers: { Authorization: `Bearer ${f.viewer.access_token}` } })).text();
  const master = await get(`${base}master.m3u8`);
  const audio: typeof ladder.audio = {};
  for (const m of master.matchAll(/TYPE=AUDIO,[^\n]*LANGUAGE="([^"]+)"[^\n]*URI="([^"]+)"/g)) {
    const playlist = new URL(m[2]!, `${base}master.m3u8`).href;
    const media = await get(playlist);
    const first = media.match(/URI="([^"]+)"/)?.[1] ?? media.split("\n").find((l) => l && !l.startsWith("#"))!;
    audio[m[1]!.slice(0, 2)] = { playlist: new URL(playlist).pathname, blob: new URL(first, playlist).pathname };
  }
  const vtt = await get(`${base}sprite.vtt`);
  const seconds = (t: string) => t.split(":").reduce((n, p) => n * 60 + Number(p), 0);
  const sprite = [...vtt.matchAll(/([\d:.]+) --> ([\d:.]+)\s+(\S+)#xywh=(\d+),(\d+),\d+,\d+/g)].map((m) => ({
    start: seconds(m[1]!), end: seconds(m[2]!), url: new URL(m[3]!, `${base}sprite.vtt`).pathname, x: Number(m[4]), y: Number(m[5]),
  }));
  const mp4 = read.files.find((x) => x.path === "video.mp4")!;
  ladder = { audio, sprite, download: new URL(mp4.url!).pathname };
});

async function open(page: Page, query: Record<string, string> = {}) {
  await page.goto(at("gallery.html", f.viewer, { page: "watch", watch: f.watch.id, ...query }));
  return page.locator("[data-demo=watch] [data-ckui=video-player]");
}

async function start(page: Page, query: Record<string, string> = {}) {
  const player = await open(page, query);
  await player.getByRole("button", { name: "Play video" }).click();
  await expect(player).toHaveAttribute("data-status", "playing", { timeout: 15_000 });
  return player;
}

const video = (player: Locator) => player.locator("video").first();
const prop = <T,>(player: Locator, f: (v: HTMLVideoElement) => T) => video(player).evaluate(f);
const time = (player: Locator) => prop(player, (v) => v.currentTime);
const logged = (page: Page) => page.evaluate(() => (window as unknown as { events: Logged[] }).events);
const desktopOnly = (name: string) => test.skip(name !== "desktop", "mouse and keyboard");

test("plays with its own controls, which hide while playing and return on movement", async ({ page }, info) => {
  desktopOnly(info.project.name);
  const player = await start(page);
  expect(await prop(player, (v) => v.controls)).toBe(false);
  await expect(player).toHaveAttribute("data-controls", "");
  await page.mouse.move(2, 2);
  await expect(player).not.toHaveAttribute("data-controls");
  await player.hover();
  await expect(player).toHaveAttribute("data-controls", "");
  await expect(player).not.toHaveAttribute("data-controls", { timeout: 5_000 });
  const t0 = await time(player);
  await page.waitForTimeout(1_500);
  expect(await time(player)).toBeGreaterThan(t0 + 1);
  // A click on the picture pauses; the controls stay while paused.
  await player.locator("[data-ckui=surface]").click({ position: { x: 60, y: 60 } });
  await expect.poll(() => prop(player, (v) => v.paused)).toBe(true);
  await page.mouse.move(2, 2);
  await expect(player).toHaveAttribute("data-controls", "");
  await expect(player.getByRole("button", { name: "Play", exact: true })).toBeVisible();
  await player.screenshot({ path: `${dir}/player-controls.png` });
});

test("page-wide shortcuts: play, seek, volume, speed, subtitles, theater, fullscreen", async ({ page }, info) => {
  desktopOnly(info.project.name);
  const player = await start(page);
  await page.locator("[data-demo=watch] h2").click(); // focus is on the page, not in the player
  await page.keyboard.press("k");
  await expect.poll(() => prop(player, (v) => v.paused)).toBe(true);
  const t = await time(player);
  await page.keyboard.press("l");
  await expect(player.locator("[data-ckui=seek-flash]")).toContainText("10 seconds");
  await page.keyboard.press("l");
  await expect(player.locator("[data-ckui=seek-flash]")).toContainText("20 seconds");
  await expect.poll(() => time(player)).toBeCloseTo(Math.min(24, t + 20), 0);
  await page.keyboard.press("ArrowLeft");
  await expect.poll(() => time(player)).toBeCloseTo(Math.min(24, t + 20) - 5, 0);
  await page.keyboard.press("5");
  await expect.poll(() => time(player)).toBeCloseTo(12, 0);
  await page.keyboard.press("m");
  expect(await prop(player, (v) => v.muted)).toBe(true);
  await page.keyboard.press("m");
  await page.keyboard.press("ArrowDown");
  expect(await prop(player, (v) => [v.muted, v.volume])).toEqual([false, 0.95]);
  await page.keyboard.press(">");
  expect(await prop(player, (v) => v.playbackRate)).toBe(1.25);
  await page.keyboard.press("<");
  expect(await prop(player, (v) => v.playbackRate)).toBe(1);
  await page.keyboard.press("k");
  await page.keyboard.press("c");
  await expect(player.locator("[data-ckui=captions]")).toHaveText("The second line");
  await page.keyboard.press("c");
  await expect(player.locator("[data-ckui=captions]")).toHaveCount(0);
  await page.keyboard.press("t");
  await expect(page.locator("main[data-theater]")).toHaveCount(1);
  await page.keyboard.press("t");
  await expect(page.locator("main[data-theater]")).toHaveCount(0);
  await page.keyboard.press("f");
  await expect.poll(() => page.evaluate(() => document.fullscreenElement?.getAttribute("data-ckui"))).toBe("video-player");
  await page.keyboard.press("f");
  await expect.poll(() => page.evaluate(() => document.fullscreenElement)).toBeNull();
});

test("audio and subtitle tracks from the master; the choices are remembered", async ({ page }, info) => {
  desktopOnly(info.project.name);
  const paths: string[] = [];
  page.on("request", (r) => paths.push(new URL(r.url()).pathname));
  const player = await start(page);
  expect(paths).toContain(ladder.audio.en!.playlist);
  const settings = async (section: string) => {
    await player.hover();
    await player.getByRole("button", { name: "Settings" }).click();
    await page.locator(`[data-ckui=settings-${section}]`).click();
    return page.locator(`[data-ckui=${section}-menu]`);
  };
  await (await settings("audio")).getByRole("menuitemradio", { name: "Japanese" }).click();
  await expect.poll(() => paths.includes(ladder.audio.ja!.blob)).toBe(true);
  await (await settings("subtitles")).getByRole("menuitemradio", { name: "Spanish" }).click();
  const captions = player.locator("[data-ckui=captions]");
  await expect(captions).toHaveText("Hola desde la primera línea");
  await expect(captions.locator("i")).toHaveText("primera");
  await player.screenshot({ path: `${dir}/player-captions.png` });

  const cc = player.getByRole("button", { name: "Subtitles" });
  await expect(cc).toHaveAttribute("aria-pressed", "true");
  await cc.click();
  await expect(captions).toHaveCount(0);
  await expect(cc).toHaveAttribute("aria-pressed", "false");
  await cc.click();
  await expect(captions).toHaveText("Hola desde la primera línea");
  expect(JSON.parse((await page.evaluate(() => localStorage.getItem("ckui.player.tracks")))!)).toEqual({ audio: "ja", subtitles: "es" });
  const events = await logged(page);
  expect(events.filter((e) => e.type === "audio").map((e) => (e.track as { lang: string }).lang)).toEqual(["ja"]);
  expect(events.filter((e) => e.type === "subtitles").map((e) => (e.track as { lang: string } | null)?.lang ?? null)).toEqual(["es", null, "es"]);

  // The next video starts in the viewer's languages.
  paths.length = 0;
  const again = await start(page);
  await expect.poll(() => paths.includes(ladder.audio.ja!.blob)).toBe(true);
  await expect(again.locator("[data-ckui=captions]")).toHaveText("Hola desde la primera línea");
});

test("the seek bar previews the sprite frame and time, and seeks by click and keys", async ({ page }, info) => {
  desktopOnly(info.project.name);
  const player = await start(page);
  await player.hover();
  const seek = player.locator("[data-ckui=seek]");
  const box = (await seek.boundingBox())!;
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  await expect(player.locator("[data-ckui=seek-preview]")).toContainText("0:12");
  // The sprite's tile for 12 s, as its VTT places it.
  const tile = ladder.sprite.find((c) => c.start <= 12 && 12 < c.end)!;
  const sprite = player.locator("[data-ckui=seek-sprite]");
  await expect(sprite).toHaveCSS("background-image", new RegExp(tile.url.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
  const offset = (n: number) => (n ? `-${n}px` : "0px");
  await expect(sprite).toHaveCSS("background-position", `${offset(tile.x)} ${offset(tile.y)}`);
  await player.screenshot({ path: `${dir}/player-seek-preview.png` });
  await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2);
  await expect.poll(() => time(player)).toBeGreaterThan(11);
  await page.keyboard.press("k");
  await expect.poll(() => prop(player, (v) => v.paused)).toBe(true);
  const at = await time(player);
  const thumb = player.getByRole("slider", { name: "Seek" });
  await expect(thumb).toHaveAttribute("aria-valuetext", /of 0:24$/);
  await thumb.focus();
  await page.keyboard.press("ArrowRight");
  await expect.poll(() => time(player)).toBeCloseTo(at + 5, 0);
  await page.keyboard.press("Home");
  await expect.poll(() => time(player)).toBe(0);
});

test("resume: starts at the given point and reports progress for resume and watch time", async ({ page }, info) => {
  desktopOnly(info.project.name);
  const player = await start(page, { t: "15" });
  expect(await time(player)).toBeGreaterThanOrEqual(15);
  await page.waitForTimeout(1_500);
  await page.keyboard.press("k");
  await expect.poll(async () => (await logged(page)).filter((e) => e.reason === "pause").length).toBe(1);
  const pause = (await logged(page)).find((e) => e.reason === "pause")!;
  expect(pause.time).toBeGreaterThan(15.5);
  expect(pause.played).toBeGreaterThan(0.5);
  expect(pause.played).toBeLessThan(pause.time - 15 + 0.5);
  expect(pause.duration).toBeCloseTo(24, 0);
  await page.keyboard.press("j");
  await expect.poll(async () => (await logged(page)).some((e) => e.reason === "seek" && Math.abs(e.time - (pause.time - 10)) < 0.5)).toBe(true);
  const types = (await logged(page)).filter((e) => e.name === "event").map((e) => e.type);
  expect(types).toEqual(expect.arrayContaining(["play", "playing", "pause", "seek"]));
});

test("mini player: docks, carries on where the player was, expands back and closes", async ({ page }, info) => {
  desktopOnly(info.project.name);
  const player = await start(page);
  await page.waitForTimeout(2_000);
  await player.hover();
  await player.getByRole("button", { name: "Mini player" }).click();
  const mini = page.locator("[data-ckui=mini-player]");
  await expect(mini).toBeVisible();
  await expect(page.locator("[data-demo=browsing]")).toBeVisible();
  await expect(mini).toHaveAttribute("data-status", "playing", { timeout: 15_000 });
  const box = (await mini.boundingBox())!;
  const view = page.viewportSize()!;
  expect(view.width - (box.x + box.width)).toBeLessThan(24);
  expect(view.height - (box.y + box.height)).toBeLessThan(24);
  expect(box.width / box.height).toBeCloseTo(16 / 9, 1);
  const handed = await time(mini);
  expect(handed).toBeGreaterThan(1.5);
  await page.screenshot({ path: `${dir}/player-mini.png` });

  await page.mouse.move(2, 2);
  await page.keyboard.press("k");
  await expect.poll(() => prop(mini, (v) => v.paused)).toBe(true);
  await page.keyboard.press("k");
  await expect.poll(() => prop(mini, (v) => v.paused)).toBe(false);
  await mini.hover();
  await mini.getByRole("button", { name: "Expand" }).click();
  await expect(mini).toHaveCount(0);
  const main = page.locator("[data-demo=watch] [data-ckui=video-player]");
  await expect(main).toHaveAttribute("data-status", "playing", { timeout: 15_000 });
  expect(await time(main)).toBeGreaterThanOrEqual(handed);

  await main.hover();
  await main.getByRole("button", { name: "Mini player" }).click();
  await mini.hover();
  await mini.getByRole("button", { name: "Close" }).click();
  await expect(mini).toHaveCount(0);
  expect((await logged(page)).map((e) => e.name)).toEqual(expect.arrayContaining(["expand", "close"]));
});

test("fullscreen keeps the controls and menus; picture in picture where the browser has it", async ({ page }, info) => {
  desktopOnly(info.project.name);
  const player = await start(page);
  await player.hover();
  await player.getByRole("button", { name: "Fullscreen" }).click();
  await expect.poll(() => page.evaluate(() => document.fullscreenElement?.getAttribute("data-ckui"))).toBe("video-player");
  await player.hover();
  await player.getByRole("button", { name: "Settings" }).click();
  await expect(page.locator("[data-ckui=settings-menu]")).toBeVisible();
  expect(await page.evaluate(() => !!document.fullscreenElement?.querySelector("[data-ckui=settings-menu]"))).toBe(true);
  await page.keyboard.press("Escape");
  await player.hover();
  await player.getByRole("button", { name: "Exit fullscreen" }).click();
  await expect.poll(() => page.evaluate(() => document.fullscreenElement)).toBeNull();

  const pip = player.getByRole("button", { name: "Picture in picture" });
  if (await page.evaluate(() => document.pictureInPictureEnabled)) {
    await player.hover();
    await pip.click();
    await expect.poll(() => page.evaluate(() => !!document.pictureInPictureElement)).toBe(true);
    await player.getByRole("button", { name: "Exit picture in picture" }).click();
    await expect.poll(() => page.evaluate(() => !!document.pictureInPictureElement)).toBe(false);
  } else await expect(pip).toHaveCount(0);
  const events = (await logged(page)).filter((e) => e.type === "fullscreen").map((e) => e.on);
  expect(events).toEqual([true, false]);
});

test("the viewer's volume is remembered across videos", async ({ page }, info) => {
  desktopOnly(info.project.name);
  const player = await start(page);
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("ArrowDown");
  expect(await prop(player, (v) => v.volume)).toBeCloseTo(0.9, 5);
  await page.reload();
  const again = await start(page);
  expect(await prop(again, (v) => v.volume)).toBeCloseTo(0.9, 5);
});

test("host slots: downloads in the bar, a version choice and an action in settings", async ({ page }, info) => {
  desktopOnly(info.project.name);
  const player = await start(page);
  await player.hover();
  await expect(player.getByRole("link", { name: "Download" })).toHaveAttribute("href", new RegExp(ladder.download));
  await player.getByRole("button", { name: "Settings" }).click();
  await page.locator("[data-ckui=settings-host-0]").click();
  await page.locator("[data-ckui=host-0-menu]").getByRole("menuitemradio", { name: "Director's cut" }).click();
  await expect(page.locator("[data-demo=watch] h2")).toHaveText("watch (v2)");
  await player.hover();
  await player.getByRole("button", { name: "Settings" }).click();
  await page.getByRole("menuitem", { name: "Report a problem" }).click();
  expect((await logged(page)).some((e) => e.name === "report")).toBe(true);
});

test("touch: a tap shows and hides the controls; double taps on a side seek 10 s and keep adding", async ({ page }, info) => {
  test.skip(info.project.name !== "mobile", "touch");
  const player = await start(page);
  await expect(player).not.toHaveAttribute("data-controls", { timeout: 5_000 });
  const box = (await player.boundingBox())!;
  const y = box.y + box.height / 3;
  await page.touchscreen.tap(box.x + box.width / 2, y);
  await expect(player).toHaveAttribute("data-controls", "");
  await page.waitForTimeout(500);
  await page.touchscreen.tap(box.x + box.width / 2, y);
  await expect(player).not.toHaveAttribute("data-controls");
  await page.waitForTimeout(500);
  const t = await time(player);
  const right = box.x + box.width * 0.85;
  await page.touchscreen.tap(right, y);
  await page.touchscreen.tap(right, y);
  await expect(player.locator("[data-ckui=seek-flash]")).toContainText("10 seconds");
  await page.touchscreen.tap(right, y);
  await expect(player.locator("[data-ckui=seek-flash]")).toContainText("20 seconds");
  await expect.poll(() => time(player)).toBeGreaterThan(Math.min(t + 19, 23));
  await page.screenshot({ path: `${dir}/player-touch-seek.png` });
});
