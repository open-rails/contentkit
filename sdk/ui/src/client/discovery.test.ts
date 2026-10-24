import { expect, expectTypeOf, it, vi } from "vitest";
import { createContentKitClient } from "./client.js";
import type { MineQuery } from "./content/favorites.js";
import type { ContentQuery, DailyReleasesQuery, HistoryQuery, KnownCardsQuery, RailQuery, RecentSearchesQuery, ReleaseDatesQuery, SearchQuery, TypeaheadQuery } from "./discovery.js";
import { popularBetween, popularQuery } from "./discovery.js";
import type { Method, RouteQuery } from "./route.js";

// Parameters the route takes that the type lacks, and the reverse; both never.
type Gap<T, M extends Method, P extends string> = Exclude<keyof RouteQuery<M, P>, keyof T> | Exclude<keyof T, keyof RouteQuery<M, P>>;

it("queries historical activity with exclusive end date and no publication-age filter", async () => {
  const requests: URL[] = [];
  const client = createContentKitClient({ baseUrl: "https://catalog.test/app", fetch: async (input) => {
    requests.push(new URL(String(input)));
    return Response.json({ contents: [] });
  } });
  await client.discovery.list(popularBetween("2026-04-01", "2026-05-01", { kind: "gallery", language: "en" }));
  expect(Object.fromEntries(requests[0]!.searchParams)).toMatchObject({
    sort: "popular", activity_from: "2026-04-01", activity_until: "2026-05-01", kind: "gallery", language: "en",
  });
  expect(requests[0]!.searchParams.has("published_within")).toBe(false);
});

it("accepts the same explicit activity window in the popularity selector query", () => {
  expect(popularQuery({ from: "2026-04-01", until: "2026-05-01" }, { kind: "gallery" })).toEqual(
    popularBetween("2026-04-01", "2026-05-01", { kind: "gallery" }),
  );
});

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
  expectTypeOf<Gap<DailyReleasesQuery, "GET", "/releases">>().toEqualTypeOf<never>();
  expectTypeOf<Gap<HistoryQuery, "GET", "/me/history">>().toEqualTypeOf<never>();
  expectTypeOf<Gap<KnownCardsQuery, "GET", "/content-cards">>().toEqualTypeOf<never>();
  expectTypeOf<Gap<RecentSearchesQuery, "GET", "/me/searches">>().toEqualTypeOf<never>();
  expectTypeOf<Gap<MineQuery, "GET", "/me/favorites">>().toEqualTypeOf<never>();
});

it("sends selection summary and bounded public card hydration IDs as ordinary query fields", async () => {
  const requests: URL[] = [];
  const client = createContentKitClient({ baseUrl: "https://catalog.test/app", fetch: async (input) => {
    requests.push(new URL(String(input)));
    return Response.json({ contents: [], summary: { filtered_count: 3, filtered_entities: [], locked_count: 2 } });
  } });
  const out = await client.discovery.list({ ids: ["first", "second"], summary: true, mode: "lock" });
  expect(requests[0]!.searchParams.get("ids")).toBe("first,second");
  expect(requests[0]!.searchParams.get("summary")).toBe("true");
  expect(out.summary?.locked_count).toBe(2);
});
