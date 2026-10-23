import type { EventPage, Totals } from "./generated/wire.js";
import type { Http } from "./http.js";
import { call } from "./route.js";

/** GET /catalog/totals filters: kinds to total (every kind when empty), languages to break them down by (every counted one when empty). */
export interface CatalogTotalsQuery {
  kinds?: readonly string[];
  languages?: readonly string[];
}

/** Readers own their cursor; filters must stay unchanged while continuing. */
export interface CatalogEventsQuery {
  after?: string;
  limit?: number;
  types?: readonly string[];
}

/** The public catalog. */
export class CatalogClient {
  constructor(private readonly http: Http) {}

  /** Read committed catalog facts without acknowledging or deleting them. */
  listEvents(q: CatalogEventsQuery = {}, signal?: AbortSignal): Promise<EventPage> {
    return call(this.http, "GET", "/events", { query: { after: q.after, limit: q.limit, types: q.types?.join(",") }, signal });
  }


  /** Content items per kind, in all and per language: how many anyone's listing shows and opens, and how many each tier's holders open. */
  totals(q: CatalogTotalsQuery = {}, signal?: AbortSignal): Promise<Totals> {
    return call(this.http, "GET", "/catalog/totals", { query: { kinds: q.kinds?.join(","), languages: q.languages?.join(",") }, signal });
  }
}
