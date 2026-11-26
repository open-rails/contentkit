import { useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { ActivityReporter, type ActivityReporterOptions, type OpenSource } from "../client/activity.js";
import type { ContentKitClient } from "../client/client.js";
import type { MineQuery } from "../client/content/favorites.js";
import type { ContentQuery, ContinueQuery, DailyReleasesQuery, HistoryQuery, RailQuery, ReleaseDatesQuery, SearchQuery, TypeaheadQuery } from "../client/discovery.js";
import { ContentKitError, toContentKitError } from "../client/errors.js";
import type { Card, ContentFilters, ContentFiltersUpdate, ContentPage, DailyReleases, DateCount, EntityCard, Interaction, InteractionPage, RecentSearch, SearchPage, Typeahead } from "../client/generated/wire.js";
import { ContentKitContext, useGuarded } from "./context.js";
import { keyOf, useContentScope, useList, useResource, type UseList } from "./use-resource.js";
import { recentSearchLimit } from "../client/browser-search-history.js";

const page = (p: ContentPage) => ({ items: p.contents, next: p.next_cursor ?? null, metadata: p.summary });
export interface UseContents extends UseList<Card, ContentPage["summary"]> { summary?: ContentPage["summary"]; popularity?: ContentPage["popularity"] }
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
export function useContents(q: ContentQuery | null, o: { client?: ContentKitClient } = {}): UseContents {
  const { client, store, scope } = useContentScope(o.client);
  const query = useDisplay(q);
  const list = useList<Card, Pick<ContentPage,"summary" | "popularity">>(store, query ? keyOf("cards", scope, "list", query) : null, { type: "cards" }, async (cursor, signal) => {
    const out = await client.discovery.list({ ...query, cursor: cursor as string | undefined, summary: cursor ? undefined : query?.summary }, signal);
    return {...page(out),metadata:{summary:out.summary,popularity:out.popularity}};
  });
  return { ...list, metadata:list.metadata?.summary, summary: list.metadata?.summary, popularity:list.metadata?.popularity };
}

/** One numbered page of a list or search: its cards and the selection's summary (total included). */
export interface UseContentsPage {
  items: Card[];
  /** A search's request_id: opens of its results send it back. */
  requestId?: string;
  popularity?: ContentPage["popularity"];
  summary?: ContentPage["summary"];
  loading: boolean;
  error?: ContentKitError;
  reload: () => void;
}

const NO_CARDS: Card[] = [];
const NO_ENTITIES: EntityCard[] = [];

function useNumbered(key: string | null, fetch: (signal: AbortSignal) => Promise<ContentPage>, o: { client?: ContentKitClient }): UseContentsPage {
  const { store } = useContentScope(o.client);
  const r = useResource<ContentPage>(store, key, { type: "cards" }, fetch);
  return { items: r.data?.contents ?? NO_CARDS, popularity:r.data?.popularity, summary: r.data?.summary, loading: r.loading || (!!key && !r.loaded), error: r.error, reload: r.reload };
}

/** Page `page` (from 1) of a list, with its summary; null reads nothing. */
export function useContentsPage(q: ContentQuery | null, page: number, o: { client?: ContentKitClient } = {}): UseContentsPage {
  const { client, scope } = useContentScope(o.client);
  const query = useDisplay(q);
  const n = Math.max(1, Math.floor(page));
  return useNumbered(query ? keyOf("cards", scope, "list-page", query, n) : null, (signal) => client.discovery.list({ ...query!, cursor: undefined, page: n, summary: query!.summary ?? true }, signal), o);
}

/** Page `page` (from 1) of a search, with its summary and, on page 1, the matching entities; nothing until the query has text. */
export function useSearchPage(q: SearchQuery | null, page: number, o: { client?: ContentKitClient } = {}): UseContentsPage & { entities: EntityCard[] } {
  const { client, store, scope, viewer } = useContentScope(o.client);
  const query = useDisplay(q?.q.trim() ? q : null);
  const n = Math.max(1, Math.floor(page));
  const isCurrent = useCurrentDiscoveryScope(client, scope);
  const key = query ? keyOf("cards", scope, "search-page", query, n) : null;
  const r = useResource<SearchPage>(store, key, { type: "cards" }, async (signal) => {
    const out = await client.discovery.search({ ...query!, cursor: undefined, page: n, summary: query!.summary ?? true }, signal);
    if (n === 1 && viewer === null && !signal.aborted && isCurrent()) client.discovery.browserSearches.record(query!.q, out.contents.length);
    return out;
  });
  return { items: r.data?.contents ?? NO_CARDS, requestId: r.data?.request_id, popularity:r.data?.popularity, summary: r.data?.summary, entities: r.data?.entities ?? NO_ENTITIES, loading: r.loading || (!!key && !r.loaded), error: r.error, reload: r.reload };
}

/** Locked contents in the same full selection, including its activity window. */
export function useLockedCount(q: ContentQuery | null, o: { client?: ContentKitClient } = {}): { count?: number; loading: boolean } {
  const { client, store, scope } = useContentScope(o.client);
  const filter = useMemo(() => {
    if (!q) return null;
    const { cursor: _cursor, ...rest } = q;
    return { ...rest, mode: "lock" as const, limit: 1, summary: true };
  }, [q]);
  const query = useDisplay(filter);
  const r = useResource<number>(store, query ? keyOf("cards", scope, "locked-summary", query) : null, { type: "cards" }, async (signal) => {
    const out = await client.discovery.list(query!, signal);
    return out.summary?.locked_count ?? 0;
  });
  return { count: r.data, loading: r.loading };
}

/** Keyword search results as cards, and the entities matching the query; nothing while q.q is empty. */
export function useSearch(q: SearchQuery | null, o: { client?: ContentKitClient } = {}): UseContents & { entities: EntityCard[]; requestId?: string } {
  const { client, store, scope, viewer } = useContentScope(o.client);
  const query = useDisplay(q?.q.trim() ? q : null);
  const isCurrent = useCurrentDiscoveryScope(client, scope);
  const list = useList<Card, Pick<SearchPage, "summary" | "entities" | "popularity" | "request_id">>(store, query ? keyOf("cards", scope, "search", query) : null, { type: "cards" }, async (cursor, signal) => {
    const out = await client.discovery.search({ ...query!, cursor: cursor as string | undefined, summary: cursor ? undefined : query?.summary }, signal);
    if (!cursor && viewer === null && !signal.aborted && isCurrent()) client.discovery.browserSearches.record(query!.q, out.contents.length);
    return { items: out.contents, next: out.next_cursor ?? null, metadata: { summary: out.summary, entities: out.entities, popularity:out.popularity, request_id: out.request_id } };
  });
  return { ...list, metadata: list.metadata?.summary, summary: list.metadata?.summary, popularity:list.metadata?.popularity, entities: list.metadata?.entities ?? NO_ENTITIES, requestId: list.metadata?.request_id };
}

export interface UseRail {
  items: Card[];
  summary?: ContentPage["summary"];
  loading: boolean;
  error?: ContentKitError;
  /** The site runs without the signal plane (not_configured): leave the rail out. */
  unavailable: boolean;
  reload: () => void;
}

function useRail(key: string | null, fetch: (signal: AbortSignal) => Promise<ContentPage>, o: { client?: ContentKitClient }): UseRail {
  const { store } = useContentScope(o.client);
  const r = useResource<ContentPage>(store, key, { type: "cards" }, fetch);
  const unavailable = r.error?.code === "not_configured";
  return { items: r.data?.contents ?? [], summary: r.data?.summary, loading: !!key && (r.loading || !r.loaded), error: unavailable ? undefined : r.error, unavailable, reload: r.reload };
}

/** Contents like one. */
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

export interface UseHistory extends UseContents {
  /** Erases the reader's history: of one content item, one kind, or (no argument) everything. */
  forget: (o?: { kind?: string; content_id?: string }) => Promise<void>;
}

/** The signed-in reader's history, most recent first; nothing signed out. */
export function useHistory(q: HistoryQuery | null = {}, o: { client?: ContentKitClient } = {}): UseHistory {
  const { client, store, scope, viewer } = useContentScope(o.client);
  const query = useDisplay(q);
  const guarded = useGuarded();
  const isCurrent = useCurrentDiscoveryScope(client, scope);
  const list = useList(store, !query || typeof viewer !== "string" ? null : keyOf("history", scope, query), { type: "history" }, async (cursor, signal) =>
    page(await client.discovery.history({ ...query, cursor: cursor as string | undefined, summary: cursor ? undefined : query?.summary }, signal)));
  const forget = useCallback((filter: { kind?: string; content_id?: string } = {}) => {
    if (!isCurrent() || typeof viewer !== "string") return Promise.resolve();
    return guarded(() => client.discovery.forgetHistory(filter));
  }, [client, viewer, guarded, isCurrent]);
  return { ...list, summary: list.metadata, forget };
}

/** Continue reading and watching: started, not finished, newest first, each card with its position; signed out, the browser's. */
export function useContinue(q: ContinueQuery | null = {}, o: { client?: ContentKitClient } = {}): UseContents {
  const { client, store, scope, viewer } = useContentScope(o.client);
  const query = useDisplay(q);
  const list = useList(store, !query || viewer === undefined ? null : keyOf("history", scope, "continue", query), { type: "history" }, async (cursor, signal) =>
    page(await client.discovery.continue({ ...query, cursor: cursor as string | undefined }, signal)));
  return { ...list, summary: undefined };
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

/** A favorite or reaction with its content's or channel's id. */
export type MyInteraction = Interaction & { id: string };

const interactions = (p: InteractionPage) => ({ items: p.items.map((x) => ({ ...x, id: x.content?.id ?? x.channel?.id ?? "" })), next: p.next_cursor ?? null, metadata: p.summary });

export interface UseInteractions extends UseList<MyInteraction, InteractionPage["summary"]> { summary?: InteractionPage["summary"] }

/** The signed-in reader's favorites, most recent first: content cards or channels. */
export function useMyFavorites(q: MineQuery = {}, o: { client?: ContentKitClient } = {}): UseInteractions {
  const { client, store, scope, viewer } = useContentScope(o.client);
  const query = useDisplay(q)!;
  const list = useList(store, viewer === null ? null : keyOf("favorites", scope, query), { type: "favorites" }, async (cursor, signal) =>
    interactions(await client.favorites.mine({ ...query, cursor: cursor as string | undefined, summary: cursor ? undefined : query.summary }, signal)),
  );
  return { ...list, summary: list.metadata };
}

export interface UseContentFilters {
  filters: ContentFilters | null;
  loading: boolean;
  error?: ContentKitError;
  saving: boolean;
  /** Atomically changes submitted entity choices; other choices are preserved. */
  save: (update: ContentFiltersUpdate) => Promise<ContentFilters>;
  setVisible: (entityId: string, visible: boolean) => Promise<ContentFilters>;
}

/** One current-domain control per entity, shared by all applies_to kinds. */
export function useContentFilters(o: { language?: string; client?: ContentKitClient } = {}): UseContentFilters {
  const { client, store, scope, viewer, language: provided } = useContentScope(o.client);
  const language = o.language ?? provided;
  const guest = viewer === null;
  const browser = client.preferences.browser;
  const local = useSyncExternalStore(browser.subscribe, browser.snapshot, browser.snapshot);
  const key = keyOf("content-filters", scope, language);
  const previous = useRef<{
    client: ContentKitClient;
    language?: string;
    guest: boolean;
    data?: ContentFilters;
  } | null>(null);
  const initial =
    guest && previous.current?.guest && previous.current.client === client && previous.current.language === language
      ? previous.current.data
      : undefined;
  const r = useResource<ContentFilters>(
    store,
    key,
    { type: "content-filters" },
    (signal) => client.discovery.contentFilters(language, signal),
    { initial },
  );
  previous.current = { client, language, guest, data: r.data };
  const applyGuest = useCallback(
    (source: ContentFilters, choices = local): ContentFilters => {
      const values = new Map(choices.filters.map((row) => [row.entity_id, row]));
      return {
        ...source,
        filters: source.filters.map((row) => {
          const choice = values.get(row.entity.id);
          return { ...row, visible: choice?.visible ?? row.default_visible, overridden: choice?.visible !== undefined };
        }),
      };
    },
    [local],
  );
  const displayed = useMemo(() => (r.data && guest ? applyGuest(r.data) : r.data), [r.data, guest, applyGuest]);
  const guarded = useGuarded();
  const isCurrent = useCurrentDiscoveryScope(client, scope);
  const [inflight, setInflight] = useState(0);
  const current = useRef(displayed);
  current.current = displayed;
  useEffect(() => {
    setInflight(0);
  }, [client, scope]);
  const save = useCallback(
    async (update: ContentFiltersUpdate) => {
      if (!isCurrent()) throw new ContentKitError("aborted", "The account changed.");
      if (guest) {
        const source = current.current;
        if (!source) throw new ContentKitError("invalid_request", "Content filters have not loaded.");
        const allowed = new Set(source.filters.map((row) => row.entity.id));
        const updates: import("../client/browser-reader-preferences.js").ReaderFilterChoice[] = [];
        const seen = new Set<string>();
        for (const row of update.filters ?? []) {
          if (!allowed.has(row.entity_id) || seen.has(row.entity_id))
            throw new ContentKitError("invalid_request", "This content filter is not configured or is set twice.");
          seen.add(row.entity_id);
          if (row.default === true && row.visible === undefined)
            updates.push({ entity_id: row.entity_id, default: true });
          else if (row.default === undefined && typeof row.visible === "boolean")
            updates.push({ entity_id: row.entity_id, visible: row.visible });
          else throw new ContentKitError("invalid_request", "A content filter needs visible or default:true.");
        }
        client.preferences.setGuestFilters(updates);
        return applyGuest(source, browser.snapshot());
      }
      const submitted = new Set((update.filters ?? []).map((row) => row.entity_id));
      setInflight((n) => n + 1);
      try {
        const out = await guarded(() => {
          if (!isCurrent()) throw new ContentKitError("aborted", "The account changed.");
          return client.discovery.setContentFilters(update, language);
        });
        if (isCurrent()) {
          const rows = new Map(out.filters.map((row) => [row.entity.id, row]));
          // Another control may already have applied a newer response. Merge
          // only this request's choices, never its stale unrelated snapshot.
          store.set<ContentFilters>(key, (previous) => previous ? {
            ...previous,
            filters: previous.filters.flatMap((row) => {
              if (!submitted.has(row.entity.id)) return [row];
              const next = rows.get(row.entity.id);
              return next ? [next] : [];
            }),
          } : out);
        }
        return out;
      } finally {
        setInflight((n) => Math.max(0, n - 1));
      }
    },
    [client, guarded, language, key, store, isCurrent, guest, browser, applyGuest],
  );
  const setVisible = useCallback(
    (entityId: string, visible: boolean) => save({ filters: [{ entity_id: entityId, visible }] }),
    [save],
  );
  return {
    filters: displayed ?? null,
    loading: r.loading || !r.loaded,
    error: r.error,
    saving: inflight > 0,
    save,
    setVisible,
  };
}

export interface UseTypeahead {
  contents: Card[];
  entities: EntityCard[];
  /** The query the suggestions answer (trimmed). */
  query: string;
  loading: boolean;
  error?: ContentKitError;
}

const NONE: Pick<UseTypeahead, "contents" | "entities"> = { contents: [], entities: [] };

/** Suggestions while the reader types: debounced, the stale ones abandoned. */
export function useTypeahead(
  text: string,
  o: Omit<TypeaheadQuery, "q"> & { debounce?: number; minLength?: number; client?: ContentKitClient; management?: boolean; request?: (query: TypeaheadQuery, signal: AbortSignal) => Promise<Typeahead> } = {},
): UseTypeahead {
  const { client, language, scope: viewerScope } = useContentScope(o.client);
  const { debounce = 150, minLength = 1, client: _own, management = false, request, ...rest } = o;
  const scope = useMemo(() => ({ viewerScope, management, request }), [viewerScope, management, request]);
  const q = text.trim();
  const query = useMemo(() => ({ display_language: language, ...rest, q }), [language, q, JSON.stringify(rest)]); // eslint-disable-line react-hooks/exhaustive-deps
  const [state, setState] = useState<UseTypeahead & { scope: typeof scope }>({ ...NONE, query: "", loading: false, scope });
  useEffect(() => {
    if (q.length < minLength) {
      setState({ ...NONE, query: "", loading: false, scope });
      return;
    }
    setState((s) => ({ ...(s.scope === scope ? s : { ...NONE, query: "" }), loading: true, scope }));
    const ctl = new AbortController();
    const timer = setTimeout(() => {
      const pending = request ? request(query, ctl.signal) : management
        ? client.entities.manage({ q, kind: query.entity_kinds?.[0], language: query.display_language, limit: query.limit ?? 10 }, ctl.signal)
          .then((page) => ({ contents: [], entities: page.entities }))
        : client.discovery.typeahead(query, ctl.signal);
      pending.then(
        (r) => { if (!ctl.signal.aborted) setState({ contents: r.contents, entities: r.entities, query: q, loading: false, scope }); },
        (e) => {
          const error = toContentKitError(e);
          if (!ctl.signal.aborted && error.code !== "aborted") setState({ ...NONE, query: q, loading: false, error, scope });
        },
      );
    }, debounce);
    return () => {
      clearTimeout(timer);
      ctl.abort();
    };
  }, [client, query, q, debounce, minLength, management, request, scope]);
  return state.scope === scope ? state : { ...NONE, query: "", loading: q.length >= minLength };
}

const reporters = new WeakMap<ContentKitClient, ActivityReporter>();

/** The client's activity reporter: one per client. */
export function reporterFor(client: ContentKitClient, o?: ActivityReporterOptions): ActivityReporter {
  let r = reporters.get(client);
  if (!r) reporters.set(client, (r = new ActivityReporter(client, o)));
  return r;
}

export function useActivityReporter(o: { client?: ContentKitClient } = {}): ActivityReporter {
  const { client } = useContentScope(o.client);
  return reporterFor(client);
}

/** Reports an open of a content item, from a list (its request_id, surface and 1-based position) or from outside. */
export function useOpen(o: { client?: ContentKitClient } = {}): (contentId: string, source?: OpenSource) => void {
  const reporter = useActivityReporter(o);
  return useCallback((contentId: string, source?: OpenSource) => reporter.open(contentId, source), [reporter]);
}

export interface UseHide {
  hidden: boolean;
  pending: boolean;
  error?: ContentKitError;
  /** Hides the item from every list the reader is served. */
  hide: () => Promise<void>;
  /** Shows it again. */
  unhide: () => Promise<void>;
  /** Reverts the last hide or unhide. */
  undo: () => Promise<void>;
}

/** "Not interested" on a card: optimistic, and reverted if the server refuses. hidden is the card's viewer.hidden. */
export function useHide(contentId: string | null | undefined, o: { client?: ContentKitClient; hidden?: boolean } = {}): UseHide {
  const { client } = useContentScope(o.client);
  const guarded = useGuarded();
  const [state, setState] = useState<{ id?: string | null; hidden?: boolean; previous?: boolean; pending: boolean; error?: ContentKitError }>({ pending: false });
  const current = state.id === contentId && state.hidden !== undefined ? state.hidden : !!o.hidden;
  const set = useCallback(async (hidden: boolean, previous: boolean) => {
    if (!contentId) return;
    setState({ id: contentId, hidden, previous, pending: true });
    try {
      await guarded(() => (hidden ? client.discovery.hide(contentId) : client.discovery.unhide(contentId)));
      setState((s) => (s.id === contentId ? { ...s, pending: false } : s));
    } catch (e) {
      setState({ id: contentId, hidden: previous, pending: false, error: toContentKitError(e) });
    }
  }, [client, contentId, guarded]);
  const hide = useCallback(() => set(true, current), [set, current]);
  const unhide = useCallback(() => set(false, current), [set, current]);
  const undo = useCallback(() => (state.id === contentId && state.previous !== undefined ? set(state.previous, current) : Promise.resolve()), [set, state, contentId, current]);
  return { hidden: current, pending: state.id === contentId && state.pending, error: state.id === contentId ? state.error : undefined, hide, unhide, undo };
}
