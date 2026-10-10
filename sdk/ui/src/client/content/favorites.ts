import type { FavoriteItem, FavoriteState, RefBody } from "../generated/wire.js";
import type { Http } from "../http.js";
import { call, type PageQuery } from "../route.js";

/** Favorites: the item's count for everyone, the signed-in caller's own. */
export class FavoritesClient {
  constructor(private readonly http: Http) {}

  /** The item's favorite count and whether the caller favorited it (never, signed out). */
  get(ref: RefBody, signal?: AbortSignal): Promise<FavoriteState> {
    return call(this.http, "GET", "/{kind}/{id}/favorite", { params: { kind: ref.kind, id: ref.id }, signal });
  }
  /** Favorites or unfavorites the item (signed in); repeating is a no-op. Resolves with the new state and count. */
  async set(ref: RefBody, favorited: boolean): Promise<FavoriteState> {
    const emit = this.http.captureChanges();
    const state: FavoriteState = await call(this.http, favorited ? "POST" : "DELETE", "/{kind}/{id}/favorite", { params: { kind: ref.kind, id: ref.id } });
    emit({ type: "favorite.changed", ref: { kind: ref.kind, id: ref.id }, favorited: state.favorited, count: state.count });
    return state;
  }
  /** The caller's favorites, newest first (references only; the host hydrates them). */
  list(q: PageQuery = {}, signal?: AbortSignal): Promise<FavoriteItem[]> {
    return call(this.http, "GET", "/favorites", { query: q, signal });
  }
}
