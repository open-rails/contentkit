import type {
  Activity,
  ActivityEvent,
  ContentFilters,
  ContentFiltersUpdate,
  ContentPage,
  DailyReleases,
  LibraryPage,
  RecentSearches,
  ReleaseDates,
  SearchPage,
  Typeahead,
} from "./generated/wire.js";
import type { Http } from "./http.js";
import { call, routeURL } from "./route.js";
import { browserSearchHistory, type BrowserSearchHistory } from "./browser-search-history.js";

/** A popularity period. */
export type Period = "7d" | "30d" | "90d" | "365d" | "all";
/** A list's order; position orders by the first of `groups` (an ordered group). */
export type ListSort = "newest" | "oldest" | "title" | "popular" | `popular:${string}` | "top_rated" | "most_favorited" | "position" | "random";
/** How locked contents list: marked (lock, the default) or left out (hide). */
export type AccessMode = "lock" | "hide";

/** Which contents a list holds. */
export interface ContentFilterQuery {
  kind?: string;
  /** Public card hydration under current domain/access/preferences; at most 100. */
  ids?: readonly string[];
  /** Entity ids every listed version is in. */
  groups?: readonly string[];
  /** Only contents this group owns: a channel's feed. */
  channel_id?: string;
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
  /** IANA time zone of released_on and release dates; UTC by default. */
  tz?: string;
  /** The reader's unseen (new to them: never opened or hidden), in-progress or completed contents; signed out, by the browser's anonymous id. */
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

/** A numbered page from 1 instead of cursor; its summary then comes with every page. */
export interface NumberedPageQuery {
  page?: number;
}

export type ContentQuery = ContentFilterQuery & ReaderQuery & CardQuery & PageQuery & NumberedPageQuery & {
  /** Selection-wide metadata; requested only on the first page. */
  summary?: boolean;
  sort?: ListSort;
  /** Inclusive UTC date (YYYY-MM-DD); requires activity_until, kind and sort=popular. */
  activity_from?: string;
  /** Exclusive UTC date (YYYY-MM-DD); requires activity_from, kind and sort=popular. */
  activity_until?: string;
};

/** A host-declared popularity bucket ID. */
export type PopularPeriod = string;
/** Explicit whole UTC dates, from inclusive and until exclusive. */
export interface PopularDateRange {from:string;until:string}
export type PopularQuery = Omit<ContentQuery,"sort"|"activity_from"|"activity_until">;

/** Read a prepared bucket; omitted ID selects the kind's first configured bucket. */
export function popularQuery(bucket: string | PopularDateRange = "", filters: PopularQuery = {}): ContentQuery {
 return typeof bucket === "string" ? {...filters,sort: bucket ? `popular:${bucket}` : "popular"} : {...filters,sort:"popular",activity_from:bucket.from,activity_until:bucket.until};
}

/** Rank a whole-day range using prepared counts, never raw processing. */
export function popularBetween(from:string,until:string,filters:PopularQuery & {kind:string}):ContentQuery {
 return popularQuery({from,until},filters);
}

export type SearchQuery = ContentFilterQuery &
  ReaderQuery &
  CardQuery &
  PageQuery &
  NumberedPageQuery & {
    q: string;
    sort?: "relevance" | ListSort;
    personalize?: boolean;
    summary?: boolean;
  };
export interface RecentSearchesQuery {
  /** 10 by default, at most 100. */
  limit?: number;
}
export type RailQuery = ContentFilterQuery & ReaderQuery & CardQuery & { limit?: number; summary?: boolean };
export type ReleaseDatesQuery = ContentFilterQuery & ReaderQuery & PageQuery & { by?: "month" | "day" };
/** Complete daily metadata for one calendar window, without pagination. */
export type DailyReleasesQuery = Omit<ContentFilterQuery, "released_on" | "tz"> & ReaderQuery & CardQuery & {
  days?: number;
  /** Inclusive YYYY-MM-DD in time_zone; today by default. Future dates are rejected. */
  end_date?: string;
  time_zone?: string;
};
/** Explicit known IDs under current reader policy. */
export type KnownCardsQuery = ReaderQuery & CardQuery & { ids: readonly string[]; kind?: string; summary?: boolean };
export type HistoryQuery = ReaderQuery & CardQuery & PageQuery & { kind?: string; status?: "seen" | "in_progress" | "completed"; summary?: boolean };
/** Continue reading and watching: started, not finished, newest first. */
export type ContinueQuery = ReaderQuery & CardQuery & PageQuery & { kind?: string };
export interface TypeaheadQuery extends ReaderQuery {
  q: string;
  /** Only contents of this kind. */
  kind?: string;
  /** The group kinds suggested; every kind with a page by default. */
  entity_kinds?: readonly string[];
  /** Per section: 8 by default, at most 20. */
  limit?: number;
}

/** Private editor navigation. Each requested section must be chosen explicitly. */
export interface AdminSuggestionsQuery {
  q: string;
  display_language?: string;
  content_kinds?: readonly string[];
  entity_kinds?: readonly string[];
  include_contents: boolean;
  include_entities: boolean;
  /** Per section: 8 by default, at most 20. */
  limit?: number;
}

// Comma-joined list parameters; `attribute` repeats instead.
const LISTS = new Set(["ids", "groups", "creators", "access", "tiers", "traits", "card_entities", "entity_kinds", "content_kinds"]);

/** A query with its list parameters comma-joined, as discovery routes read them. */
export function query(q: object): Record<string, string | number | boolean | readonly string[] | undefined> {
  const out: Record<string, string | number | boolean | readonly string[] | undefined> = {};
  for (const [k, v] of Object.entries(q)) out[k] = LISTS.has(k) && Array.isArray(v) ? (v.length ? v.join(",") : undefined) : v;
  return out;
}

/**
 * Discovery: lists, search, typeahead, similar and recommended contents,
 * release dates, the reader's content filters, history, continue reading,
 * hidden contents and activity reports.
 */
export class DiscoveryClient {
  /** Local anonymous history, isolated from authenticated server history. */
  readonly browserSearches: BrowserSearchHistory;

  constructor(private readonly http: Http) {
    this.browserSearches = browserSearchHistory(http.mount("discovery"));
  }

  /** Content unlocked by the reader's purchased entitlements. */
  library(q: { language?: string; cursor?: string; limit?: number } = {}, signal?: AbortSignal): Promise<LibraryPage> {
    return call(this.http, "GET", "/me/library", { query: q, signal });
  }

  /** Contents as cards, one per content item with the version that fits the reader. */
  list(q: ContentQuery = {}, signal?: AbortSignal): Promise<ContentPage> {
    return call(this.http, "GET", "/contents", { query: query(q) as never, signal });
  }
  /** Hydrates at most 100 known IDs in caller order; records no engagement. */
  knownCards(q: KnownCardsQuery, signal?: AbortSignal): Promise<ContentPage> {
    return call(this.http, "GET", "/content-cards", { query: query(q) as never, signal });
  }
  /** A first page is kept among the reader's recent searches. */
  async search(q: SearchQuery, signal?: AbortSignal): Promise<SearchPage> {
    const emit = q.cursor || (q.page ?? 1) > 1 ? null : this.http.captureChanges();
    const out: SearchPage = await call(this.http, "GET", "/search", { query: query(q) as never, signal });
    emit?.({ type: "searches.changed" });
    return out;
  }
  /** The reader's recent searches, newest first, since they last cleared them; signed out, the browser's anonymous id's. */
  recentSearches(q: RecentSearchesQuery = {}, signal?: AbortSignal): Promise<RecentSearches> {
    return call(this.http, "GET", "/me/searches", { query: q, signal });
  }
  /** Clears one recent search, or all of them. */
  async clearSearches(o: { query?: string } = {}): Promise<void> {
    const emit = this.http.captureChanges();
    await call(this.http, "DELETE", "/me/searches", { query: o });
    emit({ type: "searches.changed" });
  }
  /** Contents and groups while the reader types. */
  typeahead(q: TypeaheadQuery, signal?: AbortSignal): Promise<Typeahead> {
    return call(this.http, "GET", "/typeahead", { query: query(q) as never, signal });
  }
  /** Current names of authorized draft/hidden content and entities; no public search fallback. */
  adminSuggestions(q: AdminSuggestionsQuery, signal?: AbortSignal): Promise<Typeahead> {
    return call(this.http, "GET", "/manage/search-suggestions", { query: query(q) as never, signal });
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
    this.http.acceptGuestFilters(result.filters);
    return result;
  }
  /** Atomically changes submitted shared choices; omitted settings remain intact. */
  async setContentFilters(update: ContentFiltersUpdate, language?: string): Promise<ContentFilters> {
    const emit = this.http.captureChanges();
    const out: ContentFilters = await call(this.http, "PATCH", "/me/content-filters", { query: { language }, body: update });
    emit({ type: "contentFilters.changed" });
    return out;
  }
  /** What the reader opened, most recent first. */
  history(q: HistoryQuery = {}, signal?: AbortSignal): Promise<ContentPage> {
    return call(this.http, "GET", "/me/history", { query: query(q) as never, signal });
  }
  /** Continue reading and watching: contents started but not finished, newest first, each card with its position. */
  continue(q: ContinueQuery = {}, signal?: AbortSignal): Promise<ContentPage> {
    return call(this.http, "GET", "/me/continue", { query: query(q) as never, signal });
  }
  /** Erases the reader's history: of one content item, one kind or everything. Hidden contents stay hidden. */
  async forgetHistory(o: { kind?: string; content_id?: string } = {}): Promise<void> {
    const emit = this.http.captureChanges();
    await call(this.http, "DELETE", "/me/history", { query: o });
    emit({ type: "history.changed" });
  }
  /** Hides a content item from every list the reader is served ("not interested"). */
  async hide(id: string): Promise<void> {
    const emit = this.http.captureChanges();
    await call(this.http, "PUT", "/contents/{id}/hidden", { params: { id } });
    emit({ type: "hidden.changed", ids: [id] });
  }
  /** Shows a hidden content item again. */
  async unhide(id: string): Promise<void> {
    const emit = this.http.captureChanges();
    await call(this.http, "DELETE", "/contents/{id}/hidden", { params: { id } });
    emit({ type: "hidden.changed", ids: [id] });
  }
  /** Opens and progress reports; ActivityReporter batches them. */
  activity(events: ActivityEvent[], o: { keepalive?: boolean } = {}): Promise<void> {
    const body: Activity = { events, language: this.http.language() };
    return call(this.http, "POST", "/activity", { body, keepalive: o.keepalive });
  }
}
