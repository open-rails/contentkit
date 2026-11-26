import type { ContentKitClient } from "../client/client.js";
import type { ContentKitError } from "../client/errors.js";
import type { ActiveDays, MyStats } from "../client/generated/wire.js";
import type { ActiveQuery } from "../client/stats.js";
import { keyOf, useContentScope, useResource } from "./use-resource.js";

export interface UseStats<T> {
  data: T | null;
  loading: boolean;
  error?: ContentKitError;
  /** The site runs without the signal plane (not_configured). */
  unavailable: boolean;
  reload: () => void;
}

function useStats<T>(key: string | null, fetch: (c: ContentKitClient, signal: AbortSignal) => Promise<T>, o: { client?: ContentKitClient }): UseStats<T> {
  const { client, store } = useContentScope(o.client);
  const r = useResource<T>(store, key, { type: "stats" }, (signal) => fetch(client, signal));
  const unavailable = r.error?.code === "not_configured";
  return { data: r.data ?? null, loading: !!key && (r.loading || !r.loaded), error: unavailable ? undefined : r.error, unavailable, reload: r.reload };
}

/** Staff: distinct people active on the site per day, by access; not period-unique visitors. Null reads nothing. */
export function useActive(q: ActiveQuery | null, o: { client?: ContentKitClient } = {}): UseStats<ActiveDays> {
  const { scope } = useContentScope(o.client);
  return useStats(q ? keyOf("stats", scope, "active", q.days) : null, (c, s) => c.stats.active(q!, s), o);
}

/** The signed-in reader's statistics; nothing signed out. */
export function useMyStats(o: { client?: ContentKitClient } = {}): UseStats<MyStats> {
  const { scope, viewer } = useContentScope(o.client);
  return useStats(viewer === null ? null : keyOf("stats", scope, "me"), (c, s) => c.stats.mine(s), o);
}
