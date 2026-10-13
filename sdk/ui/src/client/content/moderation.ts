import type { BannedWord, BannedWordInput, HeldItem, HeldPage, IPBan, IPBanInput, ReviewOutcome } from "../generated/wire.js";
import type { Http } from "../http.js";
import { call } from "../route.js";
import type { Decision, HeldKind } from "./types.js";

export interface HeldQuery {
  kind: HeldKind;
  /** The previous page's next. */
  cursor?: string;
  limit?: number;
}

/** The review queue of held comments and content (ModerationReview), and the rules every comment and answer meets (root:comments:moderate). */
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

  /** The banned words, alphabetically, and what each does: mask or reject. */
  bannedWords(signal?: AbortSignal): Promise<BannedWord[]> {
    return call(this.http, "GET", "/moderation/banned-words", { signal });
  }
  /** Bans words, or changes what banned ones do (mask by default); the list after. */
  async banWords(words: BannedWordInput[]): Promise<BannedWord[]> {
    const emit = this.http.captureChanges();
    const out: BannedWord[] = await call(this.http, "POST", "/moderation/banned-words", { body: words });
    emit({ type: "rules.changed", rules: "words" });
    return out;
  }
  /** Unbans words; idempotent. */
  async unbanWords(words: string[]): Promise<void> {
    const emit = this.http.captureChanges();
    await call(this.http, "DELETE", "/moderation/banned-words", { body: words });
    emit({ type: "rules.changed", rules: "words" });
  }
  /** The IP bans, newest first, expired ones included. */
  ipBans(signal?: AbortSignal): Promise<IPBan[]> {
    return call(this.http, "GET", "/moderation/ip-bans", { signal });
  }
  /** Bans addresses or networks (CIDR) from commenting and answering, or replaces their bans; the list after. */
  async banIPs(bans: IPBanInput[]): Promise<IPBan[]> {
    const emit = this.http.captureChanges();
    const out: IPBan[] = await call(this.http, "POST", "/moderation/ip-bans", { body: bans });
    emit({ type: "rules.changed", rules: "ips" });
    return out;
  }
  /** Lifts the bans of addresses or networks; idempotent. */
  async liftIPBans(networks: string[]): Promise<void> {
    const emit = this.http.captureChanges();
    await call(this.http, "DELETE", "/moderation/ip-bans", { body: networks });
    emit({ type: "rules.changed", rules: "ips" });
  }
}
