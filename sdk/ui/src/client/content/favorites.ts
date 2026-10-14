import type { FavoriteState, InteractionPage } from "../generated/wire.js";
import type { Http } from "../http.js";
import { query, type AccessMode } from "../discovery.js";
import { call } from "../route.js";
import type { Target } from "./types.js";

/** A page of the caller's favorites or reactions. */
export interface MineQuery {
  /** contents (the default) or groups. */
  target?: "contents" | "groups";
  /** One content kind or group kind. */
  kind?: string;
  /** The group kinds each card names; every kind with a page by default. */
  card_groups?: readonly string[];
  cursor?: string;
  limit?: number;
  language?: string;
  display_language?: string;
  mode?: AccessMode;
}

/** Favorites: the target's count for everyone, the signed-in caller's own. */
export class FavoritesClient {
  constructor(private readonly http: Http) {}

  /** The target's favorite count and whether the caller favorited it (never, signed out). */
  get(t: Target, signal?: AbortSignal): Promise<FavoriteState> {
    const o = { params: { id: t.id }, signal };
    return t.type === "groups" ? call(this.http, "GET", "/groups/{id}/favorite", o) : call(this.http, "GET", "/contents/{id}/favorite", { ...o, query: { version: t.version } });
  }
  /** Favorites or unfavorites the target (signed in); repeating is a no-op. Resolves with the new state and count. */
  async set(t: Target, favorited: boolean): Promise<FavoriteState> {
    const emit = this.http.captureChanges();
    const method = favorited ? "POST" : "DELETE";
    const o = { params: { id: t.id } };
    const state: FavoriteState =
      t.type === "groups" ? await call(this.http, method, "/groups/{id}/favorite", o) : await call(this.http, method, "/contents/{id}/favorite", { ...o, query: { version: t.version } });
    emit({ type: "favorite.changed", target: t, favorited: state.favorited, count: state.count });
    return state;
  }
  /** The caller's favorites, most recent first: content cards or groups. */
  mine(q: MineQuery = {}, signal?: AbortSignal): Promise<InteractionPage> {
    return call(this.http, "GET", "/me/favorites", { query: query(q) as never, signal });
  }
}
