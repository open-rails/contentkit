import { useCallback, useEffect, useState, useSyncExternalStore } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { ContentKitError } from "../client/errors.js";
import type { ReadResult, RefBody } from "../client/generated/wire.js";
import type { ReadOptions } from "../client/media/api.js";
import { useOptionalContentKitClient } from "./context.js";
import { readKey, storeFor } from "./store.js";

export interface UseRead {
  read: ReadResult | null;
  loading: boolean;
  error?: ContentKitError;
  reload: () => void;
  /** Replaces the read without refetching (after a commit), for every hook showing it. */
  set: (r: ReadResult) => void;
}

export interface UseReadOptions extends ReadOptions {
  /** A read the host already has: shown until reload() or set(). */
  read?: ReadResult | null;
  /** Overrides the provider's client. */
  client?: ContentKitClient | null;
}

const noop = () => () => {};

/**
 * A read of the item (media.read), shared with every hook reading the same
 * item and options, refetched when they change. Without a client (none
 * passed, no provider) it only shows a supplied read.
 */
export function useRead(ref: RefBody, o: UseReadOptions = {}): UseRead {
  const { read: given, client: own, prefix, offset, limit, download, editor } = o;
  const client = useOptionalContentKitClient(own);
  const store = client ? storeFor(client) : null;
  const key = readKey(ref, { prefix, offset, limit, download, editor });
  // A supplied read is shown until reload() asks the server or set() replaces it.
  const [requested, setRequested] = useState<{ key: string; given?: ReadResult | null } | null>(null);
  const [local, setLocal] = useState<{ key: string; given?: ReadResult | null; read: ReadResult } | null>(null);
  const asked = requested?.key === key && requested.given === given ? requested : null;
  const fetching = !!store && (given === undefined || !!asked);
  const subscribe = useCallback(
    (l: () => void) => (store && fetching ? store.subscribe(key, { kind: ref.kind, id: ref.id }, { prefix, offset, limit, download, editor }, l) : noop()),
    // ref and options are in key.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [store, fetching, key],
  );
  const snapshot = useCallback(() => (store && fetching ? store.snapshot(key) : null), [store, fetching, key]);
  const entry = useSyncExternalStore(subscribe, snapshot, snapshot);
  useEffect(() => {
    if (store && fetching) store.load(key, !!asked);
  }, [store, fetching, key, asked]);

  const reload = useCallback(() => {
    if (given !== undefined) setRequested({ key, given });
    else store?.load(key, true);
    setLocal(null);
  }, [store, key, given]);
  const set = useCallback(
    (read: ReadResult) => {
      if (store && fetching) store.set(key, read);
      else setLocal({ key, given, read });
    },
    [store, fetching, key, given],
  );

  if (local && local.key === key && local.given === given) return { read: local.read, loading: false, reload, set };
  if (!entry) return { read: given ?? null, loading: false, reload, set };
  return { read: entry.read ?? (entry.loaded ? null : (given ?? null)), loading: entry.loading || !entry.loaded, error: entry.error, reload, set };
}
