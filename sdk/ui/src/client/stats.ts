import type { ActiveDays, MyStats } from "./generated/wire.js";
import type { Http } from "./http.js";
import { call } from "./route.js";

export interface ActiveQuery {
  /** The last days, today included: 10 by default, at most 90. */
  days?: number;
}

/** Content statistics: staff activity (content statistics permission) and the signed-in reader's own. */
export class StatsClient {
  constructor(private readonly http: Http) {}

  /** Distinct people active on the site for every UTC day of the period, newest first, by access. */
  active(q: ActiveQuery = {}, signal?: AbortSignal): Promise<ActiveDays> {
    return call(this.http, "GET", "/stats/active", { query: { days: q.days }, signal });
  }
  /** The signed-in reader's statistics: contents finished per kind, comments, reactions received and elapsed access per tier when billing supplies its history. */
  mine(signal?: AbortSignal): Promise<MyStats> {
    return call(this.http, "GET", "/me/stats", { signal });
  }
}
