import { useCallback, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { ContentKitError } from "../client/errors.js";
import { toContentKitError } from "../client/errors.js";
import type { Reaction } from "../client/content/types.js";
import type { ReactionCounts, RefBody } from "../client/generated/wire.js";
import { withReaction } from "./comments.js";
import { keyOf, useContentScope, useResource } from "./use-resource.js";

export interface UseReaction {
  counts: ReactionCounts;
  loading: boolean;
  error?: ContentKitError;
  /** Sets the caller's reaction at once; rolled back if the server refuses it. */
  set: (value: Reaction) => Promise<void>;
  /** Sets value, or clears it when it is the current one. */
  toggle: (value: 1 | -1) => Promise<void>;
}

const ZERO: ReactionCounts = { likes: 0, dislikes: 0, mine: 0 };

/** An item's likes and dislikes and the caller's own reaction (initial: counts the host already has). */
export function useReaction(ref: RefBody, o: { initial?: ReactionCounts; client?: ContentKitClient } = {}): UseReaction {
  const { client, store, viewer } = useContentScope(o.client);
  const key = keyOf("reaction", viewer, ref.kind, ref.id);
  const r = useResource<ReactionCounts>(store, key, { type: "reaction", ref: { kind: ref.kind, id: ref.id } }, (s) => client.reactions.get(ref, s), { initial: o.initial });
  const counts = r.data ?? ZERO;
  const { kind, id } = ref;
  const set = useCallback(
    async (value: Reaction) => {
      const prev = store.snapshot<ReactionCounts>(key).data ?? counts;
      const token = store.mark(key);
      store.set(key, withReaction(prev, value));
      try {
        await client.reactions.set({ kind, id }, value);
      } catch (e) {
        store.rollback(key, token, prev);
        throw toContentKitError(e);
      }
    },
    [client, store, key, kind, id, counts],
  );
  const toggle = useCallback((value: 1 | -1) => set(counts.mine === value ? 0 : value), [set, counts.mine]);
  return { counts, loading: r.loading || (!r.loaded && o.initial === undefined), error: r.error, set, toggle };
}

export interface UseFavorite {
  favorited: boolean;
  loading: boolean;
  /** A write is in flight. */
  pending: boolean;
  error?: ContentKitError;
  /** Sets it at once; rolled back if the server refuses it. */
  set: (favorited: boolean) => Promise<void>;
  toggle: () => Promise<void>;
}

/**
 * Whether the signed-in caller favorited the item (initial: the host's value).
 * Signed out (viewer null) it reads nothing and stays false.
 */
export function useFavorite(ref: RefBody, o: { initial?: boolean; client?: ContentKitClient } = {}): UseFavorite {
  const { client, store, viewer } = useContentScope(o.client);
  const key = viewer === null ? null : keyOf("favorite", viewer, ref.kind, ref.id);
  const r = useResource<boolean>(
    store,
    key,
    { type: "favorite", ref: { kind: ref.kind, id: ref.id } },
    async (s) => {
      try {
        return (await client.favorites.get(ref, s)).favorited;
      } catch (e) {
        // A caller the host did not sign in has no favorites.
        if (toContentKitError(e).code === "unauthorized") return false;
        throw e;
      }
    },
    { initial: o.initial },
  );
  const favorited = !!r.data;
  const [pending, setPending] = useState(false);
  const { kind, id } = ref;
  const set = useCallback(
    async (next: boolean) => {
      const token = key ? store.mark(key) : 0;
      if (key) store.set(key, next);
      setPending(true);
      try {
        await client.favorites.set({ kind, id }, next);
      } catch (e) {
        if (key) store.rollback(key, token, !next);
        throw toContentKitError(e);
      } finally {
        setPending(false);
      }
    },
    [client, store, key, kind, id],
  );
  const toggle = useCallback(() => set(!favorited), [set, favorited]);
  return { favorited, loading: !!key && (r.loading || (!r.loaded && o.initial === undefined)), pending, error: r.error, set, toggle };
}
