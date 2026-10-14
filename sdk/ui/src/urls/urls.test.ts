import { describe, expect, it } from "vitest";

import vectors from "../../../../contenturl/testdata/vectors.json" with { type: "json" };
import {
  type ContentLink,
  canonicalURL,
  contentPath,
  createContentURLs,
  isCode,
  languagePrefix,
  parseCode,
  parseVersionCode,
  slugFor,
  versionParam,
  withVersion,
} from "./index.js";

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

describe("version codes (shared vectors)", () => {
  for (const c of vectors.version_codes) {
    it(JSON.stringify(c.in), () => expect(parseVersionCode(c.in)).toBe(c.out));
  }
  it("reads and writes ?v=, keeping the rest of the location", () => {
    expect(versionParam("/en/g/G4VRQ3ZQ5/a?p=3&v=k3f9#top")).toBe("K3F9");
    expect(versionParam("?v=0000")).toBeNull();
    expect(versionParam(new URLSearchParams("v=K3F9"))).toBe("K3F9");
    expect(versionParam("/en/g/G4VRQ3ZQ5/a")).toBeNull();
    expect(withVersion("/en/g/G4VRQ3ZQ5/a?p=3#top", "K3F9")).toBe("/en/g/G4VRQ3ZQ5/a?p=3&v=K3F9#top");
    expect(withVersion("/en/g/G4VRQ3ZQ5/a?v=K3F9&p=3", "AB12")).toBe("/en/g/G4VRQ3ZQ5/a?v=AB12&p=3");
    expect(withVersion("/en/g/G4VRQ3ZQ5/a?v=K3F9", null)).toBe("/en/g/G4VRQ3ZQ5/a");
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
      const got = urls.canonical(location, c.link as ContentLink, { language: "negotiated" in c ? c.negotiated : undefined });
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

  it("joins paths with sub-page queries and a version", () => {
    expect(urls.path(link)).toBe("/en/g/G4VRQ3ZQ5/a-title");
    expect(urls.path(link, { language: "es", query: { p: 12, x: undefined } })).toBe("/es/g/G4VRQ3ZQ5/un-titulo?p=12");
    expect(urls.path(link, { language: "es", version: "K3F9" })).toBe("/es/g/G4VRQ3ZQ5/un-titulo?v=K3F9");
    expect(urls.path(link, { query: { p: 2 }, version: "K3F9" })).toBe("/en/g/G4VRQ3ZQ5/a-title?p=2&v=K3F9");
    expect(contentPath("watch", "G4VRQ3ZQ5", "x", { language: "en", query: "t=30" })).toBe("/en/watch/G4VRQ3ZQ5/x?t=30");
    expect(slugFor(link, "fr")).toBe("a-title");
  });

  it("prefixes every language, the default included, as the server does", () => {
    expect(urls.path(link, { language: "en" })).toBe("/en/g/G4VRQ3ZQ5/a-title");
    expect(urls.path({ ...link, slugs: { en: "en-title" } })).toBe("/en/g/G4VRQ3ZQ5/en-title");
    expect(urls.path(link, { language: "fr" })).toBe("/fr/g/G4VRQ3ZQ5/a-title");
    expect([languagePrefix(["en", "es"], "en"), languagePrefix(["en", "es"], "es"), languagePrefix(["en", "es"]), languagePrefix([], "es")]).toEqual(["en", "es", "en", "es"]);
    expect(urls.canonical("/en/g/G4VRQ3ZQ5/a-title", link)).toEqual({ path: "/en/g/G4VRQ3ZQ5/a-title", location: "/en/g/G4VRQ3ZQ5/a-title", redirect: false });
  });

  it("ignores a hash and keeps the query when canonicalizing an unprefixed location", () => {
    expect(urls.canonical("/g/g4vrq3zq5?p=3&v=K3F9#top", link)).toEqual({
      path: "/en/g/G4VRQ3ZQ5/a-title",
      location: "/en/g/G4VRQ3ZQ5/a-title?p=3&v=K3F9",
      redirect: true,
    });
    expect(urls.canonical("/g/G4VRQ3ZQ5", link, { language: "es" })?.location).toBe("/es/g/G4VRQ3ZQ5/un-titulo");
    expect(urls.canonical("/g/G4VRQ3ZQ5", link, { language: "fr" })?.location).toBe("/en/g/G4VRQ3ZQ5/a-title");
  });

  it("refuses links and configs it cannot express", () => {
    expect(() => urls.path({ content_kind: "comment", code: "G4VRQ3ZQ5", slug: "" })).toThrow(/no route/);
    expect(() => createContentURLs({ routes: {}, languages: ["en"] })).toThrow(/routes is empty/);
    expect(() => createContentURLs({ routes: { video: "watch" }, languages: [] })).toThrow(/languages is empty/);
    expect(() => createContentURLs({ routes: { video: "Watch" }, languages: ["en"] })).toThrow(/path segment/);
    expect(() => createContentURLs({ routes: { video: "watch" }, languages: ["watch"] })).toThrow(/also a route/);
    expect(() => createContentURLs({ routes: { video: "watch" }, languages: ["en"], origin: "example.com" })).toThrow(/origin/);
  });

  it("builds absolute URLs on the configured origin", () => {
    const site = createContentURLs({ routes: { gallery: "g" }, languages: ["en", "es"], origin: "https://example.com/" });
    expect(site.url(link, { language: "es" })).toBe("https://example.com/es/g/G4VRQ3ZQ5/un-titulo");
    expect(() => urls.url(link)).toThrow(/origin/);
  });
});

describe("hreflang and canonical URLs", () => {
  const link: ContentLink = { content_kind: "video", code: "G4VRQ3ZQ5", slug: "night-run", slugs: { es: "carrera-nocturna" } };
  const urls = createContentURLs({ routes: { video: "watch" }, languages: ["en", "es", "ja"], origin: "https://example.com/" });

  it("lists each language's URL with its own slug, and x-default the default language's", () => {
    expect(urls.hreflang(link)).toEqual({
      en: "https://example.com/en/watch/G4VRQ3ZQ5/night-run",
      es: "https://example.com/es/watch/G4VRQ3ZQ5/carrera-nocturna",
      ja: "https://example.com/ja/watch/G4VRQ3ZQ5/night-run",
      "x-default": "https://example.com/en/watch/G4VRQ3ZQ5/night-run",
    });
    expect(urls.hreflang(link, { defaultLanguage: "es" })["x-default"]).toBe("https://example.com/es/watch/G4VRQ3ZQ5/carrera-nocturna");
    expect(urls.hreflang(link, { languages: ["es", "fr"], origin: "https://other.test" })).toEqual({
      es: "https://other.test/es/watch/G4VRQ3ZQ5/carrera-nocturna",
      "x-default": "https://other.test/en/watch/G4VRQ3ZQ5/night-run",
    });
    expect(() => createContentURLs({ routes: { video: "watch" }, languages: ["en"] }).hreflang(link)).toThrow(/origin/);
    expect(urls.languages).toEqual(["en", "es", "ja"]);
    expect(urls.origin).toBe("https://example.com");
  });

  it("drops tracking parameters, the fragment and a trailing slash", () => {
    expect(canonicalURL("https://example.com/blog/?utm_source=x&page=2&gclid=1&fbclid=2#top")).toBe("https://example.com/blog?page=2");
    expect(canonicalURL("https://example.com/?ref=a", { drop: ["ref"] })).toBe("https://example.com/");
  });
});
