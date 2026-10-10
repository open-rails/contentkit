import { useEffect, useMemo, useRef, type ReactNode } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { ContentKitChange } from "../client/http.js";
import type { ContentURLs } from "../urls/index.js";
import { ContentKitContext, type ContentKitErrorHandler, type Navigate } from "./context.js";
import { storeFor } from "./store.js";

export interface ContentKitProviderProps {
  client: ContentKitClient;
  /** Content URL helpers (createContentURLs from `/urls`). */
  urls?: ContentURLs;
  /** Host router for in-app navigation (a canonical replace). */
  navigate?: Navigate;
  /** Fires after each successful mutation: the host's cache-invalidation hook. */
  onChange?: (change: ContentKitChange) => void;
  /** Receives every failure the components show (a component's own `onError` wins). */
  onError?: ContentKitErrorHandler;
  children?: ReactNode;
}

/** Client, URL config, callbacks and the shared read store for every hook below. Renders no DOM. */
export function ContentKitProvider({ client, urls, navigate, onChange, onError, children }: ContentKitProviderProps) {
  const changed = useRef(onChange);
  useEffect(() => {
    changed.current = onChange;
  });
  useEffect(() => client.subscribe((c) => changed.current?.(c)), [client]);
  const value = useMemo(() => ({ client, urls, navigate, onError, store: storeFor(client) }), [client, urls, navigate, onError]);
  return <ContentKitContext.Provider value={value}>{children}</ContentKitContext.Provider>;
}
