// @vitest-environment jsdom
import "./test/dom.js";
import { act, renderHook, waitFor } from "@testing-library/react";
import { beforeEach, expect, it, vi } from "vitest";
import { useHlsPlayer, type HlsPlayerOptions } from "./gallery-react.js";
import { refreshable } from "./playback.js";

// hls.js needs MediaSource, which jsdom lacks: a stand-in that records what the
// player asks of it and lets the test deliver its events.
const hls = vi.hoisted(() => {
  const instances: FakeHls[] = [];
  const Events = { ERROR: "hlsError", MANIFEST_PARSED: "hlsManifestParsed", LEVEL_SWITCHED: "hlsLevelSwitched", FRAG_LOADED: "hlsFragLoaded", BUFFER_FLUSHED: "hlsBufferFlushed" };
  class FakeHls {
    static Events = Events;
    static DefaultConfig = {};
    static isSupported = () => true;
    levels = [{ width: 480, height: 270, bitrate: 300_000 }];
    bandwidthEstimate = 1_000_000;
    startLevel = -1;
    loadLevel = -1;
    nextLevel = -1;
    nextAutoLevel = -1;
    // Every hls.currentLevel assignment: the flushing switch.
    flushes: number[] = [];
    set currentLevel(i: number) {
      this.flushes.push(i);
    }
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

const segment404 = { type: "networkError", details: "fragLoadError", fatal: false, response: { code: 404 } };

beforeEach(() => {
  hls.instances.length = 0;
  vi.spyOn(HTMLMediaElement.prototype, "play").mockResolvedValue(undefined);
  vi.spyOn(HTMLMediaElement.prototype, "pause").mockImplementation(() => {});
});

const live = () => {
  const i = hls.instances.at(-1);
  if (!i || i.destroyed) throw new Error("no live hls instance");
  return i;
};

function mount(initial: HlsPlayerOptions) {
  const video = document.createElement("video");
  Object.defineProperty(video, "currentTime", { value: 0, writable: true, configurable: true });
  const r = renderHook((p: HlsPlayerOptions) => useHlsPlayer(p), { initialProps: initial });
  act(() => r.result.current.ref(video));
  return { ...r, video };
}

async function playing(video: HTMLVideoElement) {
  await waitFor(() => expect(live().startedAt).toBeDefined());
  act(() => {
    video.dispatchEvent(new Event("play"));
    video.dispatchEvent(new Event("playing"));
  });
}

it("refreshes the grant once on 401, 403 or 404", () => {
  expect(refreshable({ kind: "access", code: "x", status: 403 })).toBe(true);
  expect(refreshable({ kind: "not_found", code: "fragLoadError/404", status: 404 })).toBe(true);
  expect(refreshable({ kind: "not_found", code: "fragLoadError/410", status: 410 })).toBe(false);
  expect(refreshable({ kind: "rate_limited", code: "x", status: 429 })).toBe(false);
  expect(refreshable({ kind: "network", code: "x", status: 0 })).toBe(false);
});

it("a segment token expiring mid-playback (404) re-grants and resumes where it was", async () => {
  const refresh = vi.fn(); // e.g. refetches the read API, which sets a fresh mt cookie
  const m = mount({ src: "https://media/item/master.m3u8", refresh });

  act(() => m.result.current.play());
  await waitFor(() => expect(hls.instances.length).toBe(1));
  act(() => live().emit("hlsManifestParsed"));
  await playing(m.video);
  expect(live().startedAt).toBe(-1);
  expect(m.result.current.status).toBe("playing");

  m.video.currentTime = 4.2;
  act(() => live().emit("hlsError", segment404));
  await waitFor(() => expect(refresh).toHaveBeenCalledTimes(1));
  await waitFor(() => expect(hls.instances.length).toBe(2));
  expect(hls.instances[0]!.destroyed).toBe(true);
  expect(live().src).toBe("https://media/item/master.m3u8");
  expect(m.result.current.error).toBeUndefined();
  act(() => live().emit("hlsManifestParsed"));
  expect(live().startedAt).toBe(4.2);
  await playing(m.video);
  expect(m.result.current.status).toBe("playing");

  // Playing again re-arms the refresh: a later expiry recovers too.
  m.video.currentTime = 9;
  act(() => live().emit("hlsError", segment404));
  await waitFor(() => expect(refresh).toHaveBeenCalledTimes(2));
  await waitFor(() => expect(hls.instances.length).toBe(3));
  act(() => live().emit("hlsManifestParsed"));
  expect(live().startedAt).toBe(9);
  expect(m.result.current.error).toBeUndefined();
});

it("a 404 that survives the refresh is not found", async () => {
  const refresh = vi.fn();
  const m = mount({ src: "https://media/gone/master.m3u8", refresh });
  act(() => m.result.current.play());
  await waitFor(() => expect(hls.instances.length).toBe(1));
  act(() => live().emit("hlsError", { ...segment404, details: "manifestLoadError" }));
  await waitFor(() => expect(refresh).toHaveBeenCalledTimes(1));
  await waitFor(() => expect(hls.instances.length).toBe(2));
  act(() => live().emit("hlsError", { ...segment404, details: "manifestLoadError" }));
  await waitFor(() => expect(m.result.current.status).toBe("error"));
  expect(m.result.current.error).toMatchObject({ kind: "not_found", status: 404 });
  expect(refresh).toHaveBeenCalledTimes(1);
});

it("without a refresh a 404 fails at once", async () => {
  const m = mount({ src: "https://media/gone/master.m3u8" });
  act(() => m.result.current.play());
  await waitFor(() => expect(hls.instances.length).toBe(1));
  act(() => live().emit("hlsError", segment404));
  await waitFor(() => expect(m.result.current.error).toMatchObject({ kind: "not_found" }));
});

const ladder = [
  { width: 854, height: 480, bitrate: 1_000_000 },
  { width: 1280, height: 720, bitrate: 2_500_000 },
  { width: 1920, height: 1080, bitrate: 5_000_000 },
];

it("a manual quality choice flushes to that level at once; Auto hands back to ABR", async () => {
  const m = mount({ src: "https://media/item/master.m3u8", qualityKey: null });
  act(() => m.result.current.play());
  await waitFor(() => expect(hls.instances.length).toBe(1));
  live().levels = ladder;
  act(() => live().emit("hlsManifestParsed"));
  await playing(m.video);

  const set = vi.fn();
  Object.defineProperty(m.video, "currentTime", { get: () => 12.5, set, configurable: true });
  act(() => m.result.current.quality.select(0));
  expect(live().flushes).toEqual([0]);
  // Once flushed, it seeks in place so the old rendition's frames are dropped.
  act(() => live().emit("hlsBufferFlushed"));
  expect(set).toHaveBeenLastCalledWith(12.5);
  act(() => live().emit("hlsBufferFlushed"));
  expect(set).toHaveBeenCalledTimes(1);
  expect(m.result.current.quality.selected).toBe(0);
  act(() => m.result.current.quality.select(2));
  expect(live().flushes).toEqual([0, 2]);

  act(() => m.result.current.quality.select(-1));
  expect(live().flushes).toEqual([0, 2]);
  expect(live().nextLevel).toBe(-1);
  expect(m.result.current.quality.selected).toBe(-1);

  // The menu's "current" follows what is actually playing.
  act(() => live().emit("hlsLevelSwitched", { level: 1 }));
  expect(m.result.current.quality.current).toBe(1);
});

it("a choice made before loading starts at that level without a flush", async () => {
  localStorage.setItem("ckui.player.quality", "1080");
  try {
    const n = mount({ src: "https://media/item/master.m3u8" });
    act(() => n.result.current.play());
    await waitFor(() => expect(hls.instances.length).toBe(1));
    live().levels = ladder;
    act(() => live().emit("hlsManifestParsed"));
    expect(live().startLevel).toBe(2);
    expect(live().loadLevel).toBe(2);
    expect(live().flushes).toEqual([]);
    expect(n.result.current.quality.selected).toBe(2);
  } finally {
    localStorage.removeItem("ckui.player.quality");
  }
});

it("a preview committed to playback drops its low start rung for ABR", async () => {
  const m = mount({ src: "https://media/item/master.m3u8" });
  act(() => m.result.current.preview(3));
  await waitFor(() => expect(hls.instances.length).toBe(1));
  live().levels = ladder;
  live().bandwidthEstimate = 50_000_000;
  act(() => live().emit("hlsManifestParsed"));
  expect(live().startLevel).toBe(0);
  expect(live().startedAt).toBe(3);

  act(() => m.result.current.play());
  expect(live().flushes).toEqual([-1]);
  expect(live().nextAutoLevel).toBeGreaterThan(0);
  expect(m.video.currentTime).toBe(0);
});
