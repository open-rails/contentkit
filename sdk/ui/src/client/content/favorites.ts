import type { FavoriteItem, FavoriteState, RefBody } from "../generated/wire.js";
import type { Http } from "../http.js";
import { call, type PageQuery } from "../route.js";

/** The signed-in caller's favorites. */
export class FavoritesClient {
  constructor(private readonly http: Http) {}

  /** Whether the caller has favorited the item. */
  get(ref: RefBody, signal?: AbortSignal): Promise<FavoriteState> {
    return call(this.http, "GET", "/{kind}/{id}/favorite", { params: { kind: ref.kind, id: ref.id }, signal });
  }
  /** Favorites or unfavorites the item; repeating is a no-op. */
  async set(ref: RefBody, favorited: boolean): Promise<FavoriteState> {
    const state: FavoriteState = await call(this.http, favorited ? "POST" : "DELETE", "/{kind}/{id}/favorite", { params: { kind: ref.kind, id: ref.id } });
    this.http.emit({ type: "favorite.changed", ref: { kind: ref.kind, id: ref.id }, favorited: state.favorited });
    return state;
  }
  /** The caller's favorites, newest first (references only; the host hydrates them). */
  list(q: PageQuery = {}, signal?: AbortSignal): Promise<FavoriteItem[]> {
    return call(this.http, "GET", "/favorites", { query: q, signal });
  }
}
