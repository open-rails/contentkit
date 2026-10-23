// @vitest-environment jsdom
import "../../src/test/dom.js";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeAll, describe, expect, it } from "vitest";
import type { ContentKitClient, Group } from "../../src/client/index.js";
import { ContentKitProvider, type GroupFilter, useContentGroups, useGroup, useGroupMembers, useGroups, useKinds, useStaffAccess } from "../../src/react/index.js";
import type { Config } from "../support/harness.js";
import { Accounts, client, fixture, harness, seedContents } from "./setup.js";

const wait = { timeout: 30_000, interval: 50 };
const run = () => Math.random().toString(36).slice(2, 8);
const name = (language: string, n: string, kind: "name" | "alias" = "name") => ({ language, kind, name: n });

describe("groups against the real ContentKit", () => {
  const h = harness();
  const acc = new Accounts(h);
  let cfg: Config;
  let staff: ContentKitClient;

  beforeAll(async () => {
    cfg = await h.config();
    await acc.load("staff", "alice");
    staff = client(h, cfg, acc.get("staff"));
  });

  const wrap = (c: ContentKitClient, language?: string) =>
    function Wrapper({ children }: { children: ReactNode }) {
      return (
        <ContentKitProvider client={c} language={language}>
          {children}
        </ContentKitProvider>
      );
    };
  const create = async (kind: string, slug: string, names: ReturnType<typeof name>[], o: { parent_id?: string; scope_id?: string } = {}): Promise<Group> =>
    (await staff.entities.create([{ kind, slug, names, ...o }]))[0]!;

  it("useGroups: a parent's children by letter bucket, order and page, named in the asked language", async () => {
    const r = run();
    const parent = await create("series", `p-${r}`, [name("en", `Parent ${r}`)]);
    const under = { parent_id: parent.id };
    const alpha = await create("series", `alpha-${r}`, [name("en", `Alpha ${r}`), name("ja", `アルファ ${r}`)], under);
    const abaco = await create("series", `abaco-${r}`, [name("en", `Ábaco ${r}`)], under);
    const beta = await create("series", `beta-${r}`, [name("en", `Beta ${r}`)], under);
    const digits = await create("series", `3x3-${r}`, [name("en", `3x3 ${r}`)], under);
    // Beta has two live contents, Alpha one: count order after the job runs.
    for (const [g, n] of [[beta, 2], [alpha, 1]] as const) {
      for (let i = 0; i < n; i++) {
        const [c] = await seedContents(staff, [{ kind: "comic", titles: { en: `Member ${r} ${i}` }, versions: [{ languages: ["en"] }] }]);
        await staff.entities.addMembers(g.id, [{ content_id: c!.id }]);
      }
    }

    const c = client(h, cfg, null);
    const { result, rerender } = renderHook((f: GroupFilter) => useGroups(f), {
      wrapper: wrap(c, "en"),
      initialProps: { kind: "series", parent: parent.id, letter: "a" } as GroupFilter,
    });
    await waitFor(() => expect(result.current.items.map((g) => g.id)).toEqual([abaco.id, alpha.id]), wait);
    expect(result.current.items[1]).toMatchObject({ name: `Alpha ${r}`, slug: `alpha-${r}`, count: 1, path: expect.stringContaining("/series/") });

    rerender({ kind: "series", parent: parent.id, letter: "#" });
    await waitFor(() => expect(result.current.items.map((g) => g.id)).toEqual([digits.id]), wait);

    rerender({ kind: "series", parent: parent.id, sort: "count", pageSize: 2 });
    await waitFor(() => expect(result.current.items.map((g) => g.id)).toEqual([beta.id, alpha.id]), wait);
    expect(result.current.hasMore).toBe(true);
    await act(() => result.current.loadMore());
    await waitFor(() => expect(result.current.items).toHaveLength(4), wait);
    expect(result.current.items.map((g) => g.count)).toEqual([2, 1, 0, 0]);
    expect(result.current.hasMore).toBe(false);

    // In Japanese, Alpha sorts by its Japanese name: no longer under "a".
    rerender({ kind: "series", parent: parent.id, letter: "#", language: "ja" });
    await waitFor(() => expect(result.current.items.map((g) => g.name).sort()).toEqual([`3x3 ${r}`, `アルファ ${r}`]), wait);
  });

  it("useGroup: create, attributes, names, text, edges, cover, merge, delete and restore", async () => {
    const r = run();
    const c = staff;
    const { result, rerender } = renderHook(({ id }: { id: string | null }) => useGroup(id), { wrapper: wrap(c, "en"), initialProps: { id: null as string | null } });
    expect(result.current.group).toBeNull();
    let artist!: Group;
    await act(async () => {
      artist = await result.current.create({ kind: "artist", slug: `artist-${r}`, names: [name("en", `Artist ${r}`)] });
    });
    await waitFor(() => expect(result.current.group?.id).toBe(artist.id), wait);
    rerender({ id: artist.id });

    const debut = new Date(Date.UTC(2020, 4, 1, 12)).toISOString();
    await act(() => result.current.update({ attributes: { artist_type: "circle", works: 3, active: true, debut } }));
    await waitFor(() => expect(result.current.group?.attributes).toEqual({ artist_type: "circle", works: 3, active: true, debut: "2020-05-01T12:00:00Z" }), wait);
    await expect(result.current.update({ attributes: { artist_type: "band" } })).rejects.toMatchObject({ code: "invalid_request" });

    await act(() => result.current.names("add", [name("ja", `アーティスト ${r}`), name("en", `A. ${r}`, "alias")]));
    await waitFor(() => expect(result.current.group?.names).toHaveLength(3), wait);
    await act(() => result.current.names("add", [name("en", `A. ${r}`)]));
    await waitFor(() => expect(result.current.group?.name).toBe(`A. ${r}`), wait);
    expect(result.current.group?.names).toContainEqual(name("en", `Artist ${r}`, "alias"));
    await act(() => result.current.update({ text: [{ language: "en", description: "Draws things.", sort_key: `a ${r}` }] }));
    await waitFor(() => expect(result.current.group?.text).toEqual([{ language: "en", description: "Draws things.", sort_key: `a ${r}` }]), wait);

    const circle = await create("artist", `circle-${r}`, [name("en", `Circle ${r}`)]);
    await act(() => result.current.edges("add", [{ relation: "member_of", to: circle.id }]));
    await waitFor(() => expect(result.current.group?.edges).toEqual([{ from: artist.id, relation: "member_of", to: circle.id }]), wait);

    // The cover: an upload to the group's folder, a member's cover, then automatic.
    await act(async () => {
      await result.current.uploadImage("cover", fixture("small.png", "image/png", "face.png"), { wait: false });
    });
    await waitFor(() => expect(result.current.group?.cover?.image).toMatch(/^i-[0-9a-f-]{36}$/), wait);
    const image = result.current.group!.cover!.image!;
    expect(await h.object({ kind: "artist", id: artist.id }, `cover/${image}`)).not.toBeNull();
    const [member] = await seedContents(staff, [{ kind: "comic", titles: { en: `Member ${r}` }, versions: [{ languages: ["en"] }] }]);
    await staff.entities.addMembers(artist.id, [{ content_id: member!.id }]);
    await act(() => result.current.update({ cover: { content_id: member!.id } }));
    await waitFor(() => expect(result.current.group?.cover).toEqual({ content_id: member!.id }), wait);
    await act(() => result.current.update({ cover: {} }));
    act(() => result.current.reload());
    await waitFor(() => expect(result.current.group?.cover).toEqual({ content_id: member!.id, auto: true }), wait);

    // Merge into the circle: the hook shows the survivor, with the names and members moved.
    await act(() => result.current.merge(circle.id));
    await waitFor(() => expect(result.current.group?.id).toBe(circle.id), wait);
    expect(result.current.group?.names.map((n) => n.name)).toEqual(expect.arrayContaining([`Circle ${r}`, `A. ${r}`, `Artist ${r}`]));
    expect((await staff.entities.members(circle.id)).members.map((m) => m.content_id)).toEqual([member!.id]);

    const { result: other } = renderHook(() => useGroup(circle.id), { wrapper: wrap(c, "en") });
    await waitFor(() => expect(other.current.group?.id).toBe(circle.id), wait);
    await act(() => other.current.remove());
    expect(other.current.group).toBeNull();
    expect(other.current.deleted).toMatchObject({ id: circle.id, state: "deleted" });
    await expect(client(h, cfg, null).entities.get(circle.id)).rejects.toMatchObject({ code: "not_found" });
    await act(() => other.current.restore());
    await waitFor(() => expect(other.current.group?.id).toBe(circle.id), wait);
    expect(other.current.deleted).toBeNull();
  });

  it("useContentGroups and useGroupMembers: roles, positions, windows and version-level memberships", async () => {
    const r = run();
    const [content] = await seedContents(staff, [{ kind: "comic", titles: { en: `Content ${r}` }, versions: [{ languages: ["en"] }, { languages: ["ja"] }] }]);
    const ja = content!.versions[1]!.id;
    const artist = await create("artist", `pub-${r}`, [name("en", `Publisher ${r}`)]);
    const series = await create("series", `ser-${r}`, [name("en", `Series ${r}`)]);
    const voice = await create("voice_actor", `va-${r}`, [name("en", `Voice ${r}`)]);
    const rail = await create("rail", `rail-${r}`, [name("en", `Rail ${r}`)]);
    const from = "2026-01-01T00:00:00Z";
    const until = "2027-01-01T00:00:00Z";

    const { result } = renderHook(() => useContentGroups(content!.id), { wrapper: wrap(staff, "en") });
    await waitFor(() => expect(result.current.loading).toBe(false), wait);
    expect(result.current.memberships).toEqual([]);
    await act(() =>
      result.current.set(
        [
          { group_id: artist.id, role: "publisher" },
          { group_id: artist.id },
          { group_id: series.id, position: 3 },
          { group_id: voice.id, version_id: ja },
          { group_id: rail.id, active_from: from, active_until: until },
        ],
        ["artist", "series", "voice_actor", "rail"],
      ),
    );
    await waitFor(() => expect(result.current.memberships).toHaveLength(5), wait);
    const by = (g: string, role = "") => result.current.memberships.find((m) => m.group_id === g && (m.role ?? "") === role);
    expect(by(artist.id, "publisher")).toMatchObject({ content_id: content!.id, state: "active" });
    expect(by(series.id)).toMatchObject({ position: 3 });
    expect(by(voice.id)).toMatchObject({ version_id: ja });
    expect(by(rail.id)).toMatchObject({ position: 1 });
    expect([by(rail.id)?.active_from, by(rail.id)?.active_until].map((t) => Date.parse(t!))).toEqual([Date.parse(from), Date.parse(until)]);
    expect([...result.current.entities.keys()].sort()).toEqual([artist.id, series.id, voice.id, rail.id].sort());
    expect(result.current.entities.get(voice.id)).toMatchObject({ kind: "voice_actor", name: `Voice ${r}` });
    await expect(result.current.set([{ group_id: series.id, version_id: ja }], ["series"])).rejects.toMatchObject({ code: "invalid_request" });

    // The rail's members, from its side: another content, an order, a new window, a removal.
    const [second] = await seedContents(staff, [{ kind: "comic", titles: { en: `Second ${r}` }, versions: [{ languages: ["en"] }] }]);
    const { result: members } = renderHook(() => useGroupMembers(rail.id), { wrapper: wrap(staff, "en") });
    await waitFor(() => expect(members.current.items.map((m) => m.content_id)).toEqual([content!.id]), wait);
    await act(async () => {
      await members.current.add([{ content_id: second!.id }]);
    });
    await waitFor(() => expect(members.current.items.map((m) => m.content_id)).toEqual([content!.id, second!.id]), wait);
    await act(async () => {
      await members.current.reorder([{ content_id: second!.id }, { content_id: content!.id }]);
    });
    await waitFor(() => expect(members.current.items.map((m) => m.content_id)).toEqual([second!.id, content!.id]), wait);
    await act(async () => {
      await members.current.add([{ content_id: second!.id, active_until: until }]);
    });
    await waitFor(() => expect(Date.parse(members.current.items[0]?.active_until ?? "")).toBe(Date.parse(until)), wait);
    await act(async () => {
      await members.current.remove([{ content_id: second!.id }]);
    });
    await waitFor(() => expect(members.current.items.map((m) => m.content_id)).toEqual([content!.id]), wait);
    // The content's memberships followed the group-side writes.
    await waitFor(() => expect(by(rail.id)?.position).toBe(2), wait);
  });

  it("useKinds reads the content model; staff access to groups", async () => {
    const { result } = renderHook(() => ({ kinds: useKinds(), staff: useStaffAccess(["groups"] as const) }), { wrapper: wrap(staff) });
    await waitFor(() => expect(result.current.kinds.kinds?.entity_kinds?.artist?.roles).toEqual(["publisher"]), wait);
    expect(result.current.kinds.kinds?.kinds.comic).toMatchObject({ route: "comics", presentation: "reader" });
    expect(result.current.kinds.kinds).not.toHaveProperty("content_filters");
    await waitFor(() => expect(result.current.staff.loading).toBe(false), wait);
    expect(result.current.staff.allowed.groups).toBe(true);
    const { result: alice } = renderHook(() => useStaffAccess(["groups"] as const), { wrapper: wrap(client(h, cfg, acc.get("alice"))) });
    await waitFor(() => expect(alice.current.loading).toBe(false), wait);
    expect(alice.current.allowed.groups).toBe(false);
  });
});
