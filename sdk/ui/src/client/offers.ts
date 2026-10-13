import type { GroupJoin, LibraryPage, Offer, OfferPage, Terms } from "./generated/wire.js";
import type { Http } from "./http.js";
import { call } from "./route.js";

/**
 * Offers: content, versions and groups put on sale (their creators, owner
 * group editors and root:offers:write holders), free group joins and the
 * reader's library.
 */
export class OffersClient {
  constructor(private readonly http: Http) {}

  /** Puts a content (or one version) on sale, or reprices it: a new price, the old one archived. */
  sell(target: { content: string; version?: string }, terms: Terms): Promise<Offer> {
    return this.write(
      target.content,
      target.version
        ? call(this.http, "PUT", "/contents/{id}/versions/{vid}/offer", { params: { id: target.content, vid: target.version }, body: terms })
        : call(this.http, "PUT", "/contents/{id}/offer", { params: { id: target.content }, body: terms }),
    );
  }
  /** Takes it off sale; buyers keep what they bought. */
  withdraw(target: { content: string; version?: string }): Promise<Offer> {
    return this.write(
      target.content,
      target.version
        ? call(this.http, "DELETE", "/contents/{id}/versions/{vid}/offer", { params: { id: target.content, vid: target.version } })
        : call(this.http, "DELETE", "/contents/{id}/offer", { params: { id: target.content } }),
    );
  }
  /** Sells a group: its key opens every member. */
  sellGroup(id: string, terms: Terms): Promise<Offer> {
    return this.writeGroup(call(this.http, "PUT", "/groups/{id}/offer", { params: { id }, body: terms }));
  }
  withdrawGroup(id: string): Promise<Offer> {
    return this.writeGroup(call(this.http, "DELETE", "/groups/{id}/offer", { params: { id } }));
  }
  /** Joins a free group (or leaves it): its key granted to the caller. */
  async join(id: string, member = true): Promise<GroupJoin> {
    const emit = this.http.captureChanges();
    const out: GroupJoin = await call(this.http, member ? "POST" : "DELETE", "/groups/{id}/join", { params: { id } });
    emit({ type: "access.changed" });
    return out;
  }
  /** What the reader bought, a cursor page at a time. */
  library(q: { language?: string; cursor?: string; limit?: number } = {}, signal?: AbortSignal): Promise<LibraryPage> {
    return call(this.http, "GET", "/me/library", { query: q, signal });
  }
  /** Staff: every offer, by state or not yet synced to billing. */
  list(q: { state?: "active" | "withdrawn"; unsynced?: boolean; cursor?: string; limit?: number } = {}, signal?: AbortSignal): Promise<OfferPage> {
    return call(this.http, "GET", "/offers", { query: q, signal });
  }

  private async write(content: string, p: Promise<Offer>): Promise<Offer> {
    const emit = this.http.captureChanges();
    const out = await p;
    emit({ type: "content.changed", id: content });
    return out;
  }

  private async writeGroup(p: Promise<Offer>): Promise<Offer> {
    const emit = this.http.captureChanges();
    const out = await p;
    emit({ type: "groups.changed" });
    return out;
  }
}
