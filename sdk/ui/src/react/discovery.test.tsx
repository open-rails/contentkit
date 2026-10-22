// @vitest-environment jsdom
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../client/client.js";
import { ContentKitProvider } from "./provider.js";
import { useHistory, useRecentSearches, useSearch } from "./discovery.js";

afterEach(() => { cleanup(); localStorage.clear(); });

it("keeps completed guest searches locally without calling account history", async () => {
  const fetcher = vi.fn(async (url: RequestInfo | URL) => {
    expect(String(url)).not.toContain("/me/searches");
    expect(new URL(String(url)).searchParams.has("session")).toBe(false);
    return Response.json({ contents: [{ id: "one" }] });
  });
  const client = createContentKitClient({ baseUrl: "https://catalog.test/hook-guest", fetch: fetcher });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer={null}>{children}</ContentKitProvider>;
  const { result } = renderHook(() => ({ list: useSearch({ q: "  cats  " }), recent: useRecentSearches() }), { wrapper });
  await waitFor(() => expect(result.current.list.loading).toBe(false));
  await waitFor(() => expect(result.current.recent.searches.map(({ query, results }) => ({ query, results }))).toEqual([{ query: "cats", results: 1 }]));
  expect(fetcher).toHaveBeenCalledTimes(1);
  await act(() => result.current.recent.clear("cats"));
  expect(result.current.recent.searches).toEqual([]);
  expect(fetcher).toHaveBeenCalledTimes(1);
});

it("uses server history exclusively for accounts and refreshes it on focus and panel reopen", async () => {
  let enabled = true;
  let serverQuery = "account one";
  const fetcher = vi.fn(async () => Response.json({ searches: [{ query: serverQuery, results: 1, searched_at: "2026-10-15T12:00:00Z" }] }));
  const client = createContentKitClient({ baseUrl: "https://catalog.test/account-focus", fetch: fetcher });
  client.discovery.browserSearches.record("guest only", 2);
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer="alice">{children}</ContentKitProvider>;
  const { result, rerender } = renderHook(() => useRecentSearches({ enabled }), { wrapper });
  await waitFor(() => expect(result.current.searches.map((row) => row.query)).toEqual(["account one"]));
  expect(fetcher).toHaveBeenCalledTimes(1);
  serverQuery = "account two";
  act(() => window.dispatchEvent(new Event("focus")));
  await waitFor(() => expect(result.current.searches.map((row) => row.query)).toEqual(["account two"]));
  expect(fetcher).toHaveBeenCalledTimes(2);
  enabled = false; rerender();
  serverQuery = "account three";
  enabled = true; rerender();
  await waitFor(() => expect(result.current.searches.map((row) => row.query)).toEqual(["account three"]));
  expect(client.discovery.browserSearches.list().map((row) => row.query)).toEqual(["guest only"]);
});

it.each([false, true])("does not save a late guest search after an account transition (round trip: %s)", async (roundTrip) => {
  let viewer: string | null = null;
  let finish!: (response: Response) => void;
  const client = createContentKitClient({ baseUrl: `https://catalog.test/late-guest-${roundTrip}`, fetch: async (url) => String(url).includes("/search?")
    ? new Promise<Response>((resolve) => { finish = resolve; })
    : Response.json({ searches: [{ query: "account only", results: 4, searched_at: "2026-10-15T12:00:00Z" }] }),
  });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer={viewer}>{children}</ContentKitProvider>;
  const { result, rerender } = renderHook(() => ({ list: useSearch({ q: "old guest" }), recent: useRecentSearches() }), { wrapper });
  await waitFor(() => expect(finish).toBeTypeOf("function"));
  const oldFinish = finish;
  viewer = "alice"; rerender();
  await waitFor(() => expect(result.current.recent.searches[0]?.query).toBe("account only"));
  if (roundTrip) { viewer = null; rerender(); }
  await act(async () => { oldFinish(Response.json({ contents: [{ id: "old" }] })); });
  expect(client.discovery.browserSearches.list()).toEqual([]);
  expect(result.current.recent.searches.map((row) => row.query)).toEqual(roundTrip ? [] : ["account only"]);
});

it("ignores old account history responses and stale clear callbacks after switching users", async () => {
  let viewer: string | null = "alice";
  let finish!: (response: Response) => void;
  let reads = 0, deletes = 0;
  const client = createContentKitClient({ baseUrl: "https://catalog.test/late-account", fetch: async (_url, init) => {
    if (init?.method === "DELETE") { deletes++; return new Response(null, { status: 204 }); }
    if (++reads === 1) return new Promise<Response>((resolve) => { finish = resolve; });
    return Response.json({ searches: [{ query: "bob only", results: 2, searched_at: "2026-10-15T12:00:00Z" }] });
  } });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer={viewer}>{children}</ContentKitProvider>;
  const { result, rerender } = renderHook(() => useRecentSearches(), { wrapper });
  await waitFor(() => expect(finish).toBeTypeOf("function"));
  const oldClear = result.current.clear;
  viewer = "bob"; rerender();
  await waitFor(() => expect(result.current.searches[0]?.query).toBe("bob only"));
  await act(async () => {
    finish(Response.json({ searches: [{ query: "alice secret", results: 3, searched_at: "2026-10-15T12:00:00Z" }] }));
    await oldClear();
  });
  expect(result.current.searches.map((row) => row.query)).toEqual(["bob only"]);
  expect(deletes).toBe(0);
  viewer = null; rerender();
  expect(result.current.searches).toEqual([]);
  expect(client.discovery.browserSearches.list()).toEqual([]);
});

it("records the first completed page once, and never records a failed search", async () => {
  let q = "pages";
  const client = createContentKitClient({ baseUrl: "https://catalog.test/page-history", fetch: async (url) => {
    const params = new URL(String(url)).searchParams;
    if (params.get("q") === "failed") return Response.json({ code: "unavailable", error: "unavailable" }, { status: 503 });
    return Response.json(params.has("cursor") ? { contents: [{ id: "two" }, { id: "three" }] } : { contents: [{ id: "one" }], next_cursor: "next" });
  } });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer={null}>{children}</ContentKitProvider>;
  const { result, rerender } = renderHook(() => ({ list: useSearch({ q }), recent: useRecentSearches() }), { wrapper });
  await waitFor(() => expect(result.current.list.items).toHaveLength(1));
  await act(() => result.current.list.loadMore());
  expect(result.current.list.items).toHaveLength(3);
  expect(result.current.recent.searches.map(({ query, results }) => ({ query, results }))).toEqual([{ query: "pages", results: 1 }]);
  q = "failed"; rerender();
  await waitFor(() => expect(result.current.list.error?.code).toBe("unavailable"));
  expect(result.current.recent.searches.map((row) => row.query)).toEqual(["pages"]);
});

it("updates visible guest recents on a storage event without an API call", async () => {
  const fetcher = vi.fn();
  const client = createContentKitClient({ baseUrl: "https://catalog.test/live-storage", fetch: fetcher });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer={null}>{children}</ContentKitProvider>;
  const { result } = renderHook(() => useRecentSearches(), { wrapper });
  const key = "contentkit:recent-searches:v1:https://catalog.test/live-storage/v1";
  const value = JSON.stringify([{ query: "other tab", results: 1, searched_at: "2026-10-15T12:00:00Z" }]);
  act(() => {
    localStorage.setItem(key, value);
    window.dispatchEvent(new StorageEvent("storage", { key, newValue: value, storageArea: localStorage }));
  });
  await waitFor(() => expect(result.current.searches[0]?.query).toBe("other tab"));
  expect(fetcher).not.toHaveBeenCalled();
});

it("lets an authenticated search refresh server recents without adding to guest history", async () => {
  let searches: { query: string; results: number; searched_at: string }[] = [];
  const client = createContentKitClient({ baseUrl: "https://catalog.test/account-search", fetch: async (url) => {
    const path = new URL(String(url));
    if (path.pathname.endsWith("/search")) {
      searches = [{ query: path.searchParams.get("q")!, results: 0, searched_at: "2026-10-15T12:00:00Z" }];
      return Response.json({ contents: [] });
    }
    return Response.json({ searches });
  } });
  client.discovery.browserSearches.record("guest retained", 1);
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer="alice">{children}</ContentKitProvider>;
  const { result } = renderHook(() => ({ list: useSearch({ q: "account query" }), recent: useRecentSearches() }), { wrapper });
  await waitFor(() => expect(result.current.recent.searches[0]?.query).toBe("account query"));
  expect(client.discovery.browserSearches.list().map((row) => row.query)).toEqual(["guest retained"]);
});


it("hydrates guest reading IDs through public policy and keeps account history authoritative", async () => {
  let viewer: string | null = null;
  const calls: URL[] = [];
  const client = createContentKitClient({ baseUrl: "https://catalog.test/guest-reading", fetch: async input => {
    const url = new URL(String(input)); calls.push(url);
    if (url.pathname.endsWith("/me/history")) return Response.json({ contents: [{ id: "account-only" }] });
    expect(url.pathname.endsWith("/content-cards")).toBe(true);
    expect(url.searchParams.get("ids")!.split(",").sort()).toEqual(["allowed", "now-restricted"]);
    return Response.json({ contents: [{ id: "allowed", kind: "gallery", version: { id: "edition", pages: 2 } }] });
  } });
  for (const content_id of ["allowed", "now-restricted"]) client.guestHistory.record({ content_id, version_id: "edition", visit_id: "visit", segment_id: content_id,
    revision: 1, active_ms: 1500, activity_token: "", pages: [{ index: 1, active_ms: 1500, ranges: [{ from: 0, to: 10_000 }] }], resume: "002", kind: "gallery" });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer={viewer}>{children}</ContentKitProvider>;
  const { result, rerender } = renderHook(() => useHistory({ kind: "gallery" }), { wrapper });
  await waitFor(() => expect(result.current.items.map(row => row.id)).toEqual(["allowed"]));
  expect(result.current.items[0]!.viewer).toMatchObject({ seen: true, completed: false, progress: 1, progress_max: 2, resume: "002" });
  expect(calls.some(url => url.pathname.endsWith("/me/history"))).toBe(false);
  const staleForget = result.current.forget;
  viewer = "account"; rerender();
  await waitFor(() => expect(result.current.items.map(row => row.id)).toEqual(["account-only"]));
  await act(() => staleForget());
  expect(client.guestHistory.snapshot()).toHaveLength(2);
  viewer = null; rerender();
  await waitFor(() => expect(result.current.items.map(row => row.id)).toEqual(["allowed"]));
  await act(() => result.current.forget());
  await waitFor(() => expect(result.current.items).toEqual([]));
});

it("keeps a previously read gallery in guest history through bounded known-ID hydration", async () => {
  const calls: URL[] = [];
  const client = createContentKitClient({ baseUrl: "https://catalog.test/known-history", fetch: async input => {
    const url = new URL(String(input)); calls.push(url);
    if (url.pathname.endsWith("/content-cards")) {
      expect(url.searchParams.get("ids")).toBe("known-gallery");
      expect(url.searchParams.get("kind")).toBe("gallery");
      expect(url.searchParams.get("mode")).toBe("hide");
      return Response.json({ contents: [{ id: "known-gallery", kind: "gallery", version: { id: "edition", pages: 2 } }] });
    }
    return Response.json({ contents: [] });
  } });
  client.guestHistory.record({ content_id: "known-gallery", version_id: "edition", visit_id: "visit", segment_id: "segment",
    revision: 1, active_ms: 1500, activity_token: "", pages: [{ index: 0, active_ms: 1500, ranges: [{ from: 0, to: 10_000 }] }], resume: "001", kind: "gallery" });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer={null}>{children}</ContentKitProvider>;
  const { result } = renderHook(() => useHistory({ kind: "gallery", mode: "hide" }), { wrapper });
  await waitFor(() => expect(result.current.items.map(row => row.id)).toEqual(["known-gallery"]));
  expect(calls.map(url => url.pathname)).toEqual(["/known-history/v1/content-cards"]);
});

it("bounds guest history summary hydration to the newest 100 known IDs", async () => {
  const client = createContentKitClient({ baseUrl: "https://catalog.test/bounded-history", fetch: async input => {
    const url = new URL(String(input));
    expect(url.pathname.endsWith("/content-cards")).toBe(true);
    const ids = url.searchParams.get("ids")!.split(",");
    expect(ids).toHaveLength(100);
    expect(ids[0]).toBe("known-100");
    expect(ids).not.toContain("known-0");
    expect(url.searchParams.get("summary")).toBe("true");
    return Response.json({ contents: [{ id: ids[0], kind: "gallery", version: { id: "edition", pages: 1 } }], summary: { total: 1, filtered_count: 0, filtered_entities: [], locked_count: 0 } });
  } });
  const now = vi.spyOn(Date, "now");
  try {
    for (let n = 0; n < 101; n++) {
      now.mockReturnValue(1_000 + n);
      client.guestHistory.record({ content_id: `known-${n}`, version_id: "edition", visit_id: "visit", segment_id: `segment-${n}`,
        revision: 1, active_ms: 1500, activity_token: "", pages: [{ index: 0, active_ms: 1500, ranges: [{ from: 0, to: 10_000 }] }], resume: "001", kind: "gallery" });
    }
  } finally {
    now.mockRestore();
  }
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer={null}>{children}</ContentKitProvider>;
  const { result } = renderHook(() => useHistory({ kind: "gallery", summary: true, limit: 10 }), { wrapper });
  await waitFor(() => expect(result.current.items.map(row => row.id)).toEqual(["known-100"]));
  expect(result.current.summary?.total).toBe(1);
});
