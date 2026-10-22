// @vitest-environment jsdom
import "../test/dom.js";
import { renderHook, waitFor } from "@testing-library/react";
import { expect, it } from "vitest";
import { createContentKitClient } from "../client/client.js";
import type { PlanSegment, ReaderPlan, ReadResult } from "../client/generated/wire.js";
import { usePlanMedia } from "../components/admin/page-picker.js";
import { useMediaRead } from "./read.js";
import { readKey } from "./store.js";
import { useSegmentPages } from "./versions.js";

const item = { kind: "gallery", id: "0192f000-0000-7000-8000-000000000001", version: "0192f000-0000-7000-8000-000000000002" };

function reader(paths: string[]) {
  const requests: URLSearchParams[] = [];
  const client = createContentKitClient({
    baseUrl: "https://api.test/contentkit",
    fetch: (async (input: RequestInfo | URL) => {
      const query = new URL(String(input)).searchParams;
      requests.push(query);
      const selected = new Set(query.getAll("path"));
      const response: ReadResult = {
        access: "partial", expires: Math.floor(Date.now() / 1000) + 3600,
        total: paths.length, offset: 0, limit: selected.size,
        files: paths.map((path) => ({ path, type: "image/webp", url: selected.has(path) ? `https://media.test/${path}?t=signed` : undefined })),
      };
      return Response.json(response);
    }) as typeof fetch,
  });
  return { client, requests };
}

it("signs a later chapter's selected paths, not the beginning of the version", async () => {
  const paths = ["low/100.webp", "low/101.webp", "low/102.webp"];
  const { client, requests } = reader(paths);
  const segment: PlanSegment = {
    item, start: 100, end: 103, released: true, locked: false,
    pages: paths.map((path, i) => ({ id: `hash-${i}`, name: `${100 + i}.png`, w: 570, h: 806, files: { low: path } })),
  };
  const { result, rerender } = renderHook(({ start, end }) => useSegmentPages(segment, "low", { client, window: { start, end } }), {
    initialProps: { start: 1, end: 3 },
  });
  await waitFor(() => expect(result.current.pages.map((p) => !!p.url)).toEqual([false, true, true]));
  expect(requests[0]!.getAll("path")).toEqual(paths.slice(1));
  expect(requests[0]!.has("offset") || requests[0]!.has("limit")).toBe(false);

  rerender({ start: 0, end: 1 });
  await waitFor(() => expect(result.current.pages.map((p) => !!p.url)).toEqual([true, false, false]));
  expect(requests.at(-1)!.getAll("path")).toEqual(paths.slice(0, 1));
});

it("batches explicit file selections without changing the metadata order", async () => {
  const paths = Array.from({ length: 130 }, (_, i) => `thumb/${i}.webp`);
  const { client, requests } = reader(paths);
  const { result } = renderHook(() => useMediaRead(item, { client, paths }));
  await waitFor(() => expect(result.current.read?.files.filter((f) => f.url)).toHaveLength(130));
  expect(requests.map((q) => q.getAll("path").length).sort((a, b) => a - b)).toEqual([2, 64, 64]);
  expect(result.current.read!.files.map((f) => f.path)).toEqual(paths);
  expect(requests.every((q) => !q.has("offset") && !q.has("limit"))).toBe(true);
});

it("an empty explicit selection does not fall back to a whole-item read", () => {
  const { client, requests } = reader(["low/1.webp"]);
  const { result } = renderHook(() => useMediaRead(item, { client, paths: [] }));
  expect(requests).toHaveLength(0);
  expect(result.current.read).toBeNull();
});

it("loads admin page choices across chapters sharing one version item", async () => {
  const paths = Array.from({ length: 140 }, (_, i) => `thumb/${i}.webp`);
  const { client, requests } = reader(paths);
  const segments = [0, 70].map((start) => ({
    item, start, end: start + 70, released: true, locked: false,
    pages: paths.slice(start, start + 70).map((path, i) => ({ id: `hash-${start + i}`, name: `${start + i}.png`, w: 570, h: 806, files: { thumb: path } })),
  }));
  const plan: ReaderPlan = { content_id: item.id, version_id: item.version, publication_id: "snapshot", pages: 140, segments, previews: [] };
  const { result } = renderHook(() => usePlanMedia(plan, client));
  await waitFor(() => expect(result.current.pages.filter((p) => p.url)).toHaveLength(140));
  expect(result.current.pages.map((p) => p.id)).toEqual(paths.map((_, i) => `hash-${i}`));
  expect(requests.map((q) => q.getAll("path").length).sort((a, b) => a - b)).toEqual([6, 6, 64, 64]);
});

it("shares equivalent selections but keeps different file grants separate", () => {
  expect(readKey(item, { paths: ["b", "a", "a"] })).toBe(readKey(item, { paths: ["a", "b"] }));
  expect(readKey(item, { paths: ["a"] })).not.toBe(readKey(item, { paths: ["b"] }));
});
