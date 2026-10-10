// @vitest-environment jsdom
import "../../src/test/dom.js";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import type { ContentKitClient, RefBody } from "../../src/client/index.js";
import { ContentKitProvider, useMediaRead } from "../../src/react/index.js";
import type { Config, TestUser } from "../support/harness.js";
import { Accounts, client, harness, item, png, recorder, wait, type Recorder } from "./setup.js";

describe("useMediaRead against the real ContentKit", () => {
  const h = harness();
  // The client sends a ref as given: only kind and id.
  const bare = ({ kind, id }: { kind: string; id: string }) => ({ kind, id });
  const accounts = new Accounts(h);
  let cfg: Config;
  let alice: TestUser;

  beforeAll(async () => {
    cfg = await h.config();
    await accounts.load("alice", "reader");
    alice = accounts.get("alice");
  });
  afterEach(() => vi.useRealTimers());

  const provider = (c: ContentKitClient) =>
    function Wrapper({ children }: { children: ReactNode }) {
      return <ContentKitProvider client={c}>{children}</ContentKitProvider>;
    };
  /** A recorder that also keeps each read's query. */
  const recording = () => {
    const r = recorder() as Recorder & { queries: URLSearchParams[] };
    const inner = r.fetch;
    r.queries = [];
    r.fetch = (input, init) => {
      const before = r.calls.length;
      const res = inner(input, init);
      if (r.calls[before] === "/read") r.queries.push(new URL(String(input)).searchParams);
      return res;
    };
    return r;
  };
  const reads = (r: Recorder) => r.calls.filter((x) => x === "/read").length;
  /** Commits n pages (originals/001.png…) to ref and waits for the worker. */
  async function pages(ref: RefBody, n: number, from = 1) {
    const c = client(h, cfg, alice);
    const ups: { path: string; blob: string }[] = [];
    for (let i = 0; i < n; i += 10) {
      const batch = Array.from({ length: Math.min(10, n - i) }, (_, k) => i + k + from);
      ups.push(...(await Promise.all(batch.map((p) => c.media.upload(png(`${p}.png`, 1000 + p, 8, 8), { ref, path: `originals/${String(p).padStart(3, "0")}.png` })))));
    }
    await c.media.commit(ref, ups.map((u) => ({ op: "put" as const, path: u.path, blob: u.blob })));
    await c.media.waitFor(ref, ups.at(-1)!.path, wait);
    await waitFor(async () => expect((await c.media.read(ref, { editor: true, prefix: "originals/", limit: 200 })).files.every((f) => !f.pending && !f.staged)).toBe(true), wait);
  }

  it("reads the item, refetches on reload, and shows a supplied read without fetching", async () => {
    const ref = bare(await item(h, "gallery", alice));
    await pages(ref, 1);
    const r = recording();
    const c = client(h, cfg, alice, { record: r });
    const { result } = renderHook(() => useMediaRead(ref, { editor: true, client: c }));
    await waitFor(() => expect(result.current.read?.files.filter((f) => f.upload).map((f) => f.path)).toEqual(["originals/001.png"]), wait);
    act(() => result.current.reload());
    await waitFor(() => expect(reads(r)).toBe(2), wait);
    const given = renderHook(() => useMediaRead(ref, { read: null, client: c }));
    expect([given.result.current.read, given.result.current.loading]).toEqual([null, false]);
    expect(reads(r)).toBe(2);
  });

  it("shares one request per item and options; set() updates every reader; a commit reloads the item's reads", async () => {
    const ref = bare(await item(h, "gallery", alice));
    const r = recording();
    const c = client(h, cfg, alice, { record: r });
    const seeding = client(h, cfg, alice);
    await seeding.media.put(png("cover.png", 1201, 90, 30), { ref, path: "cover" });
    await seeding.media.waitFor(ref, "cover", wait);
    const { result } = renderHook(
      () => ({ a: useMediaRead(ref, { editor: true, prefix: "cover" }), b: useMediaRead(ref, { editor: true, prefix: "cover" }), other: useMediaRead(ref, { editor: true }) }),
      { wrapper: provider(c) },
    );
    await waitFor(() => expect(result.current.a.loading || result.current.b.loading || result.current.other.loading).toBe(false), wait);
    expect(reads(r)).toBe(2);
    expect(result.current.b.read).toBe(result.current.a.read);

    const read = result.current.a.read!;
    act(() => result.current.a.set({ ...read, files: [{ ...read.files[0]!, meta: { by: "set" } }] }));
    expect(result.current.b.read?.files[0]?.meta).toEqual({ by: "set" });
    expect(result.current.other.read?.files[0]?.meta).toBeUndefined();

    // A commit through the client (no put, whose wait reads on its own): one reload per distinct read.
    await act(async () => {
      const up = await c.media.upload(png("c.png", 1202, 90, 30), { ref, path: "cover" });
      await c.media.commit(ref, [{ op: "put", path: up.path, blob: up.blob, edit: { rotate: 90 } }]);
    });
    await waitFor(() => expect(result.current.other.read?.files.find((f) => f.path === "cover.png")?.edit).toEqual({ rotate: 90 }), wait);
    expect(result.current.a.read?.files.find((f) => f.path === "cover.png")?.edit).toEqual({ rotate: 90 });
    expect(reads(r)).toBe(4);
  });

  it("reads windows of a long item, keeps them, and merges their URLs", async () => {
    const ref = bare(await item(h, "gallery", alice));
    await pages(ref, 120);
    const r = recording();
    // A viewer: the derived pages (low-res/), each with its signed URL.
    const c = client(h, cfg, accounts.get("reader"), { record: r });
    let window = { start: 0, end: 10, chunk: 50 };
    const { result, rerender } = renderHook(() => useMediaRead(ref, { prefix: "low-res/", window, client: c }));
    await waitFor(() => expect(result.current.read?.files).toHaveLength(120), wait);
    expect(r.queries.map((q) => [q.get("offset"), q.get("limit")])).toEqual([[null, "50"]]);
    expect(result.current.read!.files.filter((f) => f.url)).toHaveLength(50);

    window = { start: 95, end: 130, chunk: 50 };
    rerender();
    await waitFor(() => expect(result.current.read!.files.filter((f) => f.url)).toHaveLength(120), wait);
    // [95, 130) is two windows; the first stays loaded; the end is clipped to the 120 files.
    expect(r.queries.map((q) => q.get("offset")).sort()).toEqual(["100", "50", null]);
    expect(result.current.read).toMatchObject({ total: 120, offset: 0, limit: 150 });
  }, 180_000);

  it("waits out a read rate limit before showing an error", async () => {
    const ref = bare(await item(h, "gallery", alice));
    await pages(ref, 1);
    const r = recording();
    const c = client(h, cfg, alice, { record: r });
    const path = `/api/contentkit/media/gallery/${ref.id}`;
    await h.faults([{ fault: "api", path, method: "GET", status: 429, code: "rate_limited", error: "slow down", retry_after: 1, times: 1 }]);
    const { result } = renderHook(() => useMediaRead(ref, { client: c }));
    await waitFor(() => expect(result.current.read?.files).toHaveLength(1), { timeout: 5000 });
    expect(result.current.error).toBeUndefined();
    expect(reads(r)).toBe(2);

    await h.faults([{ fault: "api", path, method: "GET", status: 429, code: "rate_limited", error: "slow down", retry_after: 1, times: 10 }]);
    const failing = renderHook(() => useMediaRead(ref, { prefix: "x", client: c }));
    await waitFor(() => expect(failing.result.current.error?.code).toBe("rate_limited"), { timeout: 10_000 });
  }, 20_000);

  it("polls an editor read while uploads process, then stops", async () => {
    const ref = bare(await item(h, "gallery", alice));
    // The worker's storage requests for this item wait: its upload stays in processing.
    await h.faults([{ item: ref.id, fault: "hold" }]);
    const c = client(h, cfg, alice);
    const up = await c.media.upload(png("1.png", 1301), { ref, path: "originals/001.png" });
    await c.media.commit(ref, [{ op: "put", path: up.path, blob: up.blob }]);
    const r = recording();
    const polling = client(h, cfg, alice, { record: r });
    const { result } = renderHook(() => useMediaRead(ref, { editor: true, poll: 50, client: polling }));
    await waitFor(() => expect(result.current.processing).toBe(true), wait);
    await h.clearFaults(ref.id);
    await waitFor(() => expect(result.current.processing).toBe(false), wait);
    const n = reads(r);
    await new Promise((resolve) => setTimeout(resolve, 200));
    expect(reads(r)).toBe(n);
  });

  it("reads again shortly before the URLs expire; refresh() joins a read in flight", async () => {
    const ref = bare(await item(h, "gallery", alice));
    await pages(ref, 1);
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const r = recording();
    const c = client(h, cfg, alice, { record: r });
    const { result } = renderHook(() => useMediaRead(ref, { client: c }));
    await waitFor(() => expect(result.current.read).not.toBeNull(), wait);
    expect(reads(r)).toBe(1);
    act(() => {
      result.current.refresh();
      result.current.refresh();
    });
    await waitFor(() => expect(result.current.loading).toBe(false), wait);
    expect(reads(r)).toBe(2);
    // Tokens last an hour; the read is renewed a minute before.
    const expires = result.current.read!.expires;
    expect(expires * 1000 - Date.now()).toBeGreaterThan(30 * 60_000);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(expires * 1000 - Date.now() - 59_000);
    });
    await waitFor(() => expect(reads(r)).toBe(3), wait);
  });

  it("reloads the item's viewer reads once a polled editor read shows its uploads processed", async () => {
    const ref = bare(await item(h, "gallery", alice));
    await h.faults([{ item: ref.id, fault: "hold" }]);
    const r = recording();
    const c = client(h, cfg, alice, { record: r });
    const up = await c.media.upload(png("1.png", 1401), { ref, path: "originals/001.png" });
    await c.media.commit(ref, [{ op: "put", path: up.path, blob: up.blob }]);
    const { result } = renderHook(() => ({ editor: useMediaRead(ref, { editor: true, poll: 50 }), viewer: useMediaRead(ref, { prefix: "low-res/" }) }), { wrapper: provider(c) });
    await waitFor(() => expect(result.current.editor.processing).toBe(true), wait);
    const viewerReads = () => r.queries.filter((q) => q.get("prefix") === "low-res/").length;
    expect(viewerReads()).toBe(1);
    expect(result.current.viewer.read?.files).toEqual([]);
    await h.clearFaults(ref.id);
    await waitFor(() => expect(result.current.viewer.read?.files.map((f) => f.path)).toEqual(["low-res/001.webp"]), wait);
    expect(viewerReads()).toBe(2);
  });
});
