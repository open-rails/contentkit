// ContentKit content URLs: [/{lang}]/{route}/{CODE}[/{slug}]. The code is the
// only part the server reads; the slug is decoration. Mirrors Go's contenturl
// (the two share contenturl/testdata/vectors.json).

export const CODE_LENGTH = 9;

/** Crockford base32: digits and uppercase letters without I, L, O, U. */
export const CODE_ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";

/**
 * Reads a code with Crockford's decoding rules (any case, O as 0, I and L as 1,
 * hyphens ignored) and returns its canonical form, or null when the input is
 * not a code. An all-digit result is never a code (it reads as a legacy id).
 */
export function parseCode(input: string): string | null {
  let out = "";
  for (const ch of input) {
    if (ch === "-") continue;
    if (ch.length !== 1 || ch.charCodeAt(0) > 0x7f) return null;
    let c = ch.toUpperCase();
    if (c === "O") c = "0";
    else if (c === "I" || c === "L") c = "1";
    if (!CODE_ALPHABET.includes(c) || out.length === CODE_LENGTH) return null;
    out += c;
  }
  if (out.length !== CODE_LENGTH || !/[A-Z]/.test(out)) return null;
  return out;
}

/** Reports whether input is a code in canonical form. */
export function isCode(input: string): boolean {
  return parseCode(input) === input;
}

/** A resolved content link, as ContentKit and host APIs return it. */
export interface ContentLink {
  content_kind: string;
  code: string;
  slug: string;
  /** Localized slugs keyed by language; the default slug applies otherwise. */
  slugs?: Record<string, string>;
}

/** The slug for language, falling back to the default slug. */
export function slugFor(link: Pick<ContentLink, "slug" | "slugs">, language = ""): string {
  return (language && link.slugs?.[language]) || link.slug;
}

export type QueryInit = string | URLSearchParams | Record<string, string | number | boolean | null | undefined>;

export interface PathOptions {
  /** Language prefix segment; omitted when empty. */
  language?: string;
  /** Query string for sub-pages, e.g. { p: 12 }. Null and undefined values are dropped. */
  query?: QueryInit;
}

function queryString(query: QueryInit | undefined): string {
  if (query === undefined) return "";
  let params: URLSearchParams;
  if (typeof query === "string" || query instanceof URLSearchParams) {
    params = new URLSearchParams(query);
  } else {
    params = new URLSearchParams();
    for (const [k, v] of Object.entries(query)) {
      if (v !== null && v !== undefined) params.append(k, String(v));
    }
  }
  const s = params.toString();
  return s ? `?${s}` : "";
}

/** Joins [/{language}]/{route}/{code}[/{slug}][?query]. */
export function contentPath(route: string, code: string, slug = "", options: PathOptions = {}): string {
  const prefix = options.language ? `/${options.language}` : "";
  return `${prefix}/${route}/${code}${slug ? `/${slug}` : ""}${queryString(options.query)}`;
}

export interface ContentURLConfig {
  /** Content kind -> the host's route segment ({ video: "watch", gallery: "g" }). */
  routes: Record<string, string>;
  /** Language segments a path may start with; kept in canonical paths. */
  languages?: string[];
  /** The site's origin ("https://example.com") for absolute URLs (canonical links, og:url). */
  origin?: string;
}

export interface ParsedContentPath {
  /** "" when unprefixed. */
  language: string;
  route: string;
  /** Canonical form. */
  code: string;
  rawCode: string;
  /** As written; "" when absent. */
  slug: string;
}

export interface CanonicalResult {
  /** The canonical path. */
  path: string;
  /** path plus the location's query. */
  location: string;
  /** The location's path is not canonical: replace it with location. */
  redirect: boolean;
}

export interface ContentURLs {
  /** The canonical path of link; throws when its kind has no route. */
  path(link: ContentLink, options?: PathOptions): string;
  /** path() on the configured origin; throws without one. */
  url(link: ContentLink, options?: PathOptions): string;
  /** Splits a pathname; null when it is not a content path (or its code is invalid). */
  parse(pathname: string): ParsedContentPath | null;
  /**
   * Compares a location (pathname plus optional ?query and #hash) with link's
   * canonical path, for a client-side history.replaceState after the page's
   * data arrives. null when the location is not a content path or link's kind
   * has no route.
   */
  canonical(location: string, link: ContentLink): CanonicalResult | null;
}

const SEGMENT = /^[a-z0-9][a-z0-9_-]*$/;

/** Builds the URL helpers for one host's routes. */
export function createContentURLs(config: ContentURLConfig): ContentURLs {
  const routes = { ...config.routes };
  const origin = config.origin?.replace(/\/+$/, "");
  if (origin !== undefined && !/^https?:\/\/[^/?#]+$/.test(origin)) throw new Error(`contentkit-ui/urls: origin ${JSON.stringify(config.origin)} is not an http(s) origin`);
  const languages = [...(config.languages ?? [])];
  const routeSet = new Set(Object.values(routes));
  if (routeSet.size === 0) throw new Error("contentkit-ui/urls: routes is empty");
  for (const r of [...routeSet, ...languages]) {
    if (!SEGMENT.test(r)) throw new Error(`contentkit-ui/urls: ${JSON.stringify(r)} is not one lowercase path segment`);
  }
  for (const l of languages) {
    if (routeSet.has(l)) throw new Error(`contentkit-ui/urls: language ${JSON.stringify(l)} is also a route`);
  }

  const canonicalPath = (link: ContentLink, language: string): string | null => {
    const route = routes[link.content_kind];
    return route === undefined ? null : contentPath(route, link.code, slugFor(link, language), { language });
  };

  const parse = (pathname: string): ParsedContentPath | null => {
    if (!pathname.startsWith("/")) return null;
    let rest = pathname.slice(1);
    if (rest.endsWith("/")) rest = rest.slice(0, -1);
    let segs = rest.split("/");
    if (segs.some((s) => s === "")) return null;
    let language = "";
    if (segs.length > 2 && languages.includes(segs[0]!)) {
      language = segs[0]!;
      segs = segs.slice(1);
    }
    if (segs.length < 2 || segs.length > 3 || !routeSet.has(segs[0]!)) return null;
    const code = parseCode(segs[1]!);
    if (code === null) return null;
    return { language, route: segs[0]!, code, rawCode: segs[1]!, slug: segs[2] ?? "" };
  };

  const path = (link: ContentLink, options: PathOptions = {}): string => {
    const route = routes[link.content_kind];
    if (route === undefined) throw new Error(`contentkit-ui/urls: no route for kind ${JSON.stringify(link.content_kind)}`);
    return contentPath(route, link.code, slugFor(link, options.language), options);
  };

  return {
    path,
    url(link, options) {
      if (origin === undefined) throw new Error("contentkit-ui/urls: url() needs ContentURLConfig.origin");
      return origin + path(link, options);
    },
    parse,
    canonical(location, link) {
      const noHash = location.split("#", 1)[0]!;
      const q = noHash.indexOf("?");
      const pathname = q < 0 ? noHash : noHash.slice(0, q);
      const search = q < 0 ? "" : noHash.slice(q + 1);
      const parsed = parse(pathname);
      if (parsed === null) return null;
      const path = canonicalPath(link, parsed.language);
      if (path === null) return null;
      return { path, location: search ? `${path}?${search}` : path, redirect: pathname !== path };
    },
  };
}
