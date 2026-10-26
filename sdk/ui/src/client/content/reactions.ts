import type { ReactionCounts } from "../generated/wire.js";
import type { Http } from "../http.js";
import { call } from "../route.js";
import { reactionVerb, type Reaction, type Target } from "./types.js";

/** Likes and dislikes of a content item, a version or a group. */
export class ReactionsClient {
  constructor(private readonly http: Http) {}

  /** The target's counts and the caller's own reaction. */
  get(t: Target, signal?: AbortSignal): Promise<ReactionCounts> {
    const o = { params: { id: t.id }, signal };
    return call(this.http, "GET", `/${t.type}/{id}/reaction`, { ...o, ...(t.type === "contents" ? { query: { version: t.version } } : {}) });
  }
  /** Sets the caller's reaction (0 clears it); resolves with the new counts. */
  async set(t: Target, value: Reaction): Promise<ReactionCounts> {
    const emit = this.http.captureChanges();
    const options = { params: { id: t.id }, ...(t.type === "contents" ? { query: { version: t.version } } : {}) };
    const counts: ReactionCounts = value === 0
      ? await call(this.http, "DELETE", `/${t.type}/{id}/reaction`, options)
      : await call(this.http, "POST", `/${t.type}/{id}/${reactionVerb(value)}`, options);
    emit({ type: "reaction.changed", target: t, counts });
    return counts;
  }
}
