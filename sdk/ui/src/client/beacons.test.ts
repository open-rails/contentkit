import { afterEach, expect, it, vi } from "vitest";
import { Beacons } from "./beacons.js";
import { createContentKitClient } from "./client.js";

// A site whose signal plane is up or down (signals: false, sends refused as unavailable).
const site = (signals: boolean) => {
  const s = { up: signals, sent: [] as string[], client: undefined as unknown as ReturnType<typeof createContentKitClient> };
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = new URL(String(input), "http://x").pathname;
    s.sent.push(`${init?.method ?? "GET"} ${path}`);
    if (path === "/v1/kinds") return Response.json({ languages: ["en"], tiers: [], kinds: {}, group_kinds: {}, content_filters: [], signals: s.up });
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
    const beacons = s.sent.filter((x) => x.startsWith("POST"));
    expect(s.sent.filter((x) => x === "GET /v1/kinds")).toHaveLength(1);
    expect(beacons).toEqual(signals ? ["POST /v1/signals", "POST /v1/exposures", "POST /v1/signals"] : []);
  }
});

it("follows the signal plane as it goes down and comes back, reading the content model again after each pause", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  const s = site(false);
  const b = new Beacons(s.client, { delay: 60_000, recheck: 30_000, session: () => "s1" });
  const posts = () => s.sent.filter((x) => x.startsWith("POST")).length;
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
