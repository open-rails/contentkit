import { expect, it, vi } from "vitest";
import { Beacons } from "../../src/client/index.js";
import { Accounts, client, harness, recorder, seedContents } from "../integration/setup.js";

it("records signed SDK activity through the native API and ranks it with population normalization", async () => {
  const h = harness(), cfg = await h.config(), accounts = new Accounts(h);
  await accounts.load("staff", "alice", "bob");
  const staff = client(h, cfg, accounts.get("staff"));
  const [content] = await seedContents(staff, [{ kind: "comic", titles: { en: "Trusted activity fixture" }, versions: [{ languages: ["en"] }] }]);
  const version = content!.versions[0]!.id;
  const record = recorder();
  const alice = client(h, cfg, accounts.get("alice"), { record });
  const bob = client(h, cfg, accounts.get("bob"));
  alice.preferences.configure(accounts.id("alice"));
  await vi.waitFor(async () => { expect((await alice.kinds()).signals).toBe(true); }, { timeout: 60_000, interval: 250 });

  const contexts = await alice.discovery.activityContexts([{ content_id: content!.id, version_id: version }], undefined, "en");
  expect(contexts.enabled).toBe(true);
  expect(contexts.contexts[0]?.token).toBeTruthy();
  await expect(bob.discovery.signals([{ content_id: content!.id, version_id: version, type: "view", event_id: "foreign-receipt", occurred_at: contexts.contexts[0]!.occurred_at, progress: 1, activity_token: contexts.contexts[0]!.token }]))
    .rejects.toMatchObject({ code: "invalid_request" });

  const beacons = new Beacons(alice, { delay: 60_000 });
  const view = beacons.view(content!.id, version, { language: "en" });
  view.update({ progress: 1, duration_s: 30 });
  await beacons.flush();
  expect(record.calls.some((path) => path.endsWith("/activity-contexts"))).toBe(true);
  expect(record.calls.some((path) => path.endsWith("/signals"))).toBe(true);

  await vi.waitFor(async () => {
    const page = await alice.discovery.list({ kind: "comic", sort: "popular:7d", language: "en" });
    expect(page.ranking).toMatchObject({ policy: "v2", mode: "population_normalized" });
    const ranked = page.contents.find((row) => row.id === content!.id);
    expect(ranked?.ranking?.score).toBeGreaterThan(0);
    expect(ranked?.ranking?.participation).toBeGreaterThan(0);
  }, { timeout: 60_000, interval: 250 });
});
