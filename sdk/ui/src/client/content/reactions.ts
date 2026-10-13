import type { InteractionPage, ReactionCounts } from "../generated/wire.js";
import type { Http } from "../http.js";
import { query } from "../discovery.js";
import { call } from "../route.js";
import { reactionVerb, type Reaction, type Target } from "./types.js";
import type { MineQuery } from "./favorites.js";

/** Likes and dislikes of a content item, a version or a group. */
export class ReactionsClient {
  constructor(private readonly http: Http) {}

  /** The target's counts and the caller's own reaction. */
  get(t: Target, signal?: AbortSignal): Promise<ReactionCounts> {
    const o = { params: { id: t.id }, signal };
    return t.type === "groups" ? call(this.http, "GET", "/groups/{id}/reaction", o) : call(this.http, "GET", "/contents/{id}/reaction", { ...o, query: { version: t.version } });
  }
  /** Sets the caller's reaction (0 clears it); resolves with the new counts. */
  async set(t: Target, value: Reaction): Promise<ReactionCounts> {
    const emit = this.http.captureChanges();
    const o = { params: { id: t.id } };
    const c = { ...o, query: { version: t.version } };
    let counts: ReactionCounts;
    if (t.type === "groups") {
      counts = value === 0 ? await call(this.http, "DELETE", "/groups/{id}/reaction", o) : await call(this.http, "POST", `/groups/{id}/${reactionVerb(value)}` as const, o);
    } else {
      counts = value === 0 ? await call(this.http, "DELETE", "/contents/{id}/reaction", c) : await call(this.http, "POST", `/contents/{id}/${reactionVerb(value)}` as const, c);
    }
    emit({ type: "reaction.changed", target: t, counts });
    return counts;
  }
  /** What the signed-in caller liked or disliked, most recent first: content cards or groups (tag preferences). */
  mine(q: MineQuery & { value?: "like" | "dislike" } = {}, signal?: AbortSignal): Promise<InteractionPage> {
    return call(this.http, "GET", "/me/reactions", { query: query(q) as never, signal });
  }
}
