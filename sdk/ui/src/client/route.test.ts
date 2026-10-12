import { expect, it } from "vitest";
import { CONTENTKIT_ROUTES } from "./generated/routes.js";
import { Http } from "./http.js";
import { routeURL } from "./route.js";

it("names every route once by method and path", () => {
  const keys = CONTENTKIT_ROUTES.map((r) => `${r.method} ${r.path}`);
  expect(new Set(keys).size).toBe(keys.length);
});

it("fills params, encodes them, repeats list queries and drops empty ones, under each module's mount", () => {
  const h = new Http({ baseUrl: "/api/ck/", mounts: { groups: "https://groups.example/g" } });
  expect(routeURL(h, "GET", "/{kind}/{id}/comments", { params: { kind: "video", id: "a b/c" }, query: { sort: "best", limit: 10, offset: 0 } })).toBe(
    "/api/ck/v1/video/a%20b%2Fc/comments?sort=best&limit=10&offset=0",
  );
  expect(routeURL(h, "GET", "/manage/contents", { query: { kind: "blog", language: "" } })).toBe("/api/ck/v1/manage/contents?kind=blog");
  expect(routeURL(h, "GET", "/groups", { query: { kind: "tag", letter: "#" } })).toBe("https://groups.example/g/groups?kind=tag&letter=%23");
  expect(routeURL(h, "GET", "/{code}", { params: { code: "G4VRQ3ZQ5" } })).toBe("/api/ck/v1/codes/G4VRQ3ZQ5");
  expect(routeURL(h, "GET", "/kinds")).toBe("/api/ck/v1/kinds");
  expect(() => routeURL(h, "GET", "/contents/{id}", { params: { id: "" } })).toThrow(/needs id/);
  // @ts-expect-error: the route's wildcard is cid, not id
  expect(() => routeURL(h, "GET", "/comments/{cid}/replies", { params: { id: "1" } })).toThrow(/needs cid/);
  // @ts-expect-error: not a declared route
  expect(() => routeURL(h, "GET", "/nope")).toThrow(/no route/);
});
