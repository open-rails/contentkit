import type { ContentKitClient } from "../client/client.js";
import { toContentKitError, type ContentKitError } from "../client/errors.js";
import { sameTarget, type Target } from "../client/content/types.js";
import type { Card, Comment, Content, Poll } from "../client/generated/wire.js";
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
export interface Page<T, M = unknown> {
  items: T[];
  next: string | number | null;
  /** Metadata belongs to the first page of this selection. */
  metadata?: M;
}

export type Fetcher<T> = (signal: AbortSignal) => Promise<T>;

/** What a slot holds, for matching changes: its resource type and the ids it depends on. */
export interface Tag {
  type: string;
  readScope?: string;
  target?: Target;
  language?: string;
  id?: string;
  scope?: string;
}

interface Slot {
  tag: Tag;
  listeners: Set<() => void>;
  entry: Resource<unknown>;
  bindings: Map<symbol, Binding>;
  ctl?: AbortController;
  moreCtl?: AbortController;
  /** Bumped by mark(): an optimistic write rolls back only if no newer one started. */
  version: number;
  /** Identifies the full read; item patches and appended pages retain it. */
  readVersion: number;
}

interface Binding {
  fetcher: Fetcher<unknown>;
  more?: (cursor: string | number, signal: AbortSignal) => Promise<Page<unknown>>;
}

const activeBinding = (s: Slot) => [...s.bindings.values()].at(-1);

const IDLE: Resource<never> = { data: undefined, loading: false, loaded: false };

/**
 * The content reads hooks share, per client: one request per key while any
 * hook shows it, optimistic writes with rollback, and every change the client
 * makes applied in place (or refetched) for every hook showing it.
 */
// A cached list is a rail (an array) or a page holding it as items (Page) or
// contents (ContentPage, SearchPage); other data under the same tag, such as a
// count, holds none and is returned unchanged.
function mapList<T>(data: unknown, fn: (list: T[]) => T[]): unknown {
  if (Array.isArray(data)) return fn(data);
  for (const key of ["items", "contents"] as const) {
    const list = (data as Record<string, unknown> | null)?.[key];
    if (Array.isArray(list)) {
      const next = fn(list);
      return next === list ? data : { ...(data as object), [key]: next };
    }
  }
  return data;
}

export class ResourceStore {
  private readonly slots = new Map<string, Slot>();
  // Tokens must remain unique when a scope leaves and its slots are recreated.
  private version = 0;

  constructor(client: ContentKitClient) {
    client.subscribe((c, scope) => this.apply(c, scope));
  }

  subscribe(key: string, tag: Tag, listener: () => void): () => void {
    let s = this.slots.get(key);
    if (!s) this.slots.set(key, (s = { tag, listeners: new Set(), bindings: new Map(), entry: IDLE, version: 0, readVersion: ++this.version }));
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

  /** Registers one subscriber's key-bound fetchers; cleanup releases only it. */
  bind(key: string, fetcher: Fetcher<unknown>, more?: Binding["more"]): () => void {
    const s = this.slots.get(key);
    if (!s) return () => {};
    const owner = Symbol();
    s.bindings.set(owner, { fetcher, more });
    return () => { s.bindings.delete(owner); };
  }

  /** Fetches unless loaded or in flight; force refetches (a list from its first page). */
  load(key: string, force = false): void {
    const s = this.slots.get(key);
    const binding = s && activeBinding(s);
    if (!s || !binding || (!force && (s.entry.loaded || s.ctl))) return;
    s.ctl?.abort();
    s.moreCtl?.abort();
    s.moreCtl = undefined;
    const ctl = (s.ctl = new AbortController());
    // The last failure stays until this fetch settles: a retry must not blink an error away.
    this.update(s, { loading: true, loadingMore: false });
    binding.fetcher(ctl.signal).then(
      (data) => {
        if (s.ctl !== ctl) return;
        s.ctl = undefined;
        s.readVersion = ++this.version;
        this.update(s, { data, loading: false, loaded: true, error: undefined });
      },
      (e) => {
        if (s.ctl !== ctl) return;
        s.ctl = undefined;
        const error = toContentKitError(e);
        // A superseded request returned above. An abort from the still-current
        // fetch must settle too; otherwise no request remains but loading stays
        // true forever. Keep the error until an explicit retry succeeds.
        this.update(s, { loading: false, loaded: true, error });
      },
    );
  }

  /** Appends a list's next page; resolves once it landed. */
  async loadMore(key: string): Promise<void> {
    const s = this.slots.get(key);
    const binding = s && activeBinding(s);
    const page = s?.entry.data as Page<{ id: string }> | undefined;
    if (!s || !binding?.more || !page || page.next === null || s.moreCtl || s.ctl) return;
    const ctl = (s.moreCtl = new AbortController());
    this.update(s, { loadingMore: true });
    try {
      const next = (await binding.more(page.next, ctl.signal)) as Page<{ id: string }>;
      if (s.moreCtl !== ctl) return;
      const cur = s.entry.data as Page<{ id: string }>;
      const seen = new Set(cur.items.map((x) => x.id));
      this.update(s, { data: { ...cur, items: [...cur.items, ...next.items.filter((x) => !seen.has(x.id))], next: next.next }, error: undefined });
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
    s.readVersion = ++this.version;
    this.update(s, { data: next, loaded: true, error: undefined });
  }

  /** Starts an optimistic write on key: the token rollback() checks. */
  mark(key: string): number {
    const s = this.slots.get(key);
    return s ? (s.version = ++this.version) : 0;
  }

  /** Restores data unless a newer optimistic write started since mark(). */
  rollback<T>(key: string, token: number, data: T): void {
    const s = this.slots.get(key);
    if (s && s.version === token) this.update(s, { data, loaded: true });
  }

  /** Applies fn to matching slots, with the read identity for scoped rollback. */
  patch<T>(match: (tag: Tag) => boolean, fn: (data: T, readVersion: number) => T): void {
    for (const s of this.slots.values()) {
      if (s.entry.data !== undefined && match(s.tag)) this.update(s, { data: fn(s.entry.data as T, s.readVersion) });
    }
  }

  /** Refetches every shown slot whose tag matches. */
  invalidate(match: (tag: Tag) => boolean): void {
    for (const [key, s] of this.slots) if (match(s.tag) && s.listeners.size) this.load(key, true);
  }

  private apply(c: ContentKitChange, scope: string | null): void {
    const patch = <T>(match: (tag: Tag) => boolean, fn: (data: T) => T) => {
      this.patch<T>((t) => match(t) && (t.readScope ?? "{}") === scope, fn);
      this.invalidate((t) => match(t) && (t.readScope ?? "{}") !== scope);
    };
    const items = <T extends { id: string }>(types: string[], fn: (item: T) => T | null) =>
      patch<unknown>(
        (t) => types.includes(t.type),
        (data) =>
          mapList<T>(data, (list) => {
            let changed = false;
            const out: T[] = [];
            for (const it of list) {
              const next = fn(it);
              if (next !== it) changed = true;
              if (next) out.push(next);
            }
            return changed ? out : list;
          }),
      );
    const comments = ["comments", "replies", "latest", "admin-comments"];
    // Cards in lists (pages) and rails (arrays) show a content's counts and the viewer's state.
    const cards = (id: string, fn: (card: Card) => Card) =>
      patch<unknown>(
        (t) => t.type === "cards" || t.type === "history" || t.type === "channel-feed",
        (data) => mapList<Card>(data, (list) => (list.some((x) => x.id === id) ? list.map((x) => (x.id === id ? fn(x) : x)) : list)),
      );
    switch (c.type) {
      case "media.committed":
      case "media.processed":
        this.invalidate((t) => t.type === "page-notices" && t.id === c.ref.version);
        break;
      case "comment.created": {
        const parent = c.comment.reply_to_id;
        const published = !c.comment.moderation;
        const thread = (t: Tag) => sameTarget(t.target, c.target) && (!t.language || t.language === c.comment.language);
        const add = (t: Tag) => (parent ? t.type === "replies" && t.id === parent : t.type === "comments" && thread(t));
        patch<Page<Comment>>(add, (p) => (p.items.some((x) => x.id === c.comment.id) ? p : { ...p, items: parent ? [...p.items, c.comment] : [c.comment, ...p.items] }));
        if (parent && published) items<Comment>(["comments"], (x) => (x.id === parent ? { ...x, reply_count: x.reply_count + 1 } : x));
        this.invalidate((t) => t.type === "latest" || t.type === "admin-comments");
        if (!parent) this.invalidate((t) => t.type === "standing" && thread(t));
        break;
      }
      case "comment.updated":
        items<Comment>(comments, (x) => (x.id === c.comment.id ? { ...x, ...c.comment, author: c.comment.author ?? x.author } : x));
        this.invalidate((t) => t.type === "standing");
        break;
      case "comment.reacted":
        items<Comment>(comments, (x) => (x.id === c.id ? { ...x, ...c.counts } : x));
        break;
      case "comment.deleted": {
        let parent: string | undefined;
        // As the server lists them, a deleted root stays only while it holds replies.
        items<Comment>(["comments", "replies"], (x) => {
          if (x.id !== c.id || x.deleted) return x;
          if (!x.moderation) parent = x.reply_to_id;
          if (!x.reply_to_id && x.reply_count === 0) return null;
          return { ...x, deleted: true, body: "[deleted]", author: undefined, user_id: undefined, anon_name: undefined };
        });
        if (parent)
          items<Comment>(["comments"], (x) => {
            if (x.id !== parent) return x;
            const reply_count = Math.max(0, x.reply_count - 1);
            return x.deleted && reply_count === 0 ? null : { ...x, reply_count };
          });
        items<Comment>(["latest"], (x) => (x.id === c.id ? null : x));
        items<Comment & { deleted_at?: string }>(["admin-comments"], (x) => (x.id === c.id ? { ...x, deleted: true, deleted_at: new Date().toISOString() } : x));
        this.invalidate((t) => t.type === "standing");
        break;
      }
      case "comment.restored":
        this.invalidate((t) => comments.includes(t.type) || t.type === "standing");
        break;
      case "reaction.changed":
        patch((t) => t.type === "reaction" && sameTarget(t.target, c.target), () => c.counts);
        if (c.target.type === "contents") {
          cards(c.target.id, (x) => ({ ...x, counts: { ...x.counts, likes: c.counts.likes, dislikes: c.counts.dislikes } }));
          this.invalidate((t) => t.type === "daily-releases");
        }
        break;
      case "favorite.changed":
        patch((t) => t.type === "favorite" && sameTarget(t.target, c.target), () => ({ favorited: c.favorited, count: c.count }));
        if (c.target.type === "contents") {
          cards(c.target.id, (x) => ({ ...x, counts: { ...x.counts, favorites: c.count }, viewer: x.viewer && { ...x.viewer, favorited: c.favorited } }));
          this.invalidate((t) => t.type === "daily-releases");
        }
        this.invalidate((t) => t.type === "favorites");
        break;
      case "content.saved":
        patch<Content | null>((t) => t.type === "content" && t.id === c.content.id, () => c.content);
        // A state, kind or text change can move it between lists: refetch them.
        this.invalidate((t) => t.type === "contents" || t.type === "cards" || t.type === "channel-feed" || t.type === "daily-releases" || (t.type === "content-detail" && t.id === c.content.id));
        this.invalidate((t) => t.type === "standing" && t.target?.type === "contents" && t.target.id === c.content.id);
        break;
      case "content.changed":
        this.invalidate((t) => ((t.type === "content" || t.type === "content-detail") && t.id === c.id) || t.type === "contents" || t.type === "cards" || t.type === "channel-feed" || t.type === "daily-releases" || t.type === "history");
        this.invalidate((t) => t.type === "standing" && t.target?.type === "contents" && t.target.id === c.id);
        break;
      case "content.deleted":
        items<{ id: string }>(["contents", "cards", "history", "channel-feed"], (x) => (x.id === c.id ? null : x));
        this.invalidate((t) => ((t.type === "content" || t.type === "content-detail") && t.id === c.id) || (t.type === "contents" && t.scope === "deleted") || t.type === "daily-releases");
        this.invalidate((t) => t.type === "standing" && t.target?.type === "contents" && t.target.id === c.id);
        break;
      case "poll.created":
        this.invalidate((t) => t.type === "polls" || (t.type === "poll" && !t.id));
        break;
      case "poll.updated":
        // A latest-poll slot has no id in its tag: match by the poll it holds.
        patch<Poll | null>((t) => t.type === "poll", (p) => (p?.id === c.poll.id ? c.poll : p));
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
      case "channels.changed":
        this.invalidate((t) => t.type.startsWith("channel") || t.type === "cards" || t.type === "content-detail");
        break;
      case "channel.reposts.changed":
        this.invalidate((t) => t.type === "channel-feed" && t.id === c.id);
        break;
      case "entities.changed":
        this.invalidate((t) => t.type.startsWith("entit") || t.type === "daily-releases");
        break;
      case "contentFilters.changed":
        this.invalidate((t) => t.type === "cards" || t.type === "channel-feed" || t.type === "history" || t.type === "release-dates" || t.type === "daily-releases");
        break;
      case "access.changed":
        this.invalidate((t) => t.type === "cards" || t.type === "history" || t.type === "content-detail" || t.type === "reader-plan" || t.type.startsWith("entit") || t.type === "daily-releases" || t.type === "standing" || t.type.startsWith("channel"));
        break;
      case "engagement.changed":
        this.invalidate((t) => t.type === "history" || t.type === "cards" || t.type === "daily-releases" || t.type === "standing" || t.type.startsWith("stats") || (t.type === "content-detail" && !!t.id && c.ids.includes(t.id)));
        break;
      case "history.changed":
        this.invalidate((t) => t.type === "history" || t.type === "cards" || t.type === "daily-releases");
        break;
      case "searches.changed":
        this.invalidate((t) => t.type === "searches");
        break;
      case "rules.changed":
        this.invalidate((t) => t.type === "moderation-rules" && t.scope === c.rules);
        break;
      case "reader.changed":
        this.invalidate((t) => (t.type === "reader-plan" || t.type === "page-notices") && t.id === c.version);
        break;
      case "moderation.resolved":
        items<{ id: string }>(["held"], (x) => (x.id === c.id ? null : x));
        if (c.kind === "comment") this.invalidate((t) => comments.includes(t.type) || t.type === "standing");
        else this.invalidate((t) => (t.type === "content" && t.id === c.id) || t.type === "contents");
        break;
    }
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
