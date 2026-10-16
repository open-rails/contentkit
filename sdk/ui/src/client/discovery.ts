import type {
  ActivityContexts,
  ActivityContextsInput,
  ActivityTarget,
  ContentFilters,
  ContentFiltersUpdate,
  ContentPage,
  DailyReleases,
  ExposureInput,
  Exposures,
  RecentSearches,
  ReleaseDates,
  SignalInput,
  Signals,
  Typeahead,
} from "./generated/wire.js";
import type { Http } from "./http.js";
import { call, routeURL } from "./route.js";
import { browserSearchHistory, type BrowserSearchHistory } from "./browser-search-history.js";

/** A popularity period. */
export type Period = "7d" | "30d" | "90d" | "365d" | "all";
/** A list's order; position orders by the first of `groups` (an ordered group). */
export type ListSort = "newest" | "oldest" | "title" | "popular" | `popular:${Period}` | "top_rated" | `top_rated:${Period}` | "most_favorited" | "most_viewed" | "position" | "random";
/** How locked contents list: marked (lock, the default) or left out (hide). */
export type AccessMode = "lock" | "hide";

/** Which contents a list holds. */
export interface ContentFilterQuery {
  kind?: string;
  /** Group ids every listed version is in. */
  groups?: readonly string[];
  /** Group ids no listed version is in. */
  exclude_groups?: readonly string[];
  /** Only contents this group owns: a channel's feed. */
  owner_group?: string;
  /** User ids: only contents one of them is credited on. */
  creators?: readonly string[];
  /** public (anyone opens it now) or the level it stays locked at. */
  access?: readonly ("public" | "tier" | "group" | "ppv" | "group_ppv")[];
  /** Only contents locked behind one of these tiers. */
  tiers?: readonly string[];
  /** Version traits every listed version has. */
  traits?: readonly string[];
  /** "name:value" of the kind's filterable attributes. */
  attribute?: readonly string[];
  /** Published on this day (YYYY-MM-DD) or month (YYYY-MM). */
  released_on?: string;
  /** Only contents published within this age window; independent of the popularity score window. */
  published_within?: Period;
  /** IANA time zone of released_on and release dates; UTC by default. */
  tz?: string;
  /** The signed-in reader's unseen, in-progress or completed contents. */
  view_status?: "unseen" | "in_progress" | "completed";
}

/** How a list reads to its reader. */
export interface ReaderQuery {
  /** Only content with a live version in this language. */
  language?: string;
  /** Titles, names and paths in this language, and each card's preferred version's; language by default. */
  display_language?: string;
  mode?: AccessMode;
}

/** The group kinds cards name; every kind with a page by default. */
export interface CardQuery {
  card_entities?: readonly string[];
}

export interface PageQuery {
  cursor?: string;
  limit?: number;
}

export type ContentQuery = ContentFilterQuery & ReaderQuery & CardQuery & PageQuery & {
  sort?: ListSort;
  /** Inclusive UTC date (YYYY-MM-DD); requires activity_until, kind and sort=popular. */
  activity_from?: string;
  /** Exclusive UTC date (YYYY-MM-DD); requires activity_from, kind and sort=popular. */
  activity_until?: string;
};
export type PopularPeriod = "7d" | "90d" | "365d" | "all";
/** Whole UTC dates: from inclusive, until exclusive. */
export interface PopularDateRange {
  from: string;
  until: string;
}
export type PopularQuery = Omit<ContentQuery, "sort" | "activity_from" | "activity_until">;

/** Rank activity in the selected window; publication age is an optional separate filter. */
export function popularQuery(period: PopularPeriod | PopularDateRange = "7d", filters: PopularQuery = {}): ContentQuery {
  return typeof period === "string"
    ? { ...filters, sort: `popular:${period}` }
    : { ...filters, sort: "popular", activity_from: period.from, activity_until: period.until };
}

/** Rank activity between two UTC dates: from inclusive, until exclusive. */
export function popularBetween(from: string, until: string, filters: PopularQuery & { kind: string }): ContentQuery {
  return popularQuery({ from, until }, filters);
}

export type SearchQuery = ContentFilterQuery &
  ReaderQuery &
  CardQuery &
  PageQuery & {
    q: string;
    sort?: "relevance" | ListSort;
    personalize?: boolean;
    /** Explicit legacy server-side anonymous history. Omit for public search without server history. */
    session?: string;
  };
export interface RecentSearchesQuery {
  /** Explicit legacy server-side anonymous history; no anonymous session is created by default. */
  session?: string;
  /** 10 by default, at most 100. */
  limit?: number;
}
export type RailQuery = ContentFilterQuery & ReaderQuery & CardQuery & { limit?: number };
export type ReleaseDatesQuery = ContentFilterQuery & ReaderQuery & PageQuery & { by?: "month" | "day" };
/** Complete daily metadata for one calendar window, without pagination. */
export type DailyReleasesQuery = Omit<ContentFilterQuery, "released_on" | "tz"> & ReaderQuery & CardQuery & { days?: number; time_zone?: string };
export type HistoryQuery = ReaderQuery & CardQuery & PageQuery & { kind?: string; status?: "seen" | "in_progress" | "completed" };
export interface TypeaheadQuery extends ReaderQuery {
  q: string;
  /** Only contents of this kind. */
  kind?: string;
  /** The group kinds suggested; every kind with a page by default. */
  entity_kinds?: readonly string[];
  /** Per section: 8 by default, at most 20. */
  limit?: number;
}

// Comma-joined list parameters; `attribute` repeats instead.
const LISTS = new Set(["groups", "exclude_groups", "creators", "access", "tiers", "traits", "card_entities", "entity_kinds"]);

/** A query with its list parameters comma-joined, as discovery routes read them. */
export function query(q: object): Record<string, string | number | boolean | readonly string[] | undefined> {
  const out: Record<string, string | number | boolean | readonly string[] | undefined> = {};
  for (const [k, v] of Object.entries(q)) out[k] = LISTS.has(k) && Array.isArray(v) ? (v.length ? v.join(",") : undefined) : v;
  return out;
}

/**
 * Discovery: lists, search, typeahead, similar and recommended contents,
 * release dates, the reader's content filters and history, signals and exposures.
 */
export class DiscoveryClient {
  /** Local anonymous history, isolated from authenticated server history. */
  readonly browserSearches: BrowserSearchHistory;

  constructor(private readonly http: Http) {
    this.browserSearches = browserSearchHistory(http.mount("discovery"));
  }

  /** Contents as cards, one per content item with the version that fits the reader. */
  list(q: ContentQuery = {}, signal?: AbortSignal): Promise<ContentPage> {
    return call(this.http, "GET", "/contents", { query: query(q) as never, signal });
  }
  /** A first page is kept among the reader's recent searches. */
  async search(q: SearchQuery, signal?: AbortSignal): Promise<ContentPage> {
    const emit = q.cursor ? null : this.http.captureChanges();
    const out: ContentPage = await call(this.http, "GET", "/search", { query: query(q) as never, signal });
    emit?.({ type: "searches.changed" });
    return out;
  }
  /** The reader's recent searches, newest first, since they last cleared them. */
  recentSearches(q: RecentSearchesQuery = {}, signal?: AbortSignal): Promise<RecentSearches> {
    return call(this.http, "GET", "/me/searches", { query: q, signal });
  }
  /** Clears one recent search, or all of them. */
  async clearSearches(o: { query?: string; session?: string } = {}): Promise<void> {
    const emit = this.http.captureChanges();
    await call(this.http, "DELETE", "/me/searches", { query: o });
    emit({ type: "searches.changed" });
  }
  /** Contents and groups while the reader types. */
  typeahead(q: TypeaheadQuery, signal?: AbortSignal): Promise<Typeahead> {
    return call(this.http, "GET", "/typeahead", { query: query(q) as never, signal });
  }
  /** Contents like one, of its kind unless q.kind names another. */
  similar(id: string, q: RailQuery = {}, signal?: AbortSignal): Promise<ContentPage> {
    return call(this.http, "GET", "/contents/{id}/similar", { params: { id }, query: query(q) as never, signal });
  }
  /** For the signed-in reader. */
  recommendations(q: RailQuery = {}, signal?: AbortSignal): Promise<ContentPage> {
    return call(this.http, "GET", "/recommendations", { query: query(q) as never, signal });
  }
  /** Per month or day, newest first: how many contents the reader's listing shows released then, and how many are free. */
  releaseDates(q: ReleaseDatesQuery = {}, signal?: AbortSignal): Promise<ReleaseDates> {
    return call(this.http, "GET", "/release-dates", { query: query(q) as never, signal });
  }
  /** Every visible card in each day; opening or expanding a day needs no further request. */
  releases(q: DailyReleasesQuery = {}, signal?: AbortSignal): Promise<DailyReleases> {
    return call(this.http, "GET", "/releases", { query: query(q) as never, signal });
  }
  /** Only the current domain's reader-toggleable entities, named in the requested language. */
  async contentFilters(language?: string, signal?: AbortSignal): Promise<ContentFilters> {
    const result = await this.http.request<ContentFilters>(routeURL(this.http, "GET", "/content-filters", { query: { language } }), { signal, guestPreferences: false });
    this.http.acceptGuestFilters(result.kinds);
    return result;
  }
  /** Replaces this domain's reader settings; other domains' choices remain intact. */
  async setContentFilters(update: ContentFiltersUpdate, language?: string): Promise<ContentFilters> {
    const emit = this.http.captureChanges();
    const out: ContentFilters = await call(this.http, "PUT", "/me/content-filters", { query: { language }, body: update });
    emit({ type: "contentFilters.changed" });
    return out;
  }
  /** What the reader engaged with, most recent first. */
  history(q: HistoryQuery = {}, signal?: AbortSignal): Promise<ContentPage> {
    return call(this.http, "GET", "/me/history", { query: query(q) as never, signal });
  }
  /** Erases the reader's history: of one content item, one kind or everything. */
  async forgetHistory(o: { kind?: string; content_id?: string } = {}): Promise<void> {
    const emit = this.http.captureChanges();
    await call(this.http, "DELETE", "/me/history", { query: o });
    emit({ type: "history.changed" });
  }
  /** Capture server-owned access and eligibility at an observation's start. */
  activityContexts(targets: readonly ActivityTarget[], session?: string, language = this.http.language()): Promise<ActivityContexts> {
    const body: ActivityContextsInput = { targets: [...targets], session, language };
    return call(this.http, "POST", "/activity-contexts", { body });
  }
  /** Views and clicks; signed out, of the session named. */
  signals(signals: SignalInput[], session?: string, o: { keepalive?: boolean } = {}): Promise<void> {
    const body: Signals = { session, signals };
    return call(this.http, "POST", "/signals", { body, keepalive: o.keepalive });
  }
  /** Result lists served, rendered or shown, for click attribution. */
  exposures(exposures: ExposureInput[], session?: string, o: { keepalive?: boolean } = {}): Promise<void> {
    const body: Exposures = { session, exposures };
    return call(this.http, "POST", "/exposures", { body, keepalive: o.keepalive });
  }
}
