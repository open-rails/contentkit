import assert from "node:assert/strict";
import { existsSync } from "node:fs";
import { registerHooks } from "node:module";
import test from "node:test";
import { fileURLToPath } from "node:url";
import type { ActivityEvent } from "../src/client/generated/wire.ts";

// The sources import each other as .js (bundler resolution); run them from .ts.
registerHooks({
  resolve(specifier, context, next) {
    if (specifier.startsWith(".") && specifier.endsWith(".js") && context.parentURL) {
      const ts = new URL(specifier.replace(/\.js$/, ".ts"), context.parentURL);
      if (existsSync(fileURLToPath(ts))) return next(ts.href, context);
    }
    return next(specifier, context);
  },
});

// A browser's local storage, shared by the tests.
const storage = new Map<string, string>();
Object.assign(globalThis, {
  localStorage: {
    getItem: (k: string) => storage.get(k) ?? null,
    setItem: (k: string, v: string) => void storage.set(k, v),
    removeItem: (k: string) => void storage.delete(k),
  },
});

const { ProgressTracker, ActivityReporter } = await import("../src/client/activity.ts");
const { anonymousId } = await import("../src/client/anonymous.ts");
const { Http } = await import("../src/client/http.ts");

type Report = { active_ms: number; furthest: number; current: number; length: number; keepalive: boolean };

function tracker(o: { idleAfter?: number; every?: number } = {}) {
  let now = 0;
  const reports: Report[] = [];
  const t = new ProgressTracker({ contentId: "c", now: () => now, ...o, report: (p, keepalive) => reports.push({ ...p, keepalive }) });
  return { t, reports, advance: (ms: number) => { now += ms; } };
}

test("active time counts only while the tab is visible and the reader is active", () => {
  const { t, reports, advance } = tracker({ idleAfter: 60_000 });
  t.point(3, 20);
  advance(30_000);
  t.activity();
  advance(120_000); // idle after 60s of the last input
  assert.equal(t.pending(), 90_000);
  t.setVisible(false); // leaving reports at once, with keepalive
  assert.equal(reports.length, 1);
  assert.deepEqual(reports[0], { active_ms: 90_000, furthest: 3, current: 3, length: 20, keepalive: true });
  advance(600_000); // hidden: nothing accrues
  t.setVisible(true);
  advance(10_000);
  assert.equal(t.pending(), 10_000);
});

test("playback keeps the reader active without input", () => {
  const { t, advance } = tracker({ idleAfter: 1_000 });
  t.setPlaying(true);
  advance(240_000);
  t.point(240, 600);
  assert.equal(t.pending(), 240_000);
  t.setPlaying(false);
  advance(5_000); // one second of grace after playback stops
  assert.equal(t.pending(), 241_000);
});

test("reports every five active minutes and skips unchanged reports", () => {
  const { t, reports, advance } = tracker({ idleAfter: 60_000 });
  t.point(1, 10);
  for (let i = 0; i < 300; i++) { advance(1_000); t.activity(); t.tick(); }
  assert.equal(reports.length, 1);
  assert.equal(reports[0]!.active_ms, 300_000);
  assert.equal(reports[0]!.keepalive, false);
  t.leave(); // nothing changed since: no report
  assert.equal(reports.length, 1);
  t.point(4);
  t.leave();
  assert.equal(reports.length, 2);
  assert.deepEqual({ furthest: reports[1]!.furthest, current: reports[1]!.current }, { furthest: 4, current: 4 });
  t.point(2);
  t.stop();
  assert.deepEqual({ furthest: reports[2]!.furthest, current: reports[2]!.current }, { furthest: 4, current: 2 });
  t.point(9);
  t.leave(); // a stopped reading reports nothing more
  assert.equal(reports.length, 3);
});

test("points stay unreported until the length is known", () => {
  const { t, reports, advance } = tracker();
  t.point(5);
  advance(2_000);
  t.leave();
  assert.deepEqual(reports[0], { active_ms: 2_000, furthest: 0, current: 0, length: 0, keepalive: true });
});

test("the reporter batches opens, skips a repeated open and stops when the site logs nothing", async () => {
  const sent: ActivityEvent[][] = [];
  let signals = true;
  const client = {
    kinds: async () => ({ signals }) as never,
    discovery: { activity: async (events: ActivityEvent[]) => void sent.push(events) } as never,
  };
  let now = 0;
  const r = new ActivityReporter(client, { delay: 60_000 }, () => now);
  r.open("a", { requestId: "11111111-1111-4111-8111-111111111111", surface: "search", position: 2 });
  r.open("a"); // the reader page's own open, just after the card's
  r.progress("a", { active_ms: 9_999_999, furthest: 1, current: 1, length: 3 });
  await r.flush();
  assert.equal(sent.length, 1);
  assert.deepEqual(sent[0], [
    { type: "open", content_id: "a", request_id: "11111111-1111-4111-8111-111111111111", surface: "search", position: 2 },
    { type: "progress", content_id: "a", active_ms: 360_000, furthest: 1, current: 1, length: 3 },
  ]);
  now += 31_000;
  r.open("a"); // a later open counts again
  await r.flush();
  assert.equal(sent.length, 2);
  const off = new ActivityReporter({ ...client, kinds: async () => { signals = false; return { signals } as never; } }, {}, () => now);
  off.open("b");
  await off.flush();
  assert.equal(sent.length, 2);
});

test("signing in links the anonymous id once and starts a fresh one; signing out starts another", async () => {
  storage.clear();
  const calls: { url: string; headers: Headers; body?: string }[] = [];
  const fetch = async (url: string | URL | Request, init?: RequestInit) => {
    calls.push({ url: String(url), headers: new Headers(init?.headers), body: init?.body ? String(init.body) : undefined });
    return new Response(null, { status: 204 });
  };
  const http = new Http({ baseUrl: "https://api.test", fetch: fetch as typeof globalThis.fetch });
  http.setViewer(null);
  await http.request("https://api.test/v1/contents", { guestPreferences: false });
  const used = calls[0]!.headers.get("ContentKit-Anonymous-Id");
  assert.equal(used, anonymousId());
  assert.match(calls[0]!.headers.get("ContentKit-UTC-Offset") ?? "", /^-?\d+$/);

  http.setViewer("account-1");
  await new Promise((resolve) => setImmediate(resolve));
  const link = calls.find((c) => c.url.endsWith("/v1/me/anonymous-link"));
  assert.ok(link, "the sign-in link is sent");
  assert.deepEqual(JSON.parse(link.body!), { anonymous_id: used });
  assert.equal(link.headers.get("ContentKit-Anonymous-Id"), null);
  const fresh = anonymousId();
  assert.notEqual(fresh, used);

  await http.request("https://api.test/v1/contents", { guestPreferences: false });
  assert.equal(calls.at(-1)!.headers.get("ContentKit-Anonymous-Id"), null, "an account's requests carry no anonymous id");

  http.setViewer(null);
  assert.notEqual(anonymousId(), fresh, "signing out starts a fresh anonymous id");
  const links = calls.filter((c) => c.url.endsWith("/v1/me/anonymous-link")).length;
  http.setViewer("account-1"); // the fresh id was never sent: nothing to link
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(calls.filter((c) => c.url.endsWith("/v1/me/anonymous-link")).length, links);
});
