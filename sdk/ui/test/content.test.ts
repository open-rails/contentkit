import type { ChildProcess } from "node:child_process";
import { randomUUID } from "node:crypto";
import { readFileSync } from "node:fs";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { createContentKitClient, type ContentKitChange, type ContentKitClient } from "../src/client/index.js";
import { startServer, stopServer } from "./server.js";

const endpoint = process.env.CONTENTKIT_TEST_S3_ENDPOINT;
/** A UUIDv7 content id; suffix marks it (the fixture hides ids ending in dead, locks those ending in 10cced). */
function uuid7(suffix = ""): string {
  const hex = Date.now().toString(16).padStart(12, "0") + "7" + randomUUID().replace(/-/g, "").slice(0, 19);
  const v = hex.slice(0, 16) + "8" + hex.slice(17, 32 - suffix.length) + suffix;
  return `${v.slice(0, 8)}-${v.slice(8, 12)}-${v.slice(12, 16)}-${v.slice(16, 20)}-${v.slice(20, 32)}`;
}
const png = () => new File([readFileSync(new URL("../e2e/fixtures/small.png", import.meta.url))], "photo.png", { type: "image/png" });

describe.skipIf(!endpoint)("content modules against contentkit.Runtime.Handler", () => {
  let proc: ChildProcess;
  let base: string;
  let namespace: string;
  const changes: ContentKitChange[] = [];

  beforeAll(async () => {
    const s = await startServer(endpoint!);
    proc = s.proc;
    base = s.url;
    namespace = s.namespace;
  });
  afterAll(async () => {
    if (proc) await stopServer(proc);
  });

  /** A client as actor (signed in), or anonymous from ip; mount "/ck-members" takes nothing from signed-out visitors. */
  function ck(actor?: string, ip = "10.1.0.1", mount = "/ck"): ContentKitClient {
    const c = createContentKitClient({
      baseUrl: `${base}${mount}`,
      headers: () => ({ "X-Test-IP": ip, ...(actor ? { "X-Test-Actor": actor } : {}) }),
      media: { retryDelay: () => 100 },
      folders: { post: "ckpost", poll: "ckpoll" },
    });
    c.subscribe((change) => changes.push(change));
    return c;
  }
  const item = (kind = "video", suffix = "") => ({ kind, id: uuid7(suffix) });
  const code = (code: string) => expect.objectContaining({ name: "ContentKitError", code });

  it("posts: staff CRUD, the staff list, reactions, and the public list", async () => {
    const editor = ck("editor");
    const draft = await editor.posts.create({ title: "Draft", body: "draft body", language: "en", is_draft: true });
    const post = await editor.posts.create({ title: "Hello world", body: "first", language: "en" });
    expect(post).toMatchObject({ title: "Hello world", is_draft: false, code: expect.stringMatching(/^[0-9A-Z]{9}$/), url_slug: "hello-world" });
    expect(changes).toContainEqual({ type: "post.created", post });

    expect((await ck().posts.list({ language: "en" })).map((p) => p.id)).toContain(post.id);
    expect((await ck().posts.list()).map((p) => p.id)).not.toContain(draft.id);
    expect((await editor.posts.adminList({ draft: true })).map((p) => p.id)).toEqual(expect.arrayContaining([draft.id]));
    expect((await editor.posts.adminList({ draft: false })).map((p) => p.id)).not.toContain(draft.id);
    await expect(ck("alice").posts.adminList()).rejects.toEqual(code("forbidden"));
    await expect(ck("alice").posts.create({ title: "x", body: "y" })).rejects.toEqual(code("forbidden"));
    await expect(ck().posts.get(draft.id)).rejects.toEqual(code("not_found"));

    const updated = await editor.posts.update(post.id, { body: "edited" });
    expect(updated.body).toBe("edited");
    // A post is a reaction target like any other; its own routes answer the post's totals.
    expect(await ck("alice").reactions.set({ kind: "post", id: post.id }, 1)).toEqual({ likes: 1, dislikes: 0, mine: 1 });
    expect(await ck("bob").posts.react(post.id, -1)).toMatchObject({ total_likes: 1, total_dislikes: 1 });
    expect(await ck("alice").reactions.get({ kind: "post", id: post.id })).toEqual({ likes: 1, dislikes: 1, mine: 1 });

    await editor.posts.delete(post.id);
    expect(changes).toContainEqual({ type: "post.deleted", id: post.id });
    await expect(editor.posts.get(post.id)).rejects.toEqual(code("not_found"));

    // Staff search, the deleted list and restore.
    const word = `w${randomUUID().slice(0, 8)}`;
    const found = await editor.posts.create({ title: `About ${word.toUpperCase()}`, body: "b", language: "en" });
    expect((await editor.posts.adminList({ q: word })).map((p) => p.id)).toEqual([found.id]);
    expect((await editor.posts.adminList({ deleted: true, limit: 100 })).find((p) => p.id === post.id)).toMatchObject({ deleted_at: expect.any(String) });
    expect((await editor.posts.adminList({ limit: 100 })).map((p) => p.id)).not.toContain(post.id);
    await expect(ck("alice").posts.restore(post.id)).rejects.toEqual(code("forbidden"));
    const restored = await editor.posts.restore(post.id);
    expect(restored).toMatchObject({ id: post.id, title: "Hello world", body: "edited" });
    expect(restored.deleted_at).toBeUndefined();
    expect(changes).toContainEqual({ type: "post.restored", post: restored });
    expect((await ck().posts.get(post.id)).id).toBe(post.id);
    await expect(editor.posts.restore(post.id)).rejects.toEqual(code("not_found"));
  });

  it("media.uploadInline places a body image in the post's folder and resolves its URL; covers go through the same uploads", async () => {
    const editor = ck("editor");
    const post = await editor.posts.create({ title: "Pictures", body: "b", language: "en" });
    const img = await editor.media.uploadInline(post.id, png());
    expect(img.name).toMatch(/^i-[0-9a-f-]{36}$/);
    expect(img.path).toBe(`${img.name}.png`);
    expect(img.url).toBe(`http://media.invalid/v1/${namespace}/ckpost/${post.id}/public/${img.name}.webp`);
    expect(await editor.posts.imageURL(post.id, img.name)).toBe(img.url);

    const cover = await editor.posts.uploadCover(post.id, png());
    expect(cover).toMatch(new RegExp(`/ckpost/${post.id}/public/i-[0-9a-f-]{36}\\.webp$`));
    expect((await editor.posts.get(post.id)).cover_url).toBe(cover);
    expect(await editor.posts.uploadCover(post.id, null)).toBeNull();
    expect(changes).toContainEqual({ type: "post.changed", id: post.id });

    // Only PostWrite holders upload to a post's folder.
    await expect(ck("alice").media.uploadInline(post.id, png())).rejects.toEqual(code("forbidden"));
  });

  it("comments: threads, replies, edits, tombstones, restore, reactions and the latest feed", async () => {
    const video = item();
    const alice = ck("alice");
    const bob = ck("bob");
    const mod = ck("moderator");
    const top = await alice.comments.create(video, { body: "first!" });
    expect(top).toMatchObject({ body: "first!", user_id: "alice", author: { username: "alice" }, reply_count: 0 });
    expect(changes).toContainEqual({ type: "comment.created", ref: video, comment: top });
    const reply = await bob.comments.create(video, { body: "a reply", reply_to_id: top.id });
    await expect(bob.comments.create(video, { body: "deeper", reply_to_id: reply.id })).rejects.toEqual(code("invalid_request"));
    const anon = await ck(undefined, "10.1.0.7").comments.create(video, { body: "drive-by", anon_name: "Guest" });
    expect(anon.anon_name).toBe("Guest");
    expect(anon.user_id).toBeUndefined();
    await expect(ck().comments.create(video, { body: "nameless" })).rejects.toEqual(code("invalid_request"));

    const list = await bob.comments.list(video, { sort: "newest", limit: 10 });
    expect(list.map((c) => c.id)).toEqual([anon.id, top.id]);
    expect(list[1]!.reply_count).toBe(1);
    expect((await alice.comments.replies(top.id)).map((c) => c.body)).toEqual(["a reply"]);
    expect(await alice.comments.list(video, { limit: 1, offset: 1 })).toHaveLength(1);

    expect(await bob.comments.react(top.id, 1)).toEqual({ likes: 1, dislikes: 0, mine: 1 });
    expect(changes).toContainEqual({ type: "comment.reacted", id: top.id, counts: { likes: 1, dislikes: 0, mine: 1 } });
    expect((await bob.comments.list(video, { sort: "likes" }))[0]).toMatchObject({ id: top.id, mine: 1 });

    const edited = await alice.comments.edit(top.id, "first, edited");
    expect(edited).toMatchObject({ body: "first, edited", author: { username: "alice" }, likes: 1 });
    await expect(bob.comments.edit(top.id, "hijack")).rejects.toEqual(code("forbidden"));
    expect((await mod.comments.edit(top.id, "moderated")).author?.username).toBe("alice");

    await alice.comments.delete(top.id);
    const tomb = (await bob.comments.list(video)).find((c) => c.id === top.id)!;
    expect(tomb).toMatchObject({ deleted: true, body: "[deleted]" });
    expect(tomb.author).toBeUndefined();
    await expect(bob.comments.react(top.id, 1)).rejects.toEqual(code("not_found"));
    await expect(alice.comments.restore(top.id)).rejects.toEqual(code("forbidden"));
    expect((await mod.comments.adminList({ contentKind: "video" })).find((c) => c.id === top.id)).toMatchObject({ deleted: true, body: "moderated" });
    await mod.comments.restore(top.id);
    expect((await bob.comments.list(video)).find((c) => c.id === top.id)).toMatchObject({ deleted: false, body: "moderated" });

    const feed = await ck().comments.latest({ limit: 50 });
    expect(feed.find((c) => c.id === reply.id)).toMatchObject({ content_kind: "video", content_id: video.id, reply_to_id: top.id });
  });

  it("comments: held and rejected text, standing, and the per-caller limit", async () => {
    const video = item();
    const held = await ck("carol").comments.create(video, { body: "maybe [hold]" });
    expect(held).toMatchObject({ moderation: "held", moderation_reason: "needs a look" });
    expect((await ck("carol").comments.list(video)).map((c) => c.id)).toEqual([held.id]);
    expect(await ck("dave").comments.list(video)).toEqual([]);
    await expect(ck("carol").comments.create(video, { body: "no [reject]" })).rejects.toEqual(
      expect.objectContaining({ code: "moderation_rejected", message: "not allowed here", status: 422 }),
    );

    expect(await ck().comments.standing(video)).toEqual({ can_comment: true, anonymous: true, moderate: false, ban_scopes: [] });
    expect(await ck("creator").comments.standing(video)).toEqual({ can_comment: true, anonymous: true, user_id: "creator", moderate: false, ban_scopes: ["owner"] });
    expect(await ck("moderator").comments.standing(video)).toMatchObject({ moderate: true, ban_scopes: ["global"] });
    expect(await ck("alice").comments.standing(item("video", "10cced"))).toMatchObject({ can_comment: false });
    await expect(ck("alice").comments.standing(item("video", "dead"))).rejects.toEqual(code("not_found"));
    await expect(ck("alice").comments.list({ kind: "unknown", id: uuid7() })).rejects.toEqual(code("not_found"));

    const spammer = ck(`spam-${randomUUID()}`);
    for (let i = 0; i < 20; i++) await spammer.comments.create(video, { body: `post ${i}` });
    await expect(spammer.comments.create(video, { body: "one more" })).rejects.toEqual(
      expect.objectContaining({ code: "rate_limited", status: 429, action: "comment", retryAfter: expect.any(Number) }),
    );
  });

  it("moderation: the held queue names authors; resolving publishes or rejects at the listed revision", async () => {
    const video = item();
    const a = await ck("erin").comments.create(video, { body: "first [hold]" });
    const b = await ck("erin").comments.create(video, { body: "second [hold]" });
    const mod = ck("moderator");
    await expect(ck("alice").moderation.held({ kind: "comment" })).rejects.toEqual(code("forbidden"));
    const items: { id: string; revision: number; kind: string; author?: { username: string } }[] = [];
    let cursor: string | undefined;
    do {
      const page = await mod.moderation.held({ kind: "comment", cursor, limit: 100 });
      items.push(...page.items);
      cursor = page.next;
    } while (cursor);
    const heldA = items.find((i) => i.id === a.id)!;
    expect(heldA).toMatchObject({ kind: "comment", author: { username: "erin" } });
    await mod.moderation.resolve(heldA, "approve");
    expect(changes).toContainEqual({ type: "moderation.resolved", kind: "comment", id: a.id, decision: "approve" });
    expect((await ck("frank").comments.list(video)).map((c) => c.id)).toEqual([a.id]);

    const heldB = items.find((i) => i.id === b.id)!;
    await ck("erin").comments.edit(b.id, "second, edited [hold]");
    // The listed revision is stale: the edit must be reviewed as it now reads.
    await expect(mod.moderation.resolve(heldB, "approve")).rejects.toEqual(code("not_found"));
    await mod.moderation.resolve({ ...heldB, revision: heldB.revision + 1 }, "reject", "off topic");
    expect((await ck("erin").comments.list(video)).find((c) => c.id === b.id)).toMatchObject({ moderation: "rejected", moderation_reason: "off topic" });
  });

  it("bans: an owner's and the site's, with the notice a refused comment carries", async () => {
    const video = item();
    const troll = `troll-${randomUUID()}`;
    const creator = ck("creator");
    const ban = await creator.bans.ban("owner", troll, { reason: "spam" });
    expect(ban).toMatchObject({ user_id: troll, scope: "owner:creator", reason: "spam", expired: false, user: { username: troll } });
    expect(changes).toContainEqual({ type: "ban.saved", scope: "owner", ban });
    await expect(ck(troll).comments.create(video, { body: "hi" })).rejects.toEqual(
      expect.objectContaining({ code: "comment_banned", status: 403, ban: { scope: "owner:creator", reason: "spam" } }),
    );
    expect(await ck(troll).comments.standing(video)).toMatchObject({ can_comment: false, ban: { scope: "owner:creator", reason: "spam" } });
    expect((await creator.bans.list("owner")).map((b) => b.user_id)).toContain(troll);
    await creator.bans.lift("owner", troll);
    expect(changes).toContainEqual({ type: "ban.lifted", scope: "owner", user: troll });
    await ck(troll).comments.create(video, { body: "hi again" });

    const until = new Date(Date.now() + 86_400_000).toISOString();
    await expect(creator.bans.ban("global", troll)).rejects.toEqual(code("forbidden"));
    await ck("moderator").bans.ban("global", troll, { until });
    const notice = await ck(troll).comments.standing(item());
    expect(notice.ban).toMatchObject({ scope: "global" });
    expect(Date.parse(notice.ban!.until!)).toBe(Date.parse(until));
    expect((await ck("moderator").bans.list("global", { limit: 100 })).find((b) => b.user_id === troll)).toMatchObject({ banned_by: "moderator" });
    await ck("moderator").bans.lift("global", troll);
    expect((await ck(troll).comments.standing(item())).can_comment).toBe(true);
  });

  it("anonymous participation is the server's setting: config, standing and refusals", async () => {
    const everyone = { comments: true, reactions: true, votes: true };
    expect(await ck().config()).toEqual({ anonymous: everyone });
    const members = (actor?: string) => ck(actor, "10.4.0.1", "/ck-members");
    expect(await members().config()).toEqual({ anonymous: { comments: false, reactions: false, votes: false } });

    const video = item();
    const top = await members("alice").comments.create(video, { body: "members only" });
    expect(await members().comments.standing(video)).toEqual({ can_comment: false, anonymous: false, moderate: false, ban_scopes: [] });
    expect(await members("bob").comments.standing(video)).toMatchObject({ can_comment: true, anonymous: false, user_id: "bob" });
    const post = await members("editor").posts.create({ title: "Members", body: "b", language: "en" });
    const poll = await members("editor").polls.create({ question: "Members?", language: "en", options: [{ label: "Yes", position: 0 }, { label: "No", position: 1 }] });
    for (const refused of [
      () => members().comments.create(video, { body: "drive-by", anon_name: "Guest" }),
      () => members().comments.react(top.id, 1),
      () => members().reactions.set(video, 1),
      () => members().posts.react(post.id, 1),
      () => members().polls.vote(poll.id, poll.options[0]!.id),
    ]) {
      await expect(refused()).rejects.toEqual(expect.objectContaining({ code: "unauthorized", status: 401 }));
    }
    // Reads stay open, and the same items take anonymous interactions where the server allows them.
    expect((await members().comments.list(video)).map((c) => c.id)).toEqual([top.id]);
    expect(await ck(undefined, "10.4.0.2").reactions.set(video, 1)).toEqual({ likes: 1, dislikes: 0, mine: 1 });
    expect(await ck(undefined, "10.4.0.2").polls.vote(poll.id, poll.options[0]!.id)).toMatchObject({ voted: true, total_votes: 1 });
  });

  it("reactions and favorites: per caller (anonymous by IP for reactions), gated by the item's access", async () => {
    const video = item();
    expect(await ck().reactions.get(video)).toEqual({ likes: 0, dislikes: 0, mine: 0 });
    expect(await ck(undefined, "10.2.0.1").reactions.set(video, 1)).toEqual({ likes: 1, dislikes: 0, mine: 1 });
    expect(await ck(undefined, "10.2.0.2").reactions.set(video, -1)).toEqual({ likes: 1, dislikes: 1, mine: -1 });
    expect(await ck(undefined, "10.2.0.1").reactions.set(video, -1)).toEqual({ likes: 0, dislikes: 2, mine: -1 });
    expect(await ck(undefined, "10.2.0.1").reactions.set(video, 0)).toEqual({ likes: 0, dislikes: 1, mine: 0 });
    expect(changes).toContainEqual({ type: "reaction.changed", ref: video, counts: { likes: 0, dislikes: 1, mine: 0 } });
    await expect(ck("alice").reactions.set(item("video", "10cced"), 1)).rejects.toEqual(code("forbidden"));
    await expect(ck("alice").reactions.get(item("video", "dead"))).rejects.toEqual(code("not_found"));

    await expect(ck().favorites.get(video)).rejects.toEqual(code("unauthorized"));
    const fan = ck(`fan-${randomUUID()}`);
    expect(await fan.favorites.get(video)).toEqual({ favorited: false });
    expect(await fan.favorites.set(video, true)).toEqual({ favorited: true });
    expect(await fan.favorites.set(video, true)).toEqual({ favorited: true });
    expect(changes).toContainEqual({ type: "favorite.changed", ref: video, favorited: true });
    expect((await fan.favorites.list()).map((f) => f.content_id)).toEqual([video.id]);
    expect(await fan.favorites.set(video, false)).toEqual({ favorited: false });
    expect(await fan.favorites.list()).toEqual([]);
  });

  it("polls: final votes, free-text answers, options, images, and the staff list", async () => {
    const editor = ck("editor");
    const poll = await editor.polls.create({ question: "Best season?", language: "en", options: ["Spring", "Summer", "Autumn"].map((label, position) => ({ label, position })) });
    expect(poll).toMatchObject({ kind: "multiple_choice", total_votes: 0, voted: false, closed: false });
    const [spring, summer, autumn] = poll.options;
    expect(await ck("alice").polls.vote(poll.id, spring!.id)).toMatchObject({ voted: true, my_option: spring!.id, total_votes: 1 });
    const again = await ck("alice").polls.vote(poll.id, summer!.id);
    expect(again).toMatchObject({ my_option: spring!.id, total_votes: 1 });
    expect(await ck(undefined, "10.3.0.1").polls.vote(poll.id, summer!.id)).toMatchObject({ total_votes: 2, my_option: summer!.id });
    expect((await ck("bob").polls.get(poll.id)).voted).toBe(false);
    expect((await ck().polls.list({ language: "en", limit: 100 })).map((p) => p.id)).toContain(poll.id);

    const added = await editor.polls.addOption(poll.id, { label: "Winter" });
    expect(added).toMatchObject({ label: "Winter", position: 3 });
    expect(changes).toContainEqual({ type: "poll.changed", id: poll.id });
    await editor.polls.updateOption(poll.id, added.id, { label: "Winter!" });
    const current = (await editor.polls.get(poll.id)).options;
    await editor.polls.reorderOptions(poll.id, [current[3]!, current[0]!, current[1]!, current[2]!]);
    expect((await editor.polls.get(poll.id)).options.map((o) => o.label)).toEqual(["Winter!", "Spring", "Summer", "Autumn"]);
    await editor.polls.deleteOption(poll.id, autumn!.id);
    await editor.polls.deleteOption(poll.id, added.id);
    await expect(editor.polls.deleteOption(poll.id, spring!.id)).rejects.toEqual(code("invalid_request"));

    const optionImage = await editor.polls.uploadOptionImage(poll.id, spring!.id, png());
    expect(optionImage).toMatch(new RegExp(`/ckpoll/${poll.id}/public/i-[0-9a-f-]{36}\\.webp$`));
    const questionImage = await editor.polls.uploadImage(poll.id, png());
    const read = await ck().polls.get(poll.id);
    expect(read.image_url).toBe(questionImage);
    expect(read.options.find((o) => o.id === spring!.id)!.image_url).toBe(optionImage);
    expect(await editor.polls.setOptionImage(poll.id, spring!.id, null)).toBeNull();

    const closed = await editor.polls.update(poll.id, { closes_at: new Date(Date.now() - 60_000).toISOString() });
    expect(closed).toMatchObject({ closed: true, closes_at: expect.any(String) });
    const reopened = await editor.polls.update(poll.id, { closes_at: null });
    expect(reopened.closed).toBe(false);
    expect(reopened.closes_at).toBeUndefined();
    const hidden = await editor.polls.update(poll.id, { is_active: false });
    expect(hidden).toMatchObject({ is_active: false, closed: true });
    await expect(ck().polls.get(poll.id)).rejects.toEqual(code("not_found"));
    await expect(ck("alice").polls.vote(poll.id, spring!.id)).rejects.toEqual(expect.objectContaining({ code: "invalid_request", message: "poll is closed" }));
    expect((await editor.polls.adminList({ limit: 100 })).map((p) => p.id)).toContain(poll.id);
    await expect(ck("alice").polls.adminList()).rejects.toEqual(code("forbidden"));

    const open = await editor.polls.create({ kind: "free_text", question: "Why?", language: "en" });
    await expect(ck().polls.answer(open.id, "because")).rejects.toEqual(code("unauthorized"));
    const answered = await ck("alice").polls.answer(open.id, "Cats are great");
    expect(answered.my_answer).toMatchObject({ text: "Cats are great" });
    await ck("bob").polls.answer(open.id, "cats again");
    await ck("carol").polls.answer(open.id, "Dogs");
    const groups = (await ck("alice").polls.get(open.id)).groups!;
    expect(Object.fromEntries(groups.map((g) => [g.label, g.count]))).toEqual({ Cats: 2, Dogs: 1 });

    await editor.polls.delete(poll.id);
    expect(changes).toContainEqual({ type: "poll.deleted", id: poll.id });
  });

  it("taxonomy and codes go through the same mount", async () => {
    const staff = ck("staff");
    const slug = `tag-${randomUUID().slice(0, 8)}`;
    const [node] = await staff.taxonomy.createNodes([{ kind: "tag", slug, names: [{ language: "en", kind: "name", name: "Night Sky", source_revision: 1 }], source_revision: 1 }]);
    expect(node).toMatchObject({ kind: "tag", slug, state: "active" });
    expect(changes).toContainEqual({ type: "taxonomy.changed" });
    expect((await staff.taxonomy.nodes({ kind: "tag", id: [node!.taxonomy_id] })).nodes.map((n) => n.slug)).toEqual([slug]);
    const detail = await staff.taxonomy.names(node!.taxonomy_id, "add", [{ language: "en", kind: "alias", name: "Starry", source_revision: 2 }]);
    expect(detail.names.map((n) => n.name).sort()).toEqual(["Night Sky", "Starry"]);
    expect(Object.keys(await staff.taxonomy.counts([node!.taxonomy_id]))).toEqual([]);
    await expect(ck("alice").taxonomy.nodes()).rejects.toEqual(code("forbidden"));

    const post = await ck("editor").posts.create({ title: "Coded post", body: "b", language: "en" });
    expect(await ck().codes.resolve(post.code.toLowerCase())).toMatchObject({ content_kind: "post", content_id: post.id, path: `/blog/${post.code}/coded-post` });
    await expect(ck().codes.resolve("ZZZZZZZZ9")).rejects.toEqual(code("not_found"));
  });

  it("mounts: a content module mounted elsewhere is reached there", async () => {
    const c = createContentKitClient({ baseUrl: "http://127.0.0.1:9/nowhere", mounts: { content: `${base}/ck` }, headers: () => ({ "X-Test-Actor": "alice" }) });
    expect(c.url("content")).toBe(`${base}/ck`);
    expect(await c.reactions.get(item())).toEqual({ likes: 0, dislikes: 0, mine: 0 });
  });
});
