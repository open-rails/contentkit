// @vitest-environment jsdom
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { FakeServer, bytes, fakeClient as client } from "../../test/fake.js";
import type { ContentKitClient } from "../client/client.js";
import { ContentKitProvider, useMediaRead } from "./index.js";

const ref = { kind: "gallery", id: "0192f000-0000-7000-8000-000000000001" };
const provider = (c: ContentKitClient) =>
  function Wrapper({ children }: { children: ReactNode }) {
    return <ContentKitProvider client={c}>{children}</ContentKitProvider>;
  };
const reads = (s: FakeServer) => s.calls.filter((x) => x === "/read").length;
const pages = (n: number) => Array.from({ length: n }, (_, i) => ({ path: `originals/${String(i + 1).padStart(3, "0")}.png`, type: "image/png", size: 3 }));

afterEach(() => vi.useRealTimers());

it("reads the item, refetches on reload, and shows a supplied read without fetching", async () => {
  const s = new FakeServer();
  s.seed(ref, pages(1));
  const c = client(s);
  const { result } = renderHook(() => useMediaRead(ref, { editor: true, client: c }));
  await waitFor(() => expect(result.current.read?.files.map((f) => f.path)).toEqual(["originals/001.png"]));
  act(() => result.current.reload());
  await waitFor(() => expect(reads(s)).toBe(2));
  const given = renderHook(() => useMediaRead(ref, { read: null, client: c }));
  expect([given.result.current.read, given.result.current.loading]).toEqual([null, false]);
  expect(reads(s)).toBe(2);
});

it("shares one request per item and options; set() updates every reader; a commit reloads the item's reads", async () => {
  const s = new FakeServer();
  s.seed(ref, [{ path: "cover.png", type: "image/png", size: 3 }]);
  const c = client(s);
  const { result } = renderHook(
    () => ({ a: useMediaRead(ref, { editor: true, prefix: "cover" }), b: useMediaRead(ref, { editor: true, prefix: "cover" }), other: useMediaRead(ref, { editor: true }) }),
    { wrapper: provider(c) },
  );
  await waitFor(() => expect(result.current.a.loading || result.current.b.loading || result.current.other.loading).toBe(false));
  expect(reads(s)).toBe(2);
  expect(result.current.b.read).toBe(result.current.a.read);

  const read = result.current.a.read!;
  act(() => result.current.a.set({ ...read, files: [{ ...read.files[0]!, meta: { by: "set" } }] }));
  expect(result.current.b.read?.files[0]?.meta).toEqual({ by: "set" });
  expect(result.current.other.read?.files[0]?.meta).toBeUndefined();

  await act(async () => {
    await c.media.put(new File([bytes(100, 9)], "c.png", { type: "image/png" }), { ref, path: "cover", edit: { rotate: 90 } });
  });
  await waitFor(() => expect(result.current.other.loading).toBe(false));
  expect(result.current.a.read?.files.find((f) => f.path === "cover.png")?.edit).toEqual({ rotate: 90 });
  expect(result.current.other.read?.files.find((f) => f.path === "cover.png")?.edit).toEqual({ rotate: 90 });
  // put's own wait, then one reload per distinct read after its commit.
  expect(reads(s)).toBe(5);
});

it("reads windows of a long item, keeps them, and merges their URLs", async () => {
  const s = new FakeServer();
  s.seed(ref, pages(120));
  const c = client(s);
  let window = { start: 0, end: 10, chunk: 50 };
  const { result, rerender } = renderHook(() => useMediaRead(ref, { prefix: "originals/", window, client: c }));
  await waitFor(() => expect(result.current.read?.files).toHaveLength(120));
  expect(s.reads.map((q) => [q.get("offset"), q.get("limit")])).toEqual([[null, "50"]]);
  expect(result.current.read!.files.filter((f) => f.url)).toHaveLength(50);

  window = { start: 95, end: 130, chunk: 50 };
  rerender();
  await waitFor(() => expect(result.current.read!.files.filter((f) => f.url)).toHaveLength(120));
  // [95, 130) is two windows; the first stays loaded; the end is clipped to the 120 files.
  expect(s.reads.map((q) => q.get("offset")).sort()).toEqual(["100", "50", null]);
  expect(result.current.read).toMatchObject({ total: 120, offset: 0, limit: 150 });
});

it("waits out a read rate limit before showing an error", async () => {
  const s = new FakeServer();
  s.seed(ref, pages(1));
  s.limitReads = 1;
  const c = client(s);
  const { result } = renderHook(() => useMediaRead(ref, { client: c }));
  await waitFor(() => expect(result.current.read?.files).toHaveLength(1), { timeout: 3000 });
  expect(result.current.error).toBeUndefined();
  expect(reads(s)).toBe(2);

  s.limitReads = 10;
  const failing = renderHook(() => useMediaRead(ref, { prefix: "x", client: c }));
  await waitFor(() => expect(failing.result.current.error?.code).toBe("rate_limited"), { timeout: 6000 });
}, 10_000);

it("polls an editor read while uploads process, then stops", async () => {
  const s = new FakeServer();
  s.seed(ref, [{ path: "originals/001.png", type: "image/png", size: 3, pending: ["low"] }]);
  const c = client(s);
  const { result } = renderHook(() => useMediaRead(ref, { editor: true, poll: 50, client: c }));
  await waitFor(() => expect(result.current.processing).toBe(true));
  s.seed(ref, pages(1));
  await waitFor(() => expect(result.current.processing).toBe(false));
  const n = reads(s);
  await new Promise((r) => setTimeout(r, 200));
  expect(reads(s)).toBe(n);
});

it("reads again shortly before the URLs expire; refresh() joins a read in flight", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const s = new FakeServer();
  s.seed(ref, pages(1));
  s.expires = Math.floor(Date.now() / 1000) + 600;
  const c = client(s);
  const { result } = renderHook(() => useMediaRead(ref, { client: c }));
  await waitFor(() => expect(result.current.read).not.toBeNull());
  expect(reads(s)).toBe(1);
  act(() => {
    result.current.refresh();
    result.current.refresh();
  });
  await waitFor(() => expect(result.current.loading).toBe(false));
  expect(reads(s)).toBe(2);
  await act(async () => {
    await vi.advanceTimersByTimeAsync(540_000);
  });
  await waitFor(() => expect(reads(s)).toBe(3));
});

it("reloads the item's viewer reads once a polled editor read shows its uploads processed", async () => {
  const s = new FakeServer();
  s.seed(ref, [{ path: "originals/001.png", type: "image/png", size: 3, pending: ["low"] }]);
  const c = client(s);
  const { result } = renderHook(() => ({ editor: useMediaRead(ref, { editor: true, poll: 50 }), viewer: useMediaRead(ref, { prefix: "low-res/" }) }), { wrapper: provider(c) });
  await waitFor(() => expect(result.current.editor.processing).toBe(true));
  const viewerReads = () => s.reads.filter((q) => q.get("prefix") === "low-res/").length;
  expect(viewerReads()).toBe(1);
  s.seed(ref, [...pages(1), { path: "low-res/001.webp", type: "image/webp", size: 2, upload: false }]);
  await waitFor(() => expect(result.current.viewer.read?.files.map((f) => f.path)).toEqual(["low-res/001.webp"]));
  expect(viewerReads()).toBe(2);
});
