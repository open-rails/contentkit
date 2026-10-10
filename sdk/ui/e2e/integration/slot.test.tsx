// @vitest-environment jsdom
import "../../src/test/dom.js";
import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import type { CropSource, RefBody } from "../../src/client/index.js";
import { useSlotCrop, useSlotImage } from "../../src/react/slot.js";
import type { Config, TestUser } from "../support/harness.js";
import { Accounts, client, harness, item, png, recorder, wait } from "./setup.js";

// jsdom decodes no images: the crop source is the file's known size.
const decode = vi.fn(async (file: File): Promise<CropSource> => ({ url: "blob:preview", width: 800, height: 400, file, revoke: vi.fn() }));
const cover = { preset: "cover", aspect: "3:1", renditions: [] };

describe("useSlotImage and useSlotCrop against the real ContentKit", () => {
  const h = harness();
  const accounts = new Accounts(h);
  let cfg: Config;
  let alice: TestUser;

  beforeAll(async () => {
    cfg = await h.config();
    await accounts.load("alice");
    alice = accounts.get("alice");
  });
  afterEach(() => vi.restoreAllMocks());

  const channel = (): Promise<RefBody> => item(h, "channel", alice).then(({ kind, id }) => ({ kind, id }));
  /** The public files of the item's preset, as an editor read lists them. */
  const published = async (ref: RefBody, preset: string) =>
    (await client(h, cfg, alice).media.read(ref, { editor: true })).public?.find((p) => p.preset === preset)?.renditions ?? [];
  const seed = async (ref: RefBody, path: string, seed: number, w: number, hgt: number) => {
    const c = client(h, cfg, alice);
    await c.media.put(png(`${path}.png`, seed, w, hgt), { ref, path });
    await c.media.waitFor(ref, path, wait);
  };

  it("useSlotImage reads the upload at its path and lists the preset's public files", async () => {
    const ref = await channel();
    await seed(ref, "cover", 2001, 900, 300);
    const r = recorder();
    const c = client(h, cfg, alice, { record: r });
    const { result } = renderHook(() => useSlotImage({ client: c, ref, path: "cover", image: cover }));
    expect(result.current.loading).toBe(true);
    await waitFor(() => expect(result.current.loading).toBe(false), wait);
    expect(result.current.file).toMatchObject({ path: "cover.png", w: 900, h: 300 });
    expect(result.current.aspect).toBe("3:1");
    const first = await published(ref, "cover");
    expect(first.length).toBeGreaterThan(0);
    expect(result.current.renditions).toEqual(first);
    expect(r.calls).toContain("/read");

    // A new cover publishes a new generation; a reload adopts it.
    await seed(ref, "cover", 2002, 900, 300);
    act(() => result.current.reload());
    await waitFor(() => expect(result.current.renditions.map((x) => x.url)).not.toEqual(first.map((x) => x.url)), wait);
    expect(result.current.renditions).toEqual(await published(ref, "cover"));
  });

  it("useSlotImage uses a given read without fetching, then adopts a fresh listing on reload", async () => {
    const ref = await channel();
    const r = recorder();
    const c = client(h, cfg, alice, { record: r });
    const read = { access: "full" as const, expires: 0, total: 0, offset: 0, limit: 50, files: [] };
    const { result } = renderHook(() => useSlotImage({ client: c, ref, path: "avatar", read }));
    expect([result.current.loading, result.current.file, result.current.aspect]).toEqual([false, null, "1:1"]);
    const reads = () => r.calls.filter((p) => p === "/read");
    expect(reads()).toEqual([]);
    await seed(ref, "avatar", 2003, 400, 400);
    act(() => result.current.reload());
    await waitFor(() => expect(result.current.file?.path).toBe("avatar.png"), wait);
    expect(result.current.renditions).toEqual(await published(ref, "avatar"));
    expect(reads()).toEqual(["/read"]);
  });

  it("useSlotImage takes the aspect of the path's public preset while the item has no image", async () => {
    const ref = await channel();
    const c = client(h, cfg, alice);
    const { result } = renderHook(() => useSlotImage({ client: c, ref, path: "cover" }));
    await waitFor(() => expect(result.current.aspect).toBe("3:1"), wait);
    expect(result.current).toMatchObject({ file: null, rule: { kind: "channel", name: "cover", from: "cover", widths: [1500, 3000] } });
    // The image's own aspect wins; a path without a public preset keeps the default.
    expect(renderHook(() => useSlotImage({ client: c, ref, path: "cover", image: { ...cover, aspect: "4:1" } })).result.current.aspect).toBe("4:1");
    const file = await item(h, "file", alice);
    const plain = renderHook(() => useSlotImage({ client: c, ref: { kind: file.kind, id: file.id }, path: "file" }));
    await waitFor(() => expect(plain.result.current.loading).toBe(false), wait);
    expect([plain.result.current.aspect, plain.result.current.rule]).toEqual(["1:1", undefined]);
  });

  it("useSlotCrop: pick → edit → save uploads and puts the file with the edit, without loading retired URLs", async () => {
    const ref = await channel();
    const r = recorder();
    const c = client(h, cfg, alice, { record: r });
    const saved = vi.fn();
    const { result } = renderHook(() => useSlotCrop({ client: c, ref, path: "avatar", aspect: "1:1", decode, onSaved: saved }));
    const file = png("a.png", 2004, 800, 400);
    await act(() => result.current.pick(file));
    expect(result.current.status).toBe("cropping");
    if (result.current.status !== "cropping") return;
    expect(result.current.edit).toBeNull();
    expect(result.current.cropped).toEqual({ width: 400, height: 400 });
    const source = result.current.source;

    const edit = { crop: { x: 40, y: 0, w: 400, h: 400 }, rotate: 270 };
    act(() => result.current.setEdit(edit));
    expect(result.current.cropped).toEqual({ width: 400, height: 400 });
    const fetches = vi.spyOn(globalThis, "fetch");
    await act(async () => void (await result.current.save()));
    expect(result.current.status).toBe("done");
    expect(r.commits[0]).toEqual([{ op: "put", path: "avatar.png", blob: expect.stringMatching(/^u-/), edit }]);
    expect(saved).toHaveBeenCalledWith(expect.objectContaining({ path: "avatar.png", edit }));
    expect(source.revoke).toHaveBeenCalled();
    // Only the API and the bucket: no image of the retired avatar is fetched.
    expect(fetches.mock.calls.map(([u]) => String(u instanceof Request ? u.url : u)).filter((u) => u.startsWith(cfg.media))).toEqual([]);
  });

  it("useSlotCrop: recrop edits the committed upload from its editor view and waits for the render", async () => {
    const ref = await channel();
    const r = recorder();
    const c = client(h, cfg, alice, { record: r });
    const first = { crop: { x: 0, y: 0, w: 600, h: 200 } };
    const file = await c.media.put(png("cover.png", 2005, 900, 300), { ref, path: "cover", edit: first });
    const { result } = renderHook(() => useSlotCrop({ client: c, ref, path: "cover", file, aspect: "3:1", decode }));
    expect([result.current.canRecrop, result.current.aspect]).toEqual([true, "3:1"]);
    await act(() => result.current.recrop());
    await waitFor(() => expect(result.current.status).toBe("cropping"), wait);
    expect(result.current).toMatchObject({
      status: "cropping",
      mode: "recrop",
      edit: first,
      source: { url: expect.stringMatching(new RegExp(`^${cfg.media}/v1/${cfg.namespace}/channel/${ref.id}/private/sha256-`)), width: 900, height: 300 },
    });
    const puts = r.puts.length;
    const next = { crop: { x: 300, y: 100, w: 600, h: 200 }, rotate: 180 };
    await act(async () => void (await result.current.save(next)));
    expect(result.current).toMatchObject({ status: "done", file: { path: "cover.png", edit: next } });
    expect(r.commits.at(-1)).toEqual([{ op: "edit", path: "cover.png", edit: next }]);
    expect(r.puts.length).toBe(puts);
  });

  it("useSlotCrop keeps the source on a refused save so the user can retry", async () => {
    const ref = await channel();
    const c = client(h, cfg, alice);
    const { result } = renderHook(() => useSlotCrop({ client: c, ref, path: "avatar", aspect: "1:1", decode }));
    await act(() => result.current.pick(png("a.png", 2006, 800, 400)));
    // The next presign anywhere is refused (presign names no item in its path).
    await h.faults([{ fault: "api", path: "/api/contentkit/media/upload/presign", method: "POST", status: 413, code: "too_large", error: "too large", times: 1 }]);
    await act(async () => void (await result.current.save()));
    expect(result.current).toMatchObject({ status: "error", error: { code: "too_large" }, mode: "new" });
    expect("source" in result.current && result.current.source).toBeTruthy();
    await act(async () => void (await result.current.save()));
    expect(result.current.status).toBe("done");
  });

  it("useSlotCrop reports undecodable files and cancel closes the source", async () => {
    const ref = await channel();
    const c = client(h, cfg, alice);
    const bad = vi.fn(async () => {
      throw new Error("nope");
    });
    const { result } = renderHook(() => useSlotCrop({ client: c, ref, path: "avatar", decode: bad }));
    await act(() => result.current.pick(png("a.png", 2007)));
    expect(result.current).toMatchObject({ status: "error", error: { code: "decode" } });
    const { result: r2 } = renderHook(() => useSlotCrop({ client: c, ref, path: "avatar", decode }));
    await act(() => r2.current.pick(png("b.png", 2008)));
    const src = "source" in r2.current ? r2.current.source : undefined;
    act(() => r2.current.cancel());
    expect(r2.current.status).toBe("idle");
    expect(src?.revoke).toHaveBeenCalled();
  });
});
