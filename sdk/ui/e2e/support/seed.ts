// Seeds items through the real upload API with the built client (dist), as
// a host's own pages would, and waits for the worker's outputs.
import { readFileSync } from "node:fs";
import path from "node:path";
import { createContentKitClient, type FileInfo, type RefBody } from "@openrails/contentkit-ui/client";
import type { Harness, Item, TestUser } from "./harness.ts";

const fixtures = path.resolve(import.meta.dirname, "..", "fixtures");
const TYPES: Record<string, string> = { ".jpg": "image/jpeg", ".png": "image/png", ".mp4": "video/mp4", ".vtt": "text/vtt" };

/** A fixture as an upload. */
export function fixture(name: string): File {
  return new File([readFileSync(path.join(fixtures, name))], path.basename(name), { type: TYPES[path.extname(name)] ?? "application/octet-stream" });
}

export function contentKit(h: Harness, user: TestUser) {
  return createContentKitClient({ baseUrl: `${h.origin}/api/contentkit`, token: () => user.access_token });
}

export function mediaClient(h: Harness, user: TestUser) {
  return contentKit(h, user).media;
}

const wait = { interval: 250, timeout: 180_000 };
const refOf = (it: Item): RefBody => ({ kind: it.kind, id: it.id });

/** Waits until every upload of the item is processed and its public files are published. */
export async function processed(h: Harness, user: TestUser, ref: RefBody): Promise<FileInfo[]> {
  const c = mediaClient(h, user);
  const until = Date.now() + wait.timeout;
  for (;;) {
    const read = await c.read(ref, { editor: true });
    const failed = read.files.find((f) => f.failed);
    if (failed) throw new Error(`${ref.kind}/${ref.id} ${failed.path} failed: ${JSON.stringify(failed.failed)}`);
    if (read.files.every((f) => !f.pending?.length && !f.staged)) return read.files;
    if (Date.now() > until) throw new Error(`${ref.kind}/${ref.id} still processing: ${JSON.stringify(read.files)}`);
    await new Promise((r) => setTimeout(r, wait.interval));
  }
}

/** A channel owned by owner, with a cropped cover and an avatar unless left out. */
export async function seedChannel(h: Harness, owner: TestUser, o: { cover?: boolean; avatar?: boolean } = {}): Promise<Item> {
  const it = await h.item({ kind: "channel", owner: owner.id });
  const c = mediaClient(h, owner);
  const ref = refOf(it);
  if (o.cover ?? true) await c.put(fixture("cover.jpg"), { ref, path: "cover", edit: { crop: { x: 0, y: 200, w: 3600, h: 1200 } } });
  if (o.avatar ?? true) await c.put(fixture("avatar.jpg"), { ref, path: "avatar" });
  await processed(h, owner, ref);
  return it;
}

/** A video item with the 12 s clip as its source and the worker's poster. */
export async function seedVideo(h: Harness, owner: TestUser): Promise<Item> {
  const it = await h.item({ kind: "video", owner: owner.id });
  const c = mediaClient(h, owner);
  const ref = refOf(it);
  await c.put(fixture("clip.mp4"), { ref, path: "source" });
  await c.commit(ref, [{ op: "frame", path: "poster", auto: true }]);
  await processed(h, owner, ref);
  return it;
}

/** A watch item: the 24 s two-language source and English and Spanish subtitle uploads. */
export async function seedWatch(h: Harness, owner: TestUser): Promise<Item> {
  const it = await h.item({ kind: "watch", owner: owner.id });
  const c = mediaClient(h, owner);
  const ref = refOf(it);
  await c.put(fixture("tracks.mp4"), { ref, path: "source" });
  await c.put(fixture("tracks-en.vtt"), { ref, path: "subs/en", meta: { lang: "en", label: "English" } });
  await c.put(fixture("tracks-es.vtt"), { ref, path: "subs/es", meta: { lang: "es", label: "Spanish" } });
  await processed(h, owner, ref);
  return it;
}

/** An album of the fixtures (images, then videos, as the kind orders them), locked to viewers when access is "none". */
export async function seedAlbum(h: Harness, owner: TestUser, files: string[], o: { access?: "full" | "none"; title?: string } = {}): Promise<Item> {
  const it = await h.item({ kind: "album", owner: owner.id, access: o.access, title: o.title });
  const c = mediaClient(h, owner);
  const ref = refOf(it);
  const ups = await Promise.all(
    files.map((name, i) => {
      const f = fixture(name);
      const dir = f.type.startsWith("video/") ? "videos" : "images";
      return c.upload(f, { ref, path: `${dir}/${String(i + 1).padStart(2, "0")}-${path.basename(name, path.extname(name))}` });
    }),
  );
  await c.commit(ref, ups.map((u) => ({ op: "put" as const, path: u.path, blob: u.blob })));
  await processed(h, owner, ref);
  return it;
}

/**
 * A video with a thread (a reply, a tombstone, a long comment, likes,
 * favorites), a poll with three votes, and for staff a held comment and a
 * global ban. The viewer's own comment awaits review.
 */
export async function seedSocial(h: Harness, o: { viewer?: TestUser | null; lang?: string; staff?: boolean } = {}) {
  const [creator, bob, carol, erin, editor, moderator, spammer] = await Promise.all([
    h.user(), h.user(), h.user(), h.user(), h.user({ role: "editor" }), h.user({ role: "moderator" }), h.user(),
  ]);
  const it = await h.item({ kind: "video", owner: creator.id, title: "Night Before the Counteroffensive" });
  const item = { kind: it.kind, id: it.id };
  const [b, c, e, ed] = [bob, carol, erin, editor].map((u) => contentKit(h, u!));
  const first = await b!.comments.create(item, { body: "First! This trailer looks great." });
  await c!.comments.create(item, { body: "Agreed, the soundtrack at 1:20 is the best part.\nCan't wait for the full episode.", reply_to_id: first.id });
  const gone = await c!.comments.create(item, { body: "Oops, wrong thread." });
  await c!.comments.delete(gone.id);
  await e!.comments.create(item, {
    body:
      "I rewatched the first season before this. The pacing in the middle episodes was slow, but the payoff in the finale made up for it, and the animation in the battle scenes is some of the best I've seen this year. " +
      "If this keeps that quality I'm in. The only worry is whether they rush the adaptation now that the source material is nearly caught up.",
  });
  await b!.comments.react(first.id, 1);
  await b!.reactions.set(item, 1);
  await b!.favorites.set(item, true);
  await c!.favorites.set(item, true);
  await c!.reactions.set(item, -1);
  if (o.viewer && !o.viewer.role) await contentKit(h, o.viewer).comments.create(item, { body: "Is this the director's cut? [hold]" });
  const poll = await ed!.polls.create({
    question: "Which season should we cover next?",
    language: o.lang ?? "en",
    options: ["Spring 2026", "Summer 2026", "Autumn 2026"].map((label, position) => ({ label, position })),
  });
  await b!.polls.vote(poll.id, poll.options[0]!.id);
  await c!.polls.vote(poll.id, poll.options[1]!.id);
  await e!.polls.vote(poll.id, poll.options[0]!.id);
  if (o.staff) {
    await e!.comments.create(item, { body: "Selling cheap keys, DM me [hold]" });
    await contentKit(h, moderator!).bans.ban("global", spammer!.id, { reason: "Repeated spam links" });
  }
  return { item, poll };
}
