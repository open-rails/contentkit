import { expect, it, vi } from "vitest";
import { Beacons, EngagementTracker } from "../../src/client/index.js";
import { Accounts, client, harness, recorder, seedContents, png } from "../integration/setup.js";

it("records signed SDK activity through the native API and ranks it with population normalization", async () => {
  const h = harness(), cfg = await h.config(), accounts = new Accounts(h);
  await accounts.load("staff", "alice", "bob");
  const staff = client(h, cfg, accounts.get("staff"));
  const [content] = await seedContents(staff, [{ kind: "comic", titles: { en: "Trusted activity fixture" }, versions: [{ languages: ["en"] }] }]);
  const version = content!.versions[0]!.id;
  await staff.media.put(png("001.png", 711, 320, 450), { ref: { kind: "comic", id: content!.id, version }, path: "originals/001.png" });
  await vi.waitFor(async () => expect((await staff.contents.get(content!.id)).versions[0]?.pages).toBe(1), { timeout: 60_000, interval: 250 });
  const record = recorder();
  const alice = client(h, cfg, accounts.get("alice"), { record });
  const bob = client(h, cfg, accounts.get("bob"));
  alice.preferences.configure(accounts.id("alice"));
  await vi.waitFor(async () => { expect((await alice.kinds()).signals).toBe(true); }, { timeout: 60_000, interval: 250 });

  const contexts = await alice.discovery.activityContexts([{ content_id: content!.id, version_id: version }], undefined, "en");
  expect(contexts.enabled).toBe(true);
  expect(contexts.contexts[0]?.token).toBeTruthy();
  const snapshot = { content_id: content!.id, version_id: version, visit_id: crypto.randomUUID(), segment_id: crypto.randomUUID(), revision: 1,
    active_ms: 1000, pages: [{ index: 0, active_ms: 1000, ranges: [{ from: 0, to: 10_000 }] }], activity_token: contexts.contexts[0]!.token };
  await expect(bob.discovery.engagement([snapshot])).rejects.toMatchObject({ code: "invalid_request" });

  let now = 0;
  const reading = new EngagementTracker({ contentId: content!.id, versionId: version, now: () => now,
    begin: async () => contexts.contexts[0]!, submit: (snapshot) => alice.discovery.engagement([snapshot]) });
  reading.observe({ pages: [{ index: 0, ranges: [{ from: 0, to: 10_000 }] }], resume: "001" });
  reading.attention(true); now = 1000;
  await reading.flush();
  const beacons = new Beacons(alice, { delay: 60_000 });
  beacons.click(content!.id, { language: "en" }); await beacons.flush();
  expect(record.calls.some((path) => path.endsWith("/activity-contexts"))).toBe(true);
  expect(record.calls.some((path) => path.endsWith("/engagement"))).toBe(true);
  expect(record.calls.some((path) => path.endsWith("/signals"))).toBe(true);

  await vi.waitFor(async () => {
    const page = await alice.discovery.list({ kind: "comic", sort: "popular:7d", language: "en" });
    expect(page.ranking).toMatchObject({ policy: "v2", mode: "population_normalized" });
    const ranked = page.contents.find((row) => row.id === content!.id);
    expect(ranked?.ranking?.score).toBeGreaterThan(0);
    expect(ranked?.ranking?.participation).toBeGreaterThan(0);
  }, { timeout: 60_000, interval: 250 });
});
