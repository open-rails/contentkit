import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { ContentKitError } from "../client/errors.js";
import type { ReadResult, RefBody } from "../client/generated/wire.js";
import type { ReadOptions } from "../client/media/api.js";
import { chunkOffsets, mergeReads, processing as isProcessing, READ_CHUNK } from "../client/media/windows.js";
import { itemRef } from "../client/media/ref.js";
import { useOptionalContentKitClient, useReadScope } from "./context.js";
import { readKey, storeFor, type ReadEntry } from "./store.js";

/** Files [start, end) get URLs. */
export interface ReadWindow {
  start: number;
  end: number;
  /** Files per read. Default 50. */
  chunk?: number;
}

export interface MediaReadOptions extends Omit<ReadOptions, "offset" | "limit"> {
  /**
   * Read in windows of `chunk` files and merge them: the files in
   * [start, end) get URLs. Only the requested windows stay active and refresh;
   * a prior read supplies placeholder metadata while the new range loads.
   * Without it, one read of the server's
   * default range (or offset and limit).
   */
  window?: ReadWindow;
  offset?: number;
  limit?: number;
  /** Poll at this interval (ms) while uploads process. Explicit opt-in; editor reads otherwise observe SSE. */
  poll?: number | false;
  /** Read again shortly before the URLs expire. Default true. */
  refresh?: boolean;
  /** A read the host already has: shown until reload() or set(). */
  read?: ReadResult | null;
  /** Overrides the provider's client. */
  client?: ContentKitClient | null;
}

export interface UseMediaRead {
  /** The read (windows merged); null until it loads or when it failed. */
  read: ReadResult | null;
  loading: boolean;
  error?: ContentKitError;
  /** Uploads are still processing (editor reads). */
  processing: boolean;
  /** Reads again now, restarting a read in flight. */
  reload: () => void;
  /** Reads again unless a read is in flight: a player's grant refresh. */
  refresh: () => void;
  /** Replaces the read without refetching, for every hook showing it. */
  set: (r: ReadResult) => void;
}

const NONE: readonly ReadEntry[] = [];
const MAX_DELAY = 2 ** 31 - 1;

/** Expiry still advances while a renewal is pending, disabled or unsuccessful. */
export function useMediaGrantExpired(expires?: number): boolean {
  const [expiredAt, setExpiredAt] = useState<number | null>(null);
  useEffect(() => {
    if (!expires) return;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const expire = () => {
      const remaining = expires * 1000 - Date.now();
      if (remaining <= 0) setExpiredAt(expires);
      else timer = setTimeout(expire, Math.min(MAX_DELAY, remaining));
    };
    expire();
    return () => clearTimeout(timer);
  }, [expires]);
  return !!expires && (expiredAt === expires || expires * 1000 <= Date.now());
}

/**
 * A read of the item, shared with every hook reading the same item and
 * options: refreshed before its URLs expire, retried after a read rate limit,
 * updated by SSE while uploads process, reloaded after a commit through the client.
 * A window splits it into merged reads. Without a client (none passed, no
 * provider) it only shows a supplied read.
 */
export function useMediaRead(ref: RefBody, o: MediaReadOptions = {}): UseMediaRead {
  const { read: given, client: own, prefix, paths, offset, limit, download, editor, status, window, refresh: refreshing = true } = o;
  const poll = o.poll;
  const client = useOptionalContentKitClient(own);
  const scope = useReadScope();
  const store = client ? storeFor(client) : null;
  const chunk = window?.chunk ?? READ_CHUNK;
  const selection = JSON.stringify([...new Set(paths ?? [])].sort());
  const retainPrevious = !!window || paths !== undefined;
  const base = scope + readKey(ref, { prefix, download, editor, status }) + (paths !== undefined ? "#paths" : window ? `#${chunk}` : `#${offset ?? 0}:${limit ?? 0}`);

  // Keep one placeholder, never subscriptions for the reader's visit history.
  const [previous, setPrevious] = useState<{ base: string; store: typeof store; read: ReadResult | null; n?: number }>({ base, store, read: null });
  const same = previous.base === base && previous.store === store;
  if (!same) setPrevious({ base, store, read: null });
  const known = same ? previous.n : undefined;
  // Until the first read states the total, only the window's first chunk.
  const wanted = window ? chunkOffsets(window.start, window.end, known ?? window.start + 1, chunk) : [];
  const offsets = window ? (wanted.length ? wanted : [0]) : [offset ?? 0];
  const windows = useMemo<ReadOptions[]>(
    () => {
      if (paths !== undefined) {
        const selected = [...new Set(paths)].sort();
        return Array.from({ length: Math.ceil(selected.length / 64) }, (_, i) => ({ paths: selected.slice(i * 64, (i + 1) * 64) }));
      }
      return offsets.map((off) => ({ offset: off, limit: window ? chunk : limit }));
    },
    // Window identity depends on its values, not the caller's array identity.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [offsets.join(","), window ? chunk : limit, selection, paths === undefined],
  );
  const keys = useMemo(() => windows.map((w) => scope + readKey(ref, { prefix, download, editor, status, ...w })), [scope, windows, ref.kind, ref.id, ref.version, prefix, download, editor, status]); // eslint-disable-line react-hooks/exhaustive-deps

  // A supplied read is shown until reload() asks the server or set() replaces it.
  const [requested, setRequested] = useState<{ base: string; given?: ReadResult | null } | null>(null);
  const [local, setLocal] = useState<{ base: string; given?: ReadResult | null; read: ReadResult } | null>(null);
  const asked = requested?.base === base && requested.given === given ? requested : null;
  const fetching = !!store && (given === undefined || !!asked);

  const subscribe = useCallback(
    (l: () => void) => {
      if (!store || !fetching) return () => {};
      const offs = keys.map((key, i) => store.subscribe(key, itemRef(ref), { prefix, download, editor, status, ...windows[i] }, l, { poll, refresh: refreshing }, scope));
      return () => offs.forEach((off) => off());
    },
    // ref and options are in keys.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [store, fetching, keys, poll, refreshing],
  );
  const last = useRef<readonly ReadEntry[]>(NONE);
  const snapshot = useCallback(() => {
    if (!store || !fetching) return NONE;
    const next = keys.map((k) => store.snapshot(k));
    const prev = last.current;
    if (prev.length === next.length && prev.every((e, i) => e === next[i])) return prev;
    return (last.current = next);
  }, [store, fetching, keys]);
  const entries = useSyncExternalStore(subscribe, snapshot, snapshot);
  useEffect(() => {
    if (store && fetching) for (const k of keys) store.load(k, asked ? "reload" : "initial");
  }, [store, fetching, keys, asked]);

  const merged = useMemo(() => mergeReads(entries.map((e) => e.read)), [entries]);
  const failed = entries.some((e) => !!e.error);
  useEffect(() => {
    const read = failed ? null : merged;
    if (retainPrevious && (failed || merged || (entries.length && entries.every((e) => e.loaded)))) {
      setPrevious((prev) => prev.base === base && prev.store === store && prev.read === read ? prev : {
        base, store, read, n: merged?.total ?? (prev.base === base && prev.store === store ? prev.n : undefined),
      });
    }
  }, [retainPrevious, merged, failed, entries, base, store]);

  const reload = useCallback(() => {
    if (given !== undefined) setRequested({ base, given });
    else if (store) for (const k of keys) store.load(k, "reload");
    setLocal(null);
  }, [store, keys, base, given]);
  const refresh = useCallback(() => {
    if (given !== undefined && !asked) setRequested({ base, given });
    else if (store) for (const k of keys) store.load(k, "refresh");
  }, [store, keys, base, given, asked]);
  const set = useCallback(
    (read: ReadResult) => {
      if (store && fetching) for (const k of keys) store.set(k, read);
      else setLocal({ base, given, read });
    },
    [store, fetching, keys, base, given],
  );

  let read: ReadResult | null;
  let loading = false;
  let error: ContentKitError | undefined;
  if (local && local.base === base && local.given === given) read = local.read;
  else if (!entries.length) read = given ?? null;
  else {
    const loaded = entries.every((e) => e.loaded);
    read = merged ?? (loaded ? null : (given ?? (retainPrevious && same && !failed ? previous.read : null)));
    loading = entries.some((e) => e.loading || !e.loaded);
    error = entries.find((e) => e.error)?.error;
  }
  const expired = useMediaGrantExpired(read?.expires);
  const visible = useMemo(() => expired && read ? {
    ...read,
    files: read.files.map((file) => ({ ...file, url: undefined, editor_url: undefined })),
  } : read, [read, expired]);
  return { read: visible, loading, error, processing: isProcessing(visible), reload, refresh, set };
}
