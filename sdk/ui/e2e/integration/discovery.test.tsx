// @vitest-environment jsdom
import "../../src/test/dom.js";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeAll, describe, expect, it } from "vitest";
import { Beacons, type ContentKitClient, type GroupCard } from "../../src/client/index.js";
import {
  ContentKitProvider,
  useContentFilters,
  useContents,
  useHistory,
  useRecommendations,
  useReleaseDates,
  useSearch,
  useSimilar,
  useTypeahead,
} from "../../src/react/index.js";
import type { Config } from "../support/harness.js";
import { Accounts, client, harness, recorder } from "./setup.js";

const wait = { timeout: 30_000, interval: 50 };
// Group documents trail their writes by the search sync job's period.
const indexed = { timeout: 90_000, interval: 1000 };
const run = () => Math.random().toString(36).slice(2, 8);

describe("discovery against the real ContentKit", () => {
  const h = harness();
  const acc = new Accounts(h);
  let cfg: Config;
  let staff: ContentKitClient;

  beforeAll(async () => {
    cfg = await h.config();
    await acc.load("staff", "alice");
    staff = client(h, cfg, acc.get("staff"));
  });

  const wrap = (c: ContentKitClient, o: { viewer?: string | null; language?: string } = {}) =>
    function Wrapper({ children }: { children: ReactNode }) {
      return (
        <ContentKitProvider client={c} viewer={o.viewer} language={o.language}>
          {children}
        </ContentKitProvider>
      );
    };
  const tag = async (slug: string, name = slug): Promise<GroupCard> => {
    const found = (await staff.groups.list({ kind: "tag", slug })).groups[0];
    if (found) return found;
    const [g] = await staff.groups.create([{ kind: "tag", slug, names: [{ language: "en", kind: "name", name }] }]);
    return (await staff.groups.list({ kind: "tag", slug })).groups[0] ?? ({ ...g!, name, count: 0 } as GroupCard);
  };

  it("useContents: one card per content, sorts, filters and pages, locks or hides what the reader cannot open", async () => {
    const r = run();
    const t = await tag(`list-${r}`);
    const [a, b, premium, ja] = await h.contents([
      { kind: "comic", titles: { en: `Apple ${r}`, ja: `りんご ${r}` }, attributes: { format: "manga" }, groups: [t.id], versions: [{ languages: ["en"], traits: ["colored"], pages: 20 }, { languages: ["ja"] }] },
      { kind: "comic", titles: { en: `Banana ${r}` }, attributes: { format: "doujin" }, groups: [t.id], age_hours: 24 * 400, versions: [{ languages: ["en"] }] },
      { kind: "comic", titles: { en: `Cherry ${r}` }, access: { level: "tier", tiers: ["premium"] }, groups: [t.id], versions: [{ languages: ["en"] }] },
      { kind: "comic", titles: { ja: `ドリアン ${r}` }, groups: [t.id], versions: [{ languages: ["ja"] }] },
    ]);
    const c = client(h, cfg, null);
    const { result, rerender } = renderHook((q: Parameters<typeof useContents>[0]) => useContents(q), {
      wrapper: wrap(c, { viewer: null, language: "en" }),
      initialProps: { kind: "comic", groups: [t.id], sort: "title", limit: 2 },
    });
    await waitFor(() => expect(result.current.items.map((x) => x.id)).toEqual([a!.id, b!.id]), wait);
    expect(result.current.items[0]).toMatchObject({ title: `Apple ${r}`, code: a!.code, kind: "comic", access: { allowed: true }, version: { languages: ["en"], traits: ["colored"], pages: 20 } });
    expect(result.current.items[0]!.path).toContain(`/comics/${a!.code}`);
    expect(result.current.hasMore).toBe(true);
    await act(() => result.current.loadMore());
    await waitFor(() => expect(result.current.items).toHaveLength(4), wait);
    const locked = result.current.items.find((x) => x.id === premium!.id)!;
    expect(locked.access).toMatchObject({ allowed: false, sell: expect.arrayContaining(["premium"]) });
    expect(result.current.items.map((x) => x.id)).toContain(ja!.id);

    rerender({ kind: "comic", groups: [t.id], sort: "title", mode: "hide" });
    await waitFor(() => expect(result.current.items.map((x) => x.id).sort()).toEqual([a!.id, b!.id, ja!.id].sort()), wait);
    rerender({ kind: "comic", groups: [t.id], language: "en", attribute: ["format:manga"] });
    await waitFor(() => expect(result.current.items.map((x) => x.id)).toEqual([a!.id]), wait);
    rerender({ kind: "comic", groups: [t.id], traits: ["colored"] });
    await waitFor(() => expect(result.current.items.map((x) => x.id)).toEqual([a!.id]), wait);
    const month = new Date(Date.now() - 400 * 24 * 3600_000).toISOString().slice(0, 7);
    rerender({ kind: "comic", groups: [t.id], released_on: month });
    await waitFor(() => expect(result.current.items.map((x) => x.id)).toEqual([b!.id]), wait);

    // Titles in the provider's language, else the content's own.
    const { result: jp } = renderHook(() => useContents({ kind: "comic", groups: [t.id], sort: "title", language: "ja" }), { wrapper: wrap(c, { viewer: null, language: "ja" }) });
    await waitFor(() => expect(jp.current.items.map((x) => x.title).sort()).toEqual([`りんご ${r}`, `ドリアン ${r}`].sort()), wait);
    expect(jp.current.items.find((x) => x.id === a!.id)!.version.languages).toEqual(["ja"]);
  });

  it("useSearch and useTypeahead: results and suggestions for what is typed", async () => {
    const r = run();
    const word = `zq${r}`;
    const t = await tag(`grp-${r}`, `Grape ${word}`);
    const [one] = await h.contents([{ kind: "comic", titles: { en: `Kiwi ${word}` }, groups: [t.id], versions: [{ languages: ["en"] }] }]);
    const c = client(h, cfg, null);
    const { result, rerender } = renderHook(({ q }: { q: string }) => useSearch({ q, kind: "comic" }), { wrapper: wrap(c, { viewer: null }), initialProps: { q: "" } });
    expect(result.current.items).toEqual([]);
    expect(result.current.loading).toBe(false);
    rerender({ q: word });
    await waitFor(() => expect(result.current.items.map((x) => x.id)).toEqual([one!.id]), wait);

    const { result: ta } = renderHook(({ q }: { q: string }) => useTypeahead(q, { debounce: 10 }), { wrapper: wrap(c, { viewer: null }), initialProps: { q: word.slice(0, 5) } });
    await waitFor(() => expect(ta.current.contents.map((x) => x.id)).toContain(one!.id), wait);
    await waitFor(() => expect(ta.current.groups.map((g) => g.id)).toContain(t.id), indexed);
  });

  it("useReleaseDates: per month, how many contents and how many are free", async () => {
    const r = run();
    const t = await tag(`dates-${r}`);
    await h.contents([
      { kind: "comic", titles: { en: `Old ${r}` }, age_hours: 24 * 400, groups: [t.id], versions: [{ languages: ["en"] }] },
      { kind: "comic", titles: { en: `Old premium ${r}` }, age_hours: 24 * 400, access: { level: "tier", tiers: ["premium"] }, groups: [t.id], versions: [{ languages: ["en"] }] },
      { kind: "comic", titles: { en: `New ${r}` }, groups: [t.id], versions: [{ languages: ["en"] }] },
    ]);
    const { result } = renderHook(() => useReleaseDates({ kind: "comic", groups: [t.id] }), { wrapper: wrap(client(h, cfg, null), { viewer: null }) });
    const old = new Date(Date.now() - 400 * 24 * 3600_000).toISOString().slice(0, 7);
    const now = new Date(Date.now() - 3600_000).toISOString().slice(0, 7);
    await waitFor(() => expect(result.current.items.map(({ date, count, free }) => ({ date, count, free }))).toEqual([{ date: now, count: 1, free: 1 }, { date: old, count: 2, free: 1 }]), wait);
  });

  it("useContentFilters: a hidden-by-default group, shown, and every list following the setting", async () => {
    const gore = await tag("gore", "Gore");
    const r = run();
    const t = await tag(`filtered-${r}`);
    const [plain, gory] = await h.contents([
      { kind: "comic", titles: { en: `Plain ${r}` }, groups: [t.id], versions: [{ languages: ["en"] }] },
      { kind: "comic", titles: { en: `Gory ${r}` }, groups: [t.id, gore.id], versions: [{ languages: ["en"] }] },
    ]);
    const reader = await h.user();
    const c = client(h, cfg, reader);
    const { result } = renderHook(() => ({ filters: useContentFilters({ language: "en" }), list: useContents({ kind: "comic", groups: [t.id] }) }), { wrapper: wrap(c, { viewer: reader.id }) });
    await waitFor(() => expect(result.current.filters.filters?.filters.map((f) => f.group.slug)).toEqual(["gore"]), wait);
    expect(result.current.filters.filters!.filters[0]).toMatchObject({ label: "Gore", default_visible: false, visible: false, overridden: false });
    await waitFor(() => expect(result.current.list.items.map((x) => x.id)).toEqual([plain!.id]), wait);

    await act(() => result.current.filters.setVisible(gore.id, true));
    expect(result.current.filters.filters!.filters[0]).toMatchObject({ visible: true, overridden: true });
    await waitFor(() => expect(result.current.list.items.map((x) => x.id).sort()).toEqual([plain!.id, gory!.id].sort()), wait);
    await act(() => result.current.filters.save({ filters: [], include_deleted: false }));
    await waitFor(() => expect(result.current.list.items.map((x) => x.id)).toEqual([plain!.id]), wait);
  });

  it("cards follow favorites made through the client", async () => {
    const r = run();
    const t = await tag(`fav-${r}`);
    const [one] = await h.contents([{ kind: "comic", titles: { en: `Fig ${r}` }, groups: [t.id], versions: [{ languages: ["en"] }] }]);
    const c = client(h, cfg, acc.get("alice"));
    const { result } = renderHook(() => useContents({ kind: "comic", groups: [t.id] }), { wrapper: wrap(c, { viewer: acc.id("alice") }) });
    await waitFor(() => expect(result.current.items.map((x) => x.counts.favorites)).toEqual([0]), wait);
    await act(async () => {
      await c.favorites.set({ kind: "comic", id: one!.id }, true);
    });
    expect(result.current.items[0]!.counts.favorites).toBe(1);
    expect(result.current.items[0]!.viewer?.favorited).toBe(true);
  });

  it("without the signal plane: rails leave themselves out, history says so, beacons turn off", async () => {
    const [one] = await h.contents([{ kind: "comic", titles: { en: `Lonely ${run()}` }, versions: [{ languages: ["en"] }] }]);
    const reader = await h.user();
    const c = client(h, cfg, reader);
    const { result } = renderHook(() => ({ similar: useSimilar(one!.id), recs: useRecommendations(), history: useHistory() }), { wrapper: wrap(c, { viewer: reader.id }) });
    await waitFor(() => expect([result.current.similar.unavailable, result.current.recs.unavailable]).toEqual([true, true]), wait);
    expect(result.current.similar.items).toEqual([]);
    await waitFor(() => expect(result.current.history.error?.code).toBe("not_configured"), wait);

    // The first batch learns the site has no signal plane; later ones are not sent.
    const rec = recorder();
    const b = new Beacons(client(h, cfg, reader, { record: rec }).discovery, { delay: 10_000 });
    b.click(one!.id, { surface: "test", position: 1 });
    b.view(one!.id).update({ progress: 3 });
    await b.flush();
    b.click(one!.id);
    b.exposure({ render_id: "r1", stage: "rendered", shown: [{ content_id: one!.id, position: 1 }] });
    await b.flush();
    expect(rec.calls.filter((x) => x.endsWith("/signals") || x.endsWith("/exposures"))).toEqual([`${cfg.api}/v1/signals`]);
  });
});
