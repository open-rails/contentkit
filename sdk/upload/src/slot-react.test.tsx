// @vitest-environment jsdom
import "./test/dom.js";
import { act, renderHook, waitFor } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { FakeServer, bytes } from "../test/fake.js";
import { UploadClient } from "./client.js";
import type { CropSource } from "./image.js";
import { useSlotCrop, useSlotImage } from "./slot-react.js";

const ref = { kind: "channel", id: "7" };

function setup() {
  const s = new FakeServer();
  const c = new UploadClient({ endpoint: "http://x/api", fetch: s.fetch, transport: s.transport, retryDelay: () => 0 });
  return { s, c };
}

const png = (seed = 1) => new File([bytes(500, seed)], "a.png", { type: "image/png" });
const decode = vi.fn(async (file: File): Promise<CropSource> => ({ url: "blob:preview", width: 800, height: 400, file, revoke: vi.fn() }));

it("useSlotImage fetches the manifest and builds srcset", async () => {
  const { s, c } = setup();
  await c.uploadSlot(png(), { ref, slot: "avatar" });
  const { result } = renderHook(() => useSlotImage(c, { ref, slot: "avatar" }));
  expect(result.current.loading).toBe(true);
  await waitFor(() => expect(result.current.loading).toBe(false));
  expect(result.current.aspect).toBe("1:1");
  expect(result.current.srcSet).toMatch(/avatar_128\.webp#\d+ 128w, .* 256w, .* 512w$/);
  expect(result.current.src).toContain("avatar_512");
  expect(s.calls.filter((p) => p === "/slot")).toHaveLength(1);
});

it("useSlotImage uses a given manifest without fetching", () => {
  const { s, c } = setup();
  const manifest = { aspect: "3:1", pending: false, outputs: [{ name: "c", w: 1500, h: 500, url: "u" }] };
  const { result } = renderHook(() => useSlotImage(c, { ref, slot: "cover", manifest }));
  expect([result.current.loading, result.current.aspect, result.current.srcSet]).toEqual([false, "3:1", "u 1500w"]);
  expect(s.calls).toEqual([]);
});

it("useSlotCrop: pick → edit → save uploads the original with the edit", async () => {
  const { s, c } = setup();
  const saved = vi.fn();
  const { result } = renderHook(() => useSlotCrop(c, { ref, slot: "avatar", aspect: "1:1", decode, onSaved: saved }));
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
  expect(s.slotCalls[0]).toMatchObject({ slot: "avatar", edit });
  expect(saved).toHaveBeenCalledWith(expect.objectContaining({ aspect: "1:1" }));
  expect(source.revoke).toHaveBeenCalled();
});

it("useSlotCrop: recrop re-edits the committed original and waits for the encode", async () => {
  const { s, c } = setup();
  const first = { crop: { x: 0, y: 40, w: 800, h: 266 } };
  const { manifest } = await c.uploadSlot(png(3), { ref, slot: "cover", edit: first });
  const { result } = renderHook(() => useSlotCrop(c, { ref, slot: "cover", manifest, decode }));
  expect([result.current.canRecrop, result.current.aspect]).toEqual([true, "3:1"]);
  await act(() => result.current.recrop());
  expect(s.calls).toContain("/slot-original");
  expect(result.current).toMatchObject({ status: "cropping", mode: "recrop", edit: first, source: { url: "blob:preview" } });
  s.pendingReads = 1;
  const puts = s.puts.length;
  const next = { crop: { x: 0, y: 100, w: 800, h: 266 }, rotate: 180 };
  await act(async () => void (await result.current.save(next)));
  expect(result.current.status).toBe("done");
  expect(s.slotCalls.at(-1)).toEqual({ ref, slot: "cover", edit: next });
  expect(s.puts.length).toBe(puts);
  expect(result.current).toMatchObject({ status: "done", manifest: { pending: false } });
});

it("useSlotCrop keeps the source on a refused save so the user can retry", async () => {
  const { s, c } = setup();
  const { result } = renderHook(() => useSlotCrop(c, { ref, slot: "avatar", aspect: "1:1", decode }));
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
  const { result } = renderHook(() => useSlotCrop(c, { ref, slot: "avatar", decode: bad }));
  await act(() => result.current.pick(png()));
  expect(result.current).toMatchObject({ status: "error", error: { code: "decode" } });
  const { result: r2 } = renderHook(() => useSlotCrop(c, { ref, slot: "avatar", decode }));
  await act(() => r2.current.pick(png()));
  const src = "source" in r2.current ? r2.current.source : undefined;
  act(() => r2.current.cancel());
  expect(r2.current.status).toBe("idle");
  expect(src?.revoke).toHaveBeenCalled();
});
