import { useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { Beacons, eventId, type Attribution, type BeaconOptions, type ViewSession } from "../client/beacons.js";
import type { ContentKitClient } from "../client/client.js";
import type { MineQuery } from "../client/content/favorites.js";
import type { AccessMode, ContentQuery, DailyReleasesQuery, HistoryQuery, RailQuery, ReleaseDatesQuery, SearchQuery, TypeaheadQuery } from "../client/discovery.js";
import { ContentKitError, toContentKitError } from "../client/errors.js";
import type { Card, ContentFilters, ContentFiltersUpdate, ContentPage, DailyReleases, DateCount, GroupCard, Interaction, InteractionPage, RecentSearch } from "../client/generated/wire.js";
import { ContentKitContext, useGuarded } from "./context.js";
import { keyOf, useContentScope, useList, useResource, type UseList } from "./use-resource.js";
import { recentSearchLimit } from "../client/browser-search-history.js";

const page = (p: ContentPage) => ({ items: p.contents, next: p.next_cursor ?? null });
const NO_SEARCHES: RecentSearch[] = [];

// A new identity even after A → B → A, so late requests cannot write into the new visit.
function useCurrentDiscoveryScope(client: ContentKitClient, scope: string): () => boolean {
  const owner = useMemo(() => ({ client, scope }), [client, scope]);
  const current = useRef<typeof owner | null>(owner);
  useLayoutEffect(() => {
    current.current = owner;
    return () => { current.current = null; };
  }, [owner]);
  return useCallback(() => current.current === owner, [owner]);
}

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
    const { sort: _s, cursor: _c, limit: _l, card_entities: _g, mode: _m, ...f } = q;
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
  const { client, store, scope, viewer } = useContentScope(o.client);
  const query = useDisplay(q?.q.trim() ? q : null);
  const isCurrent = useCurrentDiscoveryScope(client, scope);
  return useList(store, query ? keyOf("cards", scope, "search", query) : null, { type: "cards" }, async (cursor, signal) => {
    const out = await client.discovery.search({ ...query!, cursor: cursor as string | undefined }, signal);
    if (!cursor && viewer === null && !signal.aborted && isCurrent()) client.discovery.browserSearches.record(query!.q, out.contents.length);
    return page(out);
  });
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

export interface UseDailyReleases {
  data?: DailyReleases;
  loading: boolean;
  error?: ContentKitError;
  reload: () => void;
}

/** One complete calendar window, shared by all readers of the same query. */
export function useDailyReleases(q: DailyReleasesQuery | null = {}, o: { client?: ContentKitClient } = {}): UseDailyReleases {
  const { client, store, scope } = useContentScope(o.client);
  const query = useDisplay(q);
  const r = useResource<DailyReleases>(store, query ? keyOf("daily-releases", scope, query) : null, { type: "daily-releases" }, (signal) => client.discovery.releases(query!, signal));
  return { data: r.data, loading: !!query && (r.loading || !r.loaded), error: r.error, reload: r.reload };
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

/** Guest history stays in this browser; account history comes only from the server. enabled false reads nothing yet. */
export function useRecentSearches(o: { limit?: number; enabled?: boolean; client?: ContentKitClient } = {}): UseRecentSearches {
  const { client, store, scope, viewer } = useContentScope(o.client);
  const guarded = useGuarded();
  const isCurrent = useCurrentDiscoveryScope(client, scope);
  const guest = viewer === null;
  const enabled = o.enabled !== false;
  const browser = client.discovery.browserSearches;
  const subscribe = useCallback((listener: () => void) => guest && enabled ? browser.subscribe(listener) : () => {}, [browser, guest, enabled]);
  const snapshot = useCallback(() => guest && enabled ? browser.snapshot() : NO_SEARCHES, [browser, guest, enabled]);
  const local = useSyncExternalStore(subscribe, snapshot, () => NO_SEARCHES);
  const searches = useMemo(() => local.slice(0, recentSearchLimit(o.limit)), [local, o.limit]);
  const key = !enabled || guest ? null : keyOf("searches", scope, o.limit ?? 10);
  const r = useResource<RecentSearch[]>(store, key, { type: "searches" }, async (s) => (await client.discovery.recentSearches({ limit: o.limit }, s)).searches);
  useEffect(() => {
    if (!key) return;
    const reload = () => { if (!store.snapshot(key).loading) store.load(key, true); };
    // Reopening a panel must not leave an already-cached account list stale.
    if (store.snapshot(key).loaded) reload();
    const visible = () => { if (document.visibilityState === "visible") reload(); };
    if (typeof window !== "undefined") window.addEventListener("focus", reload);
    if (typeof document !== "undefined") document.addEventListener("visibilitychange", visible);
    return () => {
      if (typeof window !== "undefined") window.removeEventListener("focus", reload);
      if (typeof document !== "undefined") document.removeEventListener("visibilitychange", visible);
    };
  }, [key, store]);
  const clear = useCallback(async (query?: string) => {
    if (!isCurrent()) return;
    if (guest) { browser.clear(query); return; }
    await guarded(() => isCurrent() ? client.discovery.clearSearches(query ? { query } : {}) : Promise.resolve());
  }, [browser, client, guest, guarded, isCurrent]);
  return guest
    ? { searches: enabled ? searches : NO_SEARCHES, loading: false, clear }
    : { searches: r.data ?? NO_SEARCHES, loading: !!key && (r.loading || !r.loaded), error: r.error, clear };
}

/** A favorite or reaction with its content's or group's id. */
export type MyInteraction = Interaction & { id: string };

const interactions = (p: InteractionPage) => ({ items: p.items.map((x) => ({ ...x, id: x.content?.id ?? x.entity?.id ?? "" })), next: p.next_cursor ?? null });

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
  /** null until the configured filters have loaded. */
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

/** Configured filters: browser choices for guests, server choices for accounts. */
export function useContentFilters(o: { language?: string; client?: ContentKitClient } = {}): UseContentFilters {
  const { client, store, scope, viewer, language: provided } = useContentScope(o.client);
  const language = o.language ?? provided;
  const guest = viewer === null;
  const browser = client.preferences.browser;
  const local = useSyncExternalStore(browser.subscribe, browser.snapshot, browser.snapshot);
  const key = keyOf("content-filters", scope, language);
  const previous = useRef<{ client: ContentKitClient; language?: string; guest: boolean; data?: ContentFilters } | null>(null);
  const initial = guest && previous.current?.guest && previous.current.client === client && previous.current.language === language ? previous.current.data : undefined;
  const r = useResource<ContentFilters>(store, key, { type: "content-filters" }, (s) => client.discovery.contentFilters(language, s), { initial });
  previous.current = { client, language, guest, data: r.data };
  const applyGuest = useCallback((source: ContentFilters, settings = local.filters): ContentFilters => {
    const values = new Map(settings.map((f) => [f.group_id, f.visible]));
    return { include_deleted: false, filters: source.filters.map((f) => ({ ...f, visible: values.get(f.entity.id) ?? f.default_visible, overridden: values.has(f.entity.id) })) };
  }, [local.filters]);
  const displayed = useMemo(() => r.data && guest ? applyGuest(r.data) : r.data, [r.data, guest, applyGuest]);
  const guarded = useGuarded();
  const isCurrent = useCurrentDiscoveryScope(client, scope);
  const [inflight, setInflight] = useState(0);
  const current = useRef(displayed);
  current.current = displayed;
  useEffect(() => { setInflight(0); }, [client, scope]);
  const save = useCallback(
    async (update: ContentFiltersUpdate) => {
      if (!isCurrent()) throw new ContentKitError("aborted", "The account changed.");
      if (guest) {
        const source = current.current;
        if (!source) throw new ContentKitError("invalid_request", "Content filters have not loaded.");
        const allowed = new Set(source.filters.map((f) => f.entity.id));
        if (update.filters.some((f) => !allowed.has(f.group_id))) throw new ContentKitError("invalid_request", "This content filter is not configured.");
        browser.setFilters(update.filters);
        return applyGuest(source, update.filters);
      }
      setInflight((n) => n + 1);
      try {
        const out = await guarded(() => {
          if (!isCurrent()) throw new ContentKitError("aborted", "The account changed.");
          return client.discovery.setContentFilters(update, language);
        });
        if (isCurrent()) store.set(key, out);
        return out;
      } finally {
        setInflight((n) => Math.max(0, n - 1));
      }
    },
    [client, guarded, language, key, store, isCurrent, guest, browser, applyGuest],
  );
  const settings = (f: ContentFilters) => f.filters.filter((x) => x.overridden).map((x) => ({ group_id: x.entity.id, visible: x.visible }));
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
  return { filters: displayed ?? null, loading: r.loading || !r.loaded, error: r.error, saving: inflight > 0, save, setVisible, setIncludeDeleted };
}

export interface UseTypeahead {
  contents: Card[];
  entities: GroupCard[];
  /** The query the suggestions answer (trimmed). */
  query: string;
  loading: boolean;
  error?: ContentKitError;
}

const NONE: Pick<UseTypeahead, "contents" | "entities"> = { contents: [], entities: [] };

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
        (r) => setState({ contents: r.contents, entities: r.entities, query: q, loading: false }),
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
