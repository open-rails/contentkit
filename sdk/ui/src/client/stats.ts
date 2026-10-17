import type { Activity, MyStats, TopContents } from "./generated/wire.js";
import type { Period } from "./discovery.js";
import type { Http } from "./http.js";
import { call } from "./route.js";

export interface ActivityQuery {
  kind: string;
  /** The last days, today included: 7 by default, at most 90. */
  days?: number;
}

export interface TopQuery {
  kind: string;
  /** Distinct people with measured attention (the default), or published comments. */
  metric?: "readers" | "comments";
  /** 30d by default. */
  period?: Period;
  access?: readonly ("public" | "tier" | "group" | "ppv" | "group_ppv")[];
  tiers?: readonly string[];
  /** Titles and paths in this language; the site's first by default. */
  language?: string;
  /** 10 by default, at most 100. */
  limit?: number;
}

/** Content statistics: staff activity and rankings (content statistics permission), and the signed-in reader's own. */
export class StatsClient {
  constructor(private readonly http: Http) {}

  /** A kind's measured activity per UTC day: distinct engaged people, signed-in people and focused active milliseconds. */
  activity(q: ActivityQuery, signal?: AbortSignal): Promise<Activity> {
    return call(this.http, "GET", "/stats/activity", { query: { kind: q.kind, days: q.days }, signal });
  }
  /** A kind's live, listed content ranked by distinct engaged people or published comments over a period. */
  top(q: TopQuery, signal?: AbortSignal): Promise<TopContents> {
    const { access, tiers, ...rest } = q;
    return call(this.http, "GET", "/stats/top", { query: { ...rest, access: access?.join(","), tiers: tiers?.join(",") }, signal });
  }
  /** The signed-in reader's statistics: contents finished per kind, comments, reactions received and elapsed access per tier when billing supplies its history. */
  mine(signal?: AbortSignal): Promise<MyStats> {
    return call(this.http, "GET", "/me/stats", { signal });
  }
}
