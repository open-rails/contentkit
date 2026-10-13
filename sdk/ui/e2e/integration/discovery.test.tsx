// @vitest-environment jsdom
import "../../src/test/dom.js";
import { act, render, renderHook, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeAll, describe, expect, it } from "vitest";
import { Beacons, type ContentKitClient, type GroupCard } from "../../src/client/index.js";
import { ContentGrid, ContentRail } from "../../src/index.js";
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
import { Accounts, client, harness, recorder, seedContents } from "./setup.js";

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
    const [a, b, premium, ja] = await seedContents(staff, [
      { kind: "comic", titles: { en: `Apple ${r}`, ja: `りんご ${r}` }, attributes: { format: "manga" }, groups: [t.id], versions: [{ languages: ["en"], traits: ["colored"] }, { languages: ["ja"] }] },
      { kind: "comic", titles: { en: `Banana ${r}` }, attributes: { format: "doujin" }, groups: [t.id], versions: [{ languages: ["en"] }] },
      { kind: "comic", titles: { en: `Cherry ${r}` }, access: { level: "tier", tiers: ["premium"] }, groups: [t.id], versions: [{ languages: ["en"] }] },
      { kind: "comic", titles: { ja: `ドリアン ${r}` }, groups: [t.id], versions: [{ languages: ["ja"] }] },
    ]);
    const c = client(h, cfg, null);
    const { result, rerender } = renderHook((q: Parameters<typeof useContents>[0]) => useContents(q), {
      wrapper: wrap(c, { viewer: null, language: "en" }),
      initialProps: { kind: "comic", groups: [t.id], sort: "title", limit: 2 },
    });
    await waitFor(() => expect(result.current.items.map((x) => x.id)).toEqual([a!.id, b!.id]), wait);
    expect(result.current.items[0]).toMatchObject({ title: `Apple ${r}`, code: a!.code, kind: "comic", access: { allowed: true }, version: { languages: ["en"], traits: ["colored"] } });
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
    rerender({ kind: "comic", groups: [t.id], released_on: "2001-01" });
    await waitFor(() => expect(result.current.items).toEqual([]), wait);
    rerender({ kind: "comic", groups: [t.id], released_on: new Date().toISOString().slice(0, 7), tz: "UTC", sort: "title" });
    await waitFor(() => expect(result.current.items.map((x) => x.id)).toEqual([a!.id, b!.id, premium!.id, ja!.id]), wait);

    // Titles in the provider's language, else the content's own.
    const { result: jp } = renderHook(() => useContents({ kind: "comic", groups: [t.id], sort: "title", language: "ja" }), { wrapper: wrap(c, { viewer: null, language: "ja" }) });
    await waitFor(() => expect(jp.current.items.map((x) => x.title).sort()).toEqual([`りんご ${r}`, `ドリアン ${r}`].sort()), wait);
    expect(jp.current.items.find((x) => x.id === a!.id)!.version.languages).toEqual(["ja"]);
  });

  it("useSearch and useTypeahead: results and suggestions for what is typed", async () => {
    const r = run();
    const word = `zq${r}`;
    const t = await tag(`grp-${r}`, `Grape ${word}`);
    const [one] = await seedContents(staff, [{ kind: "comic", titles: { en: `Kiwi ${word}` }, groups: [t.id], versions: [{ languages: ["en"] }] }]);
    const c = client(h, cfg, null);
    // Keyword documents trail writes by the search sync job: wait until both are indexed.
    await waitFor(async () => {
      const [found, typed] = await Promise.all([c.discovery.search({ q: word, kind: "comic" }), c.discovery.typeahead({ q: word })]);
      expect([found.contents.map((x) => x.id), typed.groups.map((g) => g.id)]).toEqual([[one!.id], [t.id]]);
    }, indexed);

    const { result, rerender } = renderHook(({ q }: { q: string }) => useSearch({ q, kind: "comic" }), { wrapper: wrap(c, { viewer: null }), initialProps: { q: "" } });
    expect(result.current.items).toEqual([]);
    expect(result.current.loading).toBe(false);
    rerender({ q: word });
    await waitFor(() => expect(result.current.items.map((x) => x.id)).toEqual([one!.id]), wait);
    expect(result.current.items[0]!.groups.tag?.map((g) => g.id)).toEqual([t.id]);

    const { result: ta } = renderHook(({ q }: { q: string }) => useTypeahead(q, { debounce: 10 }), { wrapper: wrap(c, { viewer: null }), initialProps: { q: word.slice(0, 6) } });
    await waitFor(() => expect([ta.current.contents.map((x) => x.id), ta.current.groups.map((g) => g.id)]).toEqual([[one!.id], [t.id]]), wait);
    expect(ta.current.query).toBe(word.slice(0, 6));
  });

  it("useReleaseDates: per day, how many contents and how many are free", async () => {
    const r = run();
    const t = await tag(`dates-${r}`);
    await seedContents(staff, [
      { kind: "comic", titles: { en: `Free ${r}` }, groups: [t.id], versions: [{ languages: ["en"] }] },
      { kind: "comic", titles: { en: `Premium ${r}` }, access: { level: "tier", tiers: ["premium"] }, groups: [t.id], versions: [{ languages: ["en"] }] },
    ]);
    const { result } = renderHook(() => useReleaseDates({ kind: "comic", groups: [t.id], by: "day", tz: "UTC" }), { wrapper: wrap(client(h, cfg, null), { viewer: null }) });
    const today = new Date().toISOString().slice(0, 10);
    await waitFor(() => expect(result.current.items.map(({ date, count, free }) => ({ date, count, free }))).toEqual([{ date: today, count: 2, free: 1 }]), wait);
  });

  it("useContentFilters: a hidden-by-default group, shown, and every list following the setting", async () => {
    const gore = await tag("gore", "Gore");
    const r = run();
    const t = await tag(`filtered-${r}`);
    const [plain, gory] = await seedContents(staff, [
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
    const [one] = await seedContents(staff, [{ kind: "comic", titles: { en: `Fig ${r}` }, groups: [t.id], versions: [{ languages: ["en"] }] }]);
    const c = client(h, cfg, acc.get("alice"));
    const { result } = renderHook(() => useContents({ kind: "comic", groups: [t.id] }), { wrapper: wrap(c, { viewer: acc.id("alice") }) });
    await waitFor(() => expect(result.current.items.map((x) => x.counts.favorites)).toEqual([0]), wait);
    await act(async () => {
      await c.favorites.set({ kind: "comic", id: one!.id }, true);
    });
    expect(result.current.items[0]!.counts.favorites).toBe(1);
    expect(result.current.items[0]!.viewer?.favorited).toBe(true);
  });

  it("ContentRail: a featured group's members in its order and windows, any list, no continue watching without the signal plane", async () => {
    const r = run();
    const t = await tag(`rails-${r}`);
    const [rail] = await staff.groups.create([{ kind: "rail", slug: `featured-${r}`, names: [{ language: "en", kind: "name", name: `Featured ${r}` }] }]);
    const [one, two, later] = await seedContents(
      staff,
      ["One", "Two", "Later"].map((n) => ({ kind: "comic", titles: { en: `${n} ${r}` }, groups: [t.id], versions: [{ languages: ["en"] }] })),
    );
    const tomorrow = new Date(Date.now() + 86_400_000).toISOString();
    await staff.groups.addMembers(rail!.id, [{ content_id: two!.id }, { content_id: one!.id, active_until: tomorrow }, { content_id: later!.id, active_from: tomorrow }]);
    const reader = await h.user();
    render(
      <>
        <ContentRail title="Featured" group={rail!.id} />
        <ContentRail title="Newest" list={{ kind: "comic", groups: [t.id], sort: "title" }} />
        <ContentRail title="Continue watching" history={{ status: "in_progress" }} />
      </>,
      { wrapper: wrap(client(h, cfg, reader), { viewer: reader.id, language: "en" }) },
    );
    const titles = (surface: string) => [...document.querySelectorAll(`[data-ckui=content-rail][data-surface=${surface}] [data-ckui=card-title]`)].map((x) => x.textContent);
    await waitFor(() => expect(titles("featured")).toEqual([`Two ${r}`, `One ${r}`]), wait);
    await waitFor(() => expect(titles("list")).toEqual([`Later ${r}`, `One ${r}`, `Two ${r}`]), wait);
    expect(screen.getByRole("heading", { name: "Featured" })).toBeTruthy();
    await waitFor(() => expect(document.querySelectorAll("[data-ckui=content-rail]")).toHaveLength(2), wait);
    expect(screen.queryByRole("heading", { name: "Continue watching" })).toBeNull();
  });

  it("owner_group: a group's own feed, and each card's owner chip", async () => {
    const r = run();
    const [owner] = await staff.groups.create([{ kind: "artist", slug: `owner-${r}`, names: [{ language: "en", kind: "name", name: `Studio ${r}` }] }]);
    const t = await tag(`owned-${r}`);
    const [owned] = await seedContents(staff, [
      { kind: "comic", titles: { en: `Owned ${r}` }, owner: owner!.id, groups: [t.id], versions: [{ languages: ["en"] }] },
      { kind: "comic", titles: { en: `Other ${r}` }, groups: [t.id], versions: [{ languages: ["en"] }] },
    ]);
    const c = client(h, cfg, null);
    const { result } = renderHook(() => useContents({ kind: "comic", owner_group: owner!.id }), { wrapper: wrap(c, { viewer: null, language: "en" }) });
    await waitFor(() => expect(result.current.items.map((x) => x.id)).toEqual([owned!.id]), wait);
    expect(result.current.items[0]!.owner).toMatchObject({ id: owner!.id, kind: "artist", name: `Studio ${r}` });

    const { container } = render(
      <>
        <section data-test="tag">
          <ContentGrid query={{ kind: "comic", groups: [t.id], sort: "title" }} />
        </section>
        <section data-test="feed">
          <ContentGrid query={{ kind: "comic", owner_group: owner!.id }} owner={false} />
        </section>
      </>,
      { wrapper: wrap(c, { viewer: null, language: "en" }) },
    );
    const titles = (s: string) => [...container.querySelectorAll(`[data-test=${s}] [data-ckui=card-title]`)].map((x) => x.textContent);
    await waitFor(() => expect([titles("tag"), titles("feed")]).toEqual([[`Other ${r}`, `Owned ${r}`], [`Owned ${r}`]]), wait);
    const chips = container.querySelectorAll("[data-test=tag] [data-ckui=card-owner]");
    // The avatar's initial, then the name.
    expect([...chips].map((x) => [x.getAttribute("data-id"), x.textContent])).toEqual([[owner!.id, `SStudio ${r}`]]);
    expect(container.querySelectorAll("[data-test=feed] [data-ckui=card-owner]")).toHaveLength(0);
  });

  it("without the signal plane: rails leave themselves out, history says so, beacons turn off", async () => {
    const [one] = await seedContents(staff, [{ kind: "comic", titles: { en: `Lonely ${run()}` }, versions: [{ languages: ["en"] }] }]);
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
