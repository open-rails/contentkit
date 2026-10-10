import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { ContentKitError } from "../client/errors.js";
import type { ReadResult, RefBody } from "../client/generated/wire.js";
import type { ReadOptions } from "../client/media/api.js";
import { chunkOffsets, mergeReads, processing as isProcessing, READ_CHUNK } from "../client/media/windows.js";
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
   * [start, end) get URLs, and windows once read stay loaded (and refreshed)
   * until the item or options change. Without it, one read of the server's
   * default range (or offset and limit).
   */
  window?: ReadWindow;
  offset?: number;
  limit?: number;
  /** Poll at this interval (ms) while uploads process. Default 2500 for editor reads, else off. */
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

const POLL = 2500;
const NONE: readonly ReadEntry[] = [];

/**
 * A read of the item, shared with every hook reading the same item and
 * options: refreshed before its URLs expire, retried after a read rate limit,
 * polled while uploads process, reloaded after a commit through the client.
 * A window splits it into merged reads. Without a client (none passed, no
 * provider) it only shows a supplied read.
 */
export function useMediaRead(ref: RefBody, o: MediaReadOptions = {}): UseMediaRead {
  const { read: given, client: own, prefix, offset, limit, download, editor, window, refresh: refreshing = true } = o;
  const poll = o.poll ?? (editor ? POLL : false);
  const client = useOptionalContentKitClient(own);
  const scope = useReadScope();
  const store = client ? storeFor(client) : null;
  const chunk = window?.chunk ?? READ_CHUNK;
  const base = scope + readKey(ref, { prefix, download, editor }) + (window ? `#${chunk}` : `#${offset ?? 0}:${limit ?? 0}`);

  // Windows once wanted stay loaded until the item or options change.
  const [total, setTotal] = useState<{ base: string; n?: number }>({ base });
  const known = total.base === base ? total.n : undefined;
  // Until the first read states the total, only the window's first chunk.
  const wanted = window ? chunkOffsets(window.start, window.end, known ?? window.start + 1, chunk) : [];
  const [kept, setKept] = useState<{ base: string; offsets: number[] }>({ base, offsets: [] });
  const prior = kept.base === base ? kept.offsets : [];
  if (window && (kept.base !== base || wanted.some((w) => !prior.includes(w)))) {
    setKept({ base, offsets: [...new Set([...prior, ...wanted])].sort((a, b) => a - b) });
  }
  const offsets = window ? (prior.length ? prior : wanted.length ? wanted : [0]) : [offset ?? 0];
  const windows = useMemo(
    () => offsets.map((off) => ({ offset: off, limit: window ? chunk : limit })),
    // offsets is derived from the stable kept list
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [offsets.join(","), window ? chunk : limit],
  );
  const keys = useMemo(() => windows.map((w) => scope + readKey(ref, { prefix, download, editor, offset: w.offset, limit: w.limit })), [scope, windows, ref.kind, ref.id, prefix, download, editor]); // eslint-disable-line react-hooks/exhaustive-deps

  // A supplied read is shown until reload() asks the server or set() replaces it.
  const [requested, setRequested] = useState<{ base: string; given?: ReadResult | null } | null>(null);
  const [local, setLocal] = useState<{ base: string; given?: ReadResult | null; read: ReadResult } | null>(null);
  const asked = requested?.base === base && requested.given === given ? requested : null;
  const fetching = !!store && (given === undefined || !!asked);

  const subscribe = useCallback(
    (l: () => void) => {
      if (!store || !fetching) return () => {};
      const offs = keys.map((key, i) => store.subscribe(key, { kind: ref.kind, id: ref.id }, { prefix, download, editor, ...windows[i] }, l, { poll, refresh: refreshing }, scope));
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
  useEffect(() => {
    if (window && merged && merged.total !== known) setTotal({ base, n: merged.total });
  }, [window, merged, known, base]);

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
    read = merged ?? (loaded ? null : (given ?? null));
    loading = entries.some((e) => e.loading || !e.loaded);
    error = entries.find((e) => e.error)?.error;
  }
  return { read, loading, error, processing: isProcessing(read), reload, refresh, set };
}
