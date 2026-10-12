import type { Totals } from "./generated/wire.js";
import type { Http } from "./http.js";
import { call } from "./route.js";

/** GET /catalog/totals filters: kinds to total (every listed kind when empty), languages to break them down by (every counted one when empty). */
export interface CatalogTotalsQuery {
  kinds?: readonly string[];
  languages?: readonly string[];
}

/** The public catalog. */
export class CatalogClient {
  constructor(private readonly http: Http) {}

  /** Works per kind, in all and per language: how many anyone opens and how many each tier's holders open. */
  totals(q: CatalogTotalsQuery = {}, signal?: AbortSignal): Promise<Totals> {
    return call(this.http, "GET", "/totals", { query: { kinds: q.kinds?.join(","), languages: q.languages?.join(",") }, signal });
  }
}
