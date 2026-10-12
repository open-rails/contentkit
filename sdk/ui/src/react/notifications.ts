import { useCallback, useEffect } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { Item } from "../client/generated/wire.js";
import { keyOf, useContentScope, useList, type UseList } from "./use-resource.js";

/** Options for the current person's inbox; no recipient selector is accepted. */
export interface UseNotificationsOptions {
  client?: ContentKitClient;
  unread?: boolean;
  dismissed?: boolean;
  pageSize?: number;
  enabled?: boolean;
  /** Refresh while visible. Off by default so loaded pages remain stable. */
  refreshInterval?: number | false;
}

export interface UseNotifications extends UseList<Item> {
  /** Null viewer disables reads; undefined viewer leaves authentication to the client. */
  signedOut: boolean;
  setRead: (id: string, read: boolean) => Promise<Item>;
  setDismissed: (id: string, dismissed: boolean) => Promise<Item>;
}

/** Shared, cursor-paged notification reads, isolated by viewer and access revision. */
export function useNotifications(o: UseNotificationsOptions = {}): UseNotifications {
  const { client, store, scope, viewer } = useContentScope(o.client);
  const enabled = o.enabled !== false && viewer !== null;
  const unread = o.unread ?? false;
  const size = o.pageSize ?? 20;
  const dismissed = o.dismissed ?? false;
  const key = enabled ? keyOf("notifications", scope, unread, dismissed, size) : null;
  const list = useList<Item>(store, key, { type: "notifications", scope: `${dismissed ? "archived" : "active"}:${unread ? "unread" : "all"}` }, async (cursor, signal) => {
    const page = await client.notifications.list({ unread, dismissed, limit: size, cursor: typeof cursor === "string" ? cursor : undefined, signal });
    return { items: page.items, next: page.next_cursor || null };
  });
  const setRead = useCallback((id: string, read: boolean) => client.notifications.setRead(id, read), [client]);
  const setDismissed = useCallback((id: string, dismissed: boolean) => client.notifications.setDismissed(id, dismissed), [client]);
  const refresh = o.refreshInterval ?? false;
  const reload = list.reload;
  useEffect(() => {
    if (!enabled || refresh === false || refresh <= 0) return;
    const timer = setInterval(() => {
      if (document.visibilityState === "visible") reload();
    }, Math.max(5_000, refresh));
    return () => clearInterval(timer);
  }, [enabled, refresh, reload]);
  return { ...list, signedOut: viewer === null, setRead, setDismissed };
}

/** A bounded unread-present indicator, not an invented total count. */
export function useNotificationUnread(o: Omit<UseNotificationsOptions, "unread" | "dismissed" | "pageSize"> = {}) {
  const inbox = useNotifications({ refreshInterval: 30_000, ...o, unread: true, pageSize: 1 });
  return { unread: inbox.items.length > 0, loading: inbox.loading, error: inbox.error, signedOut: inbox.signedOut, reload: inbox.reload };
}
