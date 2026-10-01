import { useCallback, useEffect, useRef, useState } from "react";
import type { ReadOptions } from "./api.js";
import type { UploadClient } from "./client.js";
import { UploadError } from "./errors.js";
import type { ReadResult, RefBody } from "./wire.gen.js";

export interface UseRead {
  read: ReadResult | null;
  loading: boolean;
  error?: UploadError;
  reload: () => void;
  /** Replaces the read without refetching (after a commit). */
  set: (r: ReadResult) => void;
}

/**
 * A read of the item (client.read), refetched when ref or options change;
 * pass read to use one the host already has.
 */
export function useRead(client: UploadClient | null | undefined, ref: RefBody, o: ReadOptions & { read?: ReadResult | null } = {}): UseRead {
  const { read: given, prefix, offset, limit, download, editor } = o;
  const key = JSON.stringify([ref.kind, ref.id, prefix, offset, limit, download, editor]);
  const [state, setState] = useState<{ key: string; read: ReadResult | null; loading: boolean; error?: UploadError }>({
    key,
    read: null,
    loading: given === undefined && !!client,
  });
  const [tick, setTick] = useState(0);
  const opts = useRef({ ref, prefix, offset, limit, download, editor });
  opts.current = { ref, prefix, offset, limit, download, editor };
  useEffect(() => {
    if (given !== undefined || !client) return;
    const ctl = new AbortController();
    setState((s) => ({ key, read: s.key === key ? s.read : null, loading: true }));
    const { ref, ...q } = opts.current;
    client.read(ref, { ...q, signal: ctl.signal }).then(
      (read) => setState({ key, read, loading: false }),
      (e) => !ctl.signal.aborted && setState({ key, read: null, loading: false, error: e instanceof UploadError ? e : new UploadError("network", String(e)) }),
    );
    return () => ctl.abort();
  }, [client, key, given, tick]);
  return {
    read: given !== undefined ? given : state.key === key ? state.read : null,
    loading: given === undefined && state.loading,
    error: given === undefined ? state.error : undefined,
    reload: useCallback(() => setTick((t) => t + 1), []),
    set: useCallback((read: ReadResult) => setState({ key, read, loading: false }), [key]),
  };
}
