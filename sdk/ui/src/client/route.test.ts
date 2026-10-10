import { expect, it } from "vitest";
import { CONTENTKIT_ROUTES } from "./generated/routes.js";
import { Http } from "./http.js";
import { routeURL } from "./route.js";

it("names every route once by method and path", () => {
  const keys = CONTENTKIT_ROUTES.map((r) => `${r.method} ${r.path}`);
  expect(new Set(keys).size).toBe(keys.length);
});

it("fills params, encodes them, repeats list queries and drops empty ones, under each module's mount", () => {
  const h = new Http({ baseUrl: "/api/ck/", mounts: { taxonomy: "https://tax.example/t" } });
  expect(routeURL(h, "GET", "/{kind}/{id}/comments", { params: { kind: "video", id: "a b/c" }, query: { sort: "best", limit: 10, offset: 0 } })).toBe(
    "/api/ck/video/a%20b%2Fc/comments?sort=best&limit=10&offset=0",
  );
  expect(routeURL(h, "GET", "/posts/admin", { query: { draft: false, language: "" } })).toBe("/api/ck/posts/admin?draft=false");
  expect(routeURL(h, "GET", "/counts", { query: { taxonomy_id: ["a", "b"] } })).toBe("https://tax.example/t/counts?taxonomy_id=a&taxonomy_id=b");
  expect(routeURL(h, "GET", "/{code}", { params: { code: "G4VRQ3ZQ5" } })).toBe("/api/ck/codes/G4VRQ3ZQ5");
  expect(() => routeURL(h, "GET", "/posts/{id}", { params: { id: "" } })).toThrow(/needs id/);
  // @ts-expect-error: the route's wildcard is cid, not id
  expect(() => routeURL(h, "GET", "/comments/{cid}/replies", { params: { id: "1" } })).toThrow(/needs cid/);
  // @ts-expect-error: not a declared route
  expect(() => routeURL(h, "GET", "/nope")).toThrow(/no route/);
});
