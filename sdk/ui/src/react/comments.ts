import { useCallback } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { BanScope, Decision, HeldKind, Reaction, Sort, Target } from "../client/content/types.js";
import type { ContentKitError } from "../client/errors.js";
import type { AdminComment, BanInput, BannedWord, BannedWordInput, Comment, CommentBan, CommentStanding, FeedItem, HeldItem, IPBan, IPBanInput, ReactionCounts } from "../client/generated/wire.js";
import type { Page, ResourceStore } from "./resources.js";
import { keyOf, offsetPages, useContentScope, useList, useResource, type UseList } from "./use-resource.js";

/** Counts after the caller's reaction changes from counts.mine to value. */
export function withReaction(counts: ReactionCounts, value: Reaction): ReactionCounts {
  const likes = counts.likes - (counts.mine === 1 ? 1 : 0) + (value === 1 ? 1 : 0);
  const dislikes = counts.dislikes - (counts.mine === -1 ? 1 : 0) + (value === -1 ? 1 : 0);
  return { likes, dislikes, mine: value };
}

const commentLists = ["comments", "replies", "latest", "admin-comments"];
const tokens = new WeakMap<ResourceStore, Map<string, number>>();

/** Sets the caller's reaction to a comment in every list showing it at once; rolls back on failure unless a newer one started. */
async function reactToComment(client: ContentKitClient, store: ResourceStore, scope: string, c: Pick<Comment, "id" | "likes" | "dislikes" | "mine">, value: Reaction) {
  let t = tokens.get(store);
  if (!t) tokens.set(store, (t = new Map()));
  const token = (t.get(c.id) ?? 0) + 1;
  t.set(c.id, token);
  const reads = new Set<number>();
  const put = (counts: ReactionCounts, rollback = false) =>
    store.patch<Page<Comment>>(
      (tag) => commentLists.includes(tag.type) && tag.readScope === scope,
      (page, readVersion) => {
        if (rollback && !reads.has(readVersion)) return page;
        reads.add(readVersion);
        return { ...page, items: page.items.map((x) => (x.id === c.id ? { ...x, ...counts } : x)) };
      },
    );
  put(withReaction({ likes: c.likes, dislikes: c.dislikes, mine: c.mine }, value));
  try {
    await client.comments.react(c.id, value);
  } catch (e) {
    if (t.get(c.id) === token) put({ likes: c.likes, dislikes: c.dislikes, mine: c.mine }, true);
    throw e;
  }
}

export interface UseCommentsOptions {
  sort?: Sort;
  /** One language's thread: the list shows it and new comments join it. */
  language?: string;
  /** Top-level comments per page. Default 20. */
  pageSize?: number;
  client?: ContentKitClient;
}

export interface UseComments extends UseList<Comment> {
  /** Comments on the item, or replies to a top-level comment (replyTo); anonymous callers give anonName. */
  post: (body: string, o?: { replyTo?: string; anonName?: string }) => Promise<Comment>;
  edit: (id: string, body: string) => Promise<Comment>;
  /** Leaves a tombstone. */
  remove: (id: string) => Promise<void>;
  /** Sets the caller's reaction at once, rolled back if the server refuses it. */
  react: (comment: Comment, value: Reaction) => Promise<void>;
  /** Restores a deleted comment (moderators). */
  restore: (id: string) => Promise<void>;
}

/** A target's top-level comments, a page at a time, with every write; new comments and replies land in place. */
export function useComments(target: Target, o: UseCommentsOptions = {}): UseComments {
  const { client, store, scope } = useContentScope(o.client);
  const size = o.pageSize ?? 20;
  const sort = o.sort ?? "newest";
  const language = o.language || undefined;
  const { type, id, version } = target;
  const key = keyOf("comments", scope, type, id, version, language, sort, size);
  const list = useList(
    store,
    key,
    { type: "comments", target: { type, id, version } },
    offsetPages(size, (q, signal) => client.comments.list({ type, id, version }, { ...q, sort, language }, signal)),
  );
  const post = useCallback<UseComments["post"]>(
    (body, p = {}) => client.comments.create({ type, id, version }, { body, reply_to_id: p.replyTo, anon_name: p.anonName, language: p.replyTo ? undefined : language }),
    [client, type, id, version, language],
  );
  const edit = useCallback((cid: string, body: string) => client.comments.edit(cid, body), [client]);
  const remove = useCallback((cid: string) => client.comments.delete(cid), [client]);
  const restore = useCallback((cid: string) => client.comments.restore(cid), [client]);
  const react = useCallback((c: Comment, v: Reaction) => reactToComment(client, store, scope, c, v), [client, store, scope]);
  return { ...list, post, edit, remove, react, restore };
}

/** A top-level comment's replies, oldest first, a page at a time; enabled false defers the first read. */
export function useCommentReplies(id: string, o: { pageSize?: number; enabled?: boolean; client?: ContentKitClient } = {}): UseList<Comment> {
  const { client, store, scope } = useContentScope(o.client);
  const size = o.pageSize ?? 10;
  const key = o.enabled === false ? null : keyOf("replies", scope, id, size);
  return useList(store, key, { type: "replies", id }, offsetPages(size, (q, signal) => client.comments.replies(id, q, signal)));
}

/** Whether the caller may comment on the target, the ban that stops it, and what it may do to others' comments. */
export function useCanComment(target: Target, o: { client?: ContentKitClient } = {}) {
  const { client, store, scope } = useContentScope(o.client);
  const { type, id, version } = target;
  const r = useResource<CommentStanding>(store, keyOf("standing", scope, type, id, version), { type: "standing", target: { type, id, version } }, (s) =>
    client.comments.standing({ type, id, version }, s),
  );
  return { standing: r.data ?? null, loading: r.loading || !r.loaded, error: r.error, reload: r.reload };
}

/** The newest published comments across the site, with their items (the host shows titles). */
export function useLatestComments(o: { pageSize?: number; client?: ContentKitClient } = {}): UseList<FeedItem> {
  const { client, store, scope } = useContentScope(o.client);
  const size = o.pageSize ?? 20;
  return useList(store, keyOf("latest", scope, size), { type: "latest" }, offsetPages(size, (q, signal) => client.comments.latest(q, signal), true));
}

export interface UseAdminComments extends UseList<AdminComment> {
  remove: (id: string) => Promise<void>;
  restore: (id: string) => Promise<void>;
  react: (comment: Comment, value: Reaction) => Promise<void>;
}

/** Staff: every comment, newest first, deleted, held and rejected ones with their bodies (root:comments:moderate); or one content kind's, or one author's. */
export function useAdminComments(o: { contentKind?: string; author?: string; pageSize?: number; client?: ContentKitClient } = {}): UseAdminComments {
  const { client, store, scope } = useContentScope(o.client);
  const size = o.pageSize ?? 25;
  const kind = o.contentKind || undefined;
  const author = o.author || undefined;
  const list = useList(
    store,
    keyOf("admin-comments", scope, kind, author, size),
    { type: "admin-comments" },
    offsetPages(size, (q, signal) => client.comments.adminList({ ...q, contentKind: kind, author }, signal)),
  );
  const remove = useCallback((id: string) => client.comments.delete(id), [client]);
  const restore = useCallback((id: string) => client.comments.restore(id), [client]);
  const react = useCallback((c: Comment, v: Reaction) => reactToComment(client, store, scope, c, v), [client, store, scope]);
  return { ...list, remove, restore, react };
}

export interface UseModerationQueue extends UseList<HeldItem> {
  /** Approves (publishes) or rejects at the revision the queue listed; the item leaves the queue. */
  resolve: (item: HeldItem, decision: Decision, reason?: string) => Promise<void>;
}

/** Staff: held comments or content awaiting review, oldest first (ModerationReview). */
export function useModerationQueue(o: { kind?: HeldKind; pageSize?: number; client?: ContentKitClient } = {}): UseModerationQueue {
  const { client, store, scope } = useContentScope(o.client);
  const kind = o.kind ?? "comment";
  const size = o.pageSize ?? 20;
  const list = useList<HeldItem>(store, keyOf("held", scope, kind, size), { type: "held" }, async (cursor, signal) => {
    const page = await client.moderation.held({ kind, cursor: typeof cursor === "string" ? cursor : undefined, limit: size }, signal);
    return { items: page.items, next: page.next ?? null };
  });
  const resolve = useCallback(async (item: HeldItem, d: Decision, reason?: string) => void (await client.moderation.resolve(item, d, reason)), [client]);
  return { ...list, resolve };
}

export interface UseCommentBans extends UseList<CommentBan & { id: string }> {
  ban: (user: string, input?: BanInput) => Promise<CommentBan>;
  lift: (user: string) => Promise<void>;
}

/** The bans of one scope: the caller's own ("owner") or the site's ("global", CommentBan), newest first. */
export function useCommentBans(o: { scope?: BanScope; pageSize?: number; enabled?: boolean; client?: ContentKitClient } = {}): UseCommentBans {
  const { client, store, scope: readScope } = useContentScope(o.client);
  const scope = o.scope ?? "global";
  const size = o.pageSize ?? 25;
  const list = useList(
    store,
    o.enabled === false ? null : keyOf("bans", readScope, scope, size),
    { type: "bans", scope },
    offsetPages(size, async (q, signal) => (await client.bans.list(scope, q, signal)).map((b) => ({ ...b, id: b.user_id }))),
  );
  const ban = useCallback((user: string, input?: BanInput) => client.bans.ban(scope, user, input), [client, scope]);
  const lift = useCallback((user: string) => client.bans.lift(scope, user), [client, scope]);
  return { ...list, ban, lift };
}

export interface UseBannedWords {
  words: BannedWord[];
  loading: boolean;
  error?: ContentKitError;
  reload: () => void;
  /** Bans words, or changes what banned ones do. */
  ban: (words: BannedWordInput[]) => Promise<BannedWord[]>;
  unban: (words: string[]) => Promise<void>;
}

/** Staff: the banned words every comment and answer meets (root:comments:moderate). */
export function useBannedWords(o: { enabled?: boolean; client?: ContentKitClient } = {}): UseBannedWords {
  const { client, store, scope } = useContentScope(o.client);
  const r = useResource<BannedWord[]>(store, o.enabled === false ? null : keyOf("moderation-rules", scope, "words"), { type: "moderation-rules", scope: "words" }, (signal) =>
    client.moderation.bannedWords(signal),
  );
  const ban = useCallback((words: BannedWordInput[]) => client.moderation.banWords(words), [client]);
  const unban = useCallback((words: string[]) => client.moderation.unbanWords(words), [client]);
  return { words: r.data ?? [], loading: o.enabled !== false && (r.loading || !r.loaded), error: r.error, reload: r.reload, ban, unban };
}

export interface UseIPBans {
  bans: IPBan[];
  loading: boolean;
  error?: ContentKitError;
  reload: () => void;
  /** Bans addresses or networks, or replaces their bans. */
  ban: (bans: IPBanInput[]) => Promise<IPBan[]>;
  lift: (networks: string[]) => Promise<void>;
}

/** Staff: the IP bans on commenting and answering, newest first (root:comments:moderate). */
export function useIPBans(o: { enabled?: boolean; client?: ContentKitClient } = {}): UseIPBans {
  const { client, store, scope } = useContentScope(o.client);
  const r = useResource<IPBan[]>(store, o.enabled === false ? null : keyOf("moderation-rules", scope, "ips"), { type: "moderation-rules", scope: "ips" }, (signal) =>
    client.moderation.ipBans(signal),
  );
  const ban = useCallback((bans: IPBanInput[]) => client.moderation.banIPs(bans), [client]);
  const lift = useCallback((networks: string[]) => client.moderation.liftIPBans(networks), [client]);
  return { bans: r.data ?? [], loading: o.enabled !== false && (r.loading || !r.loaded), error: r.error, reload: r.reload, ban, lift };
}

export interface UseGlobalCommentBan {
  /** The user's site-wide ban, expired or not; null when none. */
  ban: CommentBan | null;
  loading: boolean;
  error?: ContentKitError;
  reload: () => void;
  /** Bans them, or replaces their ban. */
  save: (input?: BanInput) => Promise<CommentBan>;
  lift: () => Promise<void>;
}

/** Staff: one user's site-wide comment ban (root:comments:ban). */
export function useGlobalCommentBan(user: string, o: { client?: ContentKitClient } = {}): UseGlobalCommentBan {
  const { client, store, scope } = useContentScope(o.client);
  const r = useResource<CommentBan | null>(store, keyOf("ban", scope, user), { type: "bans", scope: "global", id: user }, (signal) => client.bans.get("global", user, signal));
  const save = useCallback((input?: BanInput) => client.bans.ban("global", user, input), [client, user]);
  const lift = useCallback(() => client.bans.lift("global", user), [client, user]);
  return { ban: r.data ?? null, loading: r.loading || !r.loaded, error: r.error, reload: r.reload, save, lift };
}
