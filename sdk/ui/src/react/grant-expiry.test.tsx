// @vitest-environment jsdom
import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../client/client.js";
import { ContentKitError } from "../client/errors.js";
import type { ReaderPlan, ReadResult } from "../client/generated/wire.js";
import { useMediaRead } from "./read.js";
import { useReaderPlan } from "./versions.js";

const ref = { kind: "gallery", id: "0192f000-0000-7000-8000-000000000001", version: "0192f000-0000-7000-8000-000000000002" };
const client = () => createContentKitClient({ baseUrl: "https://api.test/ck" });
const plan = (): ReaderPlan => ({
  content_id: ref.id, version_id: ref.version, publication_id: "snapshot", pages: 1,
  expires: Math.floor(Date.now() / 1000) + 10,
  segments: [{ start: 0, end: 1, locked: true, released: true }],
  previews: [{ id: "a".repeat(64), index: 0, name: "one", w: 570, h: 806, url: "https://media.test/one?t=signed" }],
});
const read = (): ReadResult => ({
  access: "partial", expires: Math.floor(Date.now() / 1000) + 10, total: 1, offset: 0, limit: 1,
  files: [{ path: "low/one.webp", type: "image/webp", w: 570, h: 806, url: "https://media.test/one?t=signed", editor_url: "https://media.test/editor?t=signed" }],
  public: [{ preset: "cover", from: "cover.png", renditions: [{ url: "https://media.test/cover.webp", w: 570, h: 806 }] }],
});
const flush = async () => {
  for (let i = 0; i < 5; i++) await act(async () => { await vi.advanceTimersByTimeAsync(1); });
};
beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout"] });
  vi.setSystemTime(new Date("2026-10-22T12:00:00Z"));
});
afterEach(() => { cleanup(); vi.useRealTimers(); });

it.each([[401, "unauthorized"], [403, "forbidden"], [404, "not_found"], [410, "not_found"]] as const)("suppresses cached reader plans after %i and while retrying", async (status, code) => {
  const c = client();
  vi.spyOn(c.versions, "reader").mockResolvedValueOnce(plan()).mockRejectedValueOnce(new ContentKitError(code, code, { status })).mockImplementation(() => new Promise(() => {}));
  const { result } = renderHook(() => useReaderPlan(ref.id, ref.version, { client: c }));
  await flush();
  expect(result.current.plan?.previews).toHaveLength(1);
  act(() => result.current.reload());
  await flush();
  expect(result.current.plan).toBeNull();
  expect(result.current.error?.code).toBe(code);
  act(() => result.current.reload());
  await flush();
  expect(result.current.loading).toBe(true);
  expect(result.current.plan).toBeNull();
});

it("preserves plan metadata during a transient failure but expires previews during a hanging retry", async () => {
  const c = client();
  let finish!: (value: ReaderPlan) => void;
  vi.spyOn(c.versions, "reader").mockResolvedValueOnce(plan()).mockRejectedValueOnce(new ContentKitError("unavailable", "offline", { status: 503 })).mockImplementation(() => new Promise((resolve) => { finish = resolve; }));
  const { result } = renderHook(() => useReaderPlan(ref.id, ref.version, { client: c }));
  await flush();
  act(() => result.current.reload());
  await flush();
  expect(result.current.plan?.previews).toHaveLength(1);
  expect(result.current.plan?.pages).toBe(1);
  act(() => result.current.reload());
  await flush();
  await act(async () => { await vi.advanceTimersByTimeAsync(11_000); });
  expect(result.current.loading).toBe(true);
  expect(result.current.plan?.previews).toHaveLength(0);
  expect(result.current.plan?.pages).toBe(1);
  await act(async () => { finish(plan()); });
  await flush();
  expect(result.current.plan?.previews).toHaveLength(1);
});

it("does not retain a plan after its content context disappears", async () => {
  const c = client();
  vi.spyOn(c.versions, "reader").mockResolvedValue(plan());
  const { result, rerender } = renderHook(({ content }: { content: string | null }) => useReaderPlan(content, ref.version, { client: c }), { initialProps: { content: ref.id as string | null } });
  await flush();
  expect(result.current.plan).not.toBeNull();
  rerender({ content: null });
  expect(result.current.plan).toBeNull();
});

it.each([false, true])("expires protected read URLs while preserving public metadata (refresh=%s)", async (refresh) => {
  const c = client();
  const fetch = vi.spyOn(c.media, "read").mockResolvedValueOnce({ ...read(), expires: Math.floor(Date.now() / 1000) + 120 }).mockImplementation(() => new Promise(() => {}));
  const { result } = renderHook(() => useMediaRead(ref, { client: c, refresh }));
  await flush();
  expect(result.current.read?.files[0]?.url).toBeTruthy();
  await act(async () => { await vi.advanceTimersByTimeAsync(121_000); });
  expect(result.current.read?.files[0]).toMatchObject({ path: "low/one.webp", w: 570, h: 806 });
  expect(result.current.read?.files[0]?.url).toBeUndefined();
  expect(result.current.read?.files[0]?.editor_url).toBeUndefined();
  expect(result.current.read?.public?.[0]?.renditions[0]?.url).toBe("https://media.test/cover.webp");
  expect(fetch).toHaveBeenCalledTimes(refresh ? 2 : 1);
  expect(result.current.loading).toBe(refresh);
});
