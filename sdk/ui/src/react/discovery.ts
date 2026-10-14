import { useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";
import { Beacons, eventId, type Attribution, type BeaconOptions, type ViewSession } from "../client/beacons.js";
import type { ContentKitClient } from "../client/client.js";
import type { MineQuery } from "../client/content/favorites.js";
import type { AccessMode, ContentQuery, HistoryQuery, RailQuery, ReleaseDatesQuery, SearchQuery, TypeaheadQuery } from "../client/discovery.js";
import { toContentKitError, type ContentKitError } from "../client/errors.js";
import type { Card, ContentFilters, ContentFiltersUpdate, ContentPage, DateCount, GroupCard, Interaction, InteractionPage, RecentSearch } from "../client/generated/wire.js";
import { ContentKitContext, useGuarded } from "./context.js";
import { keyOf, useContentScope, useList, useResource, type UseList } from "./use-resource.js";

const page = (p: ContentPage) => ({ items: p.contents, next: p.next_cursor ?? null });

/** Titles, names and paths in the provider's language unless the query says otherwise. */
function useDisplay<T extends { display_language?: string }>(q: T | null): T | null {
  const language = useContext(ContentKitContext)?.language;
  return useMemo(() => (q && !q.display_language && language ? { ...q, display_language: language } : q), [q, language]);
}

/** Contents as cards, a cursor page at a time; null reads nothing. */
export function useContents(q: ContentQuery | null, o: { client?: ContentKitClient } = {}): UseList<Card> {
  const { client, store, scope } = useContentScope(o.client);
  const query = useDisplay(q);
  return useList(store, query ? keyOf("cards", scope, "list", query) : null, { type: "cards" }, async (cursor, signal) =>
    page(await client.discovery.list({ ...query, cursor: cursor as string | undefined }, signal)),
  );
}

/**
 * How many contents of a list the reader doesn't open: its filters' count
 * with locked cards less without them (sort and paging aside); null reads
 * nothing.
 */
export function useLockedCount(q: ContentQuery | null, o: { client?: ContentKitClient } = {}): { count?: number; loading: boolean } {
  const { client, store, scope } = useContentScope(o.client);
  const filter = useMemo(() => {
    if (!q) return null;
    const { sort: _s, cursor: _c, limit: _l, card_groups: _g, mode: _m, ...f } = q;
    return f;
  }, [q]);
  const query = useDisplay(filter);
  const r = useResource<number>(store, query ? keyOf("release-dates", scope, "locked", query) : null, { type: "release-dates" }, async (signal) => {
    const count = async (mode: AccessMode) =>
      (await client.discovery.releaseDates({ ...query, mode, by: "month", limit: 1000 }, signal)).dates.reduce((n, d) => n + d.count, 0);
    const [all, open] = await Promise.all([count("lock"), count("hide")]);
    return Math.max(0, all - open);
  });
  return { count: r.data, loading: r.loading };
}

/** Keyword search results as cards; nothing while q.q is empty. */
export function useSearch(q: SearchQuery | null, o: { client?: ContentKitClient } = {}): UseList<Card> {
  const { client, store, scope } = useContentScope(o.client);
  const query = useDisplay(q?.q.trim() ? q : null);
  return useList(store, query ? keyOf("cards", scope, "search", query) : null, { type: "cards" }, async (cursor, signal) =>
    page(await client.discovery.search({ ...query!, cursor: cursor as string | undefined }, signal)),
  );
}

export interface UseRail {
  items: Card[];
  loading: boolean;
  error?: ContentKitError;
  /** The site runs without the signal plane (not_configured): leave the rail out. */
  unavailable: boolean;
  reload: () => void;
}

function useRail(key: string | null, fetch: (signal: AbortSignal) => Promise<ContentPage>, o: { client?: ContentKitClient }): UseRail {
  const { store } = useContentScope(o.client);
  const r = useResource<Card[]>(store, key, { type: "cards" }, async (signal) => (await fetch(signal)).contents);
  const unavailable = r.error?.code === "not_configured";
  return { items: r.data ?? [], loading: !!key && (r.loading || !r.loaded), error: unavailable ? undefined : r.error, unavailable, reload: r.reload };
}

/** Contents like one: co-engagement, or the site's configured source. */
export function useSimilar(id: string | null | undefined, q: RailQuery = {}, o: { client?: ContentKitClient } = {}): UseRail {
  const { client, scope } = useContentScope(o.client);
  const query = useDisplay(q)!;
  return useRail(id ? keyOf("cards", scope, "similar", id, query) : null, (s) => client.discovery.similar(id!, query, s), o);
}

/** Contents for the signed-in reader; nothing signed out, or when q is null. */
export function useRecommendations(q: RailQuery | null = {}, o: { client?: ContentKitClient } = {}): UseRail {
  const { client, scope, viewer } = useContentScope(o.client);
  const query = useDisplay(q);
  return useRail(viewer === null || !query ? null : keyOf("cards", scope, "recommended", query), (s) => client.discovery.recommendations(query!, s), o);
}

/** A release date bucket, keyed by its date. */
export type ReleaseDate = DateCount & { id: string };

/** The release date index: per month or day, newest first. */
export function useReleaseDates(q: ReleaseDatesQuery = {}, o: { client?: ContentKitClient } = {}): UseList<ReleaseDate> {
  const { client, store, scope } = useContentScope(o.client);
  const query = useDisplay(q)!;
  return useList(store, keyOf("release-dates", scope, query), { type: "release-dates" }, async (cursor, signal) => {
    const r = await client.discovery.releaseDates({ ...query, cursor: cursor as string | undefined }, signal);
    return { items: r.dates.map((d) => ({ ...d, id: d.date })), next: r.next_cursor ?? null };
  });
}

export interface UseHistory extends UseList<Card> {
  /** Erases the reader's history: of one content item, one kind, or (no argument) everything. */
  forget: (o?: { kind?: string; content_id?: string }) => Promise<void>;
}

/** What the signed-in reader engaged with, most recent first; nothing signed out, or when q is null. */
export function useHistory(q: HistoryQuery | null = {}, o: { client?: ContentKitClient } = {}): UseHistory {
  const { client, store, scope, viewer } = useContentScope(o.client);
  const query = useDisplay(q);
  const guarded = useGuarded();
  const list = useList(store, viewer === null || !query ? null : keyOf("history", scope, query), { type: "history" }, async (cursor, signal) =>
    page(await client.discovery.history({ ...query, cursor: cursor as string | undefined }, signal)),
  );
  const forget = useCallback((f: { kind?: string; content_id?: string } = {}) => guarded(() => client.discovery.forgetHistory(f)), [client, guarded]);
  return { ...list, forget };
}

export interface UseRecentSearches {
  searches: RecentSearch[];
  loading: boolean;
  error?: ContentKitError;
  /** Clears one query, or every recent search. */
  clear: (query?: string) => Promise<void>;
}

/** The reader's recent searches, newest first (signed out, of this tab's session); enabled false reads nothing yet. */
export function useRecentSearches(o: { limit?: number; enabled?: boolean; client?: ContentKitClient } = {}): UseRecentSearches {
  const { client, store, scope } = useContentScope(o.client);
  const guarded = useGuarded();
  const key = o.enabled === false ? null : keyOf("searches", scope, o.limit ?? 10);
  const r = useResource<RecentSearch[]>(store, key, { type: "searches" }, async (s) => (await client.discovery.recentSearches({ limit: o.limit }, s)).searches);
  const clear = useCallback((query?: string) => guarded(() => client.discovery.clearSearches(query ? { query } : {})), [client, guarded]);
  return { searches: r.data ?? [], loading: !!key && (r.loading || !r.loaded), error: r.error, clear };
}

/** A favorite or reaction with its content's or group's id. */
export type MyInteraction = Interaction & { id: string };

const interactions = (p: InteractionPage) => ({ items: p.items.map((x) => ({ ...x, id: x.content?.id ?? x.group?.id ?? "" })), next: p.next_cursor ?? null });

/** The signed-in reader's favorites, most recent first: content cards or groups. */
export function useMyFavorites(q: MineQuery = {}, o: { client?: ContentKitClient } = {}): UseList<MyInteraction> {
  const { client, store, scope, viewer } = useContentScope(o.client);
  const query = useDisplay(q)!;
  return useList(store, viewer === null ? null : keyOf("favorites", scope, query), { type: "favorites" }, async (cursor, signal) =>
    interactions(await client.favorites.mine({ ...query, cursor: cursor as string | undefined }, signal)),
  );
}

/** What the signed-in reader liked (or, with value, disliked), most recent first. */
export function useMyReactions(q: MineQuery & { value?: "like" | "dislike" } = {}, o: { client?: ContentKitClient } = {}): UseList<MyInteraction> {
  const { client, store, scope, viewer } = useContentScope(o.client);
  const query = useDisplay(q)!;
  return useList(store, viewer === null ? null : keyOf("reactions", scope, query), { type: "reactions" }, async (cursor, signal) =>
    interactions(await client.reactions.mine({ ...query, cursor: cursor as string | undefined }, signal)),
  );
}

export interface UseContentFilters {
  /** null until read (and signed out). */
  filters: ContentFilters | null;
  loading: boolean;
  error?: ContentKitError;
  saving: boolean;
  /** Replaces every setting; the lists showing re-read. */
  save: (update: ContentFiltersUpdate) => Promise<ContentFilters>;
  /** Shows or hides one filter's group, keeping the other settings. */
  setVisible: (groupId: string, visible: boolean) => Promise<ContentFilters>;
  setIncludeDeleted: (include: boolean) => Promise<ContentFilters>;
}

/** The signed-in reader's content filters: each configured group, its default and their setting. */
export function useContentFilters(o: { language?: string; client?: ContentKitClient } = {}): UseContentFilters {
  const { client, store, scope, viewer, language: provided } = useContentScope(o.client);
  const language = o.language ?? provided;
  const key = viewer === null ? null : keyOf("content-filters", scope, language);
  const r = useResource<ContentFilters>(store, key, { type: "content-filters" }, (s) => client.discovery.contentFilters(language, s));
  const guarded = useGuarded();
  const [inflight, setInflight] = useState(0);
  const current = useRef(r.data);
  current.current = r.data;
  const save = useCallback(
    async (update: ContentFiltersUpdate) => {
      setInflight((n) => n + 1);
      try {
        const out = await guarded(() => client.discovery.setContentFilters(update, language));
        if (key) store.set(key, out);
        return out;
      } finally {
        setInflight((n) => n - 1);
      }
    },
    [client, guarded, language, key, store],
  );
  const settings = (f: ContentFilters) => f.filters.filter((x) => x.overridden).map((x) => ({ group_id: x.group.id, visible: x.visible }));
  const setVisible = useCallback(
    (groupId: string, visible: boolean) => {
      const f = current.current;
      if (!f) return Promise.reject(new Error("contentkit-ui: the content filters have not loaded"));
      return save({ filters: [...settings(f).filter((x) => x.group_id !== groupId), { group_id: groupId, visible }], include_deleted: f.include_deleted });
    },
    [save],
  );
  const setIncludeDeleted = useCallback(
    (include: boolean) => {
      const f = current.current;
      if (!f) return Promise.reject(new Error("contentkit-ui: the content filters have not loaded"));
      return save({ filters: settings(f), include_deleted: include });
    },
    [save],
  );
  return { filters: r.data ?? null, loading: !!key && (r.loading || !r.loaded), error: r.error, saving: inflight > 0, save, setVisible, setIncludeDeleted };
}

export interface UseTypeahead {
  contents: Card[];
  groups: GroupCard[];
  /** The query the suggestions answer (trimmed). */
  query: string;
  loading: boolean;
  error?: ContentKitError;
}

const NONE: Pick<UseTypeahead, "contents" | "groups"> = { contents: [], groups: [] };

/** Suggestions while the reader types: debounced, the stale ones abandoned. */
export function useTypeahead(
  text: string,
  o: Omit<TypeaheadQuery, "q"> & { debounce?: number; minLength?: number; client?: ContentKitClient } = {},
): UseTypeahead {
  const { client, language } = useContentScope(o.client);
  const { debounce = 150, minLength = 1, client: _own, ...rest } = o;
  const q = text.trim();
  const query = useMemo(() => ({ display_language: language, ...rest, q }), [language, q, JSON.stringify(rest)]); // eslint-disable-line react-hooks/exhaustive-deps
  const [state, setState] = useState<UseTypeahead>({ ...NONE, query: "", loading: false });
  useEffect(() => {
    if (q.length < minLength) {
      setState({ ...NONE, query: "", loading: false });
      return;
    }
    setState((s) => ({ ...s, loading: true }));
    const ctl = new AbortController();
    const timer = setTimeout(() => {
      client.discovery.typeahead(query, ctl.signal).then(
        (r) => setState({ contents: r.contents, groups: r.groups, query: q, loading: false }),
        (e) => {
          const error = toContentKitError(e);
          if (error.code !== "aborted") setState({ ...NONE, query: q, loading: false, error });
        },
      );
    }, debounce);
    return () => {
      clearTimeout(timer);
      ctl.abort();
    };
  }, [client, query, q, debounce, minLength]);
  return state;
}

const beacons = new WeakMap<ContentKitClient, Beacons>();

/** The client's signal and exposure batcher: one per client. */
export function beaconsFor(client: ContentKitClient, o?: BeaconOptions): Beacons {
  let b = beacons.get(client);
  if (!b) beacons.set(client, (b = new Beacons(client, o)));
  return b;
}

export function useBeacons(o: { client?: ContentKitClient } = {}): Beacons {
  const { client } = useContentScope(o.client);
  return beaconsFor(client);
}

/** A view of a content (or one version) while it is shown: report its progress as it changes. */
export function useViewSignal(content: string | null | undefined, version?: string, o: Attribution & { client?: ContentKitClient } = {}): ViewSession["update"] {
  const b = useBeacons(o);
  const { client: _c, ...attribution } = o;
  const session = useMemo(() => (content ? b.view(content, version, attribution) : null), [b, content, version]); // eslint-disable-line react-hooks/exhaustive-deps
  return useCallback((u) => session?.update(u), [session]);
}

export interface ExposureOptions {
  /** Where the list is: "home", "search", "similar". */
  surface: string;
  /** Changes with what the list shows: a new render id. */
  render: string;
  ranker?: string;
  query_id?: string;
  language?: string;
  client?: ContentKitClient;
}

export interface UseExposure {
  render_id: string;
  /** A card's ref: reports it shown once at least half of it is in view. */
  track: (content: string, position: number) => (el: Element | null) => void;
  /** Reports a click on the card at position. */
  click: (content: string, position: number) => void;
}

/** A rendered result list: each page rendered, each card shown, clicks attributed to the render. */
export function useExposure(items: readonly { id: string }[], o: ExposureOptions): UseExposure {
  const b = useBeacons(o);
  const renderId = useMemo(() => eventId(), [o.render]); // eslint-disable-line react-hooks/exhaustive-deps
  const base = useMemo(() => ({ render_id: renderId, surface: o.surface, ranker: o.ranker, query_id: o.query_id, language: o.language }), [renderId, o.surface, o.ranker, o.query_id, o.language]);
  const rendered = useRef(new Set<string>());
  const visible = useRef(new Set<string>());
  useEffect(() => {
    rendered.current = new Set();
    visible.current = new Set();
  }, [renderId]);
  useEffect(() => {
    const shown = items.flatMap((it, i) => (rendered.current.has(it.id) ? [] : [{ content_id: it.id, position: i + 1 }]));
    for (const s of shown) rendered.current.add(s.content_id);
    if (shown.length) b.exposure({ ...base, stage: "rendered", occurred_at: new Date().toISOString(), shown });
  }, [b, base, items]);
  const pending = useRef<{ content_id: string; position: number }[]>([]);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  const observer = useMemo(() => {
    if (typeof IntersectionObserver !== "function") return null;
    return new IntersectionObserver(
      (entries) => {
        for (const e of entries) {
          if (!e.isIntersecting) continue;
          const el = e.target as HTMLElement;
          const content = el.dataset.exposureContent!;
          if (visible.current.has(content)) continue;
          visible.current.add(content);
          pending.current.push({ content_id: content, position: Number(el.dataset.exposurePosition) });
        }
        timer.current ??= setTimeout(() => {
          timer.current = undefined;
          const shown = pending.current.splice(0);
          if (shown.length) b.exposure({ ...base, stage: "visible", occurred_at: new Date().toISOString(), shown });
        }, 300);
      },
      { threshold: 0.5 },
    );
  }, [b, base]);
  useEffect(() => () => observer?.disconnect(), [observer]);
  const track = useCallback(
    (content: string, position: number) => (el: Element | null) => {
      if (!el || !observer) return;
      (el as HTMLElement).dataset.exposureContent = content;
      (el as HTMLElement).dataset.exposurePosition = String(position);
      observer.observe(el);
    },
    [observer],
  );
  const click = useCallback((content: string, position: number) => b.click(content, { render_id: renderId, surface: o.surface, position }), [b, renderId, o.surface]);
  return { render_id: renderId, track, click };
}
