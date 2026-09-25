// @vitest-environment jsdom
import "../test/dom.js";
import { act, fireEvent, render, waitFor } from "@testing-library/react";
import { beforeEach, expect, it, vi } from "vitest";
import type { FileInfo, ReadResult, VideoImages } from "../wire.gen.js";
import { MediaGallery, UploadUiProvider, VideoPlayer } from "../ui.js";

// hls.js needs MediaSource, which jsdom lacks: a stand-in that records what the player asks of it.
const hls = vi.hoisted(() => {
  const instances: FakeHls[] = [];
  class FakeHls {
    static Events = { ERROR: "hlsError", MANIFEST_PARSED: "hlsManifestParsed", LEVEL_SWITCHED: "hlsLevelSwitched", FRAG_LOADED: "hlsFragLoaded" };
    static DefaultConfig = {};
    static isSupported = () => true;
    levels = [
      { width: 480, height: 270, bitrate: 300_000 },
      { width: 1920, height: 1080, bitrate: 5_000_000 },
    ];
    bandwidthEstimate = 1_000_000;
    startLevel = -1;
    loadLevel = -1;
    nextLevel = -1;
    src = "";
    startedAt: number | undefined;
    destroyed = false;
    handlers = new Map<string, ((e: string, d: unknown) => void)[]>();
    constructor() {
      instances.push(this);
    }
    on(e: string, h: (e: string, d: unknown) => void) {
      this.handlers.set(e, [...(this.handlers.get(e) ?? []), h]);
    }
    emit(e: string, d: unknown = {}) {
      for (const h of this.handlers.get(e) ?? []) h(e, d);
    }
    loadSource(src: string) {
      this.src = src;
    }
    attachMedia() {}
    startLoad(at: number) {
      this.startedAt = at;
    }
    destroy() {
      this.destroyed = true;
    }
  }
  return { instances, FakeHls };
});
vi.mock("hls.js", () => ({ default: hls.FakeHls }));

const read = (files: FileInfo[]): ReadResult => ({ access: "full", total: files.length, preview_limit: 0, offset: 0, limit: 50, expires: 0, files });
const vid = (index: number): FileInfo => ({ index, name: `${index}.mp4`, type: "video/mp4", w: 1920, h: 1080, duration: 40, hls: true });
const hlsBase = (f: FileInfo) => `/hls/${f.name}/`;
const poster = (file: string, time: number): VideoImages => ({
  poster: { aspect: "16:9", pending: false, file, time, outputs: [{ w: 480, h: 270, url: "https://m/poster.webp" }] },
});

let io: { cb: IntersectionObserverCallback; el?: Element }[] = [];

function device(hover: boolean) {
  window.matchMedia = vi.fn().mockImplementation((q: string) => ({ matches: hover && q.includes("hover: hover"), addEventListener() {}, removeEventListener() {} }));
}

beforeEach(() => {
  hls.instances.length = 0;
  io = [];
  device(true);
  HTMLMediaElement.prototype.pause = vi.fn();
  HTMLMediaElement.prototype.play = vi.fn(() => Promise.resolve());
  globalThis.IntersectionObserver = class {
    constructor(cb: IntersectionObserverCallback) {
      io.push({ cb });
    }
    observe(el: Element) {
      io.at(-1)!.el = el;
    }
    disconnect() {}
  } as unknown as typeof IntersectionObserver;
});

const players = () => [...document.querySelectorAll<HTMLElement>("[data-ckui=video-player]")];
const wait = (ms: number) => act(() => new Promise((r) => setTimeout(r, ms)));

async function hoverPreviews(el: HTMLElement) {
  fireEvent.pointerEnter(el, { pointerType: "mouse" });
  await wait(200);
  expect(hls.instances).toHaveLength(0); // nothing loads before the hover delay
  await waitFor(() => expect(hls.instances.length).toBeGreaterThan(0), { timeout: 2000 });
  const h = hls.instances.at(-1)!;
  act(() => h.emit("hlsManifestParsed"));
  return h;
}

it("desktop hover: after the delay the real HLS plays muted from the cover frame at the lowest rung; leaving unloads it", async () => {
  render(<MediaGallery read={read([vid(0)])} hlsBase={hlsBase} videoImages={poster("0.mp4", 12.5)} />);
  const [box] = players();
  const h = await hoverPreviews(box!);
  expect(h.src).toBe("/hls/0.mp4/master.m3u8");
  expect(h.startLevel).toBe(0);
  expect(h.startedAt).toBe(12.5);
  const video = box!.querySelector("video")!;
  expect(video.muted).toBe(true);
  expect(video.controls).toBe(false);
  act(() => void video.dispatchEvent(new Event("playing")));
  expect(box).toHaveAttribute("data-previewing");
  expect(box!.querySelector("img[src*='poster.webp']")).toHaveClass("opacity-0");

  fireEvent.pointerLeave(box!);
  expect(h.destroyed).toBe(true);
  expect(box).not.toHaveAttribute("data-previewing");
  expect(video.muted).toBe(false);
});

it("without a frame cover the preview starts 10% in; clicking commits to real playback from the start, with sound", async () => {
  render(<VideoPlayer base="/hls/a/" width={1920} height={1080} duration={40} />);
  const [box] = players();
  const h = await hoverPreviews(box!);
  expect(h.startedAt).toBe(4);
  const video = box!.querySelector("video")!;
  act(() => void video.dispatchEvent(new Event("playing")));
  video.currentTime = 6;
  fireEvent.click(box!.querySelector("button")!);
  expect(video.muted).toBe(false);
  expect(video.currentTime).toBe(0);
  expect(video.controls).toBe(true);
  expect(h.destroyed).toBe(false); // the same player continues
  fireEvent.pointerLeave(box!);
  expect(h.destroyed).toBe(false);
});

it("one preview at a time: hovering the next stops the previous", async () => {
  render(
    <>
      <VideoPlayer base="/hls/a/" duration={40} />
      <VideoPlayer base="/hls/b/" duration={40} />
    </>,
  );
  const [a, b] = players();
  const first = await hoverPreviews(a!);
  fireEvent.pointerEnter(b!, { pointerType: "mouse" });
  await waitFor(() => expect(hls.instances).toHaveLength(2), { timeout: 2000 });
  expect(first.destroyed).toBe(true);
  expect(hls.instances[1]!.src).toBe("/hls/b/master.m3u8");
});

it("touch: the most visible video previews as it scrolls into view", async () => {
  device(false);
  render(
    <>
      <VideoPlayer base="/hls/a/" duration={40} />
      <VideoPlayer base="/hls/b/" duration={40} />
    </>,
  );
  const [a, b] = players();
  const seen = (el: Element, ratio: number) => {
    const o = io.find((x) => x.el === el)!;
    const rect = { height: 300 * ratio } as DOMRectReadOnly;
    act(() => o.cb([{ isIntersecting: ratio > 0, intersectionRatio: ratio, intersectionRect: rect, rootBounds: { height: 800 } } as IntersectionObserverEntry], {} as IntersectionObserver));
  };
  seen(a!, 0.6);
  seen(b!, 0.9);
  await waitFor(() => expect(hls.instances).toHaveLength(1));
  expect(hls.instances[0]!.src).toBe("/hls/b/master.m3u8");
  seen(b!, 0.1);
  seen(a!, 1);
  await waitFor(() => expect(hls.instances).toHaveLength(2));
  expect(hls.instances[0]!.destroyed).toBe(true);
  expect(hls.instances[1]!.src).toBe("/hls/a/master.m3u8");
  seen(a!, 0);
  await waitFor(() => expect(hls.instances[1]!.destroyed).toBe(true));
});

it("the host setting turns it off; a locked or unencoded video never loads a stream", async () => {
  const { unmount } = render(
    <UploadUiProvider inlinePreview={false}>
      <VideoPlayer base="/hls/a/" duration={40} />
    </UploadUiProvider>,
  );
  fireEvent.pointerEnter(players()[0]!, { pointerType: "mouse" });
  await wait(700);
  expect(hls.instances).toHaveLength(0);
  unmount();

  const locked: FileInfo = { index: 0, type: "video/mp4", locked: true };
  render(<MediaGallery read={{ ...read([locked, { ...vid(1), hls: false }]), access: "none" }} hlsBase={hlsBase} defaultView="grid" storageKey={null} />);
  for (const el of document.querySelectorAll<HTMLElement>("[data-ckui=tile], [data-ckui=video-player]")) fireEvent.pointerEnter(el, { pointerType: "mouse" });
  await wait(700);
  expect(hls.instances).toHaveLength(0);
  expect(document.querySelector("[data-ckui=tile-preview]")).toBeNull();
});

it("grid tiles preview in place", async () => {
  render(<MediaGallery read={read([vid(0), vid(1)])} hlsBase={hlsBase} defaultView="grid" storageKey={null} />);
  const tile = document.querySelectorAll<HTMLElement>("[data-ckui=tile]")[1]!;
  const h = await hoverPreviews(tile);
  expect(h.src).toBe("/hls/1.mp4/master.m3u8");
  expect(h.startedAt).toBe(4);
  expect(tile.querySelector<HTMLVideoElement>("[data-ckui=tile-preview]")!.muted).toBe(true);
  fireEvent.pointerLeave(tile);
  expect(h.destroyed).toBe(true);
});
