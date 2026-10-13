import { expect, it, vi } from "vitest";
import { Beacons } from "./beacons.js";
import { createContentKitClient } from "./client.js";

const site = (signals: boolean) => {
  const sent: string[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = new URL(String(input), "http://x").pathname;
    sent.push(`${init?.method ?? "GET"} ${path}`);
    if (path === "/v1/kinds") return Response.json({ languages: ["en"], tiers: [], kinds: {}, group_kinds: {}, content_filters: [], signals });
    return new Response(null, { status: 204 });
  });
  return { sent, client: createContentKitClient({ baseUrl: "", fetch: fetch as typeof globalThis.fetch }) };
};

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
