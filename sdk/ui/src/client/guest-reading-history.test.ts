// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { GuestReadingHistory, guestStanding } from "./guest-reading-history.js";
import type { Card, EngagementSnapshot } from "./generated/wire.js";

afterEach(() => { localStorage.clear(); vi.restoreAllMocks(); });
const snapshot = (patch: Partial<EngagementSnapshot> = {}): EngagementSnapshot => ({ content_id: "gallery", version_id: "edition", visit_id: "visit", segment_id: "segment", revision: 1,
  activity_token: "private-receipt-never-persist", active_ms: 600, pages: [{ index: 9, active_ms: 600, ranges: [{ from: 0, to: 10_000 }] }], resume: "010", ...patch });
const card = { version: { id: "edition", pages: 10 } } as Card;

it("replaces retry revisions, accumulates separate visits and counts actual pages rather than highest page index", () => {
  const history = new GuestReadingHistory("reading:", window);
  history.record(snapshot()); history.record(snapshot());
  history.record(snapshot({ visit_id: "another" }));
  expect(history.snapshot()[0]!.checkpoints).toHaveLength(2);
  const viewer = guestStanding(card, history.snapshot()[0]!);
  expect(viewer.progress).toBe(100); expect(viewer.progress_max).toBe(1000); expect(viewer.completed).toBe(false);
  history.record(snapshot({ revision: 2, active_ms: 1600, pages: [{ index: 9, active_ms: 1600, ranges: [{ from: 0, to: 10_000 }] }] }));
  expect(history.snapshot()[0]!.checkpoints).toHaveLength(2);
  expect(JSON.stringify([...Object.values(localStorage)])).not.toContain("private-receipt");
});

it("preserves independent stale-tab writes, reloads and storage events, and isolates catalogs", () => {
  const a = new GuestReadingHistory("app:", window), b = new GuestReadingHistory("app:", window), other = new GuestReadingHistory("other:", window);
  const changed = vi.fn(), off = b.subscribe(changed);
  a.record(snapshot()); b.record(snapshot({ content_id: "second", visit_id: "second" }));
  window.dispatchEvent(new StorageEvent("storage", { key: Object.keys(localStorage)[0], storageArea: localStorage }));
  expect(b.snapshot().map(row => row.id).sort()).toEqual(["gallery", "second"]);
  expect(new GuestReadingHistory("app:", window).snapshot()).toHaveLength(2);
  expect(other.snapshot()).toEqual([]); expect(changed).toHaveBeenCalled();
  b.clear({ content_id: "gallery" }); expect(b.snapshot().map(row => row.id)).toEqual(["second"]); off();
});

it("does not claim a whole tall page from a small visible part or reuse another version's progress", () => {
  const history = new GuestReadingHistory("reading:", window);
  history.record(snapshot({ active_ms: 2000, pages: [{ index: 0, active_ms: 2000, ranges: [{ from: 0, to: 1000 }] }] }));
  expect(guestStanding(card, history.snapshot()[0]!).progress).toBe(10);
  expect(guestStanding({ version: { id: "replacement", pages: 10 } } as Card, history.snapshot()[0]!).seen).toBe(false);
});

it("ignores zero attention/corrupt stored geometry and remains usable when localStorage is unavailable", () => {
  localStorage.setItem("reading:bad", JSON.stringify({ ...snapshot(), saved_at: Date.now(), pages: [{ index: 0, active_ms: 1000, ranges: null }] }));
  const history = new GuestReadingHistory("reading:", window);
  expect(history.snapshot()).toEqual([]);
  history.record(snapshot({ active_ms: 0 })); expect(history.snapshot()).toEqual([]);
  vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => { throw new DOMException("blocked"); });
  history.record(snapshot()); expect(history.snapshot()).toHaveLength(1);
  history.clear(); expect(history.snapshot()).toEqual([]);
});
