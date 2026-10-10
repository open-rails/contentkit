import type { ContentKitClient } from "../client/client.js";
import { toContentKitError, type ContentKitError } from "../client/errors.js";
import type { Comment, Poll, Post, RefBody } from "../client/generated/wire.js";
import type { ContentKitChange } from "../client/http.js";

/** One shared read: its data, whether a fetch is in flight, and its last failure. */
export interface Resource<T> {
  data: T | undefined;
  loading: boolean;
  /** A fetch or set has completed. */
  loaded: boolean;
  /** A list's next page is in flight. */
  loadingMore?: boolean;
  error?: ContentKitError;
}

/** A page of a list and how to get the next one (null: no more). */
export interface Page<T> {
  items: T[];
  next: string | number | null;
}

export type Fetcher<T> = (signal: AbortSignal) => Promise<T>;

/** What a slot holds, for matching changes: its resource type and the ids it depends on. */
export interface Tag {
  type: string;
  ref?: RefBody;
  id?: string;
  scope?: string;
}

interface Slot {
  tag: Tag;
  listeners: Set<() => void>;
  entry: Resource<unknown>;
  fetcher?: Fetcher<unknown>;
  more?: (cursor: string | number, signal: AbortSignal) => Promise<Page<unknown>>;
  ctl?: AbortController;
  moreCtl?: AbortController;
  /** Bumped by mark(): an optimistic write rolls back only if no newer one started. */
  version: number;
}

const IDLE: Resource<never> = { data: undefined, loading: false, loaded: false };

const sameRef = (a?: RefBody, b?: RefBody) => !!a && !!b && a.kind === b.kind && a.id === b.id;

/**
 * The content reads hooks share, per client: one request per key while any
 * hook shows it, optimistic writes with rollback, and every change the client
 * makes applied in place (or refetched) for every hook showing it.
 */
export class ResourceStore {
  private readonly slots = new Map<string, Slot>();

  constructor(client: ContentKitClient) {
    client.subscribe((c) => this.apply(c));
  }

  subscribe(key: string, tag: Tag, listener: () => void): () => void {
    let s = this.slots.get(key);
    if (!s) this.slots.set(key, (s = { tag, listeners: new Set(), entry: IDLE, version: 0 }));
    const slot = s;
    slot.listeners.add(listener);
    return () => {
      slot.listeners.delete(listener);
      // Kept across a StrictMode remount; dropped once no hook shows it.
      queueMicrotask(() => {
        if (slot.listeners.size || this.slots.get(key) !== slot) return;
        slot.ctl?.abort();
        slot.moreCtl?.abort();
        this.slots.delete(key);
      });
    };
  }

  snapshot<T>(key: string): Resource<T> {
    return (this.slots.get(key)?.entry ?? IDLE) as Resource<T>;
  }

  /** The latest fetchers of a key: its first read, and a list's next page. */
  bind(key: string, fetcher: Fetcher<unknown>, more?: Slot["more"]): void {
    const s = this.slots.get(key);
    if (s) Object.assign(s, { fetcher, more });
  }

  /** Fetches unless loaded or in flight; force refetches (a list from its first page). */
  load(key: string, force = false): void {
    const s = this.slots.get(key);
    if (!s?.fetcher || (!force && (s.entry.loaded || s.ctl))) return;
    s.ctl?.abort();
    s.moreCtl?.abort();
    s.moreCtl = undefined;
    const ctl = (s.ctl = new AbortController());
    this.update(s, { loading: true, loadingMore: false, error: undefined });
    s.fetcher(ctl.signal).then(
      (data) => {
        if (s.ctl !== ctl) return;
        s.ctl = undefined;
        this.update(s, { data, loading: false, loaded: true, error: undefined });
      },
      (e) => {
        if (s.ctl !== ctl) return;
        s.ctl = undefined;
        const error = toContentKitError(e);
        if (error.code !== "aborted") this.update(s, { loading: false, loaded: true, error });
      },
    );
  }

  /** Appends a list's next page; resolves once it landed. */
  async loadMore(key: string): Promise<void> {
    const s = this.slots.get(key);
    const page = s?.entry.data as Page<{ id: string }> | undefined;
    if (!s?.more || !page || page.next === null || s.moreCtl || s.ctl) return;
    const ctl = (s.moreCtl = new AbortController());
    this.update(s, { loadingMore: true });
    try {
      const next = (await s.more(page.next, ctl.signal)) as Page<{ id: string }>;
      if (s.moreCtl !== ctl) return;
      const cur = s.entry.data as Page<{ id: string }>;
      const seen = new Set(cur.items.map((x) => x.id));
      this.update(s, { data: { items: [...cur.items, ...next.items.filter((x) => !seen.has(x.id))], next: next.next }, error: undefined });
    } catch (e) {
      const error = toContentKitError(e);
      if (s.moreCtl === ctl && error.code !== "aborted") this.update(s, { error });
    } finally {
      if (s.moreCtl === ctl) {
        s.moreCtl = undefined;
        this.update(s, { loadingMore: false });
      }
    }
  }

  /** Replaces a key's data for every hook showing it. */
  set<T>(key: string, data: T | ((prev: T | undefined) => T)): void {
    const s = this.slots.get(key);
    if (!s) return;
    const next = typeof data === "function" ? (data as (p: T | undefined) => T)(s.entry.data as T | undefined) : data;
    this.update(s, { data: next, loaded: true, error: undefined });
  }

  /** Starts an optimistic write on key: the token rollback() checks. */
  mark(key: string): number {
    const s = this.slots.get(key);
    return s ? ++s.version : 0;
  }

  /** Restores data unless a newer optimistic write started since mark(). */
  rollback<T>(key: string, token: number, data: T): void {
    const s = this.slots.get(key);
    if (s && s.version === token) this.update(s, { data, loaded: true });
  }

  /** Applies fn to the data of every slot whose tag matches. */
  patch<T>(match: (tag: Tag) => boolean, fn: (data: T) => T): void {
    for (const s of this.slots.values()) {
      if (s.entry.data !== undefined && match(s.tag)) this.update(s, { data: fn(s.entry.data as T) });
    }
  }

  /** Refetches every shown slot whose tag matches. */
  invalidate(match: (tag: Tag) => boolean): void {
    for (const [key, s] of this.slots) if (match(s.tag) && s.listeners.size) this.load(key, true);
  }

  private apply(c: ContentKitChange): void {
    const items = <T extends { id: string }>(types: string[], fn: (item: T) => T | null) =>
      this.patch<Page<T>>(
        (t) => types.includes(t.type),
        (page) => {
          let changed = false;
          const out: T[] = [];
          for (const it of page.items) {
            const next = fn(it);
            if (next !== it) changed = true;
            if (next) out.push(next);
          }
          return changed ? { ...page, items: out } : page;
        },
      );
    const comments = ["comments", "replies", "latest", "admin-comments"];
    switch (c.type) {
      case "comment.created": {
        const parent = c.comment.reply_to_id;
        const published = !c.comment.moderation;
        const add = (t: Tag) => (parent ? t.type === "replies" && t.id === parent : t.type === "comments" && sameRef(t.ref, c.ref));
        this.patch<Page<Comment>>(add, (p) => (p.items.some((x) => x.id === c.comment.id) ? p : { ...p, items: parent ? [...p.items, c.comment] : [c.comment, ...p.items] }));
        if (parent && published) items<Comment>(["comments"], (x) => (x.id === parent ? { ...x, reply_count: x.reply_count + 1 } : x));
        this.invalidate((t) => t.type === "latest" || t.type === "admin-comments");
        break;
      }
      case "comment.updated":
        items<Comment>(comments, (x) => (x.id === c.comment.id ? { ...x, ...c.comment, author: c.comment.author ?? x.author } : x));
        break;
      case "comment.reacted":
        items<Comment>(comments, (x) => (x.id === c.id ? { ...x, ...c.counts } : x));
        break;
      case "comment.deleted": {
        let parent: string | undefined;
        items<Comment>(["comments", "replies"], (x) => {
          if (x.id !== c.id || x.deleted) return x;
          if (!x.moderation) parent = x.reply_to_id;
          return { ...x, deleted: true, body: "[deleted]", author: undefined, user_id: undefined, anon_name: undefined };
        });
        if (parent) items<Comment>(["comments"], (x) => (x.id === parent ? { ...x, reply_count: Math.max(0, x.reply_count - 1) } : x));
        items<Comment>(["latest"], (x) => (x.id === c.id ? null : x));
        items<Comment & { deleted_at?: string }>(["admin-comments"], (x) => (x.id === c.id ? { ...x, deleted: true, deleted_at: new Date().toISOString() } : x));
        break;
      }
      case "comment.restored":
        this.invalidate((t) => comments.includes(t.type));
        break;
      case "reaction.changed":
        this.patch((t) => t.type === "reaction" && sameRef(t.ref, c.ref), () => c.counts);
        if (c.ref.kind === "post") this.postTotals(c.ref.id, c.counts.likes, c.counts.dislikes);
        break;
      case "favorite.changed":
        this.patch((t) => t.type === "favorite" && sameRef(t.ref, c.ref), () => ({ favorited: c.favorited, count: c.count }));
        this.invalidate((t) => t.type === "favorites");
        break;
      case "post.created":
        this.invalidate((t) => t.type === "posts");
        break;
      case "post.updated":
        this.patch<Post | null>((t) => t.type === "post", (p) => (p?.id === c.post.id ? c.post : p));
        items<Post>(["posts"], (x) => (x.id === c.post.id ? c.post : x));
        break;
      case "post.changed":
        this.invalidate((t) => (t.type === "post" && t.id === c.id) || t.type === "posts");
        break;
      case "post.deleted":
        items<Post>(["posts"], (x) => (x.id === c.id ? null : x));
        this.invalidate((t) => t.type === "posts" && t.scope === "deleted");
        break;
      case "post.restored":
        this.invalidate((t) => (t.type === "post" && t.id === c.post.id) || t.type === "posts");
        break;
      case "poll.created":
        this.invalidate((t) => t.type === "polls" || (t.type === "poll" && !t.id));
        break;
      case "poll.updated":
        // A latest-poll slot has no id in its tag: match by the poll it holds.
        this.patch<Poll | null>((t) => t.type === "poll", (p) => (p?.id === c.poll.id ? c.poll : p));
        items<Poll>(["polls"], (x) => (x.id === c.poll.id ? c.poll : x));
        break;
      case "poll.changed":
        this.invalidate((t) => (t.type === "poll" && (t.id === c.id || !t.id)) || t.type === "polls");
        break;
      case "poll.deleted":
        items<Poll>(["polls"], (x) => (x.id === c.id ? null : x));
        this.invalidate((t) => t.type === "poll" && !t.id);
        break;
      case "ban.saved":
      case "ban.lifted":
        this.invalidate((t) => (t.type === "bans" && t.scope === c.scope) || t.type === "standing");
        break;
      case "moderation.resolved":
        items<{ id: string }>(["held"], (x) => (x.id === c.id ? null : x));
        if (c.kind === "comment") this.invalidate((t) => comments.includes(t.type));
        else this.invalidate((t) => (t.type === "post" && t.id === c.id) || t.type === "posts");
        break;
    }
  }

  private postTotals(id: string, likes: number, dislikes: number): void {
    const fn = (p: Post): Post => (p.id === id ? { ...p, total_likes: likes, total_dislikes: dislikes } : p);
    this.patch<Post>((t) => t.type === "post" && t.id === id, fn);
    this.patch<Page<Post>>((t) => t.type === "posts", (page) => ({ ...page, items: page.items.map(fn) }));
  }

  // A new entry per change: useSyncExternalStore compares snapshots by identity.
  private update(s: Slot, patch: Partial<Resource<unknown>>): void {
    s.entry = { ...s.entry, ...patch };
    for (const l of s.listeners) l();
  }
}

const stores = new WeakMap<ContentKitClient, ResourceStore>();

/** The client's shared content store. */
export function resourcesFor(client: ContentKitClient): ResourceStore {
  let s = stores.get(client);
  if (!s) stores.set(client, (s = new ResourceStore(client)));
  return s;
}
