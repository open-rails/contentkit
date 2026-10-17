import { useCallback } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { PageNotices } from "../client/generated/wire.js";
import { keyOf, useContentScope, useResource } from "./use-resource.js";

export interface UsePageNotices {
  notices: PageNotices["notices"];
  loading: boolean;
  reload: () => void;
  dismiss: (notice: string) => Promise<void>;
  restore: (notice: string) => Promise<void>;
}

/** A version's quiet notes about automatic page changes, for its editors. */
export function usePageNotices(content: string | null | undefined, version: string | null | undefined, o: { client?: ContentKitClient } = {}): UsePageNotices {
  const { client, store, scope } = useContentScope(o.client);
  const key = content && version ? keyOf("page-notices", scope, content, version) : null;
  const r = useResource<PageNotices>(store, key, { type: "page-notices", id: version ?? undefined, scope: content ?? undefined }, (signal) => client.versions.pageNotices(content!, version!, signal));
  const current = r.data;
  const dismiss = useCallback(async (notice: string) => {
    if (!content || !version || !key) return;
    await client.versions.dismissPageNotice(content, version, notice);
    store.set(key, { notices: (current?.notices ?? []).filter((n) => n.id !== notice) });
  }, [client, content, version, key, store, current]);
  const restore = useCallback(async (notice: string) => {
    if (!content || !version || !key) return;
    store.set(key, await client.versions.restorePageNotice(content, version, notice));
  }, [client, content, version, key, store]);
  return { notices: current?.notices ?? [], loading: !!key && (r.loading || !r.loaded), reload: r.reload, dismiss, restore };
}
