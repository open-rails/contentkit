import { useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { Beacons, eventId, type Attribution, type BeaconOptions, type ViewSession } from "../client/beacons.js";
import type { ContentKitClient } from "../client/client.js";
import type { MineQuery } from "../client/content/favorites.js";
import type { ContentQuery, DailyReleasesQuery, HistoryQuery, RailQuery, ReleaseDatesQuery, SearchQuery, TypeaheadQuery } from "../client/discovery.js";
import { ContentKitError, toContentKitError } from "../client/errors.js";
import type { Card, ContentFilters, ContentFiltersUpdate, ContentPage, DailyReleases, DateCount, GroupCard, Interaction, InteractionPage, RecentSearch, Typeahead } from "../client/generated/wire.js";
import { ContentKitContext, useGuarded } from "./context.js";
import { keyOf, useContentScope, useList, useResource, type UseList } from "./use-resource.js";
import { guestStanding, type GuestReadingItem } from "../client/guest-reading-history.js";
import { recentSearchLimit } from "../client/browser-search-history.js";

const page = (p: ContentPage) => ({ items: p.contents, next: p.next_cursor ?? null, metadata: p.summary });
export interface UseContents extends UseList<Card, ContentPage["summary"]> { summary?: ContentPage["summary"] }
const NO_SEARCHES: RecentSearch[] = [];
const NO_READING: readonly GuestReadingItem[] = Object.freeze([]);

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
  const list = useList(store, query ? keyOf("cards", scope, "list", query) : null, { type: "cards" }, async (cursor, signal) =>
    page(await client.discovery.list({ ...query, cursor: cursor as string | undefined, summary: cursor ? undefined : query?.summary }, signal)),
  );
  return { ...list, summary: list.metadata };
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

/** Keyword search results as cards; nothing while q.q is empty. */
export function useSearch(q: SearchQuery | null, o: { client?: ContentKitClient } = {}): UseContents {
  const { client, store, scope, viewer } = useContentScope(o.client);
  const query = useDisplay(q?.q.trim() ? q : null);
  const isCurrent = useCurrentDiscoveryScope(client, scope);
  const list = useList(store, query ? keyOf("cards", scope, "search", query) : null, { type: "cards" }, async (cursor, signal) => {
    const out = await client.discovery.search({ ...query!, cursor: cursor as string | undefined, summary: cursor ? undefined : query?.summary }, signal);
    if (!cursor && viewer === null && !signal.aborted && isCurrent()) client.discovery.browserSearches.record(query!.q, out.contents.length);
    return page(out);
  });
  return { ...list, summary: list.metadata };
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

/** Account history is authoritative; signed-out reading stays in this browser across tabs. */
export function useHistory(q: HistoryQuery | null = {}, o: { client?: ContentKitClient } = {}): UseHistory {
  const { client, store, scope, viewer } = useContentScope(o.client);
  const query = useDisplay(q);
  const guarded = useGuarded();
  const guest = viewer === null;
  const isCurrent = useCurrentDiscoveryScope(client, scope);
  const browser = client.guestHistory;
  const enabled = query !== null;
  const subscribe = useCallback((listener: () => void) => guest && enabled ? browser.subscribe(listener) : () => {}, [browser, guest, enabled]);
  const snapshot = useCallback(() => guest && enabled ? browser.snapshot() : NO_READING, [browser, guest, enabled]);
  const local = useSyncExternalStore(subscribe, snapshot, () => NO_READING);
  const selected = useMemo(() => local.filter(row => !query?.kind || row.kind === query.kind), [local, query?.kind]);
  const localRevision = guest ? selected.map(row => [row.id, row.at, row.checkpoints.map(checkpoint => [checkpoint.visit_id, checkpoint.segment_id, checkpoint.revision])]) : undefined;
  const list = useList(store, !query || viewer === undefined ? null : keyOf("history", scope, query, localRevision), { type: "history" }, async (cursor, signal) => {
    if (!guest) return page(await client.discovery.history({ ...query, cursor: cursor as string | undefined }, signal));
    const offset = typeof cursor === "number" ? cursor : 0;
    const limit = Math.max(1, Math.min(100, query?.limit ?? 50));
    const batch = selected.slice(offset, offset + limit);
    if (!batch.length) return { items: [], next: null };
    // Public hydration enforces current hard-domain rules, access and optional preferences.
    const out = await client.discovery.list({ ids: batch.map(row => row.id), kind: query?.kind, language: query?.language,
      display_language: query?.display_language, mode: query?.mode, card_entities: query?.card_entities, limit: 100 }, signal);
    const cards = new Map(out.contents.map(card => [card.id, card]));
    const items = batch.flatMap(row => {
      const card = cards.get(row.id); if (!card) return [];
      const viewer = guestStanding(card, row);
      if (query?.status === "seen" && !viewer.seen || query?.status === "in_progress" && (!viewer.seen || viewer.completed) || query?.status === "completed" && !viewer.completed) return [];
      return [{ ...card, viewer }];
    });
    return { items, next: offset + batch.length < selected.length ? offset + batch.length : null };
  });
  const forget = useCallback((filter: { kind?: string; content_id?: string } = {}) => {
    if (!isCurrent()) return Promise.resolve();
    if (guest) { browser.clear(filter); return Promise.resolve(); }
    return guarded(() => client.discovery.forgetHistory(filter));
  }, [browser, client, guest, guarded, isCurrent]);
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
  filters: ContentFilters | null;
  loading: boolean;
  error?: ContentKitError;
  saving: boolean;
  /** Atomically changes submitted entity choices; other choices are preserved. */
  save: (update: ContentFiltersUpdate) => Promise<ContentFilters>;
  setVisible: (entityId: string, visible: boolean) => Promise<ContentFilters>;
  setIncludeDeleted: (include: boolean) => Promise<ContentFilters>;
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
        include_deleted: false,
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
      const changesDeleted = typeof update.include_deleted === "boolean";
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
            include_deleted: changesDeleted ? out.include_deleted : previous.include_deleted,
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
  const setIncludeDeleted = useCallback((include: boolean) => save({ include_deleted: include }), [save]);
  return {
    filters: displayed ?? null,
    loading: r.loading || !r.loaded,
    error: r.error,
    saving: inflight > 0,
    save,
    setVisible,
    setIncludeDeleted,
  };
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
  const { client, scope, viewer, language } = useContentScope(o.client);
  const b = beaconsFor(client);
  const isCurrent = useCurrentDiscoveryScope(client, scope);
  // Retire the mutable session whenever its owner or read scope changes.
  const holder = useMemo(() => ({ session: undefined as ViewSession | undefined }), [b, content, version, scope]); // eslint-disable-line react-hooks/exhaustive-deps
  return useCallback((u) => {
    if (!content || !isCurrent() || (viewer !== undefined && client.preferences.identity() !== viewer)) return;
    // Create after the committed provider has configured authentication, not
    // during render while the transport can still belong to the old account.
    holder.session ??= b.view(content, version, { render_id: o.render_id, surface: o.surface, position: o.position, language: o.language ?? language });
    holder.session.update(u);
  }, [b, client, content, version, viewer, language, o.language, o.render_id, o.surface, o.position, holder, isCurrent]);
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
  /** The first bounded segment; click() selects the matching segment for later positions. */
  render_id: string;
  /** A card's ref: reports it shown once at least half of it is in view. */
  track: (content: string, position: number) => (el: Element | null) => void;
  /** Reports a click on the card at position. */
  click: (content: string, position: number) => void;
}

/** A rendered result list: each page rendered, each card shown, clicks attributed to the render. */
export function useExposure(items: readonly { id: string; position?: number; version_id?: string; version?: { id: string } | null }[], o: ExposureOptions): UseExposure {
  const { client, scope, language } = useContentScope(o.client);
  const b = beaconsFor(client);
  const isCurrent = useCurrentDiscoveryScope(client, scope);
  const renderId = useMemo(() => eventId(), [o.render, scope]); // eslint-disable-line react-hooks/exhaustive-deps
  const base = useMemo(() => ({ surface: o.surface, ranker: o.ranker, query_id: o.query_id, language: o.language ?? language }), [o.surface, o.ranker, o.query_id, o.language, language]);
  const state = useMemo(() => ({ segments: new Map<number, ExposureSegment>(), pending: new Set<number>(), timer: undefined as ReturnType<typeof setTimeout> | undefined }), [renderId]); // eslint-disable-line react-hooks/exhaustive-deps
  const segmentID = useCallback((index: number) => index === 0 ? renderId : `${renderId}-${index}`, [renderId]);
  const segmentFor = useCallback((index: number) => {
    let segment = state.segments.get(index);
    if (!segment) {
      segment = { rendered: new Map(), visible: new Map(), renderedRevision: 0, visibleRevision: 0 };
      state.segments.set(index, segment);
    }
    return segment;
  }, [state]);
  useEffect(() => {
    if (!isCurrent()) return;
    const changed = new Set<number>();
    for (const [i, item] of items.entries()) {
      const supplied = item.position;
      const position = supplied !== undefined && Number.isSafeInteger(supplied) && supplied > 0 ? supplied : i + 1;
      const index = Math.floor((position - 1) / EXPOSURE_SEGMENT_SIZE), segment = segmentFor(index);
      if (segment.rendered.has(item.id)) continue;
      segment.rendered.set(item.id, { content_id: item.id, version_id: item.version_id ?? item.version?.id, position });
      changed.add(index);
    }
    for (const index of changed) {
      const segment = segmentFor(index);
      b.exposure({ ...base, render_id: segmentID(index), stage: "rendered", revision: ++segment.renderedRevision,
        occurred_at: segment.renderedAt ??= new Date().toISOString(), shown: [...segment.rendered.values()] });
    }
  }, [b, base, items, state, segmentFor, segmentID, isCurrent]);
  const observer = useMemo(() => {
    if (typeof IntersectionObserver !== "function") return null;
    return new IntersectionObserver((entries) => {
      if (!isCurrent()) return;
      for (const e of entries) {
        if (!e.isIntersecting) continue;
        const el = e.target as HTMLElement, content = el.dataset.exposureContent!;
        const position = Number(el.dataset.exposurePosition), index = Math.floor((position - 1) / EXPOSURE_SEGMENT_SIZE);
        const segment = segmentFor(index);
        if (segment.visible.has(content)) continue;
        segment.visible.set(content, segment.rendered.get(content) ?? { content_id: content, position });
        state.pending.add(index);
      }
      if (!state.pending.size) return;
      state.timer ??= setTimeout(() => {
        state.timer = undefined;
        if (!isCurrent()) return;
        for (const index of state.pending) {
          const segment = segmentFor(index);
          b.exposure({ ...base, render_id: segmentID(index), stage: "visible", revision: ++segment.visibleRevision,
            occurred_at: segment.visibleAt ??= new Date().toISOString(), shown: [...segment.visible.values()] });
        }
        state.pending.clear();
      }, 300);
    }, { threshold: 0.5 });
  }, [b, base, state, segmentFor, segmentID, isCurrent]);
  useEffect(() => () => { observer?.disconnect(); clearTimeout(state.timer); }, [observer, state]);
  useEffect(() => {
    for (const index of state.segments.keys()) b.beginExposure(segmentID(index));
    return () => { for (const index of state.segments.keys()) b.endExposure(segmentID(index)); };
  }, [b, state, segmentID]);
  const track = useCallback((content: string, position: number) => (el: Element | null) => {
    if (!el || !observer) return;
    (el as HTMLElement).dataset.exposureContent = content;
    (el as HTMLElement).dataset.exposurePosition = String(position);
    observer.observe(el);
  }, [observer]);
  const click = useCallback((content: string, position: number) => {
    if (!isCurrent()) return;
    const index = Math.floor((position - 1) / EXPOSURE_SEGMENT_SIZE);
    const item = items.find((item) => item.id === content);
    const version = item?.version_id ?? item?.version?.id;
    b.click(content, { render_id: segmentID(index), surface: o.surface, position, version_id: version, language: base.language });
  }, [b, items, segmentID, o.surface, base.language, isCurrent]);
  return { render_id: renderId, track, click };
}

type ObservedPlacement = { content_id: string; version_id?: string; position: number };
type ExposureSegment = {
  rendered: Map<string, ObservedPlacement>;
  visible: Map<string, ObservedPlacement>;
  renderedRevision: number;
  visibleRevision: number;
  renderedAt?: string;
  visibleAt?: string;
};
const EXPOSURE_SEGMENT_SIZE = 32;
