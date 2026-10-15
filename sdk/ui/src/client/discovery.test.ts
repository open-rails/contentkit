import { expect, expectTypeOf, it, vi } from "vitest";
import { createContentKitClient } from "./client.js";
import type { MineQuery } from "./content/favorites.js";
import type { ContentQuery, HistoryQuery, RailQuery, RecentSearchesQuery, ReleaseDatesQuery, SearchQuery, TypeaheadQuery } from "./discovery.js";
import type { Method, RouteQuery } from "./route.js";

// Parameters the route takes that the type lacks, and the reverse; both never.
type Gap<T, M extends Method, P extends string> = Exclude<keyof RouteQuery<M, P>, keyof T> | Exclude<keyof T, keyof RouteQuery<M, P>>;

it("does not invent anonymous server history sessions, but retains explicit sessions", async () => {
  const fetcher = vi.fn(async (_url: RequestInfo | URL) => Response.json({ contents: [], searches: [] }));
  const client = createContentKitClient({ baseUrl: "https://catalog.test/app", fetch: fetcher });
  await client.discovery.search({ q: "guest" });
  await client.discovery.recentSearches();
  await client.discovery.clearSearches();
  expect(fetcher.mock.calls.map(([url]) => new URL(String(url)).searchParams.has("session"))).toEqual([false, false, false]);
  await client.discovery.search({ q: "legacy", session: "explicit-session" });
  await client.discovery.recentSearches({ session: "explicit-session" });
  await client.discovery.clearSearches({ session: "explicit-session" });
  expect(fetcher.mock.calls.slice(3).map(([url]) => new URL(String(url)).searchParams.get("session"))).toEqual(["explicit-session", "explicit-session", "explicit-session"]);
});

it("discovery query types take exactly their routes' parameters (checked by tsc)", () => {
  expectTypeOf<Gap<ContentQuery, "GET", "/contents">>().toEqualTypeOf<never>();
  expectTypeOf<Gap<SearchQuery, "GET", "/search">>().toEqualTypeOf<never>();
  expectTypeOf<Gap<TypeaheadQuery, "GET", "/typeahead">>().toEqualTypeOf<never>();
  expectTypeOf<Gap<RailQuery, "GET", "/contents/{id}/similar">>().toEqualTypeOf<never>();
  expectTypeOf<Gap<RailQuery, "GET", "/recommendations">>().toEqualTypeOf<never>();
  expectTypeOf<Gap<ReleaseDatesQuery, "GET", "/release-dates">>().toEqualTypeOf<never>();
  expectTypeOf<Gap<HistoryQuery, "GET", "/me/history">>().toEqualTypeOf<never>();
  expectTypeOf<Gap<RecentSearchesQuery, "GET", "/me/searches">>().toEqualTypeOf<never>();
  expectTypeOf<Gap<MineQuery, "GET", "/me/favorites">>().toEqualTypeOf<never>();
  expectTypeOf<Gap<MineQuery & { value?: string }, "GET", "/me/reactions">>().toEqualTypeOf<never>();
});
