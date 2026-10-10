import { describe, expect, it } from "vitest";

import vectors from "../../../contenturl/testdata/vectors.json" with { type: "json" };
import { type ContentLink, contentPath, createContentURLs, isCode, parseCode, slugFor } from "../src/index.ts";

describe("codes (shared vectors)", () => {
  for (const c of vectors.codes) {
    it(JSON.stringify(c.in), () => {
      expect(parseCode(c.in)).toBe(c.out);
      if (c.out !== null) expect(isCode(c.out)).toBe(true);
    });
  }
  it("accepts only the canonical form as a code", () => {
    expect(isCode("G4VRQ3ZQ5")).toBe(true);
    expect(isCode("g4vrq3zq5")).toBe(false);
  });
});

describe("paths (shared vectors)", () => {
  const urls = createContentURLs({ routes: vectors.paths.routes, languages: vectors.paths.languages });
  for (const c of vectors.paths.cases) {
    it(c.name, () => {
      const location = c.query ? `${c.path}?${c.query}` : c.path;
      if (c.link === null) {
        expect(urls.parse(c.path)).toBeNull();
        return;
      }
      const got = urls.canonical(location, c.link as ContentLink);
      if (!c.matched) {
        expect(got).toBeNull();
        return;
      }
      expect(got).toEqual({ path: c.location!.split("?")[0], location: c.location, redirect: c.redirect });
    });
  }
});

describe("building", () => {
  const urls = createContentURLs({ routes: { video: "watch", gallery: "g" }, languages: ["en", "es"] });
  const link: ContentLink = { content_kind: "gallery", code: "G4VRQ3ZQ5", slug: "a-title", slugs: { es: "un-titulo" } };

  it("joins paths with sub-page queries", () => {
    expect(urls.path(link)).toBe("/g/G4VRQ3ZQ5/a-title");
    expect(urls.path(link, { language: "es", query: { p: 12, v: undefined } })).toBe("/es/g/G4VRQ3ZQ5/un-titulo?p=12");
    expect(contentPath("watch", "G4VRQ3ZQ5")).toBe("/watch/G4VRQ3ZQ5");
    expect(contentPath("watch", "G4VRQ3ZQ5", "x", { query: "t=30" })).toBe("/watch/G4VRQ3ZQ5/x?t=30");
    expect(slugFor(link, "fr")).toBe("a-title");
  });

  it("ignores a hash and keeps the query when canonicalizing", () => {
    expect(urls.canonical("/g/g4vrq3zq5?p=3#top", link)).toEqual({
      path: "/g/G4VRQ3ZQ5/a-title",
      location: "/g/G4VRQ3ZQ5/a-title?p=3",
      redirect: true,
    });
  });

  it("refuses links and configs it cannot express", () => {
    expect(() => urls.path({ content_kind: "comment", code: "G4VRQ3ZQ5", slug: "" })).toThrow(/no route/);
    expect(() => createContentURLs({ routes: {} })).toThrow(/empty/);
    expect(() => createContentURLs({ routes: { video: "Watch" } })).toThrow(/path segment/);
    expect(() => createContentURLs({ routes: { video: "watch" }, languages: ["watch"] })).toThrow(/also a route/);
  });
});
