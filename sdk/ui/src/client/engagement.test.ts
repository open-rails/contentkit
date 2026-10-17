import { expect, it, vi } from "vitest";
import { EngagementTracker, type EngagementTrackerOptions, type ReadingObservation } from "./engagement.js";
import type { ActivityContext, EngagementSnapshot } from "./generated/wire.js";

const context = (token = "receipt"): ActivityContext => ({ content_id: "gallery", version_id: "version", token, occurred_at: "2026-10-16T12:00:00Z" });
const page = (index = 0, from = 0, to = 10_000) => ({ index, ranges: [{ from, to }] });
function fixture(overrides: Partial<EngagementTrackerOptions> = {}) {
  let now = 0, sequence = 0;
  const sent: EngagementSnapshot[] = [], local: EngagementSnapshot[] = [];
  const begin = vi.fn(async () => context(`receipt-${now}`));
  const submit = vi.fn(async (snapshot: EngagementSnapshot) => { sent.push(structuredClone(snapshot)); });
  const tracker = new EngagementTracker({ contentId: "gallery", versionId: "version", now: () => now, id: () => `id-${++sequence}`,
    begin, submit, onSnapshot: (snapshot) => local.push(structuredClone(snapshot)), ...overrides });
  const observe = (observation: ReadingObservation) => tracker.observe(observation);
  return { tracker, sent, local, begin, submit, observe, at: (value: number) => { now = value; },
    reading: (pages = [page()]) => { observe({ pages, resume: "001.png" }); tracker.attention(true); } };
}

it("does not turn route opens, a clock tick or zero attention into engagement", async () => {
  const f = fixture();
  f.tracker.attention(true); f.at(30_000); f.tracker.tick(); await f.tracker.flush();
  expect(f.begin).not.toHaveBeenCalled(); expect(f.sent).toEqual([]); expect(f.local).toEqual([]);
  f.reading(); await f.tracker.flush();
  expect(f.begin).toHaveBeenCalledTimes(1); expect(f.sent).toEqual([]);
});

it("keeps short positive page partials for server-side cumulative qualification", async () => {
  const f = fixture(); f.reading(); f.at(600); await f.tracker.flush();
  expect(f.sent).toEqual([expect.objectContaining({ active_ms: 600, resume: "001.png", pages: [{ index: 0, active_ms: 600, ranges: [{ from: 0, to: 10_000 }] }] })]);
  expect(f.local[0]?.activity_token).toBe("");
});

it("unions page coverage and divides attention between simultaneously visible pages", async () => {
  const f = fixture(); f.reading([page(0, 0, 5000), page(1)]);
  f.at(1000); f.observe({ pages: [page(0, 4000, 10_000)], resume: "001.png" });
  f.at(2000); await f.tracker.flush();
  const snapshot = f.sent[0]!;
  expect(snapshot.active_ms).toBe(2000);
  expect(snapshot.pages).toEqual([{ index: 0, active_ms: 1500, ranges: [{ from: 0, to: 10_000 }] }, { index: 1, active_ms: 500, ranges: [{ from: 0, to: 10_000 }] }]);
  expect(snapshot.pages!.reduce((total, item) => total + item.active_ms, 0)).toBeLessThanOrEqual(snapshot.active_ms);
});

it("caps idle attention and never credits hidden or unfocused time", async () => {
  const f = fixture(); f.reading(); for (let at = 1000; at <= 70_000; at += 1000) { f.at(at); f.tracker.tick(); } await f.tracker.flush();
  expect(f.sent.at(-1)?.active_ms).toBe(60_000);
  f.tracker.attention(false); f.at(90_000); f.tracker.activity(); f.tracker.tick(); await f.tracker.flush();
  expect(f.sent).toHaveLength(1);
  f.tracker.attention(true); f.at(91_000); await f.tracker.flush();
  expect(f.sent.at(-1)?.active_ms).toBe(1000);
  expect(f.sent.reduce((total, snapshot) => total + snapshot.active_ms, 0)).toBe(61_000);
});

it("rotates access receipts every bounded segment while preserving one visit", async () => {
  const f = fixture({ idleAfter: 300_000 }); f.reading();
  for (let at = 1000; at <= 120_000; at += 1000) { f.at(at); f.tracker.tick(); } f.at(120_600); await f.tracker.flush();
  expect(f.begin).toHaveBeenCalledTimes(2);
  expect(f.sent).toHaveLength(2);
  expect(new Set(f.sent.map((snapshot) => snapshot.visit_id)).size).toBe(1);
  expect(new Set(f.sent.map((snapshot) => snapshot.segment_id)).size).toBe(2);
  expect(f.sent.map((snapshot) => snapshot.active_ms)).toEqual([120_000, 600]);
  expect(f.sent.map((snapshot) => snapshot.activity_token)).toEqual(["receipt-0", "receipt-120000"]);
});

it("retries the exact original revision and receipt without rebinding its audience", async () => {
  const f = fixture(); f.submit.mockRejectedValueOnce(new Error("offline")); f.reading();
  f.at(600); await f.tracker.flush(); await f.tracker.flush(true);
  expect(f.submit).toHaveBeenCalledTimes(2);
  expect(f.submit.mock.calls[1]?.[0]).toEqual(f.submit.mock.calls[0]?.[0]);
  expect(f.begin).toHaveBeenCalledTimes(1);
  expect(f.sent).toHaveLength(1);
});

it("keeps local positive partials when analytics is unavailable without sending tokenless packets", async () => {
  const f = fixture({ begin: async () => null }); f.reading(); f.at(600); await f.tracker.flush();
  expect(f.local).toHaveLength(1); expect(f.local[0]?.active_ms).toBe(600); expect(f.sent).toEqual([]);
});

it("drops a prior owner's unresolved receipt instead of sending it under another identity", async () => {
  let resolve!: (context: ActivityContext) => void;
  const f = fixture({ begin: () => new Promise((done) => { resolve = done; }) });
  f.reading(); f.at(600); const flushed = f.tracker.flush();
  f.tracker.discard(); resolve(context()); await flushed;
  expect(f.sent).toEqual([]);
});

it("rotates early rather than exceeding page or fragmented-coverage limits", async () => {
  const f = fixture({ idleAfter: 300_000 }); f.reading();
  for (let index = 0; index < 129; index++) { f.at(index * 200); f.observe({ pages: [page(index)] }); }
  f.at(129 * 200); await f.tracker.flush();
  expect(f.sent).toHaveLength(2);
  expect(f.sent.map((snapshot) => snapshot.pages?.length)).toEqual([128, 1]);
  expect(f.sent.every((snapshot) => snapshot.pages!.every((item) => item.active_ms > 0))).toBe(true);
  const fragmented = fixture(); fragmented.reading([page(0, 0, 100)]);
  for (let i = 1; i < 18; i++) { fragmented.at(i * 200); fragmented.observe({ pages: [page(0, i * 200, i * 200 + 100)] }); }
  fragmented.at(3600); await fragmented.tracker.flush();
  expect(fragmented.sent).toHaveLength(2);
  expect(fragmented.sent.every((snapshot) => snapshot.pages![0]!.ranges.length <= 16)).toBe(true);
});

it("uses actual played intervals, unions repeats, and excludes seek jumps", async () => {
  const f = fixture(); f.observe({ video: { positionMs: 0, playing: true } }); f.tracker.attention(true);
  f.at(1000); f.observe({ video: { positionMs: 1000, playing: true } });
  f.at(1100); f.observe({ video: { positionMs: 80_000, playing: true, seeking: true } });
  f.at(1200); f.observe({ video: { positionMs: 80_000, playing: true } });
  f.at(2200); f.observe({ video: { positionMs: 81_000, playing: false }, resume: "81" });
  await f.tracker.flush();
  expect(f.sent[0]?.watched).toEqual([{ from_ms: 0, to_ms: 1000 }, { from_ms: 80_000, to_ms: 81_000 }]);
  expect(f.sent[0]?.resume).toBe("81");
});

it("keeps genuine focused video playback active beyond the static-page idle budget", async () => {
  const f = fixture({ idleAfter: 1000 }); f.observe({ video: { positionMs: 0, playing: true } }); f.tracker.attention(true);
  for (let second = 1; second <= 4; second++) { f.at(second * 1000); f.observe({ video: { positionMs: second * 1000, playing: true } }); }
  await f.tracker.flush();
  expect(f.sent[0]?.active_ms).toBe(4000);
  expect(f.sent[0]?.watched).toEqual([{ from_ms: 0, to_ms: 4000 }]);
});

it("drops unobserved timer gaps instead of backfilling old attention under new access receipts", async () => {
  const f = fixture(); f.reading(); f.at(1000); f.tracker.tick();
  f.at(10_000); f.tracker.tick(); f.at(10_600); await f.tracker.flush();
  expect(f.sent.map((snapshot) => snapshot.active_ms)).toEqual([1000, 600]);
  expect(f.sent.map((snapshot) => snapshot.activity_token)).toEqual(["receipt-0", "receipt-10000"]);
  expect(new Set(f.sent.map((snapshot) => snapshot.visit_id)).size).toBe(1);
});

it("keeps one original receipt and cumulative revision identity while the visit grows", async () => {
  const f = fixture(); f.reading(); f.at(600); await f.tracker.flush();
  f.at(1000); f.observe({ pages: [page(1)], resume: "002.png" });
  f.at(1600); await f.tracker.flush();
  expect(f.begin).toHaveBeenCalledTimes(1);
  expect(f.sent.map((snapshot) => [snapshot.activity_token, snapshot.revision, snapshot.active_ms])).toEqual([
    ["receipt-0", 1, 600], ["receipt-0", 2, 1600],
  ]);
  expect(new Set(f.sent.map((snapshot) => snapshot.segment_id)).size).toBe(1);
  expect(f.sent[1]?.resume).toBe("002.png");
});

it("promotes a queued periodic checkpoint to keepalive when the page exits before dispatch", async () => {
  const submit = vi.fn(async (_snapshot: EngagementSnapshot, _options: { keepalive: boolean }) => {});
  const f = fixture({ checkpointInterval: 1000, submit }); f.reading(); f.at(1000); f.tracker.tick();
  await f.tracker.flush(true);
  expect(submit).toHaveBeenCalledTimes(1);
  expect(submit.mock.calls[0]?.[1]).toEqual({ keepalive: true });
});
