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
 * pass read to use one the host already has. reload() fetches a replacement
 * even with a supplied read; a changed supplied value takes precedence again.
 */
export function useRead(client: UploadClient | null | undefined, ref: RefBody, o: ReadOptions & { read?: ReadResult | null } = {}): UseRead {
  const { read: given, prefix, offset, limit, download, editor } = o;
  const key = JSON.stringify([ref.kind, ref.id, prefix, offset, limit, download, editor]);
  const [state, setState] = useState<{ key: string; given?: ReadResult | null; read: ReadResult | null; loading: boolean; error?: UploadError }>({
    key,
    read: null,
    loading: given === undefined && !!client,
  });
  const [request, setRequest] = useState<{ key: string; given?: ReadResult | null } | null>(null);
  const opts = useRef({ ref, prefix, offset, limit, download, editor });
  opts.current = { ref, prefix, offset, limit, download, editor };
  useEffect(() => {
    if (!client || given !== undefined && (request?.key !== key || request.given !== given)) return;
    const ctl = new AbortController();
    setState((s) => ({ key, given, read: s.key === key && s.given === given ? s.read : given ?? null, loading: true }));
    const { ref, ...q } = opts.current;
    client.read(ref, { ...q, signal: ctl.signal }).then(
      (read) => !ctl.signal.aborted && setState({ key, given, read, loading: false }),
      (e) => !ctl.signal.aborted && setState({ key, given, read: null, loading: false, error: e instanceof UploadError ? e : new UploadError("network", String(e)) }),
    );
    return () => ctl.abort();
  }, [client, key, given, request]);
  const local = state.key === key && state.given === given;
  return {
    read: local ? state.read : given ?? null,
    loading: local && state.loading,
    error: local ? state.error : undefined,
    reload: useCallback(() => setRequest({ key, given }), [key, given]),
    set: useCallback((read: ReadResult) => setState({ key, given, read, loading: false }), [key, given]),
  };
}
