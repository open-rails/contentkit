import { expectTypeOf, it } from "vitest";
import type { ContentQuery, HistoryQuery, RailQuery, ReleaseDatesQuery, SearchQuery, TypeaheadQuery } from "./discovery.js";
import type { Method, RouteQuery } from "./route.js";

// Parameters the route takes that the type lacks, and the reverse; both never.
type Gap<T, M extends Method, P extends string> = Exclude<keyof RouteQuery<M, P>, keyof T> | Exclude<keyof T, keyof RouteQuery<M, P>>;

it("discovery query types take exactly their routes' parameters (checked by tsc)", () => {
  expectTypeOf<Gap<ContentQuery, "GET", "/contents">>().toEqualTypeOf<never>();
  expectTypeOf<Gap<SearchQuery, "GET", "/search">>().toEqualTypeOf<never>();
  expectTypeOf<Gap<TypeaheadQuery, "GET", "/typeahead">>().toEqualTypeOf<never>();
  expectTypeOf<Gap<RailQuery, "GET", "/contents/{id}/similar">>().toEqualTypeOf<never>();
  expectTypeOf<Gap<RailQuery, "GET", "/recommendations">>().toEqualTypeOf<never>();
  expectTypeOf<Gap<ReleaseDatesQuery, "GET", "/release-dates">>().toEqualTypeOf<never>();
  expectTypeOf<Gap<HistoryQuery, "GET", "/me/history">>().toEqualTypeOf<never>();
});
