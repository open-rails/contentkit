// @vitest-environment jsdom
import "../../src/test/dom.js";
import { act, renderHook, waitFor } from "@testing-library/react";
import { useState, type ReactNode } from "react";
import { beforeAll, describe, expect, it } from "vitest";
import { isPending, type ContentKitClient } from "../../src/client/index.js";
import { ContentKitProvider, useContentDetail, useReaderPlan, useSegmentPages } from "../../src/react/index.js";
import { useVersionMenuItems, type PlayerMenuItem } from "../../src/index.js";
import type { Config } from "../support/harness.js";
import { Accounts, client, harness, png, seedContents, wait } from "./setup.js";

const quick = { timeout: 30_000, interval: 50 };
const run = () => Math.random().toString(36).slice(2, 8);

describe("content pages against the real ContentKit", () => {
  const h = harness();
  const acc = new Accounts(h);
  let cfg: Config;
  let staff: ContentKitClient;

  beforeAll(async () => {
    cfg = await h.config();
    await acc.load("staff");
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

  it("useContentDetail: text in the reader's language, live versions, the language's default and its decision", async () => {
    const r = run();
    const [c] = await seedContents(staff, [
      {
        kind: "comic",
        titles: { en: `Atlas ${r}`, ja: `アトラス ${r}` },
        access: { level: "tier", tiers: ["premium"], preview: { pages: 2 } },
        versions: [{ languages: ["en"] }, { languages: ["ja"], traits: ["colored"], access: { level: "public" } }, { languages: ["en"], state: "draft" }],
      },
    ]);
    const [en, ja, draft] = c!.versions;
    const anon = client(h, cfg, null);
    const { result, rerender } = renderHook(({ version }: { version?: string }) => useContentDetail(c!.id, { version }), {
      wrapper: wrap(anon, { viewer: null, language: "ja" }),
      initialProps: {},
    });
    await waitFor(() => expect(result.current.content?.title).toBe(`アトラス ${r}`), quick);
    expect(result.current.versions.map((v) => v.id)).toEqual([en!.id, ja!.id]);
    expect(result.current.versions.map((v) => v.id)).not.toContain(draft!.id);
    expect(result.current.languages).toEqual(["en", "ja"]);
    // The Japanese reader opens the Japanese version, public by its own rule.
    expect(result.current.version?.id).toBe(ja!.id);
    expect(result.current.decision).toMatchObject({ allowed: true });
    rerender({ version: en!.id });
    expect(result.current.version?.id).toBe(en!.id);
    expect(result.current.decision).toMatchObject({ allowed: false, sell: expect.arrayContaining(["premium"]), preview: { pages: 2 } });
    rerender({ version: draft!.id });
    expect(result.current.version?.id).toBe(ja!.id);

    // An edit shows on the next read; one made through the page's own client, at once.
    await staff.contents.putText(c!.id, "ja", { title: `新アトラス ${r}` });
    act(() => result.current.reload());
    await waitFor(() => expect(result.current.content?.title).toBe(`新アトラス ${r}`), quick);
    const { result: own } = renderHook(() => useContentDetail(c!.id), { wrapper: wrap(staff, { viewer: acc.id("staff"), language: "en" }) });
    await waitFor(() => expect(own.current.content?.title).toBe(`Atlas ${r}`), quick);
    await act(async () => {
      await staff.contents.putText(c!.id, "en", { title: `Atlas II ${r}` });
    });
    await waitFor(() => expect(own.current.content?.title).toBe(`Atlas II ${r}`), quick);
  });

  it("useReaderPlan: pending while the version has no publication", async () => {
    const [c] = await seedContents(staff, [{ kind: "comic", titles: { en: `Unpublished ${run()}` }, versions: [{ languages: ["en"] }] }]);
    const v = c!.versions[0]!;
    const anon = client(h, cfg, null);
    expect(isPending(await anon.versions.reader(c!.id, v.id))).toBe(true);
    const { result } = renderHook(() => useReaderPlan(c!.id, v.id, { poll: 1000 }), { wrapper: wrap(anon, { viewer: null }) });
    await waitFor(() => expect(result.current.pending).toBe(true), quick);
    expect(result.current.plan).toBeNull();
    expect(result.current.error).toBeUndefined();
  });

  it("useReaderPlan and useSegmentPages: a version's pages once the worker published them", async () => {
    const [c] = await seedContents(staff, [{ kind: "comic", titles: { en: `Pages ${run()}` }, versions: [{ languages: ["en"] }] }]);
    const v = c!.versions[0]!;
    const anon = client(h, cfg, null);
    const { result } = renderHook(() => useReaderPlan(c!.id, v.id, { poll: 1000 }), { wrapper: wrap(anon, { viewer: null }) });
    // Pages into the version's folder; the worker renders and publishes them.
    const ref = { kind: "comic", id: c!.id, version: v.id };
    for (const [i, name] of ["001.png", "002.png"].entries()) await staff.media.put(png(name, i + 1, 60, 90), { ref, path: `originals/${name}`, wait: false });
    await waitFor(() => expect(result.current.plan?.pages).toBe(2), wait).catch(async (e: Error) => {
      throw new Error(`${e.message}\npending=${result.current.pending} error=${result.current.error?.code}\n${JSON.stringify(await h.publication(ref), null, 1)}`);
    });
    const plan = result.current.plan!;
    expect(plan.segments).toHaveLength(1);
    expect(plan.segments[0]).toMatchObject({ start: 0, end: 2, released: true, locked: false });
    const { result: pages } = renderHook(() => useSegmentPages(plan.segments[0], "low"), { wrapper: wrap(anon, { viewer: null }) });
    await waitFor(() => expect(pages.current.pages.map((p) => !!p.url)).toEqual([true, true]), wait);
  });

  it("useVersionMenuItems: the player's language and version entries switch the version shown", async () => {
    const r = run();
    const [c] = await seedContents(staff, [
      {
        kind: "comic",
        titles: { en: `Reel ${r}` },
        access: { level: "tier", tiers: ["premium"] },
        versions: [{ languages: ["en"] }, { languages: ["en"], traits: ["colored"], access: { level: "public" } }, { languages: ["ja"] }],
      },
    ]);
    const [plain, colored, ja] = c!.versions;
    const { result } = renderHook(
      () => {
        const [version, setVersion] = useState<string | null>(null);
        const d = useContentDetail(c!.id, { version });
        return { shown: d.version?.id, menu: useVersionMenuItems(d.versions, d.version?.id, setVersion) as Extract<PlayerMenuItem, { options: unknown }>[] };
      },
      { wrapper: wrap(client(h, cfg, null), { viewer: null, language: "en" }) },
    );
    await waitFor(() => expect(result.current.menu).toHaveLength(2), quick);
    const [language, variant] = result.current.menu;
    expect(language).toMatchObject({ label: "Language", value: "en", options: [{ value: "en", label: "English" }, { value: "ja", label: "Japanese" }] });
    expect(variant).toMatchObject({ label: "Version", options: [{ value: plain!.id, label: "English (locked)" }, { value: colored!.id, label: "Colored" }] });

    act(() => variant!.onChange(colored!.id));
    expect(result.current.shown).toBe(colored!.id);
    expect(result.current.menu[1]!.value).toBe(colored!.id);
    act(() => language!.onChange("ja"));
    expect(result.current.shown).toBe(ja!.id);
    expect(result.current.menu.map((m) => [m.label, m.value])).toEqual([["Language", "ja"]]);
  });
});
