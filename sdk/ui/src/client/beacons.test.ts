import { afterEach, expect, it, vi } from "vitest";
import { Beacons } from "./beacons.js";
import { createContentKitClient } from "./client.js";

// A site whose signal plane is up or down (signals: false, sends refused as unavailable).
const site = (signals: boolean) => {
  const s = { up: signals, sent: [] as string[], client: undefined as unknown as ReturnType<typeof createContentKitClient> };
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = new URL(String(input), "http://x").pathname;
    s.sent.push(`${init?.method ?? "GET"} ${path}`);
    if (path === "/v1/kinds") return Response.json({ languages: ["en"], tiers: [], kinds: {}, entity_kinds: {}, signals: s.up });
    if (path === "/v1/activity-contexts") return Response.json({ enabled: false, contexts: [] });
    if (!s.up) return Response.json({ error: "Signals are unavailable right now; try again later.", code: "unavailable" }, { status: 503 });
    return new Response(null, { status: 204 });
  });
  s.client = createContentKitClient({ baseUrl: "", fetch: fetch as typeof globalThis.fetch });
  return s;
};

afterEach(() => void vi.useRealTimers());

it("sends signals and exposures only where the content model says the site records them", async () => {
  for (const signals of [false, true]) {
    const s = site(signals);
    const b = new Beacons(s.client, { delay: 60_000, session: () => "s1" });
    b.click("c1", { surface: "home", position: 1 });
    b.exposure({ render_id: "r1", stage: "rendered", shown: [{ content_id: "c1", position: 1 }] });
    await b.flush();
    b.click("c2");
    await b.flush();
    const beacons = s.sent.filter((x) => /^POST .*\/(signals|exposures)$/.test(x));
    expect(s.sent.filter((x) => x === "GET /v1/kinds")).toHaveLength(1);
    expect(beacons).toEqual(signals ? ["POST /v1/signals", "POST /v1/exposures", "POST /v1/signals"] : []);
  }
});

it("follows the signal plane as it goes down and comes back, reading the content model again after each pause", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  const s = site(false);
  const b = new Beacons(s.client, { delay: 60_000, recheck: 30_000, session: () => "s1" });
  const posts = () => s.sent.filter((x) => /^POST .*\/(signals|exposures)$/.test(x)).length;
  const models = () => s.sent.filter((x) => x === "GET /v1/kinds").length;

  b.click("c1");
  await b.flush();
  expect([posts(), models()]).toEqual([0, 1]);
  s.up = true;
  b.click("c2");
  await b.flush();
  expect([posts(), models()]).toEqual([0, 1]);

  vi.setSystemTime(Date.now() + 30_000);
  b.click("c3");
  await b.flush();
  expect([posts(), models()]).toEqual([1, 2]);

  s.up = false;
  b.click("c4");
  await b.flush();
  b.click("c5");
  await b.flush();
  expect(posts()).toBe(2);

  s.up = true;
  vi.setSystemTime(Date.now() + 30_000);
  b.click("c6");
  await b.flush();
  expect([posts(), models()]).toEqual([3, 3]);
});

it("keeps a view's first observation time across cumulative revisions", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  const records: { occurred_at: string; revision: number; progress: number }[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/view-time", fetch: async (url, init) => {
    if (String(url).endsWith("/kinds")) return Response.json({ signals: true });
    if (String(url).endsWith("/activity-contexts")) return Response.json({ enabled: false, contexts: [] });
    records.push(...JSON.parse(String(init?.body)).signals);
    return new Response(null, { status: 204 });
  } });
  const b = new Beacons(client, { delay: 60_000 });
  const view = b.view("gallery", "version");
  vi.setSystemTime(new Date("2026-10-15T23:59:00Z"));
  view.update({ progress: 1 });
  await b.flush();
  vi.setSystemTime(new Date("2026-10-16T00:05:00Z"));
  view.update({ progress: 5 });
  await b.flush();
  expect(records.map((r) => r.occurred_at)).toEqual(["2026-10-15T23:59:00.000Z", "2026-10-15T23:59:00.000Z"]);
  expect(records.map((r) => [r.revision, r.progress])).toEqual([[1, 1], [2, 5]]);
});

it.each([["alice", "bob"], ["alice", null], [null, "bob"]] as const)("drops buffered and old-session events across identity transition %s to %s", async (from, to) => {
  const sent: unknown[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/identity", fetch: async (url, init) => {
    if (String(url).endsWith("/kinds")) return Response.json({ signals: true });
    if (String(url).endsWith("/activity-contexts")) return Response.json({ enabled: false, contexts: [] });
    sent.push(JSON.parse(String(init?.body)));
    return new Response(null, { status: 204 });
  } });
  client.preferences.configure(from);
  const b = new Beacons(client, { delay: 60_000, session: () => "guest" });
  const old = b.view("old-content");
  old.update({ progress: 1 });
  b.exposure({ render_id: "old-render", stage: "rendered", shown: [{ content_id: "old-content", position: 1 }] });
  client.preferences.configure(to);
  old.update({ progress: 2 });
  b.view("new-content").update({ progress: 3 });
  await b.flush();
  expect(sent).toHaveLength(1);
  expect(sent[0]).toMatchObject({ signals: [{ content_id: "new-content", progress: 3 }] });
  client.preferences.configure(from);
  old.update({ progress: 4 });
  await b.flush();
  expect(sent).toHaveLength(1);
});

it("discards an old identity's flush when the model lookup completes late", async () => {
  let finish!: (r: Response) => void;
  const posts: unknown[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/late-model", fetch: async (url, init) => {
    if (String(url).endsWith("/kinds")) return new Promise<Response>((resolve) => { finish = resolve; });
    posts.push(JSON.parse(String(init?.body)));
    return new Response(null, { status: 204 });
  } });
  client.preferences.configure("alice");
  const b = new Beacons(client, { delay: 60_000 });
  b.click("old-content");
  const pending = b.flush();
  await vi.waitFor(() => expect(finish).toBeTypeOf("function"));
  client.preferences.configure("bob");
  finish(Response.json({ signals: true }));
  await pending;
  expect(posts).toEqual([]);
});

it("acquires a view receipt before flush and preserves its server time and token", async () => {
  const contexts: unknown[] = [], signals: { occurred_at: string; activity_token: string; revision: number }[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/receipt", fetch: async (url, init) => {
    if (String(url).endsWith("/kinds")) return Response.json({ signals: true });
    const body = JSON.parse(String(init?.body));
    if (String(url).endsWith("/activity-contexts")) {
      contexts.push(body);
      return Response.json({ enabled: true, contexts: body.targets.map((t: object) => ({ ...t, token: "signed-receipt", occurred_at: "2026-10-15T12:00:00Z" })) });
    }
    signals.push(...body.signals);
    return new Response(null, { status: 204 });
  } });
  const b = new Beacons(client, { delay: 60_000 });
  const view = b.view("content", "version");
  expect(contexts).toEqual([]);
  view.update({ progress: 2 });
  await vi.waitFor(() => expect(contexts).toHaveLength(1));
  expect(signals).toEqual([]);
  await b.flush();
  view.update({ progress: 5 });
  await b.flush();
  expect(contexts).toHaveLength(1);
  expect(signals.map((s) => [s.activity_token, s.occurred_at, s.revision])).toEqual([
    ["signed-receipt", "2026-10-15T12:00:00Z", 1], ["signed-receipt", "2026-10-15T12:00:00Z", 2],
  ]);
});

it.each(["failed", "invalid"] as const)("does not downgrade a %s receipt to an untrusted signal", async (failure) => {
  let posted = false;
  const client = createContentKitClient({ baseUrl: "https://content.test/receipt-refused", fetch: async (url) => {
    if (String(url).endsWith("/kinds")) return Response.json({ signals: true });
    if (String(url).endsWith("/activity-contexts")) return failure === "failed" ? new Response(null, { status: 503 }) : Response.json({ enabled: true, contexts: [] });
    posted = true;
    return new Response(null, { status: 204 });
  } });
  const b = new Beacons(client);
  b.view("content").update({ progress: 2 });
  await b.flush();
  expect(posted).toBe(false);
});

it("splits receipt batches at100 and keeps per-placement tokens in cumulative exposures", async () => {
  const batches: number[] = [], sent: { shown: { activity_token: string }[] }[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/exposure-receipts", fetch: async (url, init) => {
    if (String(url).endsWith("/kinds")) return Response.json({ signals: true });
    const body = JSON.parse(String(init?.body));
    if (String(url).endsWith("/activity-contexts")) {
      batches.push(body.targets.length);
      return Response.json({ enabled: true, contexts: body.targets.map((t: { content_id: string }) => ({ ...t, token: `signed-${t.content_id}`, occurred_at: "2026-10-15T12:00:00Z" })) });
    }
    sent.push(...body.exposures);
    return new Response(null, { status: 204 });
  } });
  const b = new Beacons(client, { delay: 60_000 });
  const shown = Array.from({ length: 200 }, (_, i) => ({ content_id: `content-${i}`, position: i + 1 }));
  b.exposure({ render_id: "render", stage: "visible", revision: 1, shown: shown.slice(0, 150) });
  await b.flush();
  b.exposure({ render_id: "render", stage: "visible", revision: 2, shown });
  await b.flush();
  expect(batches).toEqual([100, 50, 50]);
  expect(sent.map((x) => x.shown.length)).toEqual([150, 200]);
  expect(sent[1]?.shown.every((x) => x.activity_token.startsWith("signed-content-"))).toBe(true);
});

it("acquires an idle view's receipt on its first update after midnight", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  const at: string[] = [], sent: { occurred_at: string }[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/idle-view", fetch: async (url, init) => {
    if (String(url).endsWith("/kinds")) return Response.json({ signals: true });
    const body = JSON.parse(String(init?.body));
    if (String(url).endsWith("/activity-contexts")) {
      const time = new Date().toISOString(); at.push(time);
      return Response.json({ enabled: true, contexts: body.targets.map((t: object) => ({ ...t, token: "receipt", occurred_at: time })) });
    }
    sent.push(...body.signals);
    return new Response(null, { status: 204 });
  } });
  const b = new Beacons(client, { delay: 60_000 });
  vi.setSystemTime(new Date("2026-10-15T23:55:00Z"));
  const view = b.view("content");
  expect(at).toEqual([]);
  vi.setSystemTime(new Date("2026-10-16T00:05:00Z"));
  view.update({ progress: 1 });
  await b.flush();
  expect(at).toEqual(["2026-10-16T00:05:00.000Z"]);
  expect(sent[0]?.occurred_at).toBe(at[0]);
});

it("discards an old account's late activity receipt", async () => {
  let finish!: (response: Response) => void;
  const sent: unknown[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/late-receipt", fetch: async (url, init) => {
    if (String(url).endsWith("/kinds")) return Response.json({ signals: true });
    if (String(url).endsWith("/activity-contexts")) return new Promise<Response>((resolve) => { finish = resolve; });
    sent.push(JSON.parse(String(init?.body)));
    return new Response(null, { status: 204 });
  } });
  client.preferences.configure("alice");
  const b = new Beacons(client);
  b.view("content").update({ progress: 1 });
  await vi.waitFor(() => expect(finish).toBeTypeOf("function"));
  const pending = b.flush();
  client.preferences.configure("bob");
  finish(Response.json({ enabled: true, contexts: [{ content_id: "content", token: "alice-proof", occurred_at: "2026-10-15T12:00:00Z" }] }));
  await pending;
  expect(sent).toEqual([]);
});

it("binds the request or explicitly displayed language without sending language as a client signal claim", async () => {
  const languages: string[] = [], events: object[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/languages", language: () => "ja", fetch: async (url, init) => {
    if (String(url).endsWith("/kinds")) return Response.json({ signals: true });
    const body = JSON.parse(String(init?.body));
    if (String(url).endsWith("/activity-contexts")) { languages.push(body.language); return Response.json({ enabled: false, contexts: [] }); }
    events.push(...(body.signals ?? []));
    return new Response(null, { status: 204 });
  } });
  const b = new Beacons(client);
  b.view("first").update({ progress: 1 });
  b.click("second", { language: "de" });
  b.exposure({ render_id: "r", stage: "visible", language: "es", shown: [{ content_id: "third", position: 1 }] });
  await b.flush();
  expect(languages.sort()).toEqual(["de", "es", "ja"]);
  expect(events.every((event) => !("language" in event))).toBe(true);
});

it("accepts explicit no-signer mode without requiring a receipt array", async () => {
  let sent = false;
  const client = createContentKitClient({ baseUrl: "https://content.test/no-signer", fetch: async (url) => {
    if (String(url).endsWith("/kinds")) return Response.json({ signals: true });
    if (String(url).endsWith("/activity-contexts")) return Response.json({ enabled: false });
    sent = true;
    return new Response(null, { status: 204 });
  } });
  const b = new Beacons(client);
  b.view("content").update({ progress: 1 });
  await b.flush();
  expect(sent).toBe(true);
});

it("retries a failed receipt on a later observation of the same view", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  let attempts = 0;
  const sent: { progress: number; activity_token: string }[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/recover-receipt", fetch: async (url, init) => {
    if (String(url).endsWith("/kinds")) return Response.json({ signals: true });
    const body = JSON.parse(String(init?.body));
    if (String(url).endsWith("/activity-contexts")) {
      if (++attempts === 1) return Response.json({ code: "unavailable", error: "Retry later" }, { status: 503 });
      return Response.json({ enabled: true, contexts: body.targets.map((t: object) => ({ ...t, token: "recovered", occurred_at: new Date().toISOString() })) });
    }
    sent.push(...body.signals);
    return new Response(null, { status: 204 });
  } });
  const b = new Beacons(client, { recheck: 30_000 });
  const view = b.view("content");
  view.update({ progress: 1 });
  await b.flush();
  expect(sent).toEqual([]);
  vi.setSystemTime(Date.now() + 30_000);
  view.update({ progress: 2 });
  await b.flush();
  expect(attempts).toBe(2);
  expect(sent).toMatchObject([{ progress: 2, activity_token: "recovered" }]);
});

it("retries failed exposure receipts on a later cumulative observation", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  let attempts = 0;
  const sent: { shown: unknown[] }[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/recover-exposure", fetch: async (url, init) => {
    if (String(url).endsWith("/kinds")) return Response.json({ signals: true });
    const body = JSON.parse(String(init?.body));
    if (String(url).endsWith("/activity-contexts")) {
      if (++attempts === 1) return Response.json({ code: "unavailable", error: "Retry" }, { status: 503 });
      return Response.json({ enabled: true, contexts: body.targets.map((t: object) => ({ ...t, token: "recovered", occurred_at: new Date().toISOString() })) });
    }
    sent.push(...body.exposures);
    return new Response(null, { status: 204 });
  } });
  const b = new Beacons(client, { recheck: 30_000 });
  const first = { content_id: "one", position: 1 };
  b.exposure({ render_id: "r", stage: "visible", revision: 1, shown: [first] });
  await b.flush();
  expect(sent).toEqual([]);
  vi.setSystemTime(Date.now() + 30_000);
  b.exposure({ render_id: "r", stage: "visible", revision: 2, shown: [first, { content_id: "two", position: 2 }] });
  await b.flush();
  expect(attempts).toBe(2);
  expect(sent[0]?.shown).toHaveLength(2);
});

it("serializes keepalive bodies across clients within the shared byte budget", async () => {
  let activeBytes = 0, maximumBytes = 0;
  const bodies: number[] = [], finish: (() => void)[] = [];
  const fetcher: typeof fetch = async (url, init) => {
    if (String(url).endsWith("/kinds")) return Response.json({ signals: true });
    const body = JSON.parse(String(init?.body));
    if (String(url).endsWith("/activity-contexts")) return Response.json({ enabled: true, contexts: body.targets.map((t: object) => ({ ...t, token: "t".repeat(900), occurred_at: "2026-10-15T12:00:00Z" })) });
    expect(init?.keepalive).toBe(true);
    const size = new TextEncoder().encode(String(init?.body)).length;
    bodies.push(size); activeBytes += size; maximumBytes = Math.max(maximumBytes, activeBytes);
    return new Promise<Response>((resolve) => finish.push(() => { activeBytes -= size; resolve(new Response(null, { status: 204 })); }));
  };
  const clients = Array.from({ length: 3 }, (_, i) => new Beacons(createContentKitClient({ baseUrl: `https://content.test/keepalive-${i}`, fetch: fetcher }), { delay: 60_000 }));
  for (const [i, client] of clients.entries()) client.exposure({ render_id: `render-${i}`, stage: "visible", revision: 1, shown: Array.from({ length: 32 }, (_, j) => ({ content_id: `c-${i}-${j}`, position: j + 1 })) });
  const sent = Promise.all(clients.map((client) => client.flush(true)));
  for (let i = 0; i < 3; i++) {
    await vi.waitFor(() => expect(finish).toHaveLength(i + 1));
    finish[i]!();
  }
  await sent;
  expect(bodies.every((size) => size <= 60 * 1024)).toBe(true);
  expect(maximumBytes).toBeLessThanOrEqual(64 * 1024);
});

it("flushes large signed queues promptly using byte-bounded normal requests", async () => {
  const posts: { size: number; exposures: number }[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/byte-packing", fetch: async (url, init) => {
    if (String(url).endsWith("/kinds")) return Response.json({ signals: true });
    const body = JSON.parse(String(init?.body));
    if (String(url).endsWith("/activity-contexts")) return Response.json({ enabled: true, contexts: body.targets.map((t: object) => ({ ...t, token: "t".repeat(900), occurred_at: "2026-10-15T12:00:00Z" })) });
    expect(init?.keepalive).not.toBe(true);
    posts.push({ size: new TextEncoder().encode(String(init?.body)).length, exposures: body.exposures.length });
    return new Response(null, { status: 204 });
  } });
  const b = new Beacons(client, { delay: 60_000 });
  for (let i = 0; i < 4; i++) b.exposure({ render_id: `r-${i}`, stage: "visible", revision: 1, shown: Array.from({ length: 32 }, (_, j) => ({ content_id: `c-${i}-${j}`, position: j + 1 })) });
  await vi.waitFor(() => expect(posts.reduce((n, p) => n + p.exposures, 0)).toBe(4));
  expect(posts.every((post) => post.size <= 60 * 1024)).toBe(true);
});

it("never puts an oversized single cumulative exposure into keepalive or fragments its identity", async () => {
  const posts: { keepalive?: boolean; size: number; shown: number; render: string }[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/oversized-exposure", fetch: async (url, init) => {
    if (String(url).endsWith("/kinds")) return Response.json({ signals: true });
    const body = JSON.parse(String(init?.body));
    if (String(url).endsWith("/activity-contexts")) return Response.json({ enabled: true, contexts: body.targets.map((t: object) => ({ ...t, token: "t".repeat(900), occurred_at: "2026-10-15T12:00:00Z" })) });
    posts.push({ keepalive: init?.keepalive, size: new TextEncoder().encode(String(init?.body)).length, shown: body.exposures[0].shown.length, render: body.exposures[0].render_id });
    return new Response(null, { status: 204 });
  } });
  const b = new Beacons(client, { delay: 60_000 });
  b.exposure({ render_id: "whole-render", stage: "visible", revision: 1, shown: Array.from({ length: 200 }, (_, i) => ({ content_id: `c-${i}`, position: i + 1 })) });
  await b.flush(true);
  expect(posts).toHaveLength(1);
  expect(posts[0]).toMatchObject({ shown: 200, render: "whole-render" });
  expect(posts[0]?.size).toBeGreaterThan(64 * 1024);
  expect(posts[0]?.keepalive).not.toBe(true);
});
