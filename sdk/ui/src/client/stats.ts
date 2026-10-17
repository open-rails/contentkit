import type { Activity, MyStats } from "./generated/wire.js";
import type { Http } from "./http.js";
import { call } from "./route.js";

export interface ActivityQuery {
  kind: string;
  /** The last days, today included: 10 by default, at most 90. */
  days?: number;
}

/** Content statistics: staff activity (content statistics permission) and the signed-in reader's own. */
export class StatsClient {
  constructor(private readonly http: Http) {}

  /** A kind's measured activity for every UTC day of the period, newest first: distinct people who read or played, by audience (anon, free, premium), and focused active milliseconds. */
  activity(q: ActivityQuery, signal?: AbortSignal): Promise<Activity> {
    return call(this.http, "GET", "/stats/activity", { query: { kind: q.kind, days: q.days }, signal });
  }
  /** The signed-in reader's statistics: contents finished per kind, comments, reactions received and elapsed access per tier when billing supplies its history. */
  mine(signal?: AbortSignal): Promise<MyStats> {
    return call(this.http, "GET", "/me/stats", { signal });
  }
}
