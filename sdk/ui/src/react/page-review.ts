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
export function usePageReview(content: string, version: string, o: { client?: ContentKitClient } = {}): UsePageReview {
  const { client, store, scope } = useContentScope(o.client);
  const key = keyOf("page-review", scope, content, version);
  const r = useResource<PageReview>(store, key, { type: "page-review", id: version, scope: content }, (signal) => client.versions.pages(content, version, signal));
  const { reload } = r;
  const update = useCallback(async (patch: PageReviewPatch) => {
    try {
      const next = await client.versions.updatePages(content, version, patch);
      store.set(key, next);
      return next;
    } catch (error) {
      if (toContentKitError(error).status === 409) reload();
      throw error;
    }
  }, [client, content, version, store, key, reload]);
  return { review: r.data ?? null, loading: r.loading || !r.loaded, error: r.error, reload: r.reload, update };
}
