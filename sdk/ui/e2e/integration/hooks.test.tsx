// @vitest-environment jsdom
import "../../src/test/dom.js";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeAll, describe, expect, it } from "vitest";
import type { ContentKitClient, Op } from "../../src/client/index.js";
import { ContentKitProvider, useMediaFolder, useMediaRead, usePublicImage } from "../../src/react/index.js";
import type { Config } from "../support/harness.js";
import { Accounts, client, fixture, harness, item, png, wait, NodeFile } from "./setup.js";

describe("media hooks against the real ContentKit", () => {
  const h = harness();
  // The client sends a ref as given: only kind and id.
  const bare = ({ kind, id }: { kind: string; id: string }) => ({ kind, id });
  const accounts = new Accounts(h);
  let cfg: Config;

  beforeAll(async () => {
    cfg = await h.config();
    await accounts.load("alice", "reader");
  });

  const as = (name = "alice"): ContentKitClient => client(h, cfg, accounts.get(name));
  const provider = (c: ContentKitClient) =>
    function Wrapper({ children }: { children: ReactNode }) {
      return <ContentKitProvider client={c}>{children}</ContentKitProvider>;
    };

  it("useMediaRead polls an editor read until the worker is done, and merges a viewer's windows", async () => {
    const ref = bare(await item(h, "gallery", accounts.get("alice")));
    const c = as();
    const ops: Op[] = [];
    for (const n of [1, 2, 3]) {
      const up = await c.media.upload(png(`${n}.png`, n), { ref, path: `originals/00${n}.png` });
      ops.push({ op: "put", path: up.path, blob: up.blob });
    }
    const { result } = renderHook(() => useMediaRead(ref, { editor: true, prefix: "originals/", poll: 100 }), { wrapper: provider(c) });
    await act(async () => void (await c.media.commit(ref, ops)));
    await waitFor(() => expect(result.current.read?.files.map((f) => f.path)).toEqual(["originals/001.png", "originals/002.png", "originals/003.png"]), wait);
    await waitFor(() => expect(result.current.processing).toBe(false), wait);
    expect(result.current.read!.files.every((f) => f.upload && !f.staged && !f.pending)).toBe(true);

    const viewer = renderHook(() => useMediaRead(ref, { prefix: "low-res/", window: { start: 0, end: 3, chunk: 2 } }), { wrapper: provider(as("reader")) });
    await waitFor(() => expect(viewer.result.current.read?.files.filter((f) => f.url)).toHaveLength(3), wait);
    expect(viewer.result.current.read).toMatchObject({ access: "full", total: 3, offset: 0, limit: 4 });
  });

  it("useMediaFolder uploads files, adds them in queue order, moves, renames and removes them", async () => {
    const ref = bare(await item(h, "gallery", accounts.get("alice")));
    const c = as();
    const { result } = renderHook(() => useMediaFolder(ref, { paths: ["originals/{name}"] }), { wrapper: provider(c) });
    await waitFor(() => expect(result.current.read.read).not.toBeNull(), wait);
    act(() => void result.current.add([png("b.png", 11), png("a.png", 12), png("b.png", 13)]));
    await waitFor(() => expect(result.current.queue.ready).toBe(true), wait);
    expect(result.current.queue.items.map((i) => i.path)).toEqual(["originals/b.png", "originals/a.png", "originals/b-2.png"]);
    await act(async () => void (await result.current.commit()));
    const paths = () => result.current.uploads.map((f) => f.path);
    await waitFor(() => expect(paths()).toEqual(["originals/b.png", "originals/a.png", "originals/b-2.png"]), wait);
    expect(result.current.queue.items).toEqual([]);

    await act(async () => result.current.move("originals/a.png", 0));
    await waitFor(() => expect(paths()).toEqual(["originals/a.png", "originals/b.png", "originals/b-2.png"]), wait);
    await act(async () => result.current.rename("originals/b-2.png", "c"));
    await waitFor(() => expect(paths()).toEqual(["originals/a.png", "originals/b.png", "originals/c.png"]), wait);
    await act(async () => result.current.edit("originals/a.png", { rotate: 90 }));
    await waitFor(() => expect(result.current.uploads[0]?.edit).toEqual({ rotate: 90 }), wait);
    await act(async () => result.current.remove(["originals/b.png", "originals/c.png"]));
    await waitFor(() => expect(paths()).toEqual(["originals/a.png"]), wait);
    // The server stays the authority: a rename onto a taken name is refused.
    await act(async () => void (await c.media.commit(ref, [{ op: "put", path: "originals/z.png", blob: (await c.media.upload(png("z.png", 14), { ref, path: "originals/z.png" })).blob }])));
    await waitFor(() => expect(paths()).toContain("originals/z.png"), wait);
    await act(async () => {
      await expect(result.current.rename("originals/z.png", "a")).rejects.toMatchObject({ code: "conflict" });
    });
  });

  it("useMediaFolder screens files by the kind's rules from the editor read before anything uploads", async () => {
    // note: originals/{name} takes at most two PNGs of up to 1 MiB (its inline/{name} is server-named, not a folder path).
    const ref = bare(await item(h, "note", accounts.get("alice")));
    const { result } = renderHook(() => useMediaFolder(ref), { wrapper: provider(as()) });
    await waitFor(() => expect(result.current.groups.map((g) => g.path)).toEqual(["originals/{name}"]), wait);
    expect(result.current.groups[0]!.rule).toMatchObject({ types: ["image/png"], max_bytes: 1 << 20, max: 2 });
    const jpeg = new NodeFile([new Uint8Array(10)], "a.jpg", { type: "image/jpeg" }) as unknown as File;
    act(() => result.current.add([png("1.png", 21), png("2.png", 22), png("3.png", 23), jpeg]));
    expect(result.current.refused.map((r) => [r.file.name, r.error.code])).toEqual([
      ["3.png", "too_many_files"],
      ["a.jpg", "type_not_allowed"],
    ]);
    await waitFor(() => expect(result.current.queue.ready).toBe(true), wait);
    await act(async () => void (await result.current.commit()));
    await waitFor(() => expect(result.current.uploads.map((f) => f.path)).toEqual(["originals/1.png", "originals/2.png"]), wait);
  });

  it("useMediaFolder holds files added before the editor read and routes them by its rules", async () => {
    // note: originals/{name} takes at most two PNGs; inline/{name} (server-named) takes JPEGs too.
    const ref = bare(await item(h, "note", accounts.get("alice")));
    const { result } = renderHook(() => useMediaFolder(ref, { paths: ["originals/{name}", "inline/{name}"] }), { wrapper: provider(as()) });
    expect(result.current.read.read).toBeNull();
    act(() => result.current.add([png("1.png", 31), png("2.png", 32), png("3.png", 33), fixture("avatar.jpg", "image/jpeg", "a.jpg")]));
    expect(result.current).toMatchObject({ busy: true, fileCount: 4, refused: [] });
    expect(result.current.waiting.map((f) => f.name)).toEqual(["1.png", "2.png", "3.png", "a.jpg"]);
    expect(result.current.queue.items).toEqual([]);

    // The rules arrive: PNGs go to originals (at most 2), the JPEG only inline takes.
    await waitFor(() => expect(result.current.queue.ready).toBe(true), wait);
    expect(result.current.waiting).toEqual([]);
    expect(result.current.queue.items.map((i) => i.path)).toEqual(["originals/1.png", "originals/2.png", expect.stringMatching(/^inline\/.+\.jpg$/)]);
    expect(result.current.refused.map((r) => [r.file.name, r.error.code])).toEqual([["3.png", "too_many_files"]]);
    expect(result.current).toMatchObject({ busy: false, fileCount: 3 });
    await act(async () => void (await result.current.commit()));
    await waitFor(() => expect(result.current.groups.map((g) => g.files.length)).toEqual([2, 1]), wait);
    expect(result.current).toMatchObject({ busy: false, fileCount: 3 });
    expect(result.current.queue.items).toEqual([]);
  });

  it("useMediaFolder refuses files waiting when the editor read states no rules (a reader's)", async () => {
    const ref = bare(await item(h, "note", accounts.get("alice")));
    const { result } = renderHook(() => useMediaFolder(ref, { paths: ["originals/{name}"] }), { wrapper: provider(as("reader")) });
    act(() => result.current.add([png("1.png", 41)]));
    expect(result.current.waiting).toHaveLength(1);
    await waitFor(() => expect(result.current.refused.map((r) => [r.file.name, r.error.code])).toEqual([["1.png", "forbidden"]]), wait);
    expect(result.current.read.read).toMatchObject({ access: "full" });
    expect(result.current.read.read?.uploads).toBeUndefined();
    expect(result.current).toMatchObject({ busy: false, fileCount: 0, waiting: [] });
    expect(result.current.queue.items).toEqual([]);
  });

  it("usePublicImage shows an item's published image from its read, else the kind's default", async () => {
    const c = as();
    const ref = bare(await item(h, "gallery", accounts.get("alice")));
    await c.media.put(fixture("small.png", "image/png", "cover.png"), { ref, path: "cover" });
    await c.media.waitFor(ref, "cover", wait);
    expect(await c.media.presets()).toContainEqual(expect.objectContaining({ kind: "gallery", name: "cover", from: "cover", namespace: cfg.namespace }));
    const { result } = renderHook(() => usePublicImage("gallery", ref.id, "cover"), { wrapper: provider(c) });
    await waitFor(() => expect(result.current).toMatchObject({ loading: false, isDefault: false, image: expect.anything() }), wait);
    expect(result.current.rule).toMatchObject({ kind: "gallery", name: "cover", from: "cover", widths: [230, 460], aspect: "3:1" });
    expect(result.current.image).toMatchObject({ preset: "cover", aspect: "3:1" });
    // Published names carry a generation; a template never names them.
    const names = result.current.image!.renditions.map((r) => new URL(r.url).pathname.split("/").pop());
    expect(names.length).toBeGreaterThan(0);
    for (const name of names) expect(name).toMatch(/^cover-(230|460)-.+\.webp$/);

    // An item without a cover shows the kind's default.
    const empty = bare(await item(h, "gallery", accounts.get("alice")));
    const other = renderHook(() => usePublicImage("gallery", empty.id, "cover"), { wrapper: provider(c) });
    await waitFor(() => expect(other.result.current.isDefault).toBe(true), wait);
    expect(other.result.current.image!.renditions.map((r) => r.url)).toEqual([230, 460].map((w) => `${cfg.media}/v1/${cfg.namespace}/gallery/${empty.id}/public/cover-${w}.webp`));
  });
});
