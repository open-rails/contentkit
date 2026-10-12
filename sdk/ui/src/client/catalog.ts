import type { Totals } from "./generated/wire.js";
import type { Http } from "./http.js";
import { call } from "./route.js";

/** The public catalog. */
export class CatalogClient {
  constructor(private readonly http: Http) {}

  /** Listed works per kind: how many anyone opens and how many each tier's holders open; every listed kind when kinds is empty. */
  totals(kinds: readonly string[] = [], signal?: AbortSignal): Promise<Totals> {
    return call(this.http, "GET", "/totals", { query: { kinds: kinds.join(",") }, signal });
  }
}
