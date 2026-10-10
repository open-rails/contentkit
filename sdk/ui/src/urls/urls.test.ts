import { describe, expect, it } from "vitest";

import vectors from "../../../../contenturl/testdata/vectors.json" with { type: "json" };
import { type ContentLink, canonicalURL, contentPath, createContentURLs, isCode, parseCode, slugFor } from "./index.js";

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
    expect(() => createContentURLs({ routes: { video: "watch" }, origin: "example.com" })).toThrow(/origin/);
  });

  it("builds absolute URLs on the configured origin", () => {
    const site = createContentURLs({ routes: { gallery: "g" }, languages: ["es"], origin: "https://example.com/" });
    expect(site.url(link, { language: "es" })).toBe("https://example.com/es/g/G4VRQ3ZQ5/un-titulo");
    expect(() => urls.url(link)).toThrow(/origin/);
  });
});

describe("hreflang and canonical URLs", () => {
  const link: ContentLink = { content_kind: "video", code: "G4VRQ3ZQ5", slug: "night-run", slugs: { es: "carrera-nocturna" } };
  const urls = createContentURLs({ routes: { video: "watch" }, languages: ["en", "es", "ja"], origin: "https://example.com/" });

  it("lists each language's URL with its own slug, and x-default", () => {
    expect(urls.hreflang(link, { defaultLanguage: "en" })).toEqual({
      en: "https://example.com/en/watch/G4VRQ3ZQ5/night-run",
      es: "https://example.com/es/watch/G4VRQ3ZQ5/carrera-nocturna",
      ja: "https://example.com/ja/watch/G4VRQ3ZQ5/night-run",
      "x-default": "https://example.com/en/watch/G4VRQ3ZQ5/night-run",
    });
    expect(urls.hreflang(link, { languages: ["es", "fr"], origin: "https://other.test" })).toEqual({ es: "https://other.test/es/watch/G4VRQ3ZQ5/carrera-nocturna" });
    expect(() => createContentURLs({ routes: { video: "watch" } }).hreflang(link)).toThrow(/origin/);
    expect(urls.languages).toEqual(["en", "es", "ja"]);
    expect(urls.origin).toBe("https://example.com");
  });

  it("drops tracking parameters, the fragment and a trailing slash", () => {
    expect(canonicalURL("https://example.com/blog/?utm_source=x&page=2&gclid=1&fbclid=2#top")).toBe("https://example.com/blog?page=2");
    expect(canonicalURL("https://example.com/?ref=a", { drop: ["ref"] })).toBe("https://example.com/");
  });
});
