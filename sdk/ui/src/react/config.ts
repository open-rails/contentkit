import type { ContentKitClient } from "../client/client.js";
import type { ContentKitError } from "../client/errors.js";
import type { Config } from "../client/generated/wire.js";
import { keyOf, useContentScope, useResource } from "./use-resource.js";

export interface UseContentConfig {
  /** null until it loads (or when it fails). */
  config: Config | null;
  loading: boolean;
  error?: ContentKitError;
}

/** What the content module allows (client.config()): one read per client, whoever the viewer is. */
export function useContentConfig(o: { client?: ContentKitClient } = {}): UseContentConfig {
  const { client, store } = useContentScope(o.client);
  const r = useResource<Config>(store, keyOf("config"), { type: "config" }, (s) => client.config(s));
  return { config: r.data ?? null, loading: r.loading || !r.loaded, error: r.error };
}
