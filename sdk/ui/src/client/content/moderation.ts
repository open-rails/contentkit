import type { HeldItem, HeldPage, ReviewOutcome } from "../generated/wire.js";
import type { Http } from "../http.js";
import { call } from "../route.js";
import type { Decision, HeldKind } from "./types.js";

export interface HeldQuery {
  kind: HeldKind;
  /** The previous page's next. */
  cursor?: string;
  limit?: number;
}

/** The review queue of held comments and posts (ModerationReview). */
export class ModerationClient {
  constructor(private readonly http: Http) {}

  /** Held items awaiting review, oldest first. */
  held(q: HeldQuery, signal?: AbortSignal): Promise<HeldPage> {
    return call(this.http, "GET", "/moderation/held", { query: { ...q }, signal });
  }
  /** Approves (publishes) or rejects a held item at the revision the queue listed; conflict when it was edited since. */
  async resolve(item: Pick<HeldItem, "kind" | "id" | "revision">, decision: Decision, reason?: string): Promise<ReviewOutcome> {
    const emit = this.http.captureChanges();
    const kind = item.kind as HeldKind;
    const out: ReviewOutcome = await call(this.http, "POST", "/moderation/{kind}/{id}/resolve", {
      params: { kind, id: item.id },
      body: { decision, revision: item.revision, ...(reason ? { reason } : {}) },
    });
    emit({ type: "moderation.resolved", kind, id: item.id, decision });
    return out;
  }
}
