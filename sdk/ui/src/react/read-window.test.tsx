// @vitest-environment jsdom
import { act, cleanup, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../client/client.js";
import { ContentKitProvider } from "./provider.js";
import { useMediaRead } from "./read.js";

const ref = { kind: "gallery", id: "0192f000-0000-7000-8000-000000000001" };
const total = 2000;
const calls: number[] = [];
const response = (offset: number, limit = 50) => Response.json({
  access: "full", expires: Math.floor(Date.now() / 1000) + 120, total, offset, limit,
  files: Array.from({ length: total }, (_, i) => ({
    path: `${i}.webp`, w: 570, h: 806, size: 100,
    ...(i >= offset && i < offset + limit ? { url: `https://media.test/${i}?signed=${Date.now()}` } : {}),
  })),
});
const flush = async () => {
  for (let i = 0; i < 5; i++) await act(async () => { await vi.advanceTimersByTimeAsync(1); });
};
beforeEach(() => {
  calls.length = 0;
  vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout"] });
  vi.setSystemTime(new Date("2026-10-16T12:00:00Z"));
});
afterEach(() => { cleanup(); vi.useRealTimers(); });

it("refreshes only the current windows after distant jumps, and fetches an evicted window on revisit", async () => {
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async (url) => {
    const u = new URL(String(url));
    const offset = Number(u.searchParams.get("offset"));
    calls.push(offset);
    return response(offset);
  } });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client}>{children}</ContentKitProvider>;
  const { result, rerender } = renderHook(({ start }) => useMediaRead(ref, { prefix: "low-res/", window: { start, end: start + 4 } }), { wrapper, initialProps: { start: 0 } });
  await flush();
  for (const start of [200, 400, 600, 800, 1000]) { rerender({ start }); await flush(); }
  expect(result.current.read?.files).toHaveLength(total);
  expect(result.current.read?.files[1000]?.url).toBeTruthy();
  const before = calls.length;
  await act(async () => { await vi.advanceTimersByTimeAsync(61_000); });
  await flush();
  expect(calls.slice(before)).toEqual([1000]);
  expect(result.current.read?.files[0]?.url).toBeUndefined();
  rerender({ start: 0 });
  await flush();
  expect(calls.at(-1)).toBe(0);
  expect(calls.filter((offset) => offset === 0)).toHaveLength(2);
  expect(result.current.read?.files[0]?.url).toBeTruthy();
});

it("keeps placeholder geometry while the current window loads, aborts the abandoned window, and clears failed grants", async () => {
  const pending = new Map<number, { finish: (r: Response) => void; signal?: AbortSignal | null }>();
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async (url, init) => {
    const offset = Number(new URL(String(url)).searchParams.get("offset"));
    calls.push(offset);
    return offset === 0 ? response(0) : new Promise<Response>((finish) => pending.set(offset, { finish, signal: init?.signal }));
  } });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client}>{children}</ContentKitProvider>;
  const { result, rerender } = renderHook(({ start }) => useMediaRead(ref, { window: { start, end: start + 4 } }), { wrapper, initialProps: { start: 0 } });
  await flush();
  rerender({ start: 200 }); await flush();
  expect(result.current.loading).toBe(true);
  expect(result.current.read?.files[1000]).toMatchObject({ w: 570, h: 806 });
  rerender({ start: 400 }); await flush();
  expect(pending.get(200)?.signal?.aborted).toBe(true);
  await act(async () => { pending.get(200)!.finish(response(200)); });
  await flush();
  expect(result.current.read?.files[200]?.url).toBeUndefined();
  await act(async () => { pending.get(400)!.finish(Response.json({ code: "forbidden", error: "forbidden" }, { status: 403 })); });
  await flush();
  expect(result.current.error?.code).toBe("forbidden");
  expect(result.current.read).toBeNull();
  rerender({ start: 600 }); await flush();
  expect(result.current.read).toBeNull();
});

it("keeps every explicitly requested window and discards placeholders when the client or viewer changes", async () => {
  const makeClient = () => createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async (url) => {
    const offset = Number(new URL(String(url)).searchParams.get("offset")); calls.push(offset); return response(offset);
  } });
  let client = makeClient();
  let viewer = "alice";
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer={viewer}>{children}</ContentKitProvider>;
  const { result, rerender } = renderHook(() => useMediaRead(ref, { window: { start: 0, end: 125 } }), { wrapper });
  await flush();
  expect(calls).toEqual([0, 50, 100]);
  expect(result.current.read?.files[124]?.url).toBeTruthy();
  for (const change of [() => { client = makeClient(); }, () => { viewer = "bob"; }]) {
    change(); rerender();
    expect(result.current.read).toBeNull();
    await flush();
    expect(result.current.read?.files[124]?.url).toBeTruthy();
  }
});

it("shares status-only polling while processing and stops once ready without loading files", async () => {
  const requests: URL[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async (input) => {
    requests.push(new URL(String(input)));
    return Response.json({ access: "full", expires: 0, total: 0, offset: 0, limit: 0, files: [], status: {
      state: requests.length === 1 ? "processing" : "ready", failure_count: 0,
      uploads: { completed: requests.length === 1 ? 0 : 1, total: 1, failed: 0 },
      archives: { completed: 0, total: 0, failed: 0 }, presets: [], failures: [],
    } });
  } });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer="alice">{children}</ContentKitProvider>;
  const { result } = renderHook(() => [
    useMediaRead(ref, { editor: true, status: true, poll: 1000, refresh: false }),
    useMediaRead(ref, { editor: true, status: true, poll: 1000, refresh: false }),
  ], { wrapper });
  await flush();
  expect(requests).toHaveLength(1);
  expect(requests[0]!.searchParams.get("status")).toBe("1");
  expect(result.current[0]?.processing).toBe(true);
  await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
  await flush();
  expect(result.current[0]?.read?.files).toEqual([]);
  expect(result.current[0]?.processing).toBe(false);
  expect(requests).toHaveLength(2);
  await act(async () => { await vi.advanceTimersByTimeAsync(5000); });
  expect(requests).toHaveLength(2);
});
