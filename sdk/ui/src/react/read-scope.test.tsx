// @vitest-environment jsdom
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../client/client.js";
import { ContentKitProvider } from "./provider.js";
import { useComments } from "./comments.js";
import { useMediaRead } from "./read.js";
import { useReaction } from "./engagement.js";
import { useSlotCrop, useSlotImage } from "./slot.js";

afterEach(cleanup);
const ref = { kind: "gallery", id: "0192f000-0000-7000-8000-000000000001" };

it.each([false, true])("rolls back a failed comment reaction after a reply, unless refetched: %s", async (refetch) => {
  let mine = 0;
  const reactions: ((response: Response) => void)[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async (url, init) => {
    if (String(url).endsWith("/like")) return new Promise<Response>((resolve) => reactions.push(resolve));
    if (init?.method === "POST") return Response.json({ id: "reply", reply_to_id: "parent", body: "reply", likes: 0, dislikes: 0, reply_count: 0 });
    return Response.json([{ id: "parent", body: "parent", likes: mine, dislikes: 0, mine, reply_count: mine }]);
  } });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer="alice">{children}</ContentKitProvider>;
  const { result } = renderHook(() => useComments(ref), { wrapper });
  await waitFor(() => expect(result.current.loading).toBe(false));
  let pending: Promise<void>;
  act(() => { pending = result.current.react(result.current.items[0]!, 1); });
  const failed = expect(pending!).rejects.toMatchObject({ code: "unavailable" });
  await waitFor(() => expect(reactions).toHaveLength(1));
  await act(async () => { await result.current.post("reply", { replyTo: "parent" }); });
  expect(result.current.items[0]?.reply_count).toBe(1);
  if (refetch) {
    mine = 1;
    act(() => result.current.reload());
    await waitFor(() => expect(result.current.loading).toBe(false));
  }
  await act(async () => { reactions[0]!(Response.json({ error: "unavailable" }, { status: 503 })); await failed; });
  expect(result.current.items[0]).toMatchObject({ mine: refetch ? 1 : 0, likes: refetch ? 1 : 0, reply_count: 1 });
});

it.each(["viewer", "accessRevision", "round-trip"] as const)("discards a late crop completion after %s changes", async (transition) => {
  let scope = { viewer: "alice", accessRevision: 0 };
  let afterCommit = false;
  let currentScope = false;
  const processing: { finish: (response: Response) => void; signal?: AbortSignal | null }[] = [];
  const file = { path: "cover.jpg", upload: true, type: "image/jpeg", size: 10, w: 100, h: 100, editor_url: "https://media.test/alice.jpg" };
  const empty = { access: "none", expires: 0, total: 0, offset: 0, limit: 50, files: [] };
  const saved = vi.fn();
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async (url, init) => {
    if (String(url).endsWith("/presets")) return Response.json([]);
    if (init?.method === "POST") { afterCommit = true; return Response.json({ files: [file] }); }
    if (currentScope) return Response.json(empty);
    if (afterCommit) return new Promise<Response>((finish) => processing.push({ finish, signal: init?.signal }));
    return Response.json({ ...empty, access: "full", files: [file] });
  } });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} {...scope}>{children}</ContentKitProvider>;
  const { result, rerender } = renderHook(() => {
    const image = useSlotImage({ ref, path: "cover" });
    const crop = useSlotCrop({ ref, path: "cover", file: image.file, onSaved: (f) => { image.set(f); image.reload(); saved(f); } });
    return { image, crop };
  }, { wrapper });
  await waitFor(() => expect(result.current.image.file?.path).toBe("cover.jpg"));
  await act(async () => { await result.current.crop.recrop(); });
  let pending: ReturnType<typeof result.current.crop.save>;
  act(() => { pending = result.current.crop.save(); });
  await waitFor(() => expect(processing).toHaveLength(2));
  currentScope = true;
  scope = transition === "accessRevision" ? { ...scope, accessRevision: 1 } : { ...scope, viewer: "bob" };
  rerender();
  await waitFor(() => expect(result.current.image.loading).toBe(false));
  if (transition === "round-trip") {
    scope = { ...scope, viewer: "alice" };
    rerender();
    await waitFor(() => expect(result.current.image.loading).toBe(false));
  }
  expect(result.current.image.file).toBeNull();
  expect(result.current.crop.status).toBe("idle");
  expect(processing.every((p) => p.signal?.aborted)).toBe(true);
  await act(async () => {
    for (const p of processing) p.finish(Response.json({ ...empty, access: "full", files: [file] }));
    expect(await pending).toBeUndefined();
  });
  expect(saved).not.toHaveBeenCalled();
  expect(result.current.image.file).toBeNull();
  expect(result.current.crop.status).toBe("idle");
});

it.each([
  ["comment", "language"], ["reaction", "viewer"], ["reaction", "round-trip"],
  ["comment", "unchanged"], ["reaction", "unchanged"],
] as const)("scopes the %s response after %s", async (kind, transition) => {
  let scope = { viewer: "alice", language: "en" };
  let reads = 0;
  const onChange = vi.fn();
  const writes: ((response: Response) => void)[] = [];
  const client = createContentKitClient({
    baseUrl: "https://content.test/ck",
    language: () => scope.language,
    token: () => scope.viewer,
    fetch: async (url, init) => {
      if (init?.method === "POST") return new Promise<Response>((resolve) => writes.push(resolve));
      reads++;
      return Response.json(String(url).endsWith("/reaction") ? { likes: 0, dislikes: 0, mine: 0 } : []);
    },
  });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} {...scope} onChange={onChange}>{children}</ContentKitProvider>;
  const { result, rerender } = renderHook(() => ({ comments: useComments(ref), reaction: useReaction(ref) }), { wrapper });
  await waitFor(() => expect(result.current.comments.loading || result.current.reaction.loading).toBe(false));
  let pending: Promise<unknown>;
  act(() => { pending = kind === "comment" ? result.current.comments.post("old language") : result.current.reaction.set(1); });
  await waitFor(() => expect(writes).toHaveLength(1));
  if (transition === "language") scope = { ...scope, language: "ja" };
  if (transition === "viewer" || transition === "round-trip") scope = { ...scope, viewer: "bob" };
  rerender();
  await waitFor(() => expect(result.current.comments.loading || result.current.reaction.loading).toBe(false));
  if (transition === "round-trip") {
    scope = { ...scope, viewer: "alice" };
    rerender();
    await waitFor(() => expect(result.current.comments.loading || result.current.reaction.loading).toBe(false));
  }
  const before = reads;
  await act(async () => {
    writes[0]!(Response.json(kind === "comment"
      ? { id: "old", body: "old language", likes: 0, dislikes: 0, reply_count: 0 }
      : { likes: 1, dislikes: 0, mine: 1 }));
    await pending;
  });
  await waitFor(() => expect(result.current.comments.loading || result.current.reaction.loading).toBe(false));
  expect(result.current.comments.items.map((c) => c.id)).toEqual(kind === "comment" && transition === "unchanged" ? ["old"] : []);
  expect(result.current.reaction.counts.mine).toBe(kind === "reaction" && transition === "unchanged" ? 1 : 0);
  expect(reads).toBe(before + (transition === "unchanged" ? 0 : 1));
  expect(onChange).toHaveBeenCalledTimes(1);
  expect(onChange).toHaveBeenLastCalledWith(
    expect.objectContaining({ type: kind === "comment" ? "comment.created" : "reaction.changed" }),
    transition === "unchanged" ? JSON.stringify(scope) : null,
  );
});

it.each([false, true])("scopes processed media when access changed: %s", async (changed) => {
  let accessRevision = 0;
  let reads = 0;
  const processing: ((response: Response) => void)[] = [];
  const empty = { access: "full", expires: 0, total: 0, offset: 0, limit: 50, files: [] };
  const client = createContentKitClient({
    baseUrl: "https://content.test/ck",
    fetch: async (url) => {
      if (String(url).includes("prefix=cover")) return new Promise<Response>((resolve) => processing.push(resolve));
      reads++;
      return Response.json(empty);
    },
  });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} accessRevision={accessRevision}>{children}</ContentKitProvider>;
  const { result, rerender } = renderHook(() => useMediaRead(ref, { editor: true, poll: false }), { wrapper });
  await waitFor(() => expect(result.current.loading).toBe(false));
  const pending = client.media.waitFor(ref, "cover.jpg");
  await waitFor(() => expect(processing).toHaveLength(1));
  if (changed) accessRevision++;
  rerender();
  await waitFor(() => expect(result.current.loading).toBe(false));
  const before = reads;
  await act(async () => {
    processing[0]!(Response.json({ ...empty, files: [{ path: "cover.jpg", upload: true, size: 10 }] }));
    await pending;
  });
  await waitFor(() => expect(result.current.loading).toBe(false));
  expect(result.current.read?.files.map((f) => f.path)).toEqual(changed ? [] : ["cover.jpg"]);
  expect(reads).toBe(before + (changed ? 1 : 0));
});

it.each(["unchanged", "viewer", "round-trip"] as const)("keeps failed comment-reaction rollback in its original read after %s", async (transition) => {
  let viewer = "alice";
  let mine = 0;
  const writes: ((response: Response) => void)[] = [];
  const client = createContentKitClient({
    baseUrl: "https://content.test/ck",
    fetch: async (_url, init) => {
      if (init?.method === "POST") return new Promise<Response>((resolve) => writes.push(resolve));
      return Response.json([{ id: "comment", body: "hello", likes: 2, dislikes: 0, mine, reply_count: 0 }]);
    },
  });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer={viewer}>{children}</ContentKitProvider>;
  const { result, rerender } = renderHook(() => useComments(ref), { wrapper });
  await waitFor(() => expect(result.current.loading).toBe(false));
  let pending: Promise<void>;
  act(() => { pending = result.current.react(result.current.items[0]!, -1); });
  const failed = expect(pending!).rejects.toMatchObject({ code: "unavailable" });
  expect(result.current.items[0]?.mine).toBe(-1);
  await waitFor(() => expect(writes).toHaveLength(1));
  if (transition !== "unchanged") { viewer = "bob"; mine = 1; }
  rerender();
  await waitFor(() => expect(result.current.loading).toBe(false));
  if (transition === "round-trip") {
    viewer = "alice";
    rerender();
    await waitFor(() => expect(result.current.loading).toBe(false));
  }
  await act(async () => {
    writes[0]!(Response.json({ error: "unavailable" }, { status: 503 }));
    await failed;
  });
  expect(result.current.items[0]?.mine).toBe(transition === "unchanged" ? 0 : 1);
  expect(result.current.items[0]?.likes).toBe(2);
});

it("does not roll back a new reaction after leaving and returning to the same viewer", async () => {
  let viewer = "alice";
  const writes: ((response: Response) => void)[] = [];
  const client = createContentKitClient({
    baseUrl: "https://content.test/ck",
    fetch: async (_url, init) => init?.method === "POST"
      ? new Promise<Response>((resolve) => writes.push(resolve))
      : Response.json({ likes: 0, dislikes: 0, mine: 0 }),
  });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer={viewer}>{children}</ContentKitProvider>;
  const { result, rerender } = renderHook(() => useReaction(ref), { wrapper });
  await waitFor(() => expect(result.current.loading).toBe(false));
  let old: Promise<void>;
  act(() => { old = result.current.set(1); });
  const failed = expect(old!).rejects.toMatchObject({ code: "unavailable" });
  await waitFor(() => expect(writes).toHaveLength(1));
  for (const next of ["bob", "alice"]) {
    viewer = next;
    rerender();
    await waitFor(() => expect(result.current.loading).toBe(false));
  }
  let current: Promise<void>;
  act(() => { current = result.current.set(-1); });
  await waitFor(() => expect(writes).toHaveLength(2));
  await act(async () => {
    writes[0]!(Response.json({ error: "unavailable" }, { status: 503 }));
    await failed;
  });
  expect(result.current.counts.mine).toBe(-1);
  await act(async () => {
    writes[1]!(Response.json({ likes: 0, dislikes: 1, mine: -1 }));
    await current;
  });
  expect(result.current.counts.mine).toBe(-1);
});

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
