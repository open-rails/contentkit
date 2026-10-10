// @vitest-environment jsdom
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it } from "vitest";
import { createContentKitClient } from "../client/client.js";
import { ContentKitProvider } from "./provider.js";
import { useComments } from "./comments.js";
import { useMediaRead } from "./read.js";
import { useReaction } from "./engagement.js";

afterEach(cleanup);
const ref = { kind: "gallery", id: "0192f000-0000-7000-8000-000000000001" };

it.each(["viewer", "language", "accessRevision"] as const)("isolates mounted media and comment reads when %s changes", async (field) => {
  let scope = { viewer: "alice" as string | null, language: "en", accessRevision: 0 };
  const requests: { media: boolean; signal?: AbortSignal | null; finish: (response: Response) => void }[] = [];
  const client = createContentKitClient({
    baseUrl: "https://content.test/ck",
    language: () => scope.language,
    fetch: (url, init) => new Promise<Response>((finish) => requests.push({ media: String(url).includes("/media/"), signal: init?.signal, finish })),
  });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} {...scope}>{children}</ContentKitProvider>;
  const { result, rerender } = renderHook(() => ({
    media: useMediaRead(ref),
    sharedMedia: useMediaRead(ref),
    comments: useComments(ref),
    sharedComments: useComments(ref),
  }), { wrapper });

  const finish = (batch: typeof requests, value: number) => {
    for (const r of batch) r.finish(Response.json(r.media
      ? { access: "full", expires: 0, total: value, offset: 0, limit: 50, files: [] }
      : [{ id: String(value), body: `comment ${value}`, likes: 0, dislikes: 0, reply_count: 0 }]));
  };

  await waitFor(() => expect(requests).toHaveLength(2));
  await act(async () => finish(requests, 1));
  await waitFor(() => expect(result.current.media.read?.total).toBe(1));
  expect(result.current.sharedMedia.read).toBe(result.current.media.read);
  expect(result.current.sharedComments.items).toBe(result.current.comments.items);
  rerender();
  expect(requests).toHaveLength(2);

  // Keep old-scope refreshes in flight across the transition.
  act(() => { result.current.media.reload(); result.current.comments.reload(); });
  await waitFor(() => expect(requests).toHaveLength(4));
  const old = requests.slice(2);
  if (field === "viewer") scope = { ...scope, viewer: null };
  if (field === "language") scope = { ...scope, language: "ja" };
  if (field === "accessRevision") scope = { ...scope, accessRevision: 1 };
  rerender();
  expect(result.current.media.read).toBeNull();
  expect(result.current.comments.items).toEqual([]);
  await waitFor(() => expect(requests).toHaveLength(6));
  expect(old.every((r) => r.signal?.aborted)).toBe(true);
  await act(async () => finish(requests.slice(4), 2));
  await waitFor(() => expect(result.current.media.read?.total).toBe(2));
  // A transport can settle even after cancellation; it must not restore old data.
  await act(async () => finish(old, 99));
  expect(result.current.media.read?.total).toBe(2);
  expect(result.current.comments.items.map((c) => c.id)).toEqual(["2"]);
  expect(result.current.sharedMedia.read).toBe(result.current.media.read);
  expect(result.current.sharedComments.items).toBe(result.current.comments.items);
});

it("seeds a new viewer's read from the current initial data, not the previous viewer's", () => {
  const client = createContentKitClient({ baseUrl: "https://content.test/ck" });
  let viewer = "alice";
  let initial = { likes: 1, dislikes: 0, mine: 1 };
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer={viewer}>{children}</ContentKitProvider>;
  const { result, rerender } = renderHook(() => useReaction(ref, { initial }), { wrapper });
  expect(result.current.counts.mine).toBe(1);
  viewer = "bob";
  initial = { likes: 1, dislikes: 0, mine: 0 };
  rerender();
  expect(result.current.counts.mine).toBe(0);
});
