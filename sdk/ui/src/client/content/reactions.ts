import type { ReactionCounts, RefBody } from "../generated/wire.js";
import type { Http } from "../http.js";
import { call } from "../route.js";
import { reactionVerb, type Reaction } from "./types.js";

/** Likes and dislikes of an item (a host kind, or "post"). */
export class ReactionsClient {
  constructor(private readonly http: Http) {}

  /** The item's counts and the caller's own reaction. */
  get(ref: RefBody, signal?: AbortSignal): Promise<ReactionCounts> {
    return call(this.http, "GET", "/{kind}/{id}/reaction", { params: { kind: ref.kind, id: ref.id }, signal });
  }
  /** Sets the caller's reaction (0 clears it); resolves with the new counts. */
  async set(ref: RefBody, value: Reaction): Promise<ReactionCounts> {
    const emit = this.http.captureChanges();
    const counts: ReactionCounts = await call(this.http, "POST", `/{kind}/{id}/${reactionVerb(value)}` as const, { params: { kind: ref.kind, id: ref.id } });
    emit({ type: "reaction.changed", ref: { kind: ref.kind, id: ref.id }, counts });
    return counts;
  }
}
