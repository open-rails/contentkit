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
    return t.type === "entities" ? call(this.http, "GET", "/entities/{id}/reaction", o) : call(this.http, "GET", "/contents/{id}/reaction", { ...o, query: { version: t.version } });
  }
  /** Sets the caller's reaction (0 clears it); resolves with the new counts. */
  async set(t: Target, value: Reaction): Promise<ReactionCounts> {
    const emit = this.http.captureChanges();
    const o = { params: { id: t.id } };
    const c = { ...o, query: { version: t.version } };
    let counts: ReactionCounts;
    if (t.type === "entities") {
      counts = value === 0 ? await call(this.http, "DELETE", "/entities/{id}/reaction", o) : await call(this.http, "POST", `/entities/{id}/${reactionVerb(value)}` as const, o);
    } else {
      counts = value === 0 ? await call(this.http, "DELETE", "/contents/{id}/reaction", c) : await call(this.http, "POST", `/contents/{id}/${reactionVerb(value)}` as const, c);
    }
    emit({ type: "reaction.changed", target: t, counts });
    return counts;
  }
}
