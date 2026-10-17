// @vitest-environment jsdom
import "../test/dom.js";
import { act, cleanup, renderHook } from "@testing-library/react";
import { StrictMode, type ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../client/client.js";
import type { EngagementSnapshot } from "../client/generated/wire.js";
import { ContentKitProvider } from "./provider.js";
import { useReadingEngagement } from "./use-engagement.js";

let visible = true;
beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date", "performance", "setInterval", "clearInterval", "setTimeout", "clearTimeout"] });
  visible = true;
  vi.spyOn(document, "hasFocus").mockReturnValue(true);
  vi.spyOn(document, "visibilityState", "get").mockImplementation(() => visible ? "visible" : "hidden");
  Object.defineProperties(window, { innerHeight: { value: 1000, configurable: true }, innerWidth: { value: 1000, configurable: true } });
});
afterEach(async () => {
  cleanup(); await Promise.resolve(); document.body.innerHTML = "";
  vi.useRealTimers(); vi.restoreAllMocks();
});

function fixture({ viewer = null, enabled = true, analytics = true, idleAfter = 60_000, checkpointInterval = 120_000, strict = false }: {
  viewer?: string | null; enabled?: boolean; analytics?: boolean; idleAfter?: number; checkpointInterval?: number; strict?: boolean;
} = {}) {
  const receipts = vi.fn(async () => ({ enabled: analytics, contexts: [{ content_id: "gallery", version_id: "version", token: "receipt", occurred_at: "2026-10-16T12:00:00Z" }] }));
  const client = createContentKitClient({ baseUrl: "https://content.test/reading", fetch: async (url) => {
    if (String(url).endsWith("/activity-contexts")) return Response.json(await receipts());
    throw new Error(`Unexpected request ${url}`);
  } });
  const submit = vi.fn(async (_snapshots: readonly EngagementSnapshot[], _options?: { session?: string; keepalive?: boolean }) => {});
  const local = vi.fn();
  Object.assign(client.discovery, { engagement: submit });
  Object.assign(client, { guestHistory: { record: local } });
  let owner = viewer;
  const wrapper = ({ children }: { children: ReactNode }) => {
    const provider = <ContentKitProvider client={client} viewer={owner}>{children}</ContentKitProvider>;
    return strict ? <StrictMode>{provider}</StrictMode> : provider;
  };
  const hook = renderHook(({ mode: _mode }) => useReadingEngagement("gallery", "version", { enabled, idleAfter, checkpointInterval, kind: "gallery" }), { wrapper, initialProps: { mode: "horizontal" } });
  return { ...hook, client, receipts, submit, local, changeOwner: (next: string | null) => { owner = next; hook.rerender({ mode: "horizontal" }); } };
}

function image({ loaded = true, top = 0, height = 1000 }: { loaded?: boolean; top?: number; height?: number } = {}) {
  const element = document.createElement("img");
  const state = { loaded, top, height };
  Object.defineProperties(element, {
    complete: { get: () => state.loaded }, naturalWidth: { get: () => state.loaded ? 700 : 0 }, naturalHeight: { get: () => state.loaded ? height : 0 },
    getBoundingClientRect: { value: () => ({ x: 0, y: state.top, top: state.top, left: 0, bottom: state.top + state.height, right: 700, width: 700, height: state.height, toJSON() {} }) },
  });
  document.body.append(element);
  return { element, state };
}
const advance = async (ms: number) => { await act(async () => { await vi.advanceTimersByTimeAsync(ms); }); };

it("ignores preloads and offscreen images, then records the actually loaded visible page", async () => {
  const f = fixture(), loading = image({ loaded: false }), offscreen = image({ top: 2000 });
  act(() => { f.result.current.page(0, "001.png")(loading.element); f.result.current.page(1, "002.png")(offscreen.element); });
  await advance(2000); await act(async () => f.result.current.flush());
  expect(f.receipts).not.toHaveBeenCalled(); expect(f.submit).not.toHaveBeenCalled(); expect(f.local).not.toHaveBeenCalled();
  act(() => { loading.state.loaded = true; loading.element.dispatchEvent(new Event("load")); });
  await advance(600); await act(async () => f.result.current.flush());
  expect(f.submit).toHaveBeenCalledTimes(1);
  expect(f.submit.mock.calls[0]?.[0][0]).not.toHaveProperty("kind");
  expect(f.submit.mock.calls[0]?.[0][0]).toMatchObject({ active_ms: 600, resume: "001.png", pages: [{ index: 0, active_ms: 600 }] });
});

it("keeps page turns local and sends one periodic cumulative checkpoint", async () => {
  const f = fixture({ checkpointInterval: 2000 }), a = image(), b = image({ top: 2000 });
  act(() => { f.result.current.page(0, "001.png")(a.element); f.result.current.page(1, "002.png")(b.element); });
  await advance(1000);
  act(() => { a.state.top = -2000; b.state.top = 0; window.dispatchEvent(new Event("scroll")); });
  expect(f.submit).not.toHaveBeenCalled();
  await advance(1000);
  expect(f.submit).toHaveBeenCalledTimes(1);
  expect(f.submit.mock.calls[0]?.[0][0]).toMatchObject({ active_ms: 2000, resume: "002.png", pages: [{ index: 0, active_ms: 1000 }, { index: 1, active_ms: 1000 }] });
});

it("does not credit hidden time and flushes with keepalive on hide and exit", async () => {
  const f = fixture(), page = image();
  act(() => f.result.current.page(0, "001.png")(page.element)); await advance(1000);
  act(() => { visible = false; document.dispatchEvent(new Event("visibilitychange")); });
  await act(async () => {});
  expect(f.submit.mock.calls[0]?.[1]?.keepalive).toBe(true);
  await advance(20_000);
  act(() => { visible = true; document.dispatchEvent(new Event("visibilitychange")); }); await advance(1000);
  await act(async () => f.unmount());
  const last = f.submit.mock.calls.at(-1);
  expect(last?.[1]?.keepalive).toBe(true); expect(last?.[0][0]?.active_ms).toBe(2000);
});

it("stops idle attention until genuine input renews it", async () => {
  const f = fixture({ idleAfter: 1000 }), page = image();
  act(() => f.result.current.page(0)(page.element)); await advance(2000);
  await act(async () => f.result.current.flush());
  expect(f.submit.mock.calls.at(-1)?.[0][0]?.active_ms).toBe(1000);
  act(() => window.dispatchEvent(new WheelEvent("wheel"))); await advance(500);
  await act(async () => f.result.current.flush());
  expect(f.submit.mock.calls.at(-1)?.[0][0]?.active_ms).toBe(1500);
});

it("measures tall-page coverage instead of treating one viewport as an entire page", async () => {
  const f = fixture(), page = image({ height: 5000 });
  act(() => f.result.current.page(0, "long.png")(page.element)); await advance(500);
  act(() => { page.state.top = -1000; window.dispatchEvent(new Event("scroll")); });
  await advance(500); await act(async () => f.result.current.flush());
  expect(f.submit.mock.calls[0]?.[0][0]?.pages).toEqual([{ index: 0, active_ms: 1000, ranges: [{ from: 0, to: 4000 }] }]);
});

it("measures a text post's body as its one unit by its visible share, without waiting for an image load", async () => {
  const f = fixture(), body = document.createElement("article");
  const state = { top: 0 };
  Object.defineProperty(body, "getBoundingClientRect", { value: () => ({ x: 0, y: state.top, top: state.top, left: 0, bottom: state.top + 4000, right: 700, width: 700, height: 4000, toJSON() {} }) });
  document.body.append(body);
  act(() => f.result.current.text(body)); await advance(500);
  act(() => { state.top = -2000; window.dispatchEvent(new Event("scroll")); });
  await advance(500); await act(async () => f.result.current.flush());
  // Only the parts actually on screen count: the jump skipped 25-50%.
  expect(f.submit.mock.calls[0]?.[0][0]?.pages).toEqual([{ index: 0, active_ms: 1000, ranges: [{ from: 0, to: 2500 }, { from: 5000, to: 7500 }] }]);
});

it("preserves one visit and stable page bindings across renderer mode switches and StrictMode replay", async () => {
  const f = fixture({ strict: true }), a = image(), b = image();
  const firstRef = f.result.current.page(0, "001.png"); act(() => firstRef(a.element)); await advance(500);
  await act(async () => f.result.current.flush());
  const visit = f.submit.mock.calls[0]?.[0][0]?.visit_id;
  f.rerender({ mode: "vertical" });
  expect(f.result.current.page(0, "001.png")).toBe(firstRef);
  act(() => { firstRef(null); f.result.current.page(0, "001.png")(b.element); });
  await advance(500); await act(async () => f.result.current.flush());
  expect(f.submit.mock.calls.at(-1)?.[0][0]).toMatchObject({ visit_id: visit, active_ms: 1000 });
});

it("keeps anonymous resume locally when analytics is unavailable, without an account fallback", async () => {
  const guest = fixture({ analytics: false }), page = image();
  act(() => guest.result.current.page(0, "001.png")(page.element)); await advance(600);
  await act(async () => guest.result.current.flush());
  expect(guest.submit).not.toHaveBeenCalled(); expect(guest.local).toHaveBeenCalledTimes(1);
  expect(guest.local.mock.calls[0]?.[0]).toMatchObject({ active_ms: 600, activity_token: "", kind: "gallery" });
  await act(async () => guest.unmount());
  const account = fixture({ viewer: "alice", analytics: false }), other = image();
  act(() => account.result.current.page(0)(other.element)); await advance(600);
  await act(async () => account.result.current.flush());
  expect(account.local).not.toHaveBeenCalled(); expect(account.submit).not.toHaveBeenCalled();
});

it("retires old callbacks and unresolved receipts on an account switch", async () => {
  const f = fixture({ viewer: "alice" }), page = image();
  let resolve!: (result: Awaited<ReturnType<typeof f.receipts>>) => void;
  f.receipts.mockImplementationOnce(() => new Promise((done) => { resolve = done; }));
  const old = f.result.current.page(0, "001.png"); act(() => old(page.element)); await advance(600);
  let pending!: Promise<void>;
  act(() => { pending = f.result.current.flush(); });
  act(() => f.changeOwner("bob"));
  await act(async () => { resolve({ enabled: true, contexts: [{ content_id: "gallery", version_id: "version", token: "alice", occurred_at: "2026-10-16T12:00:00Z" }] }); await pending; });
  act(() => old(page.element)); await advance(1000);
  expect(f.submit).not.toHaveBeenCalled(); expect(f.local).not.toHaveBeenCalled();
  act(() => f.result.current.page(0, "001.png")(page.element)); await advance(600);
  await act(async () => f.result.current.flush());
  expect(f.submit).toHaveBeenCalledTimes(1);
  expect(f.submit.mock.calls[0]?.[0][0]?.activity_token).toBe("receipt");
});

it("does nothing while consent or authentication readiness disables tracking", async () => {
  const f = fixture({ enabled: false }), page = image();
  act(() => f.result.current.page(0)(page.element)); await advance(1000);
  await act(async () => f.result.current.flush());
  expect(f.submit).not.toHaveBeenCalled(); expect(f.receipts).not.toHaveBeenCalled(); expect(f.local).not.toHaveBeenCalled();
});

it("tracks actual foreground video beyond the page idle cutoff and excludes stalls and seeks", async () => {
  const f = fixture({ idleAfter: 1000 });
  const video = document.createElement("video");
  const state = { ready: 0, position: 0, seeking: false };
  Object.defineProperties(video, {
    readyState: { get: () => state.ready }, currentTime: { get: () => state.position }, paused: { get: () => false }, ended: { get: () => false }, seeking: { get: () => state.seeking },
    getBoundingClientRect: { value: () => ({ x: 0, y: 0, top: 0, left: 0, bottom: 1000, right: 700, width: 700, height: 1000, toJSON() {} }) },
  });
  document.body.append(video);
  act(() => f.result.current.video(video)); await advance(1000);
  expect(f.receipts).not.toHaveBeenCalled();
  act(() => { state.ready = 4; video.dispatchEvent(new Event("loadeddata")); });
  for (let i = 0; i < 8; i++) { state.position += 0.25; await advance(250); }
  act(() => video.dispatchEvent(new Event("waiting"))); await advance(500);
  act(() => { state.position = 80; state.seeking = true; video.dispatchEvent(new Event("seeking")); });
  act(() => { state.seeking = false; video.dispatchEvent(new Event("seeked")); video.dispatchEvent(new Event("playing")); });
  for (let i = 0; i < 4; i++) { state.position += 0.25; await advance(250); }
  await act(async () => f.result.current.flush());
  const snapshot = f.submit.mock.calls[0]?.[0][0];
  expect(snapshot?.active_ms).toBe(3000);
  expect(snapshot?.watched).toEqual([{ from_ms: 0, to_ms: 2000 }, { from_ms: 80_000, to_ms: 81_000 }]);
});

it("sends new-owner engagement with that account's authorization and provider language", async () => {
  let viewer = "alice";
  const languages: string[] = [], sent: { authorization: string | null; snapshots: EngagementSnapshot[] }[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/reading-authorization", token: () => viewer, fetch: async (url, init) => {
    if (String(url).endsWith("/activity-contexts")) {
      const body = JSON.parse(String(init?.body)); languages.push(body.language);
      return Response.json({ enabled: true, contexts: body.targets.map((target: object) => ({ ...target, token: `${viewer}-receipt`, occurred_at: "2026-10-16T12:00:00Z" })) });
    }
    if (String(url).endsWith("/engagement")) {
      sent.push({ authorization: new Headers(init?.headers).get("Authorization"), snapshots: JSON.parse(String(init?.body)).snapshots });
      return new Response(null, { status: 204 });
    }
    throw new Error(`Unexpected request ${url}`);
  } });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer={viewer} language="fr">{children}</ContentKitProvider>;
  const hook = renderHook(() => useReadingEngagement("gallery", "version"), { wrapper }), page = image();
  const old = hook.result.current.page(0, "001.png"); act(() => old(page.element)); await advance(600);
  act(() => { viewer = "bob"; hook.rerender(); });
  act(() => { old(page.element); hook.result.current.page(0, "001.png")(page.element); });
  await advance(600); await act(async () => hook.result.current.flush());
  expect(languages).toEqual(["fr", "fr"]);
  expect(sent).toHaveLength(1); expect(sent[0]?.authorization).toBe("Bearer bob");
  expect(sent[0]?.snapshots[0]).toMatchObject({ activity_token: "bob-receipt", active_ms: 600 });
  expect(sent[0]?.snapshots.every((snapshot) => !("language" in snapshot))).toBe(true);
});
