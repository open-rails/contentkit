import type { Resolved } from "./generated/wire.js";
import type { Http } from "./http.js";
import { call } from "./route.js";

/** Content codes: any spelling of a code resolves to its item and canonical path. */
export class CodesClient {
  constructor(private readonly http: Http) {}

  /** The code's item, slugs and canonical path (lang picks the localized slug); not_found or gone. */
  resolve(code: string, o: { lang?: string; signal?: AbortSignal } = {}): Promise<Resolved> {
    return call(this.http, "GET", "/{code}", { params: { code }, query: { lang: o.lang }, signal: o.signal });
  }
}
