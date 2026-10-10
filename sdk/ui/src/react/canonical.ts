import { useContext, useEffect, useMemo } from "react";
import type { ContentLink } from "../urls/index.js";
import { ContentKitContext, useContentURLs } from "./context.js";

export interface CanonicalContentOptions {
  /** og:title. */
  title?: string;
  /** og:image, an absolute URL. */
  image?: string;
  /**
   * The current location (pathname, search, hash). Default
   * window.location; pass the router's so a navigation checks again.
   */
  location?: string;
  /** Languages the content exists in, for hreflang; default every configured language. */
  languages?: readonly string[];
  /** The language hreflang x-default points at. */
  defaultLanguage?: string;
}

export interface CanonicalContent {
  /** The canonical path in the page's language; undefined until link arrives. */
  path?: string;
  /** Its absolute URL. */
  url?: string;
  /** hreflang alternates by language (and x-default). */
  alternates: Record<string, string>;
}

type Tag = { selector: string; create: () => HTMLElement; attr: string; value: string };

const meta = (property: string, value: string): Tag => ({
  selector: `meta[property="${property}"]`,
  create: () => withAttr(document.createElement("meta"), "property", property),
  attr: "content",
  value,
});

function withAttr<E extends HTMLElement>(el: E, name: string, value: string): E {
  el.setAttribute(name, value);
  return el;
}

const link = (rel: string, value: string, hreflang?: string): Tag => ({
  selector: hreflang ? `link[rel="${rel}"][hreflang="${hreflang}"]` : `link[rel="${rel}"]:not([hreflang])`,
  create: () => {
    const el = withAttr(document.createElement("link"), "rel", rel);
    if (hreflang) el.setAttribute("hreflang", hreflang);
    return el;
  },
  attr: "href",
  value,
});

/**
 * A content page's canonical URL once its data (link) arrives: replaces the
 * address with the canonical path (code spelling, merged code, current slug)
 * through the provider's navigate (else history.replaceState), and keeps
 * `<link rel=canonical>`, og:url, og:title, og:image and hreflang alternates
 * (each language's own slug) in the head while mounted. Needs the
 * provider's `urls`.
 */
export function useCanonicalContent(content: ContentLink | null | undefined, o: CanonicalContentOptions = {}): CanonicalContent {
  const urls = useContentURLs();
  const navigate = useContext(ContentKitContext)?.navigate;
  const at = o.location ?? (typeof location === "undefined" ? "" : location.pathname + location.search + location.hash);
  const kind = content?.content_kind;
  const code = content?.code;
  const slug = content?.slug;
  const slugs = JSON.stringify(content?.slugs ?? {});
  const languages = o.languages?.join(",");
  const lnk = useMemo(
    () => (kind && code ? ({ content_kind: kind, code, slug: slug ?? "", slugs: JSON.parse(slugs) as Record<string, string> } satisfies ContentLink) : null),
    [kind, code, slug, slugs],
  );

  const result = useMemo((): CanonicalContent & { redirect?: string } => {
    if (!lnk) return { alternates: {} };
    const origin = urls.origin ?? (typeof location === "undefined" ? undefined : location.origin);
    const hash = at.includes("#") ? at.slice(at.indexOf("#")) : "";
    const c = urls.canonical(at, lnk);
    const path = c?.path ?? urls.path(lnk);
    const alternates = origin ? urls.hreflang(lnk, { origin, languages: languages?.split(","), defaultLanguage: o.defaultLanguage }) : {};
    return { path, url: origin ? origin + path : undefined, alternates, redirect: c?.redirect ? c.location + hash : undefined };
  }, [lnk, at, urls, languages, o.defaultLanguage]);

  const { redirect } = result;
  useEffect(() => {
    if (!redirect) return;
    if (navigate) navigate(redirect, { replace: true });
    else history.replaceState(history.state, "", redirect);
  }, [redirect, navigate]);

  const { url, alternates } = result;
  const { title, image } = o;
  const alternatesKey = JSON.stringify(alternates);
  useEffect(() => {
    if (!url || typeof document === "undefined") return;
    const tags: Tag[] = [link("canonical", url), meta("og:url", url)];
    if (title) tags.push(meta("og:title", title));
    if (image) tags.push(meta("og:image", image));
    for (const [lang, href] of Object.entries(JSON.parse(alternatesKey) as Record<string, string>)) tags.push(link("alternate", href, lang));
    const undo: (() => void)[] = [];
    for (const t of tags) {
      const found = document.head.querySelector<HTMLElement>(t.selector);
      if (found) {
        const before = found.getAttribute(t.attr);
        found.setAttribute(t.attr, t.value);
        undo.push(() => (before === null ? found.removeAttribute(t.attr) : found.setAttribute(t.attr, before)));
      } else {
        const el = withAttr(t.create(), t.attr, t.value);
        document.head.appendChild(el);
        undo.push(() => el.remove());
      }
    }
    return () => undo.forEach((u) => u());
  }, [url, title, image, alternatesKey]);

  return { path: result.path, url: result.url, alternates: result.alternates };
}
