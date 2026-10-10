import { useCallback, useContext, useEffect, useRef, useSyncExternalStore } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { ContentKitError } from "../client/errors.js";
import { ContentKitContext, useContentKitClient, useReadScope } from "./context.js";
import { resourcesFor, type Fetcher, type Page, type Resource, type ResourceStore, type Tag } from "./resources.js";

/** The client, its shared store, and the provider's viewer and sign-in. */
export function useContentScope(own?: ContentKitClient | null) {
  const ctx = useContext(ContentKitContext);
  const client = useContentKitClient(own);
  return { client, store: resourcesFor(client), scope: useReadScope(), viewer: ctx?.viewer, onSignIn: ctx?.onSignIn };
}

const IDLE: Resource<never> = { data: undefined, loading: false, loaded: false };
const none = () => () => {};

/** A shared read: fetched once per key while shown; initial is shown (and seeds the store) until it loads. */
export function useResource<T>(
  store: ResourceStore,
  key: string | null,
  tag: Tag,
  fetcher: Fetcher<T>,
  o: { initial?: T; more?: (cursor: string | number, signal: AbortSignal) => Promise<Page<unknown>> } = {},
): Resource<T> & { reload: () => void } {
  const fetch = useRef(fetcher);
  const more = useRef(o.more);
  const initial = useRef(o.initial);
  useEffect(() => {
    fetch.current = fetcher;
    more.current = o.more;
    initial.current = o.initial;
  });
  const subscribe = useCallback(
    (l: () => void) => (key ? store.subscribe(key, tag, l) : none()),
    // The tag is part of the key.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [store, key],
  );
  const snapshot = useCallback(() => (key ? store.snapshot<T>(key) : IDLE), [store, key]);
  const entry = useSyncExternalStore(subscribe, snapshot, snapshot);
  useEffect(() => {
    if (!key) return;
    store.bind(key, (s) => fetch.current(s), more.current && ((c, s) => more.current!(c, s)));
    if (initial.current !== undefined && !store.snapshot(key).loaded && !store.snapshot(key).loading) store.set(key, initial.current);
    else store.load(key);
  }, [store, key]);
  const reload = useCallback(() => key && store.load(key, true), [store, key]);
  if (!entry.loaded && o.initial !== undefined) return { ...entry, data: o.initial, reload };
  return { ...entry, reload };
}

/** A shared list, a page at a time. */
export interface UseList<T> {
  items: T[];
  /** The first page is loading (also on reload). */
  loading: boolean;
  loadingMore: boolean;
  error?: ContentKitError;
  hasMore: boolean;
  loadMore: () => Promise<void>;
  reload: () => void;
}

export type PageFetcher<T> = (cursor: string | number | undefined, signal: AbortSignal) => Promise<Page<T>>;

export function useList<T extends { id: string }>(store: ResourceStore, key: string | null, tag: Tag, page: PageFetcher<T>): UseList<T> {
  const r = useResource<Page<T>>(store, key, tag, (s) => page(undefined, s), { more: (c, s) => page(c, s) });
  const loadMore = useCallback(() => (key ? store.loadMore(key) : Promise.resolve()), [store, key]);
  return {
    items: r.data?.items ?? [],
    loading: r.loading || (!!key && !r.loaded),
    loadingMore: !!r.loadingMore,
    error: r.error,
    hasMore: r.data ? r.data.next !== null : false,
    loadMore,
    reload: r.reload,
  };
}

/**
 * A limit/offset list as pages. A full page means more may follow; with
 * underfill (the latest feed drops items the caller may not see) only an
 * empty page ends it.
 */
export function offsetPages<T>(limit: number, list: (q: { limit: number; offset: number }, signal: AbortSignal) => Promise<T[]>, underfill = false): PageFetcher<T> {
  return async (cursor, signal) => {
    const offset = typeof cursor === "number" ? cursor : 0;
    const items = await list({ limit, offset }, signal);
    const end = underfill ? items.length === 0 : items.length < limit;
    return { items, next: end ? null : offset + limit };
  };
}

/** A stable store key. */
export const keyOf = (...parts: unknown[]) => JSON.stringify(parts);
