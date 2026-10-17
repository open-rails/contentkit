import { useCallback } from "react";
import type { ContentKitClient } from "../client/client.js";
import { toContentKitError, type ContentKitError } from "../client/errors.js";
import type { PageReview, PageReviewPatch } from "../client/generated/wire.js";
import { keyOf, useContentScope, useResource } from "./use-resource.js";

export interface UsePageReview {
  review: PageReview | null;
  loading: boolean;
  error?: ContentKitError;
  reload: () => void;
  update: (patch: PageReviewPatch) => Promise<PageReview>;
}

/** A version's retained page sources and reversible review choices. */
export function usePageReview(content: string | null | undefined, version: string | null | undefined, o: { client?: ContentKitClient } = {}): UsePageReview {
  const { client, store, scope } = useContentScope(o.client);
  const key = content && version ? keyOf("page-review", scope, content, version) : null;
  const r = useResource<PageReview>(store, key, { type: "page-review", id: version ?? undefined, scope: content ?? undefined }, (signal) => client.versions.pages(content!, version!, signal));
  const { reload } = r;
  const update = useCallback(async (patch: PageReviewPatch) => {
    if(!content||!version)throw new Error("contentkit-ui: no reader version");
    try {
      const next = await client.versions.updatePages(content, version, patch);
      if(key)store.set(key, next);
      return next;
    } catch (error) {
      if (toContentKitError(error).status === 409) reload();
      throw error;
    }
  }, [client, content, version, store, key, reload]);
  return { review: r.data ?? null, loading: !!key && (r.loading || !r.loaded), error: r.error, reload: r.reload, update };
}
