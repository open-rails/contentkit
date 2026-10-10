// @vitest-environment jsdom
import { renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient, type ContentKitClient } from "../client/client.js";
import { createContentURLs, type ContentLink } from "../urls/index.js";
import { ContentKitProvider, useCanonicalContent, type Navigate } from "./index.js";

// Canonical URLs are client-side only; the client is never called.
const offline = () => createContentKitClient({ baseUrl: "http://unused.invalid" });
const wrap = (c: ContentKitClient, extra: { urls?: ReturnType<typeof createContentURLs>; navigate?: Navigate } = {}) =>
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <ContentKitProvider client={c} {...extra}>
        {children}
      </ContentKitProvider>
    );
  };

afterEach(() => {
  document.head.innerHTML = "";
  history.replaceState(null, "", "/");
});

it("useCanonicalContent replaces a stale address and keeps canonical, og and hreflang tags while mounted", async () => {
  const c = offline();
  const urls = createContentURLs({ routes: { video: "watch" }, languages: ["en", "es"], origin: "https://example.com" });
  const navigate = vi.fn<Navigate>();
  const link: ContentLink = { content_kind: "video", code: "G4VRQ3ZQ5", slug: "night-run", slugs: { es: "carrera-nocturna" } };
  document.head.innerHTML = '<link rel="canonical" href="https://example.com/old">';
  let content: ContentLink | null = null;
  const { result, rerender, unmount } = renderHook(() => useCanonicalContent(content, { title: "Night run", location: "/es/watch/g4vrq3zq5/old-slug?t=12#c", defaultLanguage: "en" }), {
    wrapper: wrap(c, { urls, navigate }),
  });
  expect(navigate).not.toHaveBeenCalled();
  content = link;
  rerender();
  expect(navigate).toHaveBeenCalledWith("/es/watch/G4VRQ3ZQ5/carrera-nocturna?t=12#c", { replace: true });
  expect(result.current.url).toBe("https://example.com/es/watch/G4VRQ3ZQ5/carrera-nocturna");
  const head = () => [...document.head.children].map((e) => e.outerHTML);
  expect(head()).toEqual([
    '<link rel="canonical" href="https://example.com/es/watch/G4VRQ3ZQ5/carrera-nocturna">',
    '<meta property="og:url" content="https://example.com/es/watch/G4VRQ3ZQ5/carrera-nocturna">',
    '<meta property="og:title" content="Night run">',
    '<link rel="alternate" hreflang="en" href="https://example.com/en/watch/G4VRQ3ZQ5/night-run">',
    '<link rel="alternate" hreflang="es" href="https://example.com/es/watch/G4VRQ3ZQ5/carrera-nocturna">',
    '<link rel="alternate" hreflang="x-default" href="https://example.com/en/watch/G4VRQ3ZQ5/night-run">',
  ]);
  unmount();
  expect(head()).toEqual([]);
});

it("useCanonicalContent leaves none of a server-rendered page's tags to the next page after SPA navigation", () => {
  const c = offline();
  const urls = createContentURLs({ routes: { post: "p", channel: "c" }, languages: ["en", "es"], origin: "https://example.com" });
  const post: ContentLink = { content_kind: "post", code: "G4VRQ3ZQ5", slug: "night-run", slugs: { es: "carrera-nocturna" } };
  const channel: ContentLink = { content_kind: "channel", code: "H7KMN2PQ8", slug: "studio" };
  // The server rendered the post page's head.
  document.head.innerHTML = [
    '<link rel="canonical" href="https://example.com/en/p/G4VRQ3ZQ5/night-run">',
    '<meta property="og:url" content="https://example.com/en/p/G4VRQ3ZQ5/night-run">',
    '<meta property="og:title" content="Night run">',
    '<meta property="og:image" content="https://example.com/night-run.webp">',
    '<link rel="alternate" hreflang="en" href="https://example.com/en/p/G4VRQ3ZQ5/night-run">',
    '<link rel="alternate" hreflang="es" href="https://example.com/es/p/G4VRQ3ZQ5/carrera-nocturna">',
  ].join("");
  const head = () => [...document.head.children].map((e) => e.outerHTML).sort();
  const page = (link: ContentLink, location: string, title: string, image?: string) =>
    renderHook(() => useCanonicalContent(link, { title, image, location }), { wrapper: wrap(c, { urls, navigate: vi.fn() }) });

  const first = page(post, "/en/p/G4VRQ3ZQ5/night-run", "Night run", "https://example.com/night-run.webp");
  expect(document.head.querySelectorAll("link[rel=canonical]")).toHaveLength(1);
  expect(head()).toHaveLength(6);
  first.unmount();
  expect(head()).toEqual([]);

  // The router shows the channel page: its tags only, none of the post's.
  page(channel, "/en/c/H7KMN2PQ8/studio", "Studio");
  expect(head()).toEqual(
    [
      '<link rel="canonical" href="https://example.com/en/c/H7KMN2PQ8/studio">',
      '<meta property="og:url" content="https://example.com/en/c/H7KMN2PQ8/studio">',
      '<meta property="og:title" content="Studio">',
      '<link rel="alternate" hreflang="en" href="https://example.com/en/c/H7KMN2PQ8/studio">',
      '<link rel="alternate" hreflang="es" href="https://example.com/es/c/H7KMN2PQ8/studio">',
    ].sort(),
  );
});

it("useCanonicalContent without navigate replaces history in place", () => {
  const c = offline();
  const urls = createContentURLs({ routes: { video: "watch" } });
  history.replaceState(null, "", "/watch/G4VRQ3ZQ5");
  renderHook(() => useCanonicalContent({ content_kind: "video", code: "G4VRQ3ZQ5", slug: "night-run" }), { wrapper: wrap(c, { urls }) });
  expect(location.pathname).toBe("/watch/G4VRQ3ZQ5/night-run");
  expect(document.head.querySelector("link[rel=canonical]")?.getAttribute("href")).toBe(`${location.origin}/watch/G4VRQ3ZQ5/night-run`);
});
