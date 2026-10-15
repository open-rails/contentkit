// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "./client.js";

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); localStorage.clear(); });

it("persists guest searches per app with deduplication and same-tab notifications", () => {
  const first = createContentKitClient({ baseUrl: "https://catalog.test/local-one" }).discovery.browserSearches;
  const second = createContentKitClient({ baseUrl: "https://catalog.test/local-one/" }).discovery.browserSearches;
  const other = createContentKitClient({ baseUrl: "https://catalog.test/local-two" }).discovery.browserSearches;
  const changed = vi.fn();
  const off = second.subscribe(changed);
  first.record("  Cats  ", 3);
  first.record("dogs", 2);
  first.record("cats", 4);
  expect(second.list().map(({ query, results }) => ({ query, results }))).toEqual([{ query: "cats", results: 4 }, { query: "dogs", results: 2 }]);
  expect(changed).toHaveBeenCalledTimes(3);
  expect(other.list()).toEqual([]);
  expect(localStorage.length).toBe(1);
  first.clear("CATS");
  expect(second.list().map((row) => row.query)).toEqual(["dogs"]);
  first.clear();
  expect(second.list()).toEqual([]);
  off();
});

it("loads stored history after reload, bounds it, and responds to cross-tab changes", async () => {
  const history = createContentKitClient({ baseUrl: "https://catalog.test/storage-events" }).discovery.browserSearches;
  for (let i = 0; i < 105; i++) history.record(`query ${i}`, i);
  expect(history.list(500)).toHaveLength(100);
  expect(history.list(2).map((row) => row.query)).toEqual(["query 104", "query 103"]);
  const key = Object.keys(localStorage)[0]!;
  expect(key).toBe("contentkit:recent-searches:v1:https://catalog.test/storage-events/v1");
  vi.resetModules();
  const { browserSearchHistory } = await import("./browser-search-history.js");
  const reloaded = browserSearchHistory("https://catalog.test/storage-events/v1");
  expect(reloaded.list(1)[0]?.query).toBe("query 104");
  const changed = vi.fn();
  const off = reloaded.subscribe(changed);
  const next = JSON.stringify([{ query: "other tab", results: 9, searched_at: "2026-10-15T12:00:00.000Z" }]);
  localStorage.setItem(key, next);
  window.dispatchEvent(new StorageEvent("storage", { key, newValue: next, storageArea: localStorage }));
  expect(reloaded.list().map((row) => row.query)).toEqual(["other tab"]);
  localStorage.removeItem(key);
  window.dispatchEvent(new StorageEvent("storage", { key, newValue: null, storageArea: localStorage }));
  expect(reloaded.list()).toEqual([]);
  expect(changed).toHaveBeenCalledTimes(2);
  off();
});

it("ignores corrupt stored data and keeps working when storage writes fail", () => {
  const key = "contentkit:recent-searches:v1:https://catalog.test/unavailable/v1";
  localStorage.setItem(key, "not JSON");
  const history = createContentKitClient({ baseUrl: "https://catalog.test/unavailable" }).discovery.browserSearches;
  expect(history.list()).toEqual([]);
  vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => { throw new DOMException("full", "QuotaExceededError"); });
  expect(() => history.record("local fallback", 2)).not.toThrow();
  expect(history.list()[0]?.query).toBe("local fallback");
  expect(() => history.clear()).not.toThrow();
  expect(history.list()).toEqual([]);
});

it("does not share guest searches between server-rendered clients", () => {
  vi.stubGlobal("window", undefined);
  const one = createContentKitClient({ baseUrl: "/ssr" }).discovery.browserSearches;
  const two = createContentKitClient({ baseUrl: "/ssr" }).discovery.browserSearches;
  one.record("request one", 1);
  expect(one.list()[0]?.query).toBe("request one");
  expect(two.list()).toEqual([]);
});

it("remains usable when the browser denies storage reads", () => {
  vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => { throw new DOMException("blocked", "SecurityError"); });
  const history = createContentKitClient({ baseUrl: "https://catalog.test/read-denied" }).discovery.browserSearches;
  expect(() => history.record("still searchable", 0)).not.toThrow();
  expect(history.list().map((row) => row.query)).toEqual(["still searchable"]);
});
