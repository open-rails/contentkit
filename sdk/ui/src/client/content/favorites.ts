import type { FavoriteState, InteractionPage } from "../generated/wire.js";
import type { Http } from "../http.js";
import { query, type AccessMode } from "../discovery.js";
import { call } from "../route.js";
import type { Target } from "./types.js";

/** A page of the caller's favorites or reactions. */
export interface MineQuery {
  /** Content items (the default), metadata entities, or channels. */
  target?: "contents" | "entities" | "channels";
  /** One content kind or group kind. */
  kind?: string;
  /** The group kinds each card names; every kind with a page by default. */
  card_entities?: readonly string[];
  cursor?: string;
  limit?: number;
  language?: string;
  display_language?: string;
  mode?: AccessMode;
  /** Counts for the caller's matching content selection, on the first page. */
  summary?: boolean;
}

/** Favorites: the target's count for everyone, the signed-in caller's own. */
export class FavoritesClient {
  constructor(private readonly http: Http) {}

  /** The target's favorite count and whether the caller favorited it (never, signed out). */
  get(t: Target, signal?: AbortSignal): Promise<FavoriteState> {
    const o = { params: { id: t.id }, signal };
    return call(this.http, "GET", `/${t.type}/{id}/favorite`, { ...o, ...(t.type === "contents" ? { query: { version: t.version } } : {}) });
  }
  /** Favorites or unfavorites the target (signed in); repeating is a no-op. Resolves with the new state and count. */
  async set(t: Target, favorited: boolean): Promise<FavoriteState> {
    const emit = this.http.captureChanges();
    const method = favorited ? "POST" : "DELETE";
    const o = { params: { id: t.id } };
    const state: FavoriteState =
      await call(this.http, method, `/${t.type}/{id}/favorite`, { ...o, ...(t.type === "contents" ? { query: { version: t.version } } : {}) });
    emit({ type: "favorite.changed", target: t, favorited: state.favorited, count: state.count });
    return state;
  }
  /** The caller's favorites, most recent first: content cards or groups. */
  mine(q: MineQuery = {}, signal?: AbortSignal): Promise<InteractionPage> {
    return call(this.http, "GET", "/me/favorites", { query: query(q) as never, signal });
  }
}
