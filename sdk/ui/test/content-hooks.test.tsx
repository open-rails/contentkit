// @vitest-environment jsdom
import "../src/test/dom.js";
import { File as NodeFile } from "node:buffer";
import type { ChildProcess } from "node:child_process";
import { randomUUID } from "node:crypto";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { createContentKitClient, fetchTransport, type ContentKitClient } from "../src/client/index.js";
import {
  ContentKitProvider,
  useCanComment,
  useCommentBans,
  useCommentReplies,
  useComments,
  useFavorite,
  useModerationQueue,
  usePoll,
  usePollEditor,
  usePolls,
  usePost,
  usePosts,
  useReaction,
} from "../src/react/index.js";
import { startServer, stopServer } from "./server.js";

const endpoint = process.env.CONTENTKIT_TEST_S3_ENDPOINT;
// Node's File: jsdom's Blob is not a body Node's fetch can send.
const fixture = readFileSync(resolve(import.meta.dirname, "../e2e/fixtures/small.png"));
const png = () => new NodeFile([fixture], "photo.png", { type: "image/png" }) as unknown as File;
const wait = { timeout: 30_000, interval: 50 };

function uuid7(suffix = ""): string {
  const hex = Date.now().toString(16).padStart(12, "0") + "7" + randomUUID().replace(/-/g, "").slice(0, 19);
  const v = hex.slice(0, 16) + "8" + hex.slice(17, 32 - suffix.length) + suffix;
  return `${v.slice(0, 8)}-${v.slice(8, 12)}-${v.slice(12, 16)}-${v.slice(16, 20)}-${v.slice(20, 32)}`;
}

describe.skipIf(!endpoint)("content hooks against contentkit.Runtime.Handler", () => {
  let proc: ChildProcess;
  let base: string;
  let requests: string[];

  beforeAll(async () => {
    const s = await startServer(endpoint!);
    proc = s.proc;
    base = s.url;
  });
  afterAll(async () => {
    if (proc) await stopServer(proc);
  });

  /** A client as actor (anonymous from ip without one) that records each request's method and path. */
  function client(actor?: string, ip = "10.9.0.1"): ContentKitClient {
    requests = [];
    return createContentKitClient({
      baseUrl: `${base}/ck`,
      headers: () => ({ "X-Test-IP": ip, ...(actor ? { "X-Test-Actor": actor } : {}) }),
      fetch: (input, init) => {
        requests.push(`${init?.method ?? "GET"} ${new URL(String(input)).pathname.replace(/^\/ck/, "")}`);
        return fetch(input, init);
      },
      media: { transport: fetchTransport, retryDelay: () => 100 },
      folders: { post: "ckpost", poll: "ckpoll" },
    });
  }
  const wrap = (c: ContentKitClient, viewer?: string | null) =>
    function Wrapper({ children }: { children: ReactNode }) {
      return (
        <ContentKitProvider client={c} viewer={viewer}>
          {children}
        </ContentKitProvider>
      );
    };
  const video = () => ({ kind: "video", id: uuid7() });

  it("useComments: one read shared by every hook, posts and replies land in place, reactions apply at once and roll back", async () => {
    const ref = video();
    const seed = client("bob");
    const first = await seed.comments.create(ref, { body: "seeded" });
    const c = client("alice");
    const { result } = renderHook(
      () => ({ a: useComments(ref, { pageSize: 5 }), b: useComments(ref, { pageSize: 5 }), standing: useCanComment(ref), replies: useCommentReplies(first.id) }),
      { wrapper: wrap(c, "alice") },
    );
    await waitFor(() => expect(result.current.a.items.map((x) => x.id)).toEqual([first.id]), wait);
    await waitFor(() => expect(result.current.replies.loading).toBe(false), wait);
    expect(result.current.b.items).toBe(result.current.a.items);
    expect(requests.filter((r) => r.endsWith("/comments"))).toHaveLength(1);
    await waitFor(() => expect(result.current.standing.standing).toMatchObject({ can_comment: true, user_id: "alice" }), wait);

    requests.length = 0;
    let mine!: Awaited<ReturnType<typeof result.current.a.post>>;
    await act(async () => void (mine = await result.current.a.post("hello")));
    expect(result.current.a.items.map((x) => x.id)).toEqual([mine.id, first.id]);
    expect(result.current.a.items[0]!.author?.username).toBe("alice");
    await act(async () => void (await result.current.a.post("a reply", { replyTo: first.id })));
    expect(result.current.replies.items.map((x) => x.body)).toEqual(["a reply"]);
    expect(result.current.a.items.find((x) => x.id === first.id)!.reply_count).toBe(1);
    expect(requests.filter((r) => r.startsWith("GET"))).toEqual([]);

    const target = result.current.a.items.find((x) => x.id === first.id)!;
    let pending!: Promise<void>;
    act(() => void (pending = result.current.a.react(target, 1)));
    expect(result.current.a.items.find((x) => x.id === first.id)).toMatchObject({ likes: 1, mine: 1 });
    await act(() => pending);
    expect(result.current.a.items.find((x) => x.id === first.id)).toMatchObject({ likes: 1, mine: 1 });

    // Gone on the server: the optimistic like rolls back.
    await seed.comments.delete(first.id);
    const stale = result.current.a.items.find((x) => x.id === first.id)!;
    await act(async () => {
      await expect(result.current.a.react(stale, -1)).rejects.toMatchObject({ code: "not_found" });
    });
    expect(result.current.a.items.find((x) => x.id === first.id)).toMatchObject({ likes: 1, dislikes: 0, mine: 1 });

    await act(async () => void (await result.current.a.edit(mine.id, "hello, edited")));
    expect(result.current.a.items[0]).toMatchObject({ body: "hello, edited", author: { username: "alice" } });
    await act(() => result.current.a.remove(mine.id));
    expect(result.current.a.items[0]).toMatchObject({ id: mine.id, deleted: true });
  });

  it("useComments pages with loadMore", async () => {
    const ref = video();
    const seed = client("carol");
    for (let i = 0; i < 3; i++) await seed.comments.create(ref, { body: `c${i}` });
    const { result } = renderHook(() => useComments(ref, { pageSize: 2 }), { wrapper: wrap(client("dave")) });
    await waitFor(() => expect(result.current.items).toHaveLength(2), wait);
    expect(result.current.hasMore).toBe(true);
    await act(() => result.current.loadMore());
    expect(result.current.items.map((x) => x.body)).toEqual(["c2", "c1", "c0"]);
    expect(result.current.hasMore).toBe(false);
  });

  it("useReaction and useFavorite: at once, rolled back when refused; signed out the count only", async () => {
    const ref = video();
    const lockedRef = { kind: "video", id: uuid7("10cced") };
    const c = client(undefined, "10.9.1.1");
    const { result } = renderHook(() => ({ r: useReaction(ref), locked: useReaction(lockedRef) }), { wrapper: wrap(c) });
    await waitFor(() => expect(result.current.r.loading).toBe(false), wait);
    await act(() => result.current.r.toggle(1));
    expect(result.current.r.counts).toEqual({ likes: 1, dislikes: 0, mine: 1 });
    await act(() => result.current.r.toggle(1));
    expect(result.current.r.counts).toEqual({ likes: 0, dislikes: 0, mine: 0 });
    await waitFor(() => expect(result.current.locked.loading).toBe(false), wait);
    await act(async () => {
      await expect(result.current.locked.set(1)).rejects.toMatchObject({ code: "forbidden" });
    });
    expect(result.current.locked.counts).toEqual({ likes: 0, dislikes: 0, mine: 0 });

    const signedOut = client();
    const out = renderHook(() => useFavorite(ref), { wrapper: wrap(signedOut, null) });
    await waitFor(() => expect(out.result.current).toMatchObject({ favorited: false, count: 0, loading: false }), wait);
    expect(requests).toEqual([`GET /video/${ref.id}/favorite`]);
    // Not known to be signed out: the read runs, and a refused write rolls back.
    const unknown = renderHook(() => useFavorite(ref), { wrapper: wrap(client()) });
    await waitFor(() => expect(unknown.result.current.loading).toBe(false), wait);
    await act(async () => {
      await expect(unknown.result.current.toggle()).rejects.toMatchObject({ code: "unauthorized" });
    });
    expect(unknown.result.current.favorited).toBe(false);

    const fan = renderHook(() => useFavorite(ref), { wrapper: wrap(client(`fan-${randomUUID()}`)) });
    await waitFor(() => expect(fan.result.current.loading).toBe(false), wait);
    await act(() => fan.result.current.toggle());
    expect(fan.result.current).toMatchObject({ favorited: true, count: 1 });
  });

  it("usePoll: the newest live poll; a vote applies at once and is final; usePolls and usePollEditor", async () => {
    const language = `x${randomUUID().slice(0, 6)}`;
    const editor = client("editor");
    const ed = renderHook(() => ({ editor: usePollEditor(null), list: usePolls({ admin: true, language }) }), { wrapper: wrap(editor, "editor") });
    await waitFor(() => expect(ed.result.current.list.loading).toBe(false), wait);
    let made!: Awaited<ReturnType<typeof ed.result.current.editor.create>>;
    await act(async () => {
      made = await ed.result.current.editor.create(
        { question: "Tea or coffee?", language, options: [{ label: "Tea", position: 0 }, { label: "Coffee", position: 1 }] },
        { question: png(), options: [png(), null] },
      );
    });
    expect(made.imageErrors).toEqual([]);
    expect(made.poll.image_url).toMatch(/\/ckpoll\//);
    expect(made.poll.options[0]!.image_url).toMatch(/\/ckpoll\//);
    await waitFor(() => expect(ed.result.current.editor.poll?.id).toBe(made.poll.id), wait);
    await waitFor(() => expect(ed.result.current.list.items.map((p) => p.id)).toEqual([made.poll.id]), wait);
    await act(async () => void (await ed.result.current.editor.addOption("Water")));
    await waitFor(() => expect(ed.result.current.editor.poll?.options).toHaveLength(3), wait);
    const water = ed.result.current.editor.poll!.options.find((o) => o.label === "Water")!;
    await act(() => ed.result.current.editor.moveOption(water.id, 0));
    expect(ed.result.current.editor.poll!.options.map((o) => o.label)).toEqual(["Water", "Tea", "Coffee"]);
    await waitFor(async () => expect((await editor.polls.get(made.poll.id)).options.map((o) => o.label)).toEqual(["Water", "Tea", "Coffee"]), wait);

    const voter = client("alice");
    const { result } = renderHook(() => usePoll(null, { language }), { wrapper: wrap(voter, "alice") });
    await waitFor(() => expect(result.current.poll?.id).toBe(made.poll.id), wait);
    const tea = result.current.poll!.options.find((o) => o.label === "Tea")!;
    let pending!: Promise<void>;
    act(() => void (pending = result.current.vote(tea.id)));
    expect(result.current.poll).toMatchObject({ voted: true, my_option: tea.id, total_votes: 1 });
    await act(() => pending);
    expect(result.current.poll).toMatchObject({ voted: true, my_option: tea.id, total_votes: 1 });

    await act(async () => void (await ed.result.current.editor.update({ is_active: false })));
    const late = renderHook(() => usePoll(made.poll.id, { initial: { ...made.poll, closed: false } }), { wrapper: wrap(client("bob"), "bob") });
    await act(async () => {
      await expect(late.result.current.vote(tea.id)).rejects.toMatchObject({ code: "invalid_request", message: "poll is closed" });
    });
    expect(late.result.current.poll?.voted).toBe(false);
  });

  it("usePost and usePosts: create, edit, cover and body images update every view", async () => {
    const language = `y${randomUUID().slice(0, 6)}`;
    const c = client("editor");
    const { result } = renderHook(
      () => ({ post: usePost(null), all: usePosts({ admin: true, language }), drafts: usePosts({ admin: true, language, draft: true }), trash: usePosts({ admin: true, language, deleted: true }) }),
      { wrapper: wrap(c, "editor") },
    );
    await waitFor(() => expect(result.current.all.loading).toBe(false), wait);
    await act(async () => void (await result.current.post.create({ title: "Draft one", body: "b", language, is_draft: true })));
    await waitFor(() => expect(result.current.drafts.items.map((p) => p.title)).toEqual(["Draft one"]), wait);
    await waitFor(() => expect(result.current.post.post?.title).toBe("Draft one"), wait);
    await act(async () => void (await result.current.post.update({ title: "Draft, renamed" })));
    expect(result.current.drafts.items[0]!.title).toBe("Draft, renamed");
    let url = "";
    await act(async () => void (url = await result.current.post.uploadImage(png())));
    expect(url).toMatch(/^blob:/); // the file itself until the post's image is served
    await act(async () => void (await result.current.post.setCover(png())));
    await waitFor(() => expect(result.current.post.post?.cover_url).toMatch(/\/ckpost\//), wait);
    await act(() => result.current.post.remove());
    expect(result.current.all.items).toEqual([]);
    await waitFor(() => expect(result.current.trash.items.map((p) => p.title)).toEqual(["Draft, renamed"]), wait);
    await act(async () => void (await result.current.post.restore()));
    await waitFor(() => expect(result.current.all.items.map((p) => p.title)).toEqual(["Draft, renamed"]), wait);
    await waitFor(() => expect(result.current.trash.items).toEqual([]), wait);
    await waitFor(() => expect(result.current.post.post?.title).toBe("Draft, renamed"), wait);
  });

  it("usePost: an unpublished post's images show through its editor read; stored bodies keep their public URLs", async () => {
    const c = client("editor");
    const writer = renderHook(() => usePost(null), { wrapper: wrap(c, "editor") });
    await act(async () => void (await writer.result.current.create({ title: "With pictures", body: "b", language: "en", is_draft: true })));
    await waitFor(() => expect(writer.result.current.post).not.toBeNull(), wait);
    const id = writer.result.current.post!.id;
    // A fresh upload shows from its file; the stored body names its public URL.
    let fresh = "";
    await act(async () => void (fresh = await writer.result.current.uploadImage(png())));
    expect(fresh).toMatch(/^blob:/);
    // The fixture's posts take plain text (tags stripped); a host's HTML sanitizer keeps <img src>.
    await act(async () => void (await writer.result.current.update({ body: `Look: ${fresh}` })));
    const url = writer.result.current.post!.body.slice("Look: ".length);
    expect(url).toMatch(new RegExp(`/ckpost/${id}/public/i-[0-9a-f-]{36}\\.webp$`));
    expect(writer.result.current.imageSrc(url)).toBe(fresh);
    await act(async () => void (await writer.result.current.setCover(png())));
    await waitFor(() => expect(writer.result.current.post?.cover_url).toMatch(/\/ckpost\//), wait);
    writer.unmount();

    // Reopened, the draft's images show through the editor read once their editor views render.
    const { result } = renderHook(() => usePost(id), { wrapper: wrap(client("editor"), "editor") });
    await waitFor(() => expect(result.current.imageSrc(url)).toContain("?t="), wait);
    const shown = result.current.imageSrc(url)!;
    expect(shown).not.toContain("/public/");
    expect(result.current.imageSrc(result.current.post!.cover_url)).toContain("?t=");
    expect(result.current.editorBody).toBe(`Look: ${shown}`);
    expect(result.current.imageSrc("https://example.com/other.png")).toBe("https://example.com/other.png");
    await act(async () => void (await result.current.update({ body: `${result.current.editorBody} More.` })));
    expect(result.current.post!.body).toBe(`Look: ${url} More.`);
    expect(result.current.storedBody(result.current.editorBody!)).toBe(result.current.post!.body);

    // The folder is its editors': another caller cannot read it.
    await expect(client("alice").media.read({ kind: "ckpost", id }, { editor: true })).rejects.toMatchObject({ code: "not_found" });
    // Published, its public URLs serve and no editor read is made.
    await act(async () => void (await result.current.update({ is_draft: false })));
    expect(result.current.imageSrc(url)).toBe(url);
  });

  it("useModerationQueue and useCommentBans: a resolved item leaves the queue; a ban joins the list", async () => {
    const ref = video();
    const held = await client("erin").comments.create(ref, { body: "hmm [hold]" });
    const c = client("moderator");
    const { result } = renderHook(() => ({ queue: useModerationQueue({ pageSize: 100 }), bans: useCommentBans({ scope: "global" }) }), { wrapper: wrap(c, "moderator") });
    await waitFor(() => expect(result.current.queue.items.some((i) => i.id === held.id)).toBe(true), wait);
    while (result.current.queue.hasMore && !result.current.queue.items.some((i) => i.id === held.id)) await act(() => result.current.queue.loadMore());
    const item = result.current.queue.items.find((i) => i.id === held.id)!;
    expect(item.author?.username).toBe("erin");
    await act(() => result.current.queue.resolve(item, "approve"));
    expect(result.current.queue.items.some((i) => i.id === held.id)).toBe(false);

    const troll = `troll-${randomUUID()}`;
    await act(async () => void (await result.current.bans.ban(troll, { reason: "spam" })));
    await waitFor(() => expect(result.current.bans.items.find((b) => b.user_id === troll)).toMatchObject({ reason: "spam" }), wait);
    await act(() => result.current.bans.lift(troll));
    await waitFor(() => expect(result.current.bans.items.some((b) => b.user_id === troll)).toBe(false), wait);
  });
});
