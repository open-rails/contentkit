import type { BannedWord, BannedWordInput, HeldItem, HeldPage, IPBan, IPBanInput, LocationCheck, LocationRule, LocationRuleHistoryPage, LocationRuleInput, LocationRulePage, LocationRuleUpdate, ReviewOutcome } from "../generated/wire.js";
import type { Http } from "../http.js";
import { call } from "../route.js";
import type { Decision, HeldKind } from "./types.js";

export interface HeldQuery {
  kind: HeldKind;
  /** The previous page's next. */
  cursor?: string;
  limit?: number;
}

/**
 * The review queue of held comments and content (ModerationReview), the rules every comment and answer meets (root:comments:moderate),
 * and the location rules that hide the site or content from visitors by country, state or network (root:locations:write).
 */
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

  /** Location rules, newest first, with the configured domains a rule may name. */
  locationRules(q: { limit?: number; offset?: number } = {}, signal?: AbortSignal): Promise<LocationRulePage> {
    return call(this.http, "GET", "/moderation/location-rules", { query: { ...q }, signal });
  }
  /** Adds a location rule; it applies to the next request. */
  async createLocationRule(rule: LocationRuleInput): Promise<LocationRule> {
    const emit = this.http.captureChanges();
    const out: LocationRule = await call(this.http, "POST", "/moderation/location-rules", { body: rule });
    emit({ type: "rules.changed", rules: "locations" });
    return out;
  }
  /** Changes the supplied fields of a rule, including switching it on or off. */
  async updateLocationRule(id: string, patch: LocationRuleUpdate): Promise<LocationRule> {
    const emit = this.http.captureChanges();
    const out: LocationRule = await call(this.http, "PATCH", "/moderation/location-rules/{id}", { params: { id }, body: patch });
    emit({ type: "rules.changed", rules: "locations" });
    return out;
  }
  /** Deletes a rule; its history stays. */
  async deleteLocationRule(id: string): Promise<void> {
    const emit = this.http.captureChanges();
    await call(this.http, "DELETE", "/moderation/location-rules/{id}", { params: { id } });
    emit({ type: "rules.changed", rules: "locations" });
  }
  /** Who changed location rules and when, newest first; one rule's when id is given, deleted ones included. */
  locationRuleHistory(id?: string, q: { limit?: number; offset?: number } = {}, signal?: AbortSignal): Promise<LocationRuleHistoryPage> {
    return id
      ? call(this.http, "GET", "/moderation/location-rules/{id}/history", { params: { id }, query: { ...q }, signal })
      : call(this.http, "GET", "/moderation/location-rules/history", { query: { ...q }, signal });
  }
  /** Where an address resolves to, and which enabled rules cover it on each domain. */
  checkLocation(ip: string, signal?: AbortSignal): Promise<LocationCheck> {
    return call(this.http, "GET", "/moderation/location-rules/check", { query: { ip }, signal });
  }
}
