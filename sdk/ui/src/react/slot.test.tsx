// @vitest-environment jsdom
import "./test/dom.js";
import { act, renderHook, waitFor } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { FakeServer, bytes, fakeClient } from "../test/fake.js";
import type { CropSource } from "./image.js";
import { useSlotCrop, useSlotImage } from "./slot-react.js";

const ref = { kind: "channel", id: "0192f000-0000-7000-8000-000000000007" };
const image = { preset: "cover", aspect: "3:1", renditions: [
  { w: 1500, h: 500, url: "https://media.x/cover-1500-generation.webp" },
  { w: 3000, h: 1000, url: "https://media.x/cover-3000-generation.webp" },
] };

function setup() {
  const s = new FakeServer();
  s.publicImages.set(`${ref.kind}/${ref.id}`, [{ from: "cover.png", ...image }]);
  return { s, c: fakeClient(s) };
}

const png = (seed = 1) => new File([bytes(500, seed)], "a.png", { type: "image/png" });
const decode = vi.fn(async (file: File): Promise<CropSource> => ({ url: "blob:preview", width: 800, height: 400, file, revoke: vi.fn() }));
globalThis.fetch = vi.fn(async () => new Response(null)) as typeof fetch;

it("useSlotImage reads the upload at its path and lists the preset's public files", async () => {
  const { s, c } = setup();
  await c.put(png(), { ref, path: "cover" });
  const { result } = renderHook(() => useSlotImage(c, { ref, path: "cover", image }));
  expect(result.current.loading).toBe(true);
  await waitFor(() => expect(result.current.loading).toBe(false));
  expect(result.current.file).toMatchObject({ path: "cover.png", w: 4000 });
  expect(result.current.aspect).toBe("3:1");
  expect(result.current.renditions).toEqual(image.renditions);
  expect(s.calls.at(-1)).toBe("/read");
  const replacement = { from: "cover.png", preset: "cover", renditions: [{ url: "https://media.x/cover-next.webp", w: 400, h: 133 }] };
  s.publicImages.set(`${ref.kind}/${ref.id}`, [replacement]);
  act(() => result.current.reload());
  await waitFor(() => expect(result.current.renditions).toEqual(replacement.renditions));
});

it("useSlotImage uses a given read without fetching, then adopts a fresh listing on reload", async () => {
  const { s, c } = setup();
  const read = { access: "full" as const, expires: 0, total: 0, offset: 0, limit: 50, files: [] };
  const { result } = renderHook(() => useSlotImage(c, { ref, path: "avatar", read }));
  expect([result.current.loading, result.current.file, result.current.aspect]).toEqual([false, null, "1:1"]);
  expect(s.calls).toEqual([]);
  s.seed(ref, [{ path: "avatar.png", type: "image/png", size: 10, w: 400, h: 400 }]);
  const replacement = { from: "avatar.png", preset: "avatar", renditions: [{ url: "https://media.x/avatar-next.webp", w: 400, h: 400 }] };
  s.publicImages.set(`${ref.kind}/${ref.id}`, [replacement]);
  act(() => result.current.reload());
  await waitFor(() => expect(result.current.file?.path).toBe("avatar.png"));
  expect(result.current.renditions).toEqual(replacement.renditions);
  expect(s.calls).toEqual(["/read"]);
});

it("useSlotCrop: pick → edit → save uploads and puts the file with the edit, without reloading retired URLs", async () => {
  const { s, c } = setup();
  const saved = vi.fn();
  const { result } = renderHook(() => useSlotCrop(c, { ref, path: "avatar", aspect: "1:1", decode, onSaved: saved }));
  const file = png(2);
  await act(() => result.current.pick(file));
  expect(result.current.status).toBe("cropping");
  if (result.current.status !== "cropping") return;
  expect(result.current.edit).toBeNull();
  expect(result.current.cropped).toEqual({ width: 400, height: 400 });
  const source = result.current.source;

  const edit = { crop: { x: 40, y: 0, w: 400, h: 400 }, rotate: 270 };
  act(() => result.current.setEdit(edit));
  expect(result.current.cropped).toEqual({ width: 400, height: 400 });
  await act(async () => void (await result.current.save()));
  expect(result.current.status).toBe("done");
  expect(s.commits[0]).toEqual([{ op: "put", path: "avatar.png", blob: expect.stringMatching(/^u-/), edit }]);
  expect(saved).toHaveBeenCalledWith(expect.objectContaining({ path: "avatar.png", edit }));
  expect(source.revoke).toHaveBeenCalled();
  expect(fetch).not.toHaveBeenCalled();
});

it("useSlotCrop: recrop edits the committed upload from its editor view and waits for the render", async () => {
  const { s, c } = setup();
  const first = { crop: { x: 0, y: 40, w: 800, h: 266 } };
  const file = await c.put(png(3), { ref, path: "cover", edit: first });
  const { result } = renderHook(() => useSlotCrop(c, { ref, path: "cover", file, aspect: "3:1", decode }));
  expect([result.current.canRecrop, result.current.aspect]).toEqual([true, "3:1"]);
  await act(() => result.current.recrop());
  expect(result.current).toMatchObject({ status: "cropping", mode: "recrop", edit: first, source: { url: "fake://cdn/private/e-cover.png", width: 4000, height: 3000 } });
  const puts = s.puts.length;
  const next = { crop: { x: 0, y: 100, w: 800, h: 266 }, rotate: 180 };
  await act(async () => void (await result.current.save(next)));
  expect(result.current).toMatchObject({ status: "done", file: { path: "cover.png", edit: next } });
  expect(s.commits.at(-1)).toEqual([{ op: "edit", path: "cover.png", edit: next }]);
  expect(s.puts.length).toBe(puts);
});

it("useSlotCrop keeps the source on a refused save so the user can retry", async () => {
  const { s, c } = setup();
  const { result } = renderHook(() => useSlotCrop(c, { ref, path: "avatar", aspect: "1:1", decode }));
  await act(() => result.current.pick(png(4)));
  s.refuse = { status: 413, code: "too_large", error: "too large" };
  await act(async () => void (await result.current.save()));
  expect(result.current).toMatchObject({ status: "error", error: { code: "too_large" }, mode: "new" });
  expect("source" in result.current && result.current.source).toBeTruthy();
  s.refuse = undefined;
  await act(async () => void (await result.current.save()));
  expect(result.current.status).toBe("done");
});

it("useSlotCrop reports undecodable files and cancel closes the source", async () => {
  const { c } = setup();
  const bad = vi.fn(async () => {
    throw new Error("nope");
  });
  const { result } = renderHook(() => useSlotCrop(c, { ref, path: "avatar", decode: bad }));
  await act(() => result.current.pick(png()));
  expect(result.current).toMatchObject({ status: "error", error: { code: "decode" } });
  const { result: r2 } = renderHook(() => useSlotCrop(c, { ref, path: "avatar", decode }));
  await act(() => r2.current.pick(png()));
  const src = "source" in r2.current ? r2.current.source : undefined;
  act(() => r2.current.cancel());
  expect(r2.current.status).toBe("idle");
  expect(src?.revoke).toHaveBeenCalled();
});
